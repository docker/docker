package apisocket

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/moby/sys/reexec"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/skip"
)

const (
	socketConnectCommand = "apisocket-test-connect"
	unprivilegedUID      = 65534
)

func TestMain(m *testing.M) {
	reexec.Register(socketConnectCommand, func() {
		if uid := os.Geteuid(); uid != unprivilegedUID {
			fmt.Fprintf(os.Stderr, "connection probe has unexpected UID %d\n", uid)
			os.Exit(1)
		}

		c, err := net.DialTimeout("unix", os.Args[1], 5*time.Second) // #nosec G704 -- reexec dials a test-owned Unix socket.
		if c != nil {
			defer c.Close()
		}
		if os.Args[2] == "true" && err == nil {
			return
		}
		if os.Args[2] == "false" && errors.Is(err, os.ErrPermission) {
			return
		}
		fmt.Fprintf(os.Stderr, "unexpected connection result (allow %s): %v\n", os.Args[2], err)
		os.Exit(1)
	})
	if reexec.Init() {
		return
	}
	os.Exit(m.Run())
}

func TestManagerRejectsOtherUsers(t *testing.T) {
	t.Parallel()
	skip.If(t, os.Geteuid() != 0, "requires root to connect as another user")

	// Keep every parent traversable so only the socket can deny access.
	dir, err := os.MkdirTemp("/tmp", "apisocket")
	assert.NilError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	assert.NilError(t, os.Chmod(dir, 0o755))

	path := filepath.Join(dir, "api.sock")

	m := NewManager()

	t.Cleanup(func() { assert.NilError(t, m.Close()) })
	assert.NilError(t, m.Open(t.Context(), path))

	checkConnection := func(gid uint32, allow bool) {
		t.Helper()

		// Keep the creating thread alive for reexec's parent-death signal.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		cmd := reexec.Command(socketConnectCommand, path, strconv.FormatBool(allow))
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: unprivilegedUID, Gid: gid}
		out, err := cmd.CombinedOutput()
		assert.NilError(t, err, "connection probe with GID %d (allow %t): %s", gid, allow, out)
	}

	sameGroup := uint32(os.Getegid())
	checkConnection(sameGroup, false)
	checkConnection(sameGroup+1, false)

	// Prove both probes can reach the listener when socket access is allowed.
	assert.NilError(t, os.Chmod(path, 0o666))
	checkConnection(sameGroup, true)
	checkConnection(sameGroup+1, true)
}
