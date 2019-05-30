package daemon // import "github.com/docker/docker/internal/test/daemon"

import (
	"fmt"
	"strconv"

	"golang.org/x/sys/windows"
	"gotest.tools/assert"
)

// SignalDaemonDump sends a signal to the daemon to write a dump file
func SignalDaemonDump(pid int) {
	ev, _ := windows.UTF16PtrFromString("Global\\docker-daemon-" + strconv.Itoa(pid))
	h2, err := windows.OpenEvent(0x0002, false, ev)
	if h2 == 0 || err != nil {
		return
	}
	windows.PulseEvent(h2)
}

func signalDaemonReload(pid int) error {
	return fmt.Errorf("daemon reload not supported")
}

func cleanupNetworkNamespace(t testingT, execRoot string) {
}

// CgroupNamespace returns the cgroup namespace the daemon is running in
func (d *Daemon) CgroupNamespace(t assert.TestingT) string {
	assert.Assert(t, false)
	return "cgroup namespaces are not supported on Windows"
}
