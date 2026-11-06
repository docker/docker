package containerd

import (
	"context"
	"os"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/log"
	"github.com/moby/sys/user"
)

// remappedSnapshotter makes the remapped root own the root directory of the
// snapshots it prepares without a parent, as the remapped diff service does
// for the layers it applies.
type remappedSnapshotter struct {
	snapshots.Snapshotter
	uid, gid int
}

// NewRemappedSnapshotter returns sn, with the root directory of the snapshots
// prepared without a parent owned by the remapped root of idMapping.
// Otherwise, the remapped root could not create files in the root directory
// of a container based on such a snapshot.
func NewRemappedSnapshotter(sn snapshots.Snapshotter, idMapping user.IdentityMapping) snapshots.Snapshotter {
	if idMapping.Empty() {
		return sn
	}
	uid, gid := idMapping.RootPair()
	return &remappedSnapshotter{Snapshotter: sn, uid: uid, gid: gid}
}

func (s *remappedSnapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	mounts, err := s.Snapshotter.Prepare(ctx, key, parent, opts...)
	if err != nil || parent != "" {
		return mounts, err
	}

	if len(mounts) == 1 && mounts[0].Type == "bind" {
		err = os.Lchown(mounts[0].Source, s.uid, s.gid)
	} else {
		err = mount.WithTempMount(ctx, mounts, func(root string) error {
			return os.Lchown(root, s.uid, s.gid)
		})
	}
	if err != nil {
		if rmErr := s.Snapshotter.Remove(ctx, key); rmErr != nil {
			log.G(ctx).WithError(rmErr).WithField("key", key).Warn("failed to remove snapshot")
		}
		return nil, err
	}
	return mounts, nil
}
