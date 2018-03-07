// +build !windows

package daemon // import "github.com/docker/docker/testutil/daemon"

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/moby/sys/mount"
	"golang.org/x/sys/unix"
	"gotest.tools/v3/assert"
)

// cleanupMount unmounts the daemon root directory, or logs a message if
// unmounting failed.
func cleanupMount(t testing.TB, d *Daemon) {
	t.Helper()
	if err := mount.Unmount(d.Root); err != nil {
		d.log.Logf("[%s] unable to unmount daemon root (%s): %v", d.id, d.Root, err)
	}
}

func cleanupNetworkNamespace(t testing.TB, d *Daemon) {
	t.Helper()
	// Cleanup network namespaces in the exec root of this
	// daemon because this exec root is specific to this
	// daemon instance and has no chance of getting
	// cleaned up when a new daemon is instantiated with a
	// new exec root.
	netnsPath := filepath.Join(d.execRoot, "netns")
	files, err := ioutil.ReadDir(netnsPath)
	if err != nil {
		if !os.IsNotExist(err) {
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
		if err := unix.Unmount(path, unix.MNT_DETACH); err != nil && err != unix.EINVAL && err != unix.ENOENT {
			t.Logf("[%s] unmount of network namespace %s failed: %v", d.id, path, err)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Logf("[%s] unable to remove network namespace %s: %s", d.id, path, err)
		}
	}
}

// CgroupNamespace returns the cgroup namespace the daemon is running in
func (d *Daemon) CgroupNamespace(t testing.TB) string {
	link, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/cgroup", d.Pid()))
	assert.NilError(t, err)

	return strings.TrimSpace(link)
}

// SignalDaemonDump sends a signal to the daemon to write a dump file
func SignalDaemonDump(pid int) {
	_ = unix.Kill(pid, unix.SIGQUIT)
}

func signalDaemonReload(pid int) error {
	return unix.Kill(pid, unix.SIGHUP)
}

func setsid(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}
