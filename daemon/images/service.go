package images

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/container"
	daemonevents "github.com/moby/moby/v2/daemon/events"
	"github.com/moby/moby/v2/daemon/internal/distribution"
	"github.com/moby/moby/v2/daemon/internal/distribution/metadata"
	"github.com/moby/moby/v2/daemon/internal/distribution/xfer"
	"github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/internal/layer"
	refstore "github.com/moby/moby/v2/daemon/internal/refstore"
	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

type containerStore interface {
	// First is used by image delete
	First(container.StoreFilter) *container.Container
	// List is used by image prune, and image list
	List() []*container.Container
	// Get is used by CommitBuildStep
	// TODO: remove, only used for CommitBuildStep
	Get(string) *container.Container
}

// ImageServiceConfig is the configuration used to create a new ImageService
type ImageServiceConfig struct {
	ContainerStore            containerStore
	DistributionMetadataStore metadata.Store
	EventsService             *daemonevents.Events
	ImageStore                image.Store
	LayerStore                layer.Store
	MaxConcurrentDownloads    int
	MaxConcurrentUploads      int
	MaxDownloadAttempts       int
	ReferenceStore            refstore.Store
	RegistryService           distribution.RegistryResolver
	ContentStore              content.Store
	Leases                    leases.Manager
	ContentNamespace          string
}

// NewImageService returns a new ImageService from a configuration
func NewImageService(ctx context.Context, config ImageServiceConfig) *ImageService {
	log.G(ctx).Debugf("Max Concurrent Downloads: %d", config.MaxConcurrentDownloads)
	log.G(ctx).Debugf("Max Concurrent Uploads: %d", config.MaxConcurrentUploads)
	log.G(ctx).Debugf("Max Download Attempts: %d", config.MaxDownloadAttempts)

	return &ImageService{
		containers:                config.ContainerStore,
		distributionMetadataStore: config.DistributionMetadataStore,
		downloadManager:           xfer.NewLayerDownloadManager(config.LayerStore, config.MaxConcurrentDownloads, xfer.WithMaxDownloadAttempts(config.MaxDownloadAttempts)),
		eventsService:             config.EventsService,
		imageStore:                &imageStoreWithLease{Store: config.ImageStore, leases: config.Leases, ns: config.ContentNamespace},
		layerStore:                config.LayerStore,
		referenceStore:            config.ReferenceStore,
		registryService:           config.RegistryService,
		uploadManager:             xfer.NewLayerUploadManager(config.MaxConcurrentUploads),
		leases:                    config.Leases,
		content:                   config.ContentStore,
		contentNamespace:          config.ContentNamespace,
	}
}

// ImageService provides a backend for image management
type ImageService struct {
	containers                containerStore
	distributionMetadataStore metadata.Store
	downloadManager           *xfer.LayerDownloadManager
	eventsService             *daemonevents.Events
	imageStore                image.Store
	layerStore                layer.Store
	pruneRunning              atomic.Bool
	referenceStore            refstore.Store
	registryService           distribution.RegistryResolver
	uploadManager             *xfer.LayerUploadManager
	leases                    leases.Manager
	content                   content.Store
	contentNamespace          string
	squashCache               sync.Map // squashCacheKey → image.ID
}

// DistributionServices provides daemon image storage services
type DistributionServices struct {
	DownloadManager   *xfer.LayerDownloadManager
	V2MetadataService metadata.V2MetadataService
	LayerStore        layer.Store
	ImageStore        image.Store
	ReferenceStore    refstore.Store
}

// DistributionServices return services controlling daemon image storage
func (i *ImageService) DistributionServices() DistributionServices {
	return DistributionServices{
		DownloadManager:   i.downloadManager,
		V2MetadataService: metadata.NewV2MetadataService(i.distributionMetadataStore),
		LayerStore:        i.layerStore,
		ImageStore:        i.imageStore,
		ReferenceStore:    i.referenceStore,
	}
}

// CountImages returns the number of images stored by ImageService
// called from info.go
func (i *ImageService) CountImages(ctx context.Context) int {
	return i.imageStore.Len()
}

// Children returns the children image.IDs for a parent image.
// called from list.go to filter containers
// TODO: refactor to expose an ancestry for image.ID?
func (i *ImageService) Children(_ context.Context, id image.ID) ([]image.ID, error) {
	return i.imageStore.Children(id), nil
}

