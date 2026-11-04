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
	_ = filepath.WalkDir(filepath.Join(d.execRoot, "netns"), func(path string, _ os.DirEntry, _ error) error {
		if err := unix.Unmount(path, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
			t.Logf("[%s] unmount of %s failed: %v", d.id, path, err)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Logf("[%s] error removing network namespace %s: %v", d.id, path, err)
		}
		return nil
	})
}

// CgroupNamespace returns the cgroup namespace the daemon is running in
func (d *Daemon) CgroupNamespace(t testing.TB) string {
	link, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/cgroup", d.DockerdPid(t)))
	assert.NilError(t, err)

	return strings.TrimSpace(link)
}
