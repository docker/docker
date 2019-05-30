// +build !windows

package fs

import (
	"time"

	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

func fixRootDirectory(p string) string {
	return p
}

func Utimes(p string, tm *time.Time) error {
	if tm == nil {
		return nil
	}

	ts, err := unix.TimeToTimespec(*tm)
	if err != nil {
		return err
	}

	timespec := []unix.Timespec{ts, ts}
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, p, timespec, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return errors.Wrapf(err, "failed to utime %s", p)
	}

	return nil
}
