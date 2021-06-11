package service // import "github.com/docker/docker/volume/service"

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/volume"
	"github.com/docker/docker/volume/drivers"
	volumemounts "github.com/docker/docker/volume/mounts"
	"github.com/docker/docker/volume/service/opts"
	"github.com/moby/locker"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	bolt "go.etcd.io/bbolt"
)

const (
	volumeDataDir = "volumes"
)

type volumeWrapper struct {
	volume.Volume
	labels  map[string]string
	scope   string
	options map[string]string
}

func (v volumeWrapper) Options() map[string]string {
	if v.options == nil {
		return nil
	}
	options := make(map[string]string, len(v.options))
	for key, value := range v.options {
		options[key] = value
	}
	return options
}

func (v volumeWrapper) Labels() map[string]string {
	if v.labels == nil {
		return nil
	}

	labels := make(map[string]string, len(v.labels))
	for key, value := range v.labels {
		labels[key] = value
	}
	return labels
}

func (v volumeWrapper) Scope() string {
	return v.scope
}

func (v volumeWrapper) CachedPath() string {
	if vv, ok := v.Volume.(interface {
		CachedPath() string
	}); ok {
		return vv.CachedPath()
	}
	return v.Volume.Path()
}

// StoreOpt sets options for a VolumeStore
type StoreOpt func(store *VolumeStore) error

