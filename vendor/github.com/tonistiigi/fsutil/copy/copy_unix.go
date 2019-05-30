// +build solaris darwin freebsd

package fs

import (
	"io"
	"os"
	"syscall"

	"github.com/containerd/containerd/sys"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

func getUidGid(fi os.FileInfo) (uid, gid int) {
	st := fi.Sys().(*syscall.Stat_t)
	return int(st.Uid), int(st.Gid)
}

func (c *copier) copyFileInfo(fi os.FileInfo, name string) error {
	st := fi.Sys().(*syscall.Stat_t)
	chown := c.chown
	if chown == nil {
		uid, gid := getUidGid(fi)
		chown = &ChownOpt{Uid: uid, Gid: gid}
	}
	if err := Chown(name, chown); err != nil {
		return errors.Wrapf(err, "failed to chown %s", name)
	}

	m := fi.Mode()
	if c.mode != nil {
		m = (m & ^os.FileMode(0777)) | os.FileMode(*c.mode&0777)
	}
	if (fi.Mode() & os.ModeSymlink) != os.ModeSymlink {
		if err := os.Chmod(name, m); err != nil {
			return errors.Wrapf(err, "failed to chmod %s", name)
		}
	}

	if c.utime != nil {
		if err := Utimes(name, c.utime); err != nil {
			return err
		}
	} else {
		timespec := []unix.Timespec{unix.Timespec(sys.StatAtime(st)), unix.Timespec(sys.StatMtime(st))}
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, name, timespec, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return errors.Wrapf(err, "failed to utime %s", name)
		}
	}
	return nil
}

func copyFileContent(dst, src *os.File) error {
	buf := bufferPool.Get().(*[]byte)
	_, err := io.CopyBuffer(dst, src, *buf)
	bufferPool.Put(buf)

	return err
}

func copyDevice(dst string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("unsupported stat type")
	}
	return unix.Mknod(dst, uint32(fi.Mode()), int(st.Rdev))
}
