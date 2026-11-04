//go:build !windows

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/v2/integration-cli/checker"
	"github.com/moby/moby/v2/internal/testutil"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/poll"
)

func (s *DockerSwarmSuite) TestSwarmVolumePlugin(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := s.AddDaemon(ctx, c, true, true)

	out, err := d.Cmd("service", "create", "--detach", "--no-resolve-image", "--mount", "type=volume,source=my-volume,destination=/foo,volume-driver=customvolumedriver", "--name", "top", "busybox", "top")
	assert.NilError(c, err, out)

	// Make sure task stays pending before plugin is available
	poll.WaitOn(c, pollCheck(c, d.CheckServiceTasksInStateWithError(ctx, "top", swarm.TaskStatePending, "missing plugin on 1 node"), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))

	plugin := newVolumePlugin(c, "customvolumedriver")
	defer plugin.Close()

	// create a dummy volume to trigger lazy loading of the plugin
	out, err = d.Cmd("volume", "create", "-d", "customvolumedriver", "hello")
	assert.NilError(c, err, out)

	// TODO(aaronl): It will take about 15 seconds for swarm to realize the
	// plugin was loaded. Switching the test over to plugin v2 would avoid
	// this long delay.

	// make sure task has been deployed.
	poll.WaitOn(c, pollCheck(c, d.CheckActiveContainerCount(ctx), checker.Equals(1)), poll.WithTimeout(defaultReconciliationTimeout))

	out, err = d.Cmd("ps", "-q")
	assert.NilError(c, err)
	containerID := strings.TrimSpace(out)

	out, err = d.Cmd("inspect", "-f", "{{json .Mounts}}", containerID)
	assert.NilError(c, err)

	var mounts []struct {
		Name   string
		Driver string
	}

	assert.NilError(c, json.NewDecoder(strings.NewReader(out)).Decode(&mounts))
	assert.Equal(c, len(mounts), 1, out)
	assert.Equal(c, mounts[0].Name, "my-volume")
	assert.Equal(c, mounts[0].Driver, "customvolumedriver")
}

// Test network plugin filter in swarm
func (s *DockerSwarmSuite) TestSwarmNetworkPluginV2(c *testing.T) {
	testRequires(c, IsAmd64)
	ctx := testutil.GetContext(c)
	d1 := s.AddDaemon(ctx, c, true, true)
	d2 := s.AddDaemon(ctx, c, true, false)

	// install plugin on d1 and d2
	const pluginName = "aragunathan/global-net-plugin:latest"

	_, err := d1.Cmd("plugin", "install", pluginName, "--grant-all-permissions")
	assert.NilError(c, err)

	_, err = d2.Cmd("plugin", "install", pluginName, "--grant-all-permissions")
	assert.NilError(c, err)

	// create network
	const networkName = "globalnet"
	_, err = d1.Cmd("network", "create", "--driver", pluginName, networkName)
	assert.NilError(c, err)

	// create a global service to ensure that both nodes will have an instance
	const serviceName = "my-service"
	_, err = d1.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", serviceName, "--mode=global", "--network", networkName, "busybox", "top")
	assert.NilError(c, err)

	// wait for tasks ready
	poll.WaitOn(c, pollCheck(c, reducedCheck(sumAsIntegers, d1.CheckActiveContainerCount(ctx), d2.CheckActiveContainerCount(ctx)), checker.Equals(2)), poll.WithTimeout(defaultReconciliationTimeout))

	// remove service
	_, err = d1.Cmd("service", "rm", serviceName)
	assert.NilError(c, err)

	// wait to ensure all containers have exited before removing the plugin. Else there's a
	// possibility of container exits erroring out due to plugins being unavailable.
	poll.WaitOn(c, pollCheck(c, reducedCheck(sumAsIntegers, d1.CheckActiveContainerCount(ctx), d2.CheckActiveContainerCount(ctx)), checker.Equals(0)), poll.WithTimeout(defaultReconciliationTimeout))

	// disable plugin on worker
	_, err = d2.Cmd("plugin", "disable", "-f", pluginName)
	assert.NilError(c, err)

	const imgName = "busybox:latest"
	// create a new global service again.
	_, err = d1.Cmd("service", "create", "--detach", "--no-resolve-image", "--name", serviceName, "--mode=global", "--network", networkName, imgName, "top")
	assert.NilError(c, err)

	poll.WaitOn(c, pollCheck(c, d1.CheckRunningTaskImages(ctx), checker.DeepEquals(map[string]int{imgName: 1})), poll.WithTimeout(defaultReconciliationTimeout))
}
