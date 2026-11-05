//go:build !windows

package daemon

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/moby/sys/mount"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

const (
	defaultContainerdSocket      = "/var/run/docker/containerd/containerd.sock"
	defaultDockerdRootlessBinary = "dockerd-rootless.sh"
	defaultUnixSocket            = "/var/run/docker.sock"
)

func (d *Daemon) rootlessCommand(dockerdBinary string) (string, []string, error) {
	if d.rootlessUser == nil {
		return dockerdBinary, nil, nil
	}
	if d.dockerdBinary != DefaultDockerdBinary {
		return "", nil, errors.Errorf("[%s] DOCKER_ROOTLESS doesn't support non-default dockerd binary path %q", d.id, d.dockerdBinary)
	}
	// Older sudo versions use secure_path to look up the command even when
	// PATH is preserved for the command's environment.
	rootlessBinary, err := exec.LookPath(defaultDockerdRootlessBinary)
	if err != nil {
		return "", nil, err
	}
	env := []string{
		"-u", d.rootlessUser.Username,
		"--preserve-env",
		"--preserve-env=PATH", // Pass through PATH, overriding secure_path.
		"XDG_RUNTIME_DIR=" + d.rootlessXDGRuntimeDir,
		"HOME=" + d.rootlessUser.HomeDir,
	}
	if os.Geteuid() != 0 {
		// When the test binary itself runs as a non-root user, d.rootlessUser
		// may be that same real user account, which could already be running
		// a rootless dockerd of its own outside of this test suite (e.g. set
		// up via dockerd-rootless-setuptool.sh + systemd). Use a state dir
		// that's distinct from the default $XDG_RUNTIME_DIR/dockerd-rootless
		// to avoid colliding with it.
		env = append(env, "DOCKERD_ROOTLESS_ROOTLESSKIT_STATE_DIR="+filepath.Join(d.rootlessXDGRuntimeDir, "dockerd-rootless-testsuite"))
	}
	return "sudo", append(env, "--", rootlessBinary), nil
}

func (d *Daemon) platformArgs() []string {
	return []string{"--userland-proxy=" + strconv.FormatBool(d.userlandProxy)}
}

func defaultHostConfig() (*http.Transport, string, string, string) {
	return &http.Transport{}, "http", "unix", defaultUnixSocket
}

// cleanupMount unmounts the daemon root directory, or logs a message if
// unmounting failed.
func cleanupMount(t testing.TB, d *Daemon) {
	t.Helper()
	if err := mount.Unmount(d.Root); err != nil {
		d.log.Logf("[%s] unable to unmount daemon root (%s): %v", d.id, d.Root, err)
	}
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