// CreateLayer creates a filesystem layer for a container.
// called from create.go
// TODO: accept an opt struct instead of container?
func (i *ImageService) CreateLayer(container *container.Container, initFunc layer.MountInit) (container.RWLayer, error) {
	var img *image.Image
	if container.ImageID != "" {
		containerImg, err := i.imageStore.Get(container.ImageID)
		if err != nil {
			return nil, err
		}
		img = containerImg
	}

	rwLayerOpts := &layer.CreateRWLayerOpts{
		MountLabel: container.MountLabel,
		InitFunc:   initFunc,
		StorageOpt: container.HostConfig.StorageOpt,
	}

	return i.CreateLayerFromImage(img, container.ID, rwLayerOpts)
}

// CreateLayerFromImage creates a file system from an arbitrary image
// Used to mount an image inside another
func (i *ImageService) CreateLayerFromImage(img *image.Image, layerName string, rwLayerOpts *layer.CreateRWLayerOpts) (container.RWLayer, error) {
	var layerID layer.ChainID
	if img != nil {
		layerID = img.RootFS.ChainID()
	}

	return i.layerStore.CreateRWLayer(layerName, layerID, rwLayerOpts)
}

// GetLayerByID returns a layer by ID
// called from daemon.go Daemon.restore().
func (i *ImageService) GetLayerByID(cid string) (container.RWLayer, error) {
	return i.layerStore.GetRWLayer(cid)
}

// LayerStoreStatus returns the status for each layer store
// called from info.go
func (i *ImageService) LayerStoreStatus() [][2]string {
	return i.layerStore.DriverStatus()
}

// GetLayerMountID returns the mount ID for a layer
// called from daemon.go Daemon.Shutdown(), and Daemon.Cleanup() (cleanup is actually containerCleanup)
// TODO: needs to be refactored to Unmount (see callers), or removed and replaced with GetLayerByID
func (i *ImageService) GetLayerMountID(cid string) (string, error) {
	return i.layerStore.GetMountID(cid)
}

// Cleanup resources before the process is shutdown.
// called from daemon.go Daemon.Shutdown()
func (i *ImageService) Cleanup() error {
	if err := i.layerStore.Cleanup(); err != nil {
		return errors.Wrap(err, "error during layerStore.Cleanup()")
	}
	return nil
}

// StorageDriver returns the name of the storage driver used by the ImageService.
func (i *ImageService) StorageDriver() string {
	return i.layerStore.DriverName()
}

// ReleaseLayer releases a layer allowing it to be removed
// called from delete.go Daemon.cleanupContainer().
func (i *ImageService) ReleaseLayer(rwlayer container.RWLayer) error {
	l, ok := rwlayer.(layer.RWLayer)
	if !ok {
		return fmt.Errorf("unexpected RWLayer type: %T", rwlayer)
	}

	metaData, err := i.layerStore.ReleaseRWLayer(l)
	for _, m := range metaData {
		log.G(context.TODO()).WithField("chainID", m.ChainID).Infof("release RWLayer: cleaned up layer %s", m.ChainID)
	}
	if err != nil && !errors.Is(err, layer.ErrMountDoesNotExist) && !errors.Is(err, os.ErrNotExist) {
		return errors.Wrapf(err, "driver %q failed to remove root filesystem",
			i.layerStore.DriverName())
	}
	return nil
}

// ImageDiskUsage returns the number of bytes used by content and layer stores
// called from disk_usage.go
func (i *ImageService) ImageDiskUsage(ctx context.Context) (int64, error) {
	var allLayersSize int64
	layerRefs := i.getLayerRefs()
	allLayers := i.layerStore.Map()
	for _, l := range allLayers {
		select {
		case <-ctx.Done():
			return allLayersSize, ctx.Err()
		default:
			size := l.DiffSize()
			if _, ok := layerRefs[l.ChainID()]; ok {
				allLayersSize += size
			}
		}
	}
	return allLayersSize, nil
}

func (i *ImageService) getLayerRefs() map[layer.ChainID]int {
	tmpImages := i.imageStore.Map()
	layerRefs := map[layer.ChainID]int{}
	for id, img := range tmpImages {
		dgst := digest.Digest(id)
		if len(i.referenceStore.References(dgst)) == 0 && len(i.imageStore.Children(id)) != 0 {
			continue
		}

		rootFS := *img.RootFS
		rootFS.DiffIDs = nil
		for _, id := range img.RootFS.DiffIDs {
			rootFS.Append(id)
			chid := rootFS.ChainID()
			layerRefs[chid]++
		}
	}

	return layerRefs
}

// UpdateConfig values
//
// called from reload.go
func (i *ImageService) UpdateConfig(maxDownloads, maxUploads int) {
	if i.downloadManager != nil && maxDownloads != 0 {
		i.downloadManager.SetConcurrency(maxDownloads)
	}
	if i.uploadManager != nil && maxUploads != 0 {
		i.uploadManager.SetConcurrency(maxUploads)
	}
}
