package volume

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/v2/daemon/internal/filters"
	"github.com/moby/moby/v2/daemon/internal/versions"
	"github.com/moby/moby/v2/daemon/server/httputils"
	"github.com/moby/moby/v2/daemon/server/volumebackend"
	"github.com/moby/moby/v2/daemon/volume/service/opts"
	"github.com/moby/moby/v2/errdefs"
	"github.com/pkg/errors"
)

const (
	// clusterVolumesVersion defines the API version that swarm cluster volume
	// functionality was introduced. avoids the use of magic numbers.
	clusterVolumesVersion = "1.42"
)

func (v *volumeRouter) getVolumesList(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	f, err := filters.FromJSON(r.Form.Get("filters"))
	if err != nil {
		return errors.Wrap(err, "error reading volume filters")
	}
	volumes, warnings, err := v.backend.List(ctx, f)
	if err != nil {
		return err
	}

	version := httputils.VersionFromContext(ctx)
	if versions.GreaterThanOrEqualTo(version, clusterVolumesVersion) && v.cluster.IsManager() {
		clusterVolumes, swarmErr := v.cluster.GetVolumes(volumebackend.ListOptions{Filters: f})
		if swarmErr != nil {
			// if there is a swarm error, we may not want to error out right
			// away. the local list probably worked. instead, let's do what we
			// do if there's a bad driver while trying to list: add the error
			// to the warnings. don't do this if swarm is not initialized.
			warnings = append(warnings, swarmErr.Error())
		}
		// add the cluster volumes to the return
		volumes = append(volumes, clusterVolumes...)
	}

	return httputils.WriteJSON(w, http.StatusOK, &volume.ListResponse{Volumes: volumes, Warnings: warnings})
}

func (v *volumeRouter) getVolumeByName(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}
	version := httputils.VersionFromContext(ctx)

	// re: volume name duplication
	//
	// we prefer to get volumes locally before attempting to get them from the
	// cluster. Local volumes can only be looked up by name, but cluster
	// volumes can also be looked up by ID.
	vol, err := v.backend.Get(ctx, vars["name"], opts.WithGetResolveStatus, opts.WithGetResolveSize)

	// if the volume is not found in the regular volume backend, and the client
	// is using an API version greater than 1.42 (when cluster volumes were
	// introduced), then check if Swarm has the volume.
	if cerrdefs.IsNotFound(err) && versions.GreaterThanOrEqualTo(version, clusterVolumesVersion) && v.cluster.IsManager() {
		swarmVol, err := v.cluster.GetVolume(vars["name"])
		// if swarm returns an error and that error indicates that swarm is not
		// initialized, return original NotFound error. Otherwise, we'd return
		// a weird swarm unavailable error on non-swarm engines.
		if err != nil {
			return err
		}
		vol = &swarmVol
	} else if err != nil {
		// otherwise, if this isn't NotFound, or this isn't a high enough version,
		// just return the error by itself.
		return err
	}

	return httputils.WriteJSON(w, http.StatusOK, vol)
}

func (v *volumeRouter) postVolumesCreate(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	var req volume.CreateRequest
	if err := httputils.ReadJSON(r, &req); err != nil {
		return err
	}

	var (
		vol     *volume.Volume
		err     error
		version = httputils.VersionFromContext(ctx)
	)

	// if the ClusterVolumeSpec is filled in, then this is a cluster volume
	// and is created through the swarm cluster volume backend.
	//
	// re: volume name duplication
	//
	// As it happens, there is no good way to prevent duplication of a volume
	// name between local and cluster volumes. This is because Swarm volumes
	// can be created from any manager node, bypassing most of the protections
	// we could put into the engine side.
	//
	// Instead, we will allow creating a volume with a duplicate name, which
	// should not break anything.
	if req.ClusterVolumeSpec != nil && versions.GreaterThanOrEqualTo(version, clusterVolumesVersion) {
		log.G(ctx).Debug("using cluster volume")
		vol, err = v.cluster.CreateVolume(req)
	} else {
		log.G(ctx).Debug("using regular volume")
		vol, err = v.backend.Create(ctx, req.Name, req.Driver, opts.WithCreateOptions(req.DriverOpts), opts.WithCreateLabels(req.Labels))
	}

	if err != nil {
		return err
	}
	return httputils.WriteJSON(w, http.StatusCreated, vol)
}

func (v *volumeRouter) putVolumesUpdate(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if !v.cluster.IsManager() {
		return errdefs.Unavailable(errors.New("volume update only valid for cluster volumes, but swarm is unavailable"))
	}

	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	rawVersion := r.URL.Query().Get("version")
	version, err := strconv.ParseUint(rawVersion, 10, 64)
	if err != nil {
		err = fmt.Errorf("invalid swarm object version '%s': %v", rawVersion, err)
		return errdefs.InvalidParameter(err)
	}

	var req volumebackend.UpdateOptions
	if err := httputils.ReadJSON(r, &req); err != nil {
		return err
	}

	return v.cluster.UpdateVolume(vars["name"], version, req)
}

func (v *volumeRouter) deleteVolumes(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}
	force := httputils.BoolValue(r, "force")

	// First we try deleting local volume. The volume may not be found as a
	// local volume, but could be a cluster volume, so we ignore "not found"
	// errors at this stage. Note that no "not found" error is produced if
	// "force" is enabled.
	err := v.backend.Remove(ctx, vars["name"], opts.WithPurgeOnError(force))
	if err != nil && !cerrdefs.IsNotFound(err) {
		return err
	}

	// If no volume was found, the volume may be a cluster volume. If force
	// is enabled, the volume backend won't return an error for non-existing
	// volumes, so we don't know if removal succeeded (or not volume existed).
	// In that case we always try to delete cluster volumes as well.
	if cerrdefs.IsNotFound(err) || force {
		version := httputils.VersionFromContext(ctx)
		if versions.GreaterThanOrEqualTo(version, clusterVolumesVersion) && v.cluster.IsManager() {
			err = v.cluster.RemoveVolume(vars["name"], force)
		}
	}

	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (v *volumeRouter) postVolumesPrune(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	pruneFilters, err := filters.FromJSON(r.Form.Get("filters"))
	if err != nil {
		return err
	}

	// API version 1.42 changes behavior where prune should only prune anonymous volumes.
	// To keep older API behavior working, we need to add this filter option to consider all (local) volumes for pruning, not just anonymous ones.
	if versions.LessThan(httputils.VersionFromContext(ctx), "1.42") {
		pruneFilters.Add("all", "true")
	}

	pruneReport, err := v.backend.Prune(ctx, pruneFilters)
	if err != nil {
		return err
	}
	return httputils.WriteJSON(w, http.StatusOK, pruneReport)
}
