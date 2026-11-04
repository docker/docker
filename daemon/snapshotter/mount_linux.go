package snapshotter

import (
	"github.com/containerd/containerd/v2/core/mount"
	"golang.org/x/sys/unix"
)

func unmount(target string) error {
	return mount.Unmount(target, unix.MNT_DETACH)
}
