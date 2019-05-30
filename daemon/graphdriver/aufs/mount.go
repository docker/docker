// +build linux

package aufs // import "github.com/docker/docker/daemon/graphdriver/aufs"

import (
	"os/exec"
	"syscall"

	"github.com/docker/docker/pkg/mount"
)

// Unmount the target specified.
func Unmount(target string) error {
	const (
		EINVAL  = 22 // if auplink returns this,
		retries = 3  // retry a few times
	)

	for i := 0; ; i++ {
		out, err := exec.Command("auplink", target, "flush").CombinedOutput()
		if err == nil {
			break
		}
		rc := 0
		if exiterr, ok := err.(*exec.ExitError); ok {
			if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
				rc = status.ExitStatus()
			}
		}
		if i >= retries || rc != EINVAL {
			logger.WithError(err).WithField("method", "Unmount").Warnf("auplink flush failed: %s", out)
			break
		}
		// auplink failed to find target in /proc/self/mounts because
		// kernel can't guarantee continuity while reading from it
		// while mounts table is being changed
		logger.Debugf("auplink flush error (retrying %d/%d): %s", i+1, retries, out)
	}

	return mount.Unmount(target)
}
