package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	"gotest.tools/v3/assert"
)

func cleanupNetworkNamespace(t testing.TB, d *Daemon) {
	t.Helper()
	// Cleanup network namespaces in the exec root of this
	// daemon because this exec root is specific to this
	// daemon instance and has no chance of getting
	// cleaned up when a new daemon is instantiated with a
	// new exec root.
	netnsPath := filepath.Join(d.execRoot, "netns")
	files, err := os.ReadDir(netnsPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			t.Logf("[%s] unable to read netns path (%s): %s", d.id, netnsPath, err)
		}
		return
	}

	for _, dir := range files {
		if !dir.IsDir() {
			continue
		}

		path := filepath.Join(netnsPath, dir.Name())

		// Unmount the network namespace using MNT_DETACH (lazy unmount).
		// Ignore EINVAL ("target is not a mount point"), and ENOENT (not found),
		// which could occur if the network namespace was already gone.
		if err := unix.Unmount(path, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
			t.Logf("[%s] unmount of network namespace %s failed: %v", d.id, path, err)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Logf("[%s] unable to remove network namespace %s: %s", d.id, path, err)
		}
	}
}

// CgroupNamespace returns the cgroup namespace the daemon is running in
func (d *Daemon) CgroupNamespace(t testing.TB) string {
	link, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/cgroup", d.DockerdPid(t)))
	assert.NilError(t, err)

	return strings.TrimSpace(link)
}
