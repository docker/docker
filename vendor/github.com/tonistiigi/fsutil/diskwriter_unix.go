//go:build !windows

package fsutil

import (
	"os"
	"syscall"

	"github.com/containerd/continuity/sysx"
	"github.com/pkg/errors"
	"github.com/tonistiigi/fsutil/types"
)

func rewriteMetadata(p string, stat *types.Stat) error {
	for key, value := range stat.Xattrs {
		sysx.LSetxattr(p, key, value, 0)
	}

	if err := os.Lchown(p, int(stat.Uid), int(stat.Gid)); err != nil {
		return errors.WithStack(err)
	}

	if os.FileMode(stat.Mode)&os.ModeSymlink == 0 {
		if err := os.Chmod(p, os.FileMode(stat.Mode)); err != nil {
			return errors.WithStack(err)
		}
	}

	if err := chtimes(p, stat.ModTime); err != nil {
		return err
	}

	return nil
}

// handleTarTypeBlockCharFifo is an OS-specific helper function used by
// createTarFile to handle the following types of header: Block; Char; Fifo
func handleTarTypeBlockCharFifo(path string, stat *types.Stat) error {
	mode := uint32(stat.Mode & 07777)
	if os.FileMode(stat.Mode)&os.ModeCharDevice != 0 {
		mode |= syscall.S_IFCHR
	} else if os.FileMode(stat.Mode)&os.ModeNamedPipe != 0 {
		mode |= syscall.S_IFIFO
	} else {
		mode |= syscall.S_IFBLK
	}

	if err := createSpecialFile(path, mode, stat); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func renameFile(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		return errors.Wrapf(err, "failed to rename %s to %s", src, dst)
	}
	return nil
}
