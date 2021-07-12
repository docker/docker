package system

import (
	"sync"

	"golang.org/x/sys/unix"
)

var seccompSupported bool
var seccompOnce sync.Once

func SeccompSupported() bool {
	seccompOnce.Do(func() {
		seccompSupported = getSeccompSupported()
	})
	return seccompSupported
}

func getSeccompSupported() bool {
	if err := unix.Prctl(unix.PR_GET_SECCOMP, 0, 0, 0, 0); err != unix.EINVAL {
		// Make sure the kernel has CONFIG_SECCOMP_FILTER.
		if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, 0, 0, 0); err != unix.EINVAL {
			return true
		}
	}
	return false
}