// NewStore creates a new volume store at the given path
func NewStore(rootPath string, drivers *drivers.Store, opts ...StoreOpt) (*VolumeStore, error) {
	vs := &VolumeStore{
		locks:   &locker.Locker{},
		names:   make(map[string]volume.Volume),
		refs:    make(map[string]map[string]struct{}),
		labels:  make(map[string]map[string]string),
		options: make(map[string]map[string]string),
		drivers: drivers,
	}

	for _, o := range opts {
		if err := o(vs); err != nil {
			return nil, err
		}
	}

	if rootPath != "" {
		// initialize metadata store
		volPath := filepath.Join(rootPath, volumeDataDir)
		if err := os.MkdirAll(volPath, 0750); err != nil {
			return nil, err
		}

		var err error
		vs.db, err = bolt.Open(filepath.Join(volPath, "metadata.db"), 0600, &bolt.Options{Timeout: 1 * time.Second})
		if err != nil {
			return nil, errors.Wrap(err, "error while opening volume store metadata database")
		}

		// initialize volumes bucket
		if err := vs.db.Update(func(tx *bolt.Tx) error {
			if _, err := tx.CreateBucketIfNotExists(volumeBucketName); err != nil {
				return errors.Wrap(err, "error while setting up volume store metadata database")
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}

	vs.restore()

	return vs, nil
}

// WithEventLogger configures the VolumeStore with the given VolumeEventLogger
func WithEventLogger(logger VolumeEventLogger) StoreOpt {
	return func(store *VolumeStore) error {
		store.eventLogger = logger
		return nil
	}
}

func (s *VolumeStore) getNamed(name string) (volume.Volume, bool) {
	s.globalLock.RLock()
	v, exists := s.names[name]
	s.globalLock.RUnlock()
	return v, exists
}

func (s *VolumeStore) setNamed(v volume.Volume, ref string) {
	name := v.Name()

	s.globalLock.Lock()
	s.names[name] = v
	if len(ref) > 0 {
		if s.refs[name] == nil {
			s.refs[name] = make(map[string]struct{})
		}
		s.refs[name][ref] = struct{}{}
	}
	s.globalLock.Unlock()
}

// hasRef returns true if the given name has at least one ref.
// Callers of this function are expected to hold the name lock.
func (s *VolumeStore) hasRef(name string) bool {
	s.globalLock.RLock()
	l := len(s.refs[name])
	s.globalLock.RUnlock()
	return l > 0
}

// getRefs gets the list of refs for a given name
// Callers of this function are expected to hold the name lock.
func (s *VolumeStore) getRefs(name string) []string {
	s.globalLock.RLock()
	defer s.globalLock.RUnlock()

	refs := make([]string, 0, len(s.refs[name]))
	for r := range s.refs[name] {
		refs = append(refs, r)
	}

	return refs
}

// purge allows the cleanup of internal data on docker in case
// the internal data is out of sync with volumes driver plugins.
func (s *VolumeStore) purge(ctx context.Context, name string) error {
	s.globalLock.Lock()
	defer s.globalLock.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	v, exists := s.names[name]
	if exists {
		driverName := v.DriverName()
		if _, err := s.drivers.ReleaseDriver(driverName); err != nil {
			logrus.WithError(err).WithField("driver", driverName).Error("Error releasing reference to volume driver")
		}
	}
	if err := s.removeMeta(name); err != nil {
		logrus.Errorf("Error removing volume metadata for volume %q: %v", name, err)
	}
	delete(s.names, name)
	delete(s.refs, name)
	delete(s.labels, name)
	delete(s.options, name)
	return nil
}

// VolumeStore is responsible for storing and reference counting volumes.
type VolumeStore struct {
	// locks ensures that only one action is being performed on a particular volume at a time without locking the entire store
	// since actions on volumes can be quite slow, this ensures the store is free to handle requests for other volumes.
	locks   *locker.Locker
	drivers *drivers.Store
	// globalLock is used to protect access to mutable structures used by the store object
	globalLock sync.RWMutex
	// names stores the volume name -> volume relationship.
	// This is used for making lookups faster so we don't have to probe all drivers
	names map[string]volume.Volume
	// refs stores the volume name and the list of things referencing it
	refs map[string]map[string]struct{}
	// labels stores volume labels for each volume
	labels map[string]map[string]string
	// options stores volume options for each volume
	options map[string]map[string]string

	db          *bolt.DB
	eventLogger VolumeEventLogger
}

func filterByDriver(names []string) filterFunc {
	return func(v volume.Volume) bool {
		for _, name := range names {
			if name == v.DriverName() {
				return true
			}
		}
		return false
	}
}

func (s *VolumeStore) byReferenced(referenced bool) filterFunc {
	return func(v volume.Volume) bool {
		return s.hasRef(v.Name()) == referenced
	}
}

func (s *VolumeStore) filter(ctx context.Context, vols *[]volume.Volume, by By) (warnings []string, err error) {
	// note that this specifically does not support the `FromList` By type.
	switch f := by.(type) {
	case nil:
		if *vols == nil {
			var ls []volume.Volume
			ls, warnings, err = s.list(ctx)
			if err != nil {
				return warnings, err
			}
			*vols = ls
		}
	case byDriver:
		if *vols != nil {
			filter(vols, filterByDriver([]string(f)))
			return nil, nil
		}
		var ls []volume.Volume
		ls, warnings, err = s.list(ctx, []string(f)...)
		if err != nil {
			return nil, err
		}
		*vols = ls
	case ByReferenced:
		// TODO(@cpuguy83): It would be nice to optimize this by looking at the list
		// of referenced volumes, however the locking strategy makes this difficult
		// without either providing inconsistent data or deadlocks.
		if *vols == nil {
			var ls []volume.Volume
			ls, warnings, err = s.list(ctx)
			if err != nil {
				return nil, err
			}
			*vols = ls
		}
		filter(vols, s.byReferenced(bool(f)))
	case andCombinator:
		for _, by := range f {
			w, err := s.filter(ctx, vols, by)
			if err != nil {
				return warnings, err
			}
			warnings = append(warnings, w...)
		}
	case orCombinator:
		for _, by := range f {
			switch by.(type) {
			case byDriver:
				var ls []volume.Volume
				w, err := s.filter(ctx, &ls, by)
				if err != nil {
					return warnings, err
				}
				warnings = append(warnings, w...)
			default:
				ls, w, err := s.list(ctx)
				if err != nil {
					return warnings, err
				}
				warnings = append(warnings, w...)
				w, err = s.filter(ctx, &ls, by)
				if err != nil {
					return warnings, err
				}
				warnings = append(warnings, w...)
				*vols = append(*vols, ls...)
			}
		}
		unique(vols)
	case CustomFilter:
		if *vols == nil {
			var ls []volume.Volume
			ls, warnings, err = s.list(ctx)
			if err != nil {
				return nil, err
			}
			*vols = ls
		}
		filter(vols, filterFunc(f))
	default:
		return nil, errdefs.InvalidParameter(errors.Errorf("unsupported filter: %T", f))
	}
	return warnings, nil
}

func unique(ls *[]volume.Volume) {
	names := make(map[string]bool, len(*ls))
	filter(ls, func(v volume.Volume) bool {
		if names[v.Name()] {
			return false
		}
		names[v.Name()] = true
		return true
	})
}

// Find lists volumes filtered by the past in filter.
// If a driver returns a volume that has name which conflicts with another volume from a different driver,
// the first volume is chosen and the conflicting volume is dropped.
func (s *VolumeStore) Find(ctx context.Context, by By) (vols []volume.Volume, warnings []string, err error) {
	logrus.WithField("ByType", fmt.Sprintf("%T", by)).WithField("ByValue", fmt.Sprintf("%+v", by)).Debug("VolumeStore.Find")
	switch f := by.(type) {
	case nil, orCombinator, andCombinator, byDriver, ByReferenced, CustomFilter:
		warnings, err = s.filter(ctx, &vols, by)
	case fromList:
		warnings, err = s.filter(ctx, f.ls, f.by)
	default:
		// Really shouldn't be possible, but makes sure that any new By's are added to this check.
		err = errdefs.InvalidParameter(errors.Errorf("unsupported filter type: %T", f))
	}
	if err != nil {
		return nil, nil, &OpErr{Err: err, Op: "list"}
	}

	var out []volume.Volume

	for _, v := range vols {
		name := normalizeVolumeName(v.Name())

		s.locks.Lock(name)
		storedV, exists := s.getNamed(name)
		// Note: it's not safe to populate the cache here because the volume may have been
		// deleted before we acquire a lock on its name
		if exists && storedV.DriverName() != v.DriverName() {
			logrus.Warnf("Volume name %s already exists for driver %s, not including volume returned by %s", v.Name(), storedV.DriverName(), v.DriverName())
			s.locks.Unlock(v.Name())
			continue
		}

		out = append(out, v)
		s.locks.Unlock(v.Name())
	}
	return out, warnings, nil
}

type filterFunc func(volume.Volume) bool

func filter(vols *[]volume.Volume, fn filterFunc) {
	var evict []int
	for i, v := range *vols {
		if !fn(v) {
			evict = append(evict, i)
		}
	}

	for n, i := range evict {
		copy((*vols)[i-n:], (*vols)[i-n+1:])
		(*vols)[len(*vols)-1] = nil
		*vols = (*vols)[:len(*vols)-1]
	}
}

// list goes through each volume driver and asks for its list of volumes.
// TODO(@cpuguy83): plumb context through
func (s *VolumeStore) list(ctx context.Context, driverNames ...string) ([]volume.Volume, []string, error) {
	var (
		ls       = []volume.Volume{} // do not return a nil value as this affects filtering
		warnings []string
	)

	var dls []volume.Driver

	all, err := s.drivers.GetAllDrivers()
	if err != nil {
		return nil, nil, err
	}
	if len(driverNames) == 0 {
		dls = all
	} else {
		idx := make(map[string]bool, len(driverNames))
		for _, name := range driverNames {
			idx[name] = true
		}
		for _, d := range all {
			if idx[d.Name()] {
				dls = append(dls, d)
			}
		}
	}

	type vols struct {
		vols       []volume.Volume
		err        error
		driverName string
	}
	chVols := make(chan vols, len(dls))

	for _, vd := range dls {
		go func(d volume.Driver) {
			vs, err := d.List()
			if err != nil {
				chVols <- vols{driverName: d.Name(), err: &OpErr{Err: err, Name: d.Name(), Op: "list"}}
				return
			}
			for i, v := range vs {
				s.globalLock.RLock()
				vs[i] = volumeWrapper{v, s.labels[v.Name()], d.Scope(), s.options[v.Name()]}
				s.globalLock.RUnlock()
			}

			chVols <- vols{vols: vs}
		}(vd)
	}

	badDrivers := make(map[string]struct{})
	for i := 0; i < len(dls); i++ {
		vs := <-chVols

		if vs.err != nil {
			warnings = append(warnings, vs.err.Error())
			badDrivers[vs.driverName] = struct{}{}
		}
		ls = append(ls, vs.vols...)
	}

	if len(badDrivers) > 0 {
		s.globalLock.RLock()
		for _, v := range s.names {
			if _, exists := badDrivers[v.DriverName()]; exists {
				ls = append(ls, v)
			}
		}
		s.globalLock.RUnlock()
	}
	return ls, warnings, nil
}

// Create creates a volume with the given name and driver
// If the volume needs to be created with a reference to prevent race conditions
// with volume cleanup, make sure to use the `CreateWithReference` option.
func (s *VolumeStore) Create(ctx context.Context, name, driverName string, createOpts ...opts.CreateOption) (volume.Volume, error) {
	var cfg opts.CreateConfig
	for _, o := range createOpts {
		o(&cfg)
	}

	name = normalizeVolumeName(name)
	s.locks.Lock(name)
	defer s.locks.Unlock(name)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	v, created, err := s.create(ctx, name, driverName, cfg.Options, cfg.Labels)
	if err != nil {
		if _, ok := err.(*OpErr); ok {
			return nil, err
		}
		return nil, &OpErr{Err: err, Name: name, Op: "create"}
	}

	if created && s.eventLogger != nil {
		s.eventLogger.LogVolumeEvent(v.Name(), "create", map[string]string{"driver": v.DriverName()})
	}
	s.setNamed(v, cfg.Reference)
	return v, nil
}

// checkConflict checks the local cache for name collisions with the passed in name,
// for existing volumes with the same name but in a different driver.
// This is used by `Create` as a best effort to prevent name collisions for volumes.
// If a matching volume is found that is not a conflict that is returned so the caller
// does not need to perform an additional lookup.
// When no matching volume is found, both returns will be nil
//
// Note: This does not probe all the drivers for name collisions because v1 plugins
// are very slow, particularly if the plugin is down, and cause other issues,
// particularly around locking the store.
// TODO(cpuguy83): With v2 plugins this shouldn't be a problem. Could also potentially
// use a connect timeout for this kind of check to ensure we aren't blocking for a
// long time.
func (s *VolumeStore) checkConflict(ctx context.Context, name, driverName string) (volume.Volume, error) {
	// check the local cache
	v, _ := s.getNamed(name)
	if v == nil {
		return nil, nil
	}

	vDriverName := v.DriverName()
	var conflict bool
	if driverName != "" {
		// Retrieve canonical driver name to avoid inconsistencies (for example
		// "plugin" vs. "plugin:latest")
		vd, err := s.drivers.GetDriver(driverName)
		if err != nil {
			return nil, err
		}

		if vDriverName != vd.Name() {
			conflict = true
		}
	}

	// let's check if the found volume ref
	// is stale by checking with the driver if it still exists
	exists, err := volumeExists(ctx, s.drivers, v)
	if err != nil {
		return nil, errors.Wrapf(errNameConflict, "found reference to volume '%s' in driver '%s', but got an error while checking the driver: %v", name, vDriverName, err)
	}

	if exists {
		if conflict {
			return nil, errors.Wrapf(errNameConflict, "driver '%s' already has volume '%s'", vDriverName, name)
		}
		return v, nil
	}

	if s.hasRef(v.Name()) {
		// Containers are referencing this volume but it doesn't seem to exist anywhere.
		// Return a conflict error here, the user can fix this with `docker volume rm -f`
		return nil, errors.Wrapf(errNameConflict, "found references to volume '%s' in driver '%s' but the volume was not found in the driver -- you may need to remove containers referencing this volume or force remove the volume to re-create it", name, vDriverName)
	}

	// doesn't exist, so purge it from the cache
	s.purge(ctx, name)
	return nil, nil
}

// volumeExists returns if the volume is still present in the driver.
// An error is returned if there was an issue communicating with the driver.
func volumeExists(ctx context.Context, store *drivers.Store, v volume.Volume) (bool, error) {
	exists, err := lookupVolume(ctx, store, v.DriverName(), v.Name())
	if err != nil {
		return false, err
	}
	return exists != nil, nil
}

// create asks the given driver to create a volume with the name/opts.
// If a volume with the name is already known, it will ask the stored driver for the volume.
// If the passed in driver name does not match the driver name which is stored
//  for the given volume name, an error is returned after checking if the reference is stale.
// If the reference is stale, it will be purged and this create can continue.
// It is expected that callers of this function hold any necessary locks.
func (s *VolumeStore) create(ctx context.Context, name, driverName string, opts, labels map[string]string) (volume.Volume, bool, error) {
	// Validate the name in a platform-specific manner

	// volume name validation is specific to the host os and not on container image
	parser := volumemounts.NewParser()
	err := parser.ValidateVolumeName(name)
	if err != nil {
		return nil, false, err
	}

	v, err := s.checkConflict(ctx, name, driverName)
	if err != nil {
		return nil, false, err
	}

	if v != nil {
		// there is an existing volume, if we already have this stored locally, return it.
		// TODO: there could be some inconsistent details such as labels here
		if vv, _ := s.getNamed(v.Name()); vv != nil {
			return vv, false, nil
		}
	}

	// Since there isn't a specified driver name, let's see if any of the existing drivers have this volume name
	if driverName == "" {
		v, _ = s.getVolume(ctx, name, "")
		if v != nil {
			return v, false, nil
		}
	}

	if driverName == "" {
		driverName = volume.DefaultDriverName
	}
	vd, err := s.drivers.CreateDriver(driverName)
	if err != nil {
		return nil, false, &OpErr{Op: "create", Name: name, Err: err}
	}

	logrus.Debugf("Registering new volume reference: driver %q, name %q", vd.Name(), name)
	if v, _ = vd.Get(name); v == nil {
		v, err = vd.Create(name, opts)
		if err != nil {
			if _, err := s.drivers.ReleaseDriver(driverName); err != nil {
				logrus.WithError(err).WithField("driver", driverName).Error("Error releasing reference to volume driver")
			}
			return nil, false, err
		}
	}

	s.globalLock.Lock()
	s.labels[name] = labels
	s.options[name] = opts
	s.refs[name] = make(map[string]struct{})
	s.globalLock.Unlock()

	metadata := volumeMetadata{
		Name:    name,
		Driver:  vd.Name(),
		Labels:  labels,
		Options: opts,
	}

	if err := s.setMeta(name, metadata); err != nil {
		return nil, true, err
	}
	return volumeWrapper{v, labels, vd.Scope(), opts}, true, nil
}

// Get looks if a volume with the given name exists and returns it if so
func (s *VolumeStore) Get(ctx context.Context, name string, getOptions ...opts.GetOption) (volume.Volume, error) {
	var cfg opts.GetConfig
	for _, o := range getOptions {
		o(&cfg)
	}
	name = normalizeVolumeName(name)
	s.locks.Lock(name)
	defer s.locks.Unlock(name)

	v, err := s.getVolume(ctx, name, cfg.Driver)
	if err != nil {
		return nil, &OpErr{Err: err, Name: name, Op: "get"}
	}
	if cfg.Driver != "" && v.DriverName() != cfg.Driver {
		return nil, &OpErr{Name: name, Op: "get", Err: errdefs.Conflict(errors.New("found volume driver does not match passed in driver"))}
	}
	s.setNamed(v, cfg.Reference)
	return v, nil
}

// getVolume requests the volume, if the driver info is stored it just accesses that driver,
// if the driver is unknown it probes all drivers until it finds the first volume with that name.
// it is expected that callers of this function hold any necessary locks
func (s *VolumeStore) getVolume(ctx context.Context, name, driverName string) (volume.Volume, error) {
	var meta volumeMetadata
	meta, err := s.getMeta(name)
	if err != nil {
		return nil, err
	}

	if driverName != "" {
		if meta.Driver == "" {
			meta.Driver = driverName
		}
		if driverName != meta.Driver {
			return nil, errdefs.Conflict(errors.New("provided volume driver does not match stored driver"))
		}
	}

	if driverName == "" {
		driverName = meta.Driver
	}
	if driverName == "" {
		s.globalLock.RLock()
		select {
		case <-ctx.Done():
			s.globalLock.RUnlock()
			return nil, ctx.Err()
		default:
		}
		v, exists := s.names[name]
		s.globalLock.RUnlock()
		if exists {
			meta.Driver = v.DriverName()
			if err := s.setMeta(name, meta); err != nil {
				return nil, err
			}
		}
	}

	if meta.Driver != "" {
		vol, err := lookupVolume(ctx, s.drivers, meta.Driver, name)
		if err != nil {
			return nil, err
		}
		if vol == nil {
			s.purge(ctx, name)
			return nil, errNoSuchVolume
		}

		var scope string
		vd, err := s.drivers.GetDriver(meta.Driver)
		if err == nil {
			scope = vd.Scope()
		}
		return volumeWrapper{vol, meta.Labels, scope, meta.Options}, nil
	}

	logrus.Debugf("Probing all drivers for volume with name: %s", name)
	drivers, err := s.drivers.GetAllDrivers()
	if err != nil {
		return nil, err
	}

	for _, d := range drivers {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		v, err := d.Get(name)
		if err != nil || v == nil {
			continue
		}
		meta.Driver = v.DriverName()
		if err := s.setMeta(name, meta); err != nil {
			return nil, err
		}
		return volumeWrapper{v, meta.Labels, d.Scope(), meta.Options}, nil
	}
	return nil, errNoSuchVolume
}

// lookupVolume gets the specified volume from the specified driver.
// This will only return errors related to communications with the driver.
// If the driver returns an error that is not communication related the
//   error is logged but not returned.
// If the volume is not found it will return `nil, nil``
// TODO(@cpuguy83): plumb through the context to lower level components
func lookupVolume(ctx context.Context, store *drivers.Store, driverName, volumeName string) (volume.Volume, error) {
	if driverName == "" {
		driverName = volume.DefaultDriverName
	}
	vd, err := store.GetDriver(driverName)
	if err != nil {
		return nil, errors.Wrapf(err, "error while checking if volume %q exists in driver %q", volumeName, driverName)
	}
	v, err := vd.Get(volumeName)
	if err != nil {
		var nErr net.Error
		if errors.As(err, &nErr) {
			if v != nil {
				volumeName = v.Name()
				driverName = v.DriverName()
			}
			return nil, errors.Wrapf(err, "error while checking if volume %q exists in driver %q", volumeName, driverName)
		}

		// At this point, the error could be anything from the driver, such as "no such volume"
		// Let's not check an error here, and instead check if the driver returned a volume
		logrus.WithError(err).WithField("driver", driverName).WithField("volume", volumeName).Debug("Error while looking up volume")
	}
	return v, nil
}

// Remove removes the requested volume. A volume is not removed if it has any refs
func (s *VolumeStore) Remove(ctx context.Context, v volume.Volume, rmOpts ...opts.RemoveOption) error {
	var cfg opts.RemoveConfig
	for _, o := range rmOpts {
		o(&cfg)
	}

	name := v.Name()
	s.locks.Lock(name)
	defer s.locks.Unlock(name)

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	if s.hasRef(name) {
		return &OpErr{Err: errVolumeInUse, Name: name, Op: "remove", Refs: s.getRefs(name)}
	}

	v, err := s.getVolume(ctx, name, v.DriverName())
	if err != nil {
		return err
	}

	vd, err := s.drivers.GetDriver(v.DriverName())
	if err != nil {
		return &OpErr{Err: err, Name: v.DriverName(), Op: "remove"}
	}

	logrus.Debugf("Removing volume reference: driver %s, name %s", v.DriverName(), name)
	vol := unwrapVolume(v)

	err = vd.Remove(vol)
	if err != nil {
		err = &OpErr{Err: err, Name: name, Op: "remove"}
	}

	if err == nil || cfg.PurgeOnError {
		if e := s.purge(ctx, name); e != nil && err == nil {
			err = e
		}
	}
	if err == nil && s.eventLogger != nil {
		s.eventLogger.LogVolumeEvent(v.Name(), "destroy", map[string]string{"driver": v.DriverName()})
	}
	return err
}

// Release releases the specified reference to the volume
func (s *VolumeStore) Release(ctx context.Context, name string, ref string) error {
	s.locks.Lock(name)
	defer s.locks.Unlock(name)
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	s.globalLock.Lock()
	defer s.globalLock.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	if s.refs[name] != nil {
		delete(s.refs[name], ref)
	}
	return nil
}

// CountReferences gives a count of all references for a given volume.
func (s *VolumeStore) CountReferences(v volume.Volume) int {
	name := normalizeVolumeName(v.Name())

	s.locks.Lock(name)
	defer s.locks.Unlock(name)
	s.globalLock.Lock()
	defer s.globalLock.Unlock()

	return len(s.refs[name])
}

func unwrapVolume(v volume.Volume) volume.Volume {
	if vol, ok := v.(volumeWrapper); ok {
		return vol.Volume
	}

	return v
}

// Shutdown releases all resources used by the volume store
// It does not make any changes to volumes, drivers, etc.
func (s *VolumeStore) Shutdown() error {
	return s.db.Close()
}
