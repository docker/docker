package images // import "github.com/docker/docker/daemon/images"

import (
	"context"
	"os"
	"sync"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/platforms"
	"github.com/docker/docker/container"
	daemonevents "github.com/docker/docker/daemon/events"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/system"
	"github.com/docker/docker/registry"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type containerStore interface {
	// used by image delete
	First(container.StoreFilter) *container.Container
	// used by image prune, and image list
	List() []*container.Container
	// TODO: remove, only used for CommitBuildStep
	Get(string) *container.Container
}

// LayerBackend represents a layer storage backend along with its platform
type LayerBackend struct {
	layer.Store
	Platform platforms.Matcher
}

// ImageServiceConfig is the configuration used to create a new ImageService
type ImageServiceConfig struct {
	DefaultNamespace       string
	DefaultPlatform        ocispec.Platform
	Client                 *containerd.Client
	EventsService          *daemonevents.Events
	LayerBackends          []LayerBackend
	MaxConcurrentDownloads int
	MaxConcurrentUploads   int
	MaxDownloadAttempts       int

	// TODO(containerd): use containerd client after containers updated
	// to store all metadata in containerd
	ContainerStore containerStore

	// TODO(containerd): deprecated, move functions which require this
	// out of image service
	RegistryService registry.Service
}

// NewImageService returns a new ImageService from a configuration
func NewImageService(config ImageServiceConfig) *ImageService {
	logrus.Debugf("Max Concurrent Downloads: %d", config.MaxConcurrentDownloads)
	logrus.Debugf("Max Concurrent Uploads: %d", config.MaxConcurrentUploads)
	logrus.Debugf("Max Download Attempts: %d", config.MaxDownloadAttempts)

	var pc orderedPlatformComparer
	layerStores := map[string]layer.Store{}
	for _, backend := range config.LayerBackends {
		pc.matchers = append(pc.matchers, backend.Platform)
		layerStores[backend.DriverName()] = backend.Store
	}

	return &ImageService{
		namespace:       config.DefaultNamespace,
		defaultPlatform: config.DefaultPlatform,
		platforms:       pc,
		client:          config.Client,
		containers:      config.ContainerStore,
		cache:           map[string]*cache{},
		eventsService:   config.EventsService,
		layerBackends:   config.LayerBackends,
		layerStores:     layerStores,

		registryService: config.RegistryService,
	}
}

// TODO(containerd): add upstream constructor
type orderedPlatformComparer struct {
	matchers []platforms.Matcher
}

func (c orderedPlatformComparer) Match(platform ocispec.Platform) bool {
	for _, m := range c.matchers {
		if m.Match(platform) {
			return true
		}
	}
	return false
}

func (c orderedPlatformComparer) Less(p1 ocispec.Platform, p2 ocispec.Platform) bool {
	for _, m := range c.matchers {
		p1m := m.Match(p1)
		p2m := m.Match(p2)
		if p1m && !p2m {
			return true
		}
		if p1m || p2m {
			return false
		}
	}
	return false
}

// ImageService provides a backend for image management
type ImageService struct {
	namespace       string
	defaultPlatform ocispec.Platform
	client          *containerd.Client
	containers      containerStore
	eventsService   *daemonevents.Events
	layerStores     map[string]layer.Store
	layerBackends   []LayerBackend
	platforms       platforms.MatchComparer
	pruneRunning    int32

	// namespaced cache
	cache  map[string]*cache
	cacheL sync.RWMutex

	// To be replaced by containerd client
	registryService registry.Service
}

// CountImages returns the number of images stored by ImageService
// called from info.go
func (i *ImageService) CountImages(ctx context.Context) (int, error) {
	is := i.client.ImageService()
	imgs, err := is.List(ctx)
	if err != nil {
		return 0, err
	}

	return len(imgs), nil
}

// GetImageBackend returns the storage backend used by the given image
// TODO(containerd): return more abstract interface to support snapshotters
func (i *ImageService) GetImageBackend(image RuntimeImage) (layer.Store, error) {
	if image.Config.Digest != "" {
		// TODO(containerd): Get from content-store label
		// TODO(containerd): Lookup by layer store names
	}
	if image.Platform.OS == "" {
		image.Platform = i.defaultPlatform
	}
	for _, backend := range i.layerBackends {
		if backend.Platform.Match(image.Platform) {
			return backend.Store, nil
		}
	}

	return nil, errdefs.System(errors.Wrapf(system.ErrNotSupportedOperatingSystem, "no layer storage backend configured for %s", image.Platform.OS))
}

// GetLayerStore returns the best layer store for the given platform
// Deprecated: Do not access layer stores directly, snapshotters may be used
func (i *ImageService) GetLayerStore(platform ocispec.Platform) (layer.Store, error) {
	return i.getLayerStore(platform)
}

func (i *ImageService) getLayerStore(platform ocispec.Platform) (layer.Store, error) {
	for _, backend := range i.layerBackends {
		if backend.Platform.Match(platform) {
			return backend.Store, nil
		}
	}

	return nil, errdefs.Unavailable(errors.Errorf("no layer storage backend configured for %s", platform.OS))
}

// GetLayerByID returns a layer by ID and operating system
// called from daemon.go Daemon.restore(), and Daemon.containerExport()
func (i *ImageService) GetLayerByID(cid string, driver string) (layer.RWLayer, error) {
	ls, ok := i.layerStores[driver]
	if !ok {
		return nil, errdefs.NotFound(errors.Errorf("driver not found: %s", driver))
	}

	return ls.GetRWLayer(cid)
}

// LayerStoreStatus returns the status for each layer store
// called from info.go
func (i *ImageService) LayerStoreStatus() map[string][][2]string {
	result := make(map[string][][2]string)
	for _, backend := range i.layerBackends {
		result[backend.DriverName()] = backend.DriverStatus()
	}
	return result
}

// GetLayerMountID returns the mount ID for a layer
// called from daemon.go Daemon.Shutdown(), and Daemon.Cleanup() (cleanup is actually continerCleanup)
// TODO: needs to be refactored to Unmount (see callers), or removed and replaced
// with GetLayerByID
func (i *ImageService) GetLayerMountID(cid string, driver string) (string, error) {
	ls, ok := i.layerStores[driver]
	if !ok {
		return "", errdefs.NotFound(errors.Errorf("driver not found: %s", driver))
	}

	return ls.GetMountID(cid)
}

// Cleanup resources before the process is shutdown.
// called from daemon.go Daemon.Shutdown()
func (i *ImageService) Cleanup() {
	for _, backend := range i.layerBackends {
		if err := backend.Cleanup(); err != nil {
			logrus.Errorf("Error during layer Store.Cleanup(): %v %s", err, backend.DriverName())
		}
	}
}

// DriverName returns the name of the graph driver for the given platform
func (i *ImageService) DriverName(p ocispec.Platform) string {
	ls, err := i.getLayerStore(p)
	if err != nil {
		return ""
	}

	return ls.DriverName()
}

// ReleaseLayer releases a layer allowing it to be removed
func (i *ImageService) ReleaseLayer(rwlayer layer.RWLayer, driver string) error {
	ls, ok := i.layerStores[driver]
	if !ok {
		return errdefs.NotFound(errors.Errorf("driver not found: %s", driver))
	}
	metadata, err := ls.ReleaseRWLayer(rwlayer)
	layer.LogReleaseMetadata(metadata)
	if err != nil && err != layer.ErrMountDoesNotExist && !os.IsNotExist(errors.Cause(err)) {
		return errors.Wrapf(err, "driver %q failed to remove root filesystem", ls.DriverName())
	}
	return nil
}

// LayerDiskUsage returns the number of bytes used by layer stores
// called from disk_usage.go
func (i *ImageService) LayerDiskUsage(ctx context.Context) (int64, error) {
	c, err := i.getCache(ctx)
	if err != nil {
		return 0, err
	}

	var layers []layer.Layer
	c.m.RLock()
	for _, lm := range c.layers {
		for _, l := range lm {
			layers = append(layers, l)
		}
	}
	c.m.RUnlock()

	// TODO(containerd): Get from containerd snapshotters also

	var allLayersSize int64
	for _, l := range layers {
		select {
		case <-ctx.Done():
			return allLayersSize, ctx.Err()
		default:
			size, err := l.DiffSize()
			if err == nil {
				allLayersSize += size
			} else {
				logrus.Warnf("failed to get diff size for layer %v", l.ChainID())
			}
		}
	}
	return allLayersSize, nil
}

// UpdateConfig values
//
// called from reload.go
func (i *ImageService) UpdateConfig(maxDownloads, maxUploads *int) {
	// TODO(containerd): store these locally to configure download/upload
}
