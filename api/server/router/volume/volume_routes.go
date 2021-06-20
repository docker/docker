package volume // import "github.com/docker/docker/api/server/router/volume"

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/docker/docker/api/server/httputils"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/versions"
	volumetypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/volume/service/opts"
	"github.com/pkg/errors"
)

const (
	// clusterVolumesVersion defines the API version that swarm cluster volume
	// functionality was introduced. avoids the use of magic numbers.
	clusterVolumesVersion = "1.42"
)

// isNoSwarmErr is a helper function that checks if the given error is a result
// of swarm not being initialized on this node. by abstracting this to a
// function, if we later change how we detect that no swarm is initialized, we
// only need to do it in one place.
func isNoSwarmErr(err error) bool {
	// TODO(dperny): there's no specific error type for swarm not initialized,
	// but only the error for swarm not initialized contains the substring
	// "docker swarm init", so that's what we can check. we should probably
	// amend the cluster backend to provide a more specific error type to
	// indicate this.
	return err != nil &&
		errdefs.IsUnavailable(err) &&
		strings.Contains(err.Error(), "docker swarm init")
}

func (v *volumeRouter) getVolumesList(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	filters, err := filters.FromJSON(r.Form.Get("filters"))
	if err != nil {
		return errdefs.InvalidParameter(errors.Wrap(err, "error reading volume filters"))
	}
	volumes, warnings, err := v.backend.List(ctx, filters)
	if err != nil {
		return err
	}

	version := httputils.VersionFromContext(ctx)
	if versions.GreaterThanOrEqualTo(version, clusterVolumesVersion) {
		clusterVolumes, swarmErr := v.cluster.GetVolumes(types.VolumeListOptions{Filters: filters})
		if swarmErr != nil && !isNoSwarmErr(swarmErr) {
			// if there is a swarm error, we may not want to error out right
			// away. the local list probably worked. instead, let's do what we
			// do if there's a bad driver while trying to list: add the error
			// to the warnings. don't do this if swarm is not initialized.
			warnings = append(warnings, swarmErr.Error())
		}
		// add the cluster volumes to the return
		volumes = append(volumes, clusterVolumes...)
	}

	return httputils.WriteJSON(w, http.StatusOK, &volumetypes.VolumeListOKBody{Volumes: volumes, Warnings: warnings})
}

func (v *volumeRouter) getVolumeByName(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}
	version := httputils.VersionFromContext(ctx)

	volume, err := v.backend.Get(ctx, vars["name"], opts.WithGetResolveStatus)

	// if the volume is not found in the regular volume backend, and the client
	// is using an API version greater than 1.42 (when cluster volumes were
	// introduced), then check if Swarm has the volume.
	if errdefs.IsNotFound(err) && versions.GreaterThanOrEqualTo(version, clusterVolumesVersion) {
		var (
			swarmVol types.Volume
			swarmErr error
		)
		swarmVol, swarmErr = v.cluster.GetVolume(vars["name"])
		// if swarm returns an error and that error indicates that swarm is not
		// initialized, return original NotFound error. Otherwise, we'd return
		// a weird swarm unavailable error on non-swarm engines.
		if swarmErr != nil {
			if !isNoSwarmErr(swarmErr) {
				return swarmErr
			} else {
				return err
			}
		}
		volume = &swarmVol
	} else if err != nil {
		// otherwise, if this isn't NotFound, or this isn't a high enough version,
		// just return the error by itself.
		return err
	}

	return httputils.WriteJSON(w, http.StatusOK, volume)
}

func (v *volumeRouter) postVolumesCreate(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	if err := httputils.CheckForJSON(r); err != nil {
		return err
	}

	version := httputils.VersionFromContext(ctx)

	var req volumetypes.VolumeCreateBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if err == io.EOF {
			return errdefs.InvalidParameter(errors.New("got EOF while reading request body"))
		}
		return errdefs.InvalidParameter(err)
	}

	var (
		volume *types.Volume
		err    error
	)

	// if the ClusterVolumeSpec is filled in, then this is a cluster volume
	// and is created through the swarm cluster volume backend.
	//
	// TODO(dperny): VERY IMPORTANT DO NOT SHIP WITHOUT FIX:
	//
	//   we MUST create a way to prevent duplication of names between regular
	//   and cluster volumes.
	//
	if req.ClusterVolumeSpec != nil && versions.GreaterThanOrEqualTo(version, clusterVolumesVersion) {
		volume, err = v.cluster.CreateVolume(req)
	} else {
		volume, err = v.backend.Create(ctx, req.Name, req.Driver, opts.WithCreateOptions(req.DriverOpts), opts.WithCreateLabels(req.Labels))
	}

	if err != nil {
		return err
	}
	return httputils.WriteJSON(w, http.StatusCreated, volume)
}

func (v *volumeRouter) postVolumesUpdate(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}

	if err := httputils.CheckForJSON(r); err != nil {
		return err
	}

	rawVersion := r.URL.Query().Get("version")
	version, err := strconv.ParseUint(rawVersion, 10, 64)
	if err != nil {
		fmt.Errorf("invalid swarm object version '%s': %v", rawVersion, err)
		return errdefs.InvalidParameter(err)
	}

	var req volumetypes.VolumeUpdateBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if err == io.EOF {
			return errdefs.InvalidParameter(errors.New("got EOF while reaading request body"))
		}
		return errdefs.InvalidParameter(err)
	}

	if err := v.cluster.UpdateVolume(vars["name"], version, req); err != nil {
		if errdefs.IsUnavailable(err) {
			return errors.Wrapf(err, "volume update only valid for cluster volumes, but swarm unavailable")
		}
		return err
	}

	return nil
}

func (v *volumeRouter) deleteVolumes(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	if err := httputils.ParseForm(r); err != nil {
		return err
	}
	force := httputils.BoolValue(r, "force")

	version := httputils.VersionFromContext(ctx)

	err := v.backend.Remove(ctx, vars["name"], opts.WithPurgeOnError(force))
	if err != nil && errdefs.IsNotFound(err) && versions.GreaterThanOrEqualTo(version, clusterVolumesVersion) {
		swarmErr := v.cluster.RemoveVolume(vars["name"])
		if swarmErr != nil {
			if !isNoSwarmErr(swarmErr) {
				return swarmErr
			}
			return err
		}
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

	pruneReport, err := v.backend.Prune(ctx, pruneFilters)
	if err != nil {
		return err
	}
	return httputils.WriteJSON(w, http.StatusOK, pruneReport)
}
