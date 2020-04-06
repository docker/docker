// +build !windows

package logging

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/testutil/daemon"
	"gotest.tools/assert"
	"gotest.tools/skip"
)

// Regression test for #35553
// Ensure that a daemon with a log plugin set as the default logger for containers
// does not keep the daemon from starting.
func TestDaemonStartWithLogOpt(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon, "cannot run daemon when remote daemon")
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	t.Parallel()

	d := daemon.New(t)
	d.Start(t, "--iptables=false")
	defer d.Stop(t)

	c := d.NewClientT(t)
	ctx := context.Background()

	createPlugin(t, c, "test", "dummy", asLogDriver)
	err := c.PluginEnable(ctx, "test", types.PluginEnableOptions{Timeout: 30})
	assert.Check(t, err)
	defer c.PluginRemove(ctx, "test", types.PluginRemoveOptions{Force: true})

	d.Stop(t)
	d.Start(t, "--iptables=false", "--log-driver=test", "--log-opt=foo=bar")
}
