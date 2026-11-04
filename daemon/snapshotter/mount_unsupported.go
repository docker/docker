//go:build !linux && !windows

package snapshotter

import cerrdefs "github.com/containerd/errdefs"

func unmount(string) error {
	return cerrdefs.ErrNotImplemented
}
