package containerd

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/plugins"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/containerd/identitycache"
	daemonevents "github.com/moby/moby/v2/daemon/events"
	dimages "github.com/moby/moby/v2/daemon/images"
	"github.com/moby/moby/v2/daemon/internal/distribution"
	"github.com/moby/moby/v2/daemon/snapshotter"
	"github.com/moby/moby/v2/errdefs"
	policyverifier "github.com/moby/policy-helpers"
	"github.com/moby/sys/user"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/semaphore"
)

// ImageService implements daemon.ImageService
type ImageService struct {
	client              *containerd.Client
	images              c8dimages.Store
	content             content.Store
	containers          container.Store
	snapshotterServices map[string]snapshots.Snapshotter
	snapshotter         string
	registryHosts       docker.RegistryHosts
	registryService     distribution.RegistryResolver
	eventsService       *daemonevents.Events
	pruneRunning        atomic.Bool
	// activeLeasesMu guards activeLeases. Leases created by withLease that
	// are still in use (e.g. an in-flight pull) are tracked so that
	// ImagePrune can skip them, avoiding deletion of leases that protect
	// content being written.
	activeLeasesMu sync.Mutex
	activeLeases   map[string]struct{}
	refCountMounter     snapshotter.Mounter
	idMapping           user.IdentityMapping
	policyVerifier      func() (*policyverifier.Verifier, error)
	identity            imageIdentityState
	features            featuresProvider

	// transferLimitMu keeps limiter pointers and their settings consistent while
	// configuration reload replaces them.
	transferLimitMu        sync.Mutex
	maxConcurrentDownloads int
	downloadLimiter        *semaphore.Weighted
	uploadLimiter          *semaphore.Weighted

	// defaultPlatformOverride is used in tests to override the host platform.
	defaultPlatformOverride *ocispec.Platform
}

func newTransferLimiter(maxConcurrent int) *semaphore.Weighted {
	if maxConcurrent <= 0 {
		return nil
	}
	return semaphore.NewWeighted(int64(maxConcurrent))
}

type ImageServiceConfig struct {
	Client                 *containerd.Client
	Containers             container.Store
	Snapshotter            string
	IdentityCacheBackend   identitycache.Backend
	RegistryHosts          docker.RegistryHosts
	Registry               distribution.RegistryResolver
	EventsService          *daemonevents.Events
	RefCountMounter        snapshotter.Mounter
	IDMapping              user.IdentityMapping
	PolicyVerifierProvider func() (*policyverifier.Verifier, error)
	MaxConcurrentDownloads int
	MaxConcurrentUploads   int
	Features               featuresProvider
}

// featuresProvider provides the daemon's feature flags.
type featuresProvider interface {
	// Features returns the current feature flags, which can change when the
	// daemon configuration is reloaded.
	Features() map[string]bool
}

// NewService creates a new ImageService.
func NewService(config ImageServiceConfig) *ImageService {
	log.G(context.TODO()).Debugf("Max Concurrent Downloads: %d", config.MaxConcurrentDownloads)
	log.G(context.TODO()).Debugf("Max Concurrent Uploads: %d", config.MaxConcurrentUploads)

	service := &ImageService{
		client:  config.Client,
		images:  config.Client.ImageService(),
		content: config.Client.ContentStore(),
		snapshotterServices: map[string]snapshots.Snapshotter{
			config.Snapshotter: config.Client.SnapshotService(config.Snapshotter),
		},
		containers:      config.Containers,
		snapshotter:     config.Snapshotter,
		registryHosts:   config.RegistryHosts,
		registryService: config.Registry,
		eventsService:   config.EventsService,
		refCountMounter: config.RefCountMounter,
		idMapping:       config.IDMapping,
		policyVerifier:  config.PolicyVerifierProvider,
		features:        config.Features,
		identity: imageIdentityState{
			cache: make(map[string]imageIdentityCacheEntry),
			cacheStore: func() identitycache.Backend {
				if config.IdentityCacheBackend != nil {
					return config.IdentityCacheBackend
				}
				return identitycache.NewNopBackend()
			}(),
		},
		activeLeases: map[string]struct{}{},
	}
	service.setTransferLimits(config.MaxConcurrentDownloads, config.MaxConcurrentUploads)
	service.startImageIdentityCacheRefresh()
	return service
}

func (i *ImageService) setTransferLimits(maxDownloads, maxUploads int) {
	i.maxConcurrentDownloads = maxDownloads
	i.downloadLimiter = newTransferLimiter(maxDownloads)
	i.uploadLimiter = newTransferLimiter(maxUploads)
}

