// +build freebsd

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package fs

import (
	"os"
	"syscall"

	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

func copyDevice(dst string, fi os.FileInfo) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("unsupported stat type")
	}
	return unix.Mknod(dst, uint32(fi.Mode()), st.Rdev)
}

func utimesNano(name string, atime, mtime syscall.Timespec) error {
	at := unix.NsecToTimespec(atime.Nano())
	mt := unix.NsecToTimespec(mtime.Nano())
	utimes := [2]unix.Timespec{at, mt}
	return unix.UtimesNanoAt(unix.AT_FDCWD, name, utimes[0:], unix.AT_SYMLINK_NOFOLLOW)
}
