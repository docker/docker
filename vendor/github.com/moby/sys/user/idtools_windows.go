package user

import (
	"os"
	"syscall"
)

// mkdirAs creates path, optionally creating any missing parent directories.
//
// On Windows this is currently a thin wrapper around os.Mkdir and
// os.MkdirAll. Unlike the Unix implementation, ownership and permission
// bits are not applied.
func mkdirAs(path string, _ os.FileMode, _, _ int, mkAll bool, _ ...MkdirOpt) error {
	if mkAll {
		return os.MkdirAll(path, 0)
	}
	stat, err := os.Stat(path)
	if err == nil {
		if !stat.IsDir() {
			return &os.PathError{Op: "mkdir", Path: path, Err: syscall.ENOTDIR}
		}
		return nil
	}

	return os.Mkdir(path, 0)
}
