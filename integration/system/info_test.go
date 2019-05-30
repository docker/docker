package system // import "github.com/docker/docker/integration/system"

import (
	"context"
	"fmt"
	"testing"

	"github.com/docker/docker/internal/test/daemon"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/skip"
)

func TestInfoAPI(t *testing.T) {
	defer setupTest(t)()
	client := testEnv.APIClient()

	info, err := client.Info(context.Background())
	assert.NilError(t, err)

	// always shown fields
	stringsToCheck := []string{
		"ID",
		"Containers",
		"ContainersRunning",
		"ContainersPaused",
		"ContainersStopped",
		"Images",
		"LoggingDriver",
		"OperatingSystem",
		"NCPU",
		"OSType",
		"Architecture",
		"MemTotal",
		"KernelVersion",
		"Driver",
		"ServerVersion",
		"SecurityOptions"}

	out := fmt.Sprintf("%+v", info)
	for _, linePrefix := range stringsToCheck {
		assert.Check(t, is.Contains(out, linePrefix))
	}
}

func TestInfoAPIWarnings(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon, "cannot run daemon when remote daemon")
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "FIXME")
	d := daemon.New(t)
	c := d.NewClientT(t)

	d.StartWithBusybox(t, "-H=0.0.0.0:23756", "-H="+d.Sock())
	defer d.Stop(t)

	info, err := c.Info(context.Background())
	assert.NilError(t, err)

	stringsToCheck := []string{
		"Access to the remote API is equivalent to root access",
		"http://0.0.0.0:23756",
	}

	out := fmt.Sprintf("%+v", info)
	for _, linePrefix := range stringsToCheck {
		assert.Check(t, is.Contains(out, linePrefix))
	}
}
