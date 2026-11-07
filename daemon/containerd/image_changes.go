package containerd

import (
	"context"
	"fmt"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/log"
	"github.com/moby/go-archive"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/stringid"
)

func (i *ImageService) Changes(ctx context.Context, ctr *container.Container) ([]archive.Change, error) {
	rwl := ctr.RWLayer
	if rwl == nil {
		return nil, fmt.Errorf("RWLayer is unexpectedly nil for container %s", ctr.ID)
	}
	release, err := i.acquireChanges(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	snapshotter := i.client.SnapshotService(ctr.Driver)
	info, err := snapshotter.Stat(ctx, ctr.ID)
	if err != nil {
		return nil, err
	}

	id := stringid.GenerateRandomID()
	parentViewKey := ctr.ID + "-parent-view-" + id
	imageMounts, _ := snapshotter.View(ctx, parentViewKey, info.Parent)

	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := snapshotter.Remove(cleanupCtx, parentViewKey); err != nil {
			log.G(ctx).WithError(err).Warn("error removing the parent view snapshot")
		}
	}()

	containerRoot, err := rwl.Mount(ctr.GetMountLabel())
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := rwl.Unmount(); err != nil {
			log.G(ctx).WithFields(log.Fields{"error": err, "container": ctr.ID}).Warn("Failed to unmount container RWLayer after export")
		}
	}()
	var changes []archive.Change
	err = mount.WithReadonlyTempMount(ctx, imageMounts, func(imageRoot string) error {
		changes, err = archive.ChangesDirsContext(ctx, containerRoot, imageRoot)
		return err
	})
	return changes, err
}

// acquireChanges reserves one slot for a potentially memory-intensive filesystem
// comparison. A nil limiter is allowed for ImageService values constructed
// directly in tests.
func (i *ImageService) acquireChanges(ctx context.Context) (func(), error) {
	if i.changesLimiter == nil {
		return func() {}, nil
	}
	if err := i.changesLimiter.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	return func() { i.changesLimiter.Release(1) }, nil
}
