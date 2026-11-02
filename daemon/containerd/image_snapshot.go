package containerd

import (
	"context"
	"fmt"
	"os"
	"strings"

	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/internal/layer"
	"github.com/moby/moby/v2/daemon/snapshotter"
	"github.com/moby/moby/v2/errdefs"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// CreateLayer creates a new layer for a container.
func (i *ImageService) CreateLayer(ctr *container.Container, initFunc layer.MountInit) (container.RWLayer, error) {
	var descriptor *ocispec.Descriptor
	if ctr.ImageManifest != nil {
		descriptor = ctr.ImageManifest
	}

	rwLayerOpts := &layer.CreateRWLayerOpts{
		StorageOpt: ctr.HostConfig.StorageOpt,
	}

	return i.createLayer(descriptor, ctr.ID, rwLayerOpts, initFunc)
}

// CreateLayerFromImage creates a new layer from an image
func (i *ImageService) CreateLayerFromImage(img *image.Image, layerName string, rwLayerOpts *layer.CreateRWLayerOpts) (container.RWLayer, error) {
	var descriptor *ocispec.Descriptor
	if img != nil {
		descriptor = img.Details.ManifestDescriptor
	}

	return i.createLayer(descriptor, layerName, rwLayerOpts, nil)
}

func (i *ImageService) createLayer(descriptor *ocispec.Descriptor, layerName string, rwLayerOpts *layer.CreateRWLayerOpts, initFunc layer.MountInit) (container.RWLayer, error) {
	ctx := context.TODO()
	var parentSnapshot string
	if descriptor != nil {
		snapshot, err := i.getImageSnapshot(ctx, descriptor)
		if err != nil {
			return nil, err
		}
		parentSnapshot = snapshot
	}

	// TODO: Consider a better way to do this. It is better to have a container directly
	// reference a snapshot, however, that is not done today because a container may
	// removed and recreated with nothing holding the snapshot in between. Consider
	// removing this lease and only temporarily holding a lease on re-create, using
	// non-expiring leases introduces the possibility of leaking resources.
	ls := i.client.LeasesService()
	lease, err := ls.Create(ctx, leases.WithID(layerName))
	if err != nil {
		return nil, err
	}
	ctx = leases.WithLease(ctx, lease.ID)

	if initFunc != nil {
		if err := i.prepareInitLayer(ctx, layerName, parentSnapshot, initFunc); err != nil {
			return nil, err
		}
		parentSnapshot = layerName + "-init"
	}

	sn := i.client.SnapshotService(i.StorageDriver())
	if !i.idMapping.Empty() {
		err = i.remapSnapshot(ctx, sn, layerName, parentSnapshot)
	} else {
		_, err = sn.Prepare(ctx, layerName, parentSnapshot)
	}

	if err != nil {
		return nil, err
	}

	return &rwLayer{
		id:              layerName,
		snapshotterName: i.StorageDriver(),
		snapshotter:     sn,
		refCountMounter: i.refCountMounter,
		lease:           lease,
	}, nil
}

func (i *ImageService) getImageSnapshot(ctx context.Context, descriptor *ocispec.Descriptor) (string, error) {
	c8dImg := c8dimages.Image{
		Target: *descriptor,
	}

	platformImg, err := i.NewImageManifest(ctx, c8dImg, c8dImg.Target)
	if err != nil {
		return "", err
	}

	cfgDesc, err := platformImg.Config(ctx)
	if err != nil {
		return "", err
	}

	if strings.HasPrefix(strings.ToLower(cfgDesc.MediaType), "application/vnd.docker.ai.") {
		return "", errors.New("Running AI models directly by the Engine is not supported yet, please use 'docker model run' instead")
	}

	unpacked, err := platformImg.IsUnpacked(ctx, i.snapshotter)
	if err != nil {
		return "", err
	}

	if !unpacked {
		if err := platformImg.Unpack(ctx, i.snapshotter); err != nil {
			return "", err
		}
	}

	diffIDs, err := platformImg.RootFS(ctx)
	if err != nil {
		return "", err
	}

	snapshot := identity.ChainID(diffIDs).String()

	return snapshot, nil
}

type rwLayer struct {
	id              string
	snapshotter     snapshots.Snapshotter
	snapshotterName string
	refCountMounter snapshotter.Mounter
	root            string
	lease           leases.Lease
}

func (l *rwLayer) mounts(ctx context.Context) ([]mount.Mount, error) {
	return l.snapshotter.Mounts(ctx, l.id)
}

func (l *rwLayer) Mount(mountLabel string) (string, error) {
	ctx := context.TODO()

	// TODO: Investigate how we can handle mountLabel
	_ = mountLabel
	mounts, err := l.mounts(ctx)
	if err != nil {
		return "", err
	}

	var root string
	if root, err = l.refCountMounter.Mount(mounts, l.id); err != nil {
		return "", fmt.Errorf("failed to mount %s: %w", root, err)
	}
	l.root = root

	log.G(ctx).WithFields(log.Fields{"container": l.id, "root": root, "snapshotter": l.snapshotterName}).Debug("container mounted via snapshotter")
	return root, nil
}

// GetLayerByID returns a layer by ID
// called from daemon.go Daemon.restore().
func (i *ImageService) GetLayerByID(cid string) (container.RWLayer, error) {
	ctx := context.TODO()

	sn := i.client.SnapshotService(i.StorageDriver())
	if _, err := sn.Stat(ctx, cid); err != nil {
		if !cerrdefs.IsNotFound(err) {
			return nil, fmt.Errorf("failed to stat snapshot %s: %w", cid, err)
		}
		return nil, errdefs.NotFound(fmt.Errorf("RW layer for container %s not found", cid))
	}

	ls := i.client.LeasesService()
	lss, err := ls.List(ctx, "id=="+cid)
	if err != nil {
		return nil, err
	}

	switch len(lss) {
	case 0:
		return nil, errdefs.NotFound(errors.New("rw layer lease not found for container " + cid))
	case 1:
		// found
	default:
		log.G(ctx).WithFields(log.Fields{"container": cid, "leases": lss}).Warn("multiple leases with the same id found, this should not happen")
	}

	root, err := i.refCountMounter.Mounted(cid)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// TODO(thaJeztah): should os.ErrNotExist be handled by refCountMounter, and should it still attempt to decrement?
		//	see https://github.com/moby/moby/blob/f74e5d48b3a8a83a6b969be2dd2b14af3d9d4d22/daemon/snapshotter/mount.go#L96-L111
		log.G(ctx).WithFields(log.Fields{
			"error":     err,
			"container": cid,
		}).Warn("failed to determine if container is already mounted")
	}

	return &rwLayer{
		id:              cid,
		snapshotterName: i.StorageDriver(),
		snapshotter:     sn,
		refCountMounter: i.refCountMounter,
		lease:           lss[0],
		root:            root,
	}, nil
}

