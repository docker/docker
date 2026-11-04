package service

import (
	"context"
	"strconv"
	"sync/atomic"

	"github.com/containerd/log"
	"github.com/moby/moby/api/types/events"
	volumetypes "github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/v2/daemon/internal/directory"
	"github.com/moby/moby/v2/daemon/internal/filters"
	"github.com/moby/moby/v2/daemon/internal/idtools"
	"github.com/moby/moby/v2/daemon/internal/stringid"
	"github.com/moby/moby/v2/daemon/volume"
	"github.com/moby/moby/v2/daemon/volume/drivers"
	"github.com/moby/moby/v2/daemon/volume/service/opts"
	"github.com/moby/moby/v2/errdefs"
	"github.com/moby/moby/v2/pkg/plugingetter"
	"github.com/pkg/errors"
)

type driverLister interface {
	GetDriverList() []string
}

// VolumeEventLogger interface provides methods to log volume-related events
type VolumeEventLogger interface {
	// LogVolumeEvent generates an event related to a volume.
	LogVolumeEvent(volumeID string, action events.Action, attributes map[string]string)
}

// VolumesService manages access to volumes
// This is used as the main access point for volumes to higher level services and the API.
type VolumesService struct {
	vs           *VolumeStore
	ds           driverLister
	pruneRunning atomic.Bool
	eventLogger  VolumeEventLogger
}

// NewVolumeService creates a new volume service
func NewVolumeService(root string, pg plugingetter.PluginGetter, rootIDs idtools.Identity, logger VolumeEventLogger) (*VolumesService, error) {
	ds := drivers.NewStore(pg)
	if err := setupDefaultDriver(ds, root, rootIDs); err != nil {
		return nil, err
	}

	vs, err := NewStore(root, ds, WithEventLogger(logger))
	if err != nil {
		return nil, err
	}
	return &VolumesService{vs: vs, ds: ds, eventLogger: logger}, nil
}

// GetDriverList gets the list of registered volume drivers
func (s *VolumesService) GetDriverList() []string {
	return s.ds.GetDriverList()
}

// AnonymousLabel is the label used to indicate that a volume is anonymous
// This is set automatically on a volume when a volume is created without a name specified, and as such an id is generated for it.
const AnonymousLabel = "com.docker.volume.anonymous"

// Create creates a volume
// If the caller is creating this volume to be consumed immediately, it is
// expected that the caller specifies a reference ID.
// This reference ID will protect this volume from removal.
//
// A good example for a reference ID is a container's ID.
// When whatever is going to reference this volume is removed the caller should dereference the volume by calling `Release`.
func (s *VolumesService) Create(ctx context.Context, name, driverName string, options ...opts.CreateOption) (*volumetypes.Volume, error) {
	if name == "" {
		name = stringid.GenerateRandomID()
		if driverName == "" {
			driverName = volume.DefaultDriverName
		}
		options = append(options, opts.WithCreateLabel(AnonymousLabel, ""))
		log.G(ctx).WithFields(log.Fields{"volume": name, "driver": driverName}).Debug("Creating anonymous volume")
	} else {
		log.G(ctx).WithField("volume", name).Debug("Creating named volume")
	}
	v, err := s.vs.Create(ctx, name, driverName, options...)
	if err != nil {
		return nil, err
	}

	apiV := volumeToAPIType(v)
	return &apiV, nil
}

// Get returns details about a volume
func (s *VolumesService) Get(ctx context.Context, name string, getOpts ...opts.GetOption) (*volumetypes.Volume, error) {
	v, err := s.vs.Get(ctx, name, getOpts...)
	if err != nil {
		return nil, err
	}
	vol := volumeToAPIType(v)

	var cfg opts.GetConfig
	for _, o := range getOpts {
		o(&cfg)
	}

	if cfg.ResolveStatus {
		vol.Status = v.Status()
	}

	if cfg.ResolveSize {
		if vol.Mountpoint == "" {
			vol.Mountpoint = v.Path()
		}
		sz, err := directory.Size(ctx, vol.Mountpoint)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				log.G(ctx).WithFields(log.Fields{
					"error":  err,
					"volume": v.Name(),
				}).Warn("Failed to determine size of volume")
			}
			sz = -1
		}
		vol.UsageData = &volumetypes.UsageData{Size: sz, RefCount: int64(s.vs.CountReferences(v))}
	}
	return &vol, nil
}

// Mount mounts the volume
// Callers should specify a unique reference for each Mount/Unmount pair.
//
// Example:
// ```go
// mountID := "randomString"
// s.Mount(ctx, vol, mountID)
// s.Unmount(ctx, vol, mountID)
// ```
func (s *VolumesService) Mount(ctx context.Context, vol *volumetypes.Volume, ref string) (string, error) {
	v, err := s.vs.Get(ctx, vol.Name, opts.WithGetDriver(vol.Driver))
	if err != nil {
		if IsNotExist(err) {
			err = errdefs.NotFound(err)
		}
		return "", err
	}
	return v.Mount(ref)
}

// Unmount unmounts the volume.
// Note that depending on the implementation, the volume may still be mounted due to other resources using it.
//
// The reference specified here should be the same reference specified during `Mount` and should be
// unique for each mount/unmount pair.
// See `Mount` documentation for an example.
func (s *VolumesService) Unmount(ctx context.Context, vol *volumetypes.Volume, ref string) error {
	v, err := s.vs.Get(ctx, vol.Name, opts.WithGetDriver(vol.Driver))
	if err != nil {
		if IsNotExist(err) {
			err = errdefs.NotFound(err)
		}
		return err
	}
	return v.Unmount(ref)
}

