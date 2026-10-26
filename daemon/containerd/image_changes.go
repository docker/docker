package containerd

import (
	"context"
	"errors"
	"fmt"

	"github.com/containerd/containerd/v2/core/mount"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/moby/go-archive"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/stringid"
)

func (i *ImageService) Changes(ctx context.Context, ctr *container.Container) (_ []archive.Change, retErr error) {
	rwl := ctr.RWLayer
	if rwl == nil {
		return nil, fmt.Errorf("RWLayer is unexpectedly nil for container %s", ctr.ID)
	}
	// Keep the parent view and its activation from being garbage collected
	// while they are in use.
	ctx, release, err := i.withLease(ctx, false)
	if err != nil {
		return nil, fmt.Errorf("failed to create lease for diff: %w", err)
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
		if err := snapshotter.Remove(ctx, parentViewKey); err != nil {
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

	mm := i.client.MountManager()
	// Use a temporary activation, so that a single-mount view is mounted too
	active, err := mm.Activate(ctx, parentViewKey, imageMounts, mount.WithTemporary)
	if err == nil {
		defer func() {
			if err := mm.Deactivate(context.WithoutCancel(ctx), parentViewKey); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("failed to deactivate mounts: %w", err))
			}
		}()
		imageMounts = active.System
	} else if !cerrdefs.IsNotImplemented(err) {
		return nil, fmt.Errorf("failed to activate mounts: %w", err)
	}

	var changes []archive.Change
	err = mount.WithReadonlyTempMount(ctx, imageMounts, func(imageRoot string) error {
		changes, err = archive.ChangesDirs(containerRoot, imageRoot)
		return err
	})
	return changes, err
}