func (l *rwLayer) Unmount() error {
	ctx := context.TODO()

	if l.root == "" {
		target, err := l.refCountMounter.Mounted(l.id)
		if err != nil {
			log.G(ctx).WithField("id", l.id).Warn("failed to determine if container is already mounted")
		}
		if target == "" {
			return errors.New("layer not mounted")
		}
		l.root = target
	}

	if err := l.refCountMounter.Unmount(l.root); err != nil {
		log.G(ctx).WithField("container", l.id).WithError(err).Error("error unmounting container")
		return fmt.Errorf("failed to unmount %s: %w", l.root, err)
	}

	return nil
}

func (l rwLayer) Metadata() (map[string]string, error) {
	return map[string]string{
		"ID": l.id,
	}, nil
}

// ReleaseLayer releases a layer allowing it to be removed
// called from delete.go Daemon.cleanupContainer(), and Daemon.containerExport()
func (i *ImageService) ReleaseLayer(rwlayer container.RWLayer) error {
	c8dLayer, ok := rwlayer.(*rwLayer)
	if !ok {
		return fmt.Errorf("invalid layer type %T", rwlayer)
	}

	ls := i.client.LeasesService()
	if err := ls.Delete(context.Background(), c8dLayer.lease, leases.SynchronousDelete); err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

func (i *ImageService) prepareInitLayer(ctx context.Context, id string, parent string, setupInit func(string) error) error {
	sn := i.client.SnapshotService(i.StorageDriver())

	mounts, err := sn.Prepare(ctx, id+"-init-key", parent)
	if err != nil {
		return err
	}

	if setupInit != nil {
		if err := mount.WithTempMount(ctx, mounts, func(root string) error {
			return setupInit(root)
		}); err != nil {
			return err
		}
	}

	return sn.Commit(ctx, id+"-init", id+"-init-key")
}

// calculateSnapshotParentUsage returns the usage of all ancestors of the
// provided snapshot. It doesn't include the size of the snapshot itself.
func calculateSnapshotParentUsage(ctx context.Context, snapshotter snapshots.Snapshotter, snapshotID string) (snapshots.Usage, error) {
	info, err := snapshotter.Stat(ctx, snapshotID)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return snapshots.Usage{}, errdefs.NotFound(err)
		}
		return snapshots.Usage{}, errdefs.System(errors.Wrapf(err, "snapshotter.Stat failed for %s", snapshotID))
	}
	if info.Parent == "" {
		return snapshots.Usage{}, errdefs.NotFound(fmt.Errorf("snapshot %s has no parent", snapshotID))
	}

	return calculateSnapshotTotalUsage(ctx, snapshotter, info.Parent)
}

// calculateSnapshotTotalUsage returns the total usage of that snapshot
// including all of its ancestors.
func calculateSnapshotTotalUsage(ctx context.Context, snapshotter snapshots.Snapshotter, snapshotID string) (snapshots.Usage, error) {
	var total snapshots.Usage
	next := snapshotID

	for next != "" {
		usage, err := snapshotter.Usage(ctx, next)
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				return total, errdefs.NotFound(errors.Wrapf(err, "non-existing ancestor of %s", snapshotID))
			}
			return total, errdefs.System(errors.Wrapf(err, "snapshotter.Usage failed for %s", next))
		}
		total.Size += usage.Size
		total.Inodes += usage.Inodes

		info, err := snapshotter.Stat(ctx, next)
		if err != nil {
			return total, errdefs.System(errors.Wrapf(err, "snapshotter.Stat failed for %s", next))
		}
		next = info.Parent
	}
	return total, nil
}