// Release releases a volume reference
func (s *VolumesService) Release(ctx context.Context, name string, ref string) error {
	return s.vs.Release(ctx, name, ref)
}

// Remove removes a volume
// An error is returned if the volume is still referenced.
func (s *VolumesService) Remove(ctx context.Context, name string, rmOpts ...opts.RemoveOption) error {
	var cfg opts.RemoveConfig
	for _, o := range rmOpts {
		o(&cfg)
	}

	v, err := s.vs.Get(ctx, name)
	if err != nil {
		if IsNotExist(err) && cfg.PurgeOnError {
			return nil
		}
		return err
	}

	err = s.vs.Remove(ctx, v, rmOpts...)
	if IsNotExist(err) {
		err = nil
	} else if IsInUse(err) {
		err = errdefs.Conflict(err)
	} else if IsNotExist(err) && cfg.PurgeOnError {
		err = nil
	}
	return err
}

var acceptedPruneFilters = map[string]bool{
	"label":  true,
	"label!": true,
	// All tells the filter to consider all volumes not just anonymous ones.
	"all": true,
}

var acceptedListFilters = map[string]bool{
	"dangling": true,
	"name":     true,
	"driver":   true,
	"label":    true,
}

// LocalVolumesSize gets all local volumes and fetches their size on disk
// Note that this intentionally skips volumes which have mount options. Typically
// volumes with mount options are not really local even if they are using the
// local driver.
func (s *VolumesService) LocalVolumesSize(ctx context.Context) ([]volumetypes.Volume, error) {
	ls, _, err := s.vs.Find(ctx, And(ByDriver(volume.DefaultDriverName), CustomFilter(func(v volume.Volume) bool {
		dv, ok := v.(volume.DetailedVolume)
		return ok && len(dv.Options()) == 0
	})))
	if err != nil {
		return nil, err
	}
	return s.volumesToAPI(ctx, ls, calcSize(true)), nil
}

// Prune removes (local) volumes which match the past in filter arguments.
// Note that this intentionally skips volumes with mount options as there would
// be no space reclaimed in this case.
func (s *VolumesService) Prune(ctx context.Context, filter filters.Args) (*volumetypes.PruneReport, error) {
	if !s.pruneRunning.CompareAndSwap(false, true) {
		return nil, errdefs.Conflict(errors.New("a prune operation is already running"))
	}
	defer s.pruneRunning.Store(false)

	if err := withPrune(filter); err != nil {
		return nil, err
	}

	by, err := filtersToBy(filter, acceptedPruneFilters)
	if err != nil {
		return nil, err
	}
	ls, _, err := s.vs.Find(ctx, And(ByDriver(volume.DefaultDriverName), ByReferenced(false), by, CustomFilter(func(v volume.Volume) bool {
		dv, ok := v.(volume.DetailedVolume)
		return ok && len(dv.Options()) == 0
	})))
	if err != nil {
		return nil, err
	}

	rep := &volumetypes.PruneReport{VolumesDeleted: make([]string, 0, len(ls))}
	for _, v := range ls {
		select {
		case <-ctx.Done():
			err := ctx.Err()
			if err == context.Canceled {
				err = nil
			}
			return rep, err
		default:
		}

		vSize, err := directory.Size(ctx, v.Path())
		if err != nil {
			log.G(ctx).WithFields(log.Fields{
				"error":  err,
				"volume": v.Name(),
			}).Warn("could not determine size of volume")
		}
		if err := s.vs.Remove(ctx, v); err != nil {
			log.G(ctx).WithFields(log.Fields{
				"error":  err,
				"volume": v.Name(),
			}).Warn("Could not remove volume")
			continue
		}
		rep.SpaceReclaimed += uint64(vSize)
		rep.VolumesDeleted = append(rep.VolumesDeleted, v.Name())
	}
	s.eventLogger.LogVolumeEvent("", events.ActionPrune, map[string]string{
		"reclaimed": strconv.FormatInt(int64(rep.SpaceReclaimed), 10),
	})
	return rep, nil
}

// List gets the list of volumes which match the past in filters
// If filters is nil or empty all volumes are returned.
func (s *VolumesService) List(ctx context.Context, filter filters.Args) (volumes []volumetypes.Volume, warnings []string, _ error) {
	by, err := filtersToBy(filter, acceptedListFilters)
	if err != nil {
		return nil, nil, err
	}

	vols, warns, err := s.vs.Find(ctx, by)
	if err != nil {
		return nil, nil, err
	}

	return s.volumesToAPI(ctx, vols, useCachedPath(true)), warns, nil
}

// Shutdown shuts down the image service and dependencies
func (s *VolumesService) Shutdown() error {
	return s.vs.Shutdown()
}

// LiveRestoreVolume passes through the LiveRestoreVolume call to the volume if it is implemented
// otherwise it is a no-op.
func (s *VolumesService) LiveRestoreVolume(ctx context.Context, vol *volumetypes.Volume, ref string) error {
	v, err := s.vs.Get(ctx, vol.Name, opts.WithGetDriver(vol.Driver))
	if err != nil {
		return err
	}
	rlv, ok := v.(volume.LiveRestorer)
	if !ok {
		log.G(ctx).WithField("volume", vol.Name).Debugf("volume does not implement LiveRestoreVolume: %T", v)
		return nil
	}
	return rlv.LiveRestoreVolume(ctx, ref)
}