// trackLease adds a lease ID to the set of active leases.
// It is safe to call even if activeLeases has not been initialized
// (e.g. in tests that construct ImageService directly).
func (i *ImageService) trackLease(id string) {
	i.activeLeasesMu.Lock()
	defer i.activeLeasesMu.Unlock()
	if i.activeLeases == nil {
		i.activeLeases = make(map[string]struct{})
	}
	i.activeLeases[id] = struct{}{}
}

// untrackLease removes a lease ID from the set of active leases.
func (i *ImageService) untrackLease(id string) {
	i.activeLeasesMu.Lock()
	defer i.activeLeasesMu.Unlock()
	if i.activeLeases != nil {
		delete(i.activeLeases, id)
	}
}

// activeLeaseSnapshot returns a snapshot of the current active lease IDs.
func (i *ImageService) activeLeaseSnapshot() map[string]struct{} {
	i.activeLeasesMu.Lock()
	defer i.activeLeasesMu.Unlock()
	active := make(map[string]struct{}, len(i.activeLeases))
	for id := range i.activeLeases {
		active[id] = struct{}{}
	}
	return active
}

func (i *ImageService) snapshotterService(snapshotter string) snapshots.Snapshotter {
	s, ok := i.snapshotterServices[snapshotter]
	if !ok {
		s = i.client.SnapshotService(snapshotter)
		i.snapshotterServices[snapshotter] = s
	}

	return s
}

// DistributionServices return services controlling daemon image storage.
func (i *ImageService) DistributionServices() dimages.DistributionServices {
	return dimages.DistributionServices{}
}

// CountImages returns the number of images stored by ImageService
// called from info.go
func (i *ImageService) CountImages(ctx context.Context) int {
	imgs, err := i.client.ListImages(ctx)
	if err != nil {
		return 0
	}

	uniqueImages := map[digest.Digest]struct{}{}
	for _, i := range imgs {
		dgst := i.Target().Digest
		if _, ok := uniqueImages[dgst]; !ok {
			uniqueImages[dgst] = struct{}{}
		}
	}

	return len(uniqueImages)
}

// LayerStoreStatus returns the status for each layer store
// called from info.go
func (i *ImageService) LayerStoreStatus() [][2]string {
	// TODO(thaJeztah) do we want to add more details about the driver here?
	return [][2]string{
		{"driver-type", string(plugins.SnapshotPlugin)},
	}
}

// GetLayerMountID returns the mount ID for a layer
// called from daemon.go Daemon.Shutdown(), and Daemon.Cleanup() (cleanup is actually containerCleanup)
// TODO: needs to be refactored to Unmount (see callers), or removed and replaced with GetLayerByID
func (i *ImageService) GetLayerMountID(cid string) (string, error) {
	return "", errdefs.NotImplemented(errors.New("not implemented"))
}

// Cleanup resources before the process is shutdown.
// called from daemon.go Daemon.Shutdown()
func (i *ImageService) Cleanup() error {
	i.stopImageIdentityCacheRefresh()
	if i.identity.cacheStore != nil {
		return i.identity.cacheStore.Close()
	}
	return nil
}

// StorageDriver returns the name of the default storage-driver (snapshotter)
// used by the ImageService.
func (i *ImageService) StorageDriver() string {
	return i.snapshotter
}

