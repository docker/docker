// +build !windows

package mount

import "golang.org/x/sys/unix"

func unmountBare(target string, flags int) error {
	return unix.Unmount(target, flags)
}

func unmount(target string, flags int) error {
	err := unmountBare(target, flags)
	if err == nil || err == unix.EINVAL {
		// Ignore "not mounted" error here. Note the same error
		// can be returned if flags are invalid, so this code
		// assumes that the flags value is always correct.
		return nil
	}

	return &mountError{
		op:     "umount",
		target: target,
		flags:  uintptr(flags),
		err:    err,
	}
}