// ImageDiskUsage returns the number of bytes used by content and layer stores
// called from disk_usage.go
func (i *ImageService) ImageDiskUsage(ctx context.Context) (int64, error) {
	imgs, err := i.images.List(ctx)
	if err != nil {
		return 0, err
	}

	var diskUsage int64
	// TODO(thaJeztah): do we need to take multiple snapshotters into account? See https://github.com/moby/moby/issues/45273
	snapshotter := i.client.SnapshotService(i.snapshotter)
	visitedSnapshots := make(map[string]struct{})
	visitedImages := make(map[digest.Digest]struct{})
	for _, img := range imgs {
		if err := i.walkImageManifests(ctx, img, func(platformImg *ImageManifest) error {
			unpacked, err := platformImg.IsUnpacked(ctx, i.snapshotter)
			if err != nil {
				log.G(ctx).WithFields(log.Fields{
					"error":    err,
					"image":    img.Name,
					"target":   img.Target,
					"manifest": platformImg.Target(),
				}).Warn("failed to check whether image manifest is unpacked")
				return nil
			}
			if unpacked {
				diffIDs, err := platformImg.RootFS(ctx)
				if err != nil {
					log.G(ctx).WithFields(log.Fields{
						"error":    err,
						"image":    img.Name,
						"target":   img.Target,
						"manifest": platformImg.Target(),
					}).Debug("failed to get image rootfs")
					return nil
				}

				for snapshot := identity.ChainID(diffIDs).String(); snapshot != ""; {
					if _, ok := visitedSnapshots[snapshot]; ok {
						break
					}
					visitedSnapshots[snapshot] = struct{}{}

					usage, err := snapshotter.Usage(ctx, snapshot)
					if err != nil {
						if cerrdefs.IsNotFound(err) {
							log.G(ctx).WithFields(log.Fields{
								"image":    img.Name,
								"target":   img.Target,
								"manifest": platformImg.Target(),
								"snapshot": snapshot,
							}).Debug("snapshot not found while counting image disk usage")
							break
						}
						log.G(ctx).WithFields(log.Fields{
							"error":    err,
							"image":    img.Name,
							"target":   img.Target,
							"manifest": platformImg.Target(),
							"snapshot": snapshot,
						}).Warn("failed to get snapshot usage for image disk usage")
						break
					}

					// Don't accumulate usage.Size just yet
					// If the Stat below fails with NotFound, it means the
					// snapshot is already gone at this point.
					info, err := snapshotter.Stat(ctx, snapshot)
					if err != nil {
						if cerrdefs.IsNotFound(err) {
							log.G(ctx).WithFields(log.Fields{
								"image":    img.Name,
								"target":   img.Target,
								"manifest": platformImg.Target(),
								"snapshot": snapshot,
							}).Debug("snapshot not found while counting image disk usage")
							break
						}
						log.G(ctx).WithFields(log.Fields{
							"error":    err,
							"image":    img.Name,
							"target":   img.Target,
							"manifest": platformImg.Target(),
							"snapshot": snapshot,
						}).Warn("failed to get snapshot info for image disk usage")
						break
					}

					log.G(ctx).WithFields(log.Fields{
						"image":    img.Name,
						"target":   img.Target,
						"manifest": platformImg.Target(),
						"snapshot": snapshot,
						"size":     usage.Size,
						"inodes":   usage.Inodes,
					}).Debug("counting snapshot in image disk usage")

					diskUsage += usage.Size
					snapshot = info.Parent
				}
			}
			return nil
		}); err != nil {
			log.G(ctx).WithFields(log.Fields{
				"error":  err,
				"image":  img.Name,
				"target": img.Target,
			}).Warn("failed to calculate image snapshot disk usage")
		}

		// Include the size of content size from the images.
		if err := i.walkPresentChildren(ctx, img.Target, func(ctx context.Context, desc ocispec.Descriptor) error {
			if _, ok := visitedImages[desc.Digest]; ok {
				return nil
			}
			visitedImages[desc.Digest] = struct{}{}

			diskUsage += desc.Size
			return nil
		}); err != nil {
			return 0, err
		}
	}
	return diskUsage, nil
}

// UpdateConfig values
//
// called from reload.go
func (i *ImageService) UpdateConfig(maxDownloads, maxUploads int) {
	i.transferLimitMu.Lock()
	defer i.transferLimitMu.Unlock()

	i.setTransferLimits(maxDownloads, maxUploads)
}

// GetContainerLayerSize returns the real size & virtual size of the container.
func (i *ImageService) GetContainerLayerSize(ctx context.Context, containerID string) (int64, int64, error) {
	ctr := i.containers.Get(containerID)
	if ctr == nil {
		return 0, 0, nil
	}

	snapshotter := i.client.SnapshotService(ctr.Driver)
	rwLayerUsage, err := snapshotter.Usage(ctx, containerID)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return 0, 0, errdefs.NotFound(fmt.Errorf("rw layer snapshot not found for container %s", containerID))
		}
		return 0, 0, errdefs.System(errors.Wrapf(err, "snapshotter.Usage failed for %s", containerID))
	}

	unpackedUsage, err := calculateSnapshotParentUsage(ctx, snapshotter, containerID)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			log.G(ctx).WithField("ctr", containerID).Warn("parent of container snapshot no longer present")
		} else {
			log.G(ctx).WithError(err).WithField("ctr", containerID).Warn("unexpected error when calculating usage of the parent snapshots")
		}
	}
	log.G(ctx).WithFields(log.Fields{
		"rwLayerUsage": rwLayerUsage.Size,
		"unpacked":     unpackedUsage.Size,
	}).Debug("GetContainerLayerSize")

	// TODO(thaJeztah): include content-store size for the image (similar to "GET /images/json")
	return rwLayerUsage.Size, rwLayerUsage.Size + unpackedUsage.Size, nil
}
