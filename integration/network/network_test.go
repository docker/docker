package network // import "github.com/docker/docker/integration/network"

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"fmt"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/integration/internal/container"
	"github.com/docker/docker/integration/internal/network"
	"github.com/docker/docker/integration/internal/swarm"
	"github.com/docker/docker/testutil/daemon"
	"github.com/docker/docker/testutil/request"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/icmd"
	"gotest.tools/v3/skip"
)

func TestRunContainerWithBridgeNone(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon, "cannot start daemon on remote test run")
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, IsUserNamespace())
	skip.If(t, testEnv.IsRootless, "rootless mode has different view of network")

	d := daemon.New(t)
	d.StartWithBusybox(t, "-b", "none")
	defer d.Stop(t)

	c := d.NewClientT(t)
	ctx := context.Background()

	id1 := container.Run(ctx, t, c)
	defer c.ContainerRemove(ctx, id1, types.ContainerRemoveOptions{Force: true})

	result, err := container.Exec(ctx, c, id1, []string{"ip", "l"})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(false, strings.Contains(result.Combined(), "eth0")), "There shouldn't be eth0 in container in default(bridge) mode when bridge network is disabled")

	id2 := container.Run(ctx, t, c, container.WithNetworkMode("bridge"))
	defer c.ContainerRemove(ctx, id2, types.ContainerRemoveOptions{Force: true})

	result, err = container.Exec(ctx, c, id2, []string{"ip", "l"})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(false, strings.Contains(result.Combined(), "eth0")), "There shouldn't be eth0 in container in bridge mode when bridge network is disabled")

	nsCommand := "ls -l /proc/self/ns/net | awk -F '->' '{print $2}'"
	cmd := exec.Command("sh", "-c", nsCommand)
	stdout := bytes.NewBuffer(nil)
	cmd.Stdout = stdout
	err = cmd.Run()
	assert.NilError(t, err, "Failed to get current process network namespace: %+v", err)

	id3 := container.Run(ctx, t, c, container.WithNetworkMode("host"))
	defer c.ContainerRemove(ctx, id3, types.ContainerRemoveOptions{Force: true})

	result, err = container.Exec(ctx, c, id3, []string{"sh", "-c", nsCommand})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(stdout.String(), result.Combined()), "The network namespace of container should be the same with host when --net=host and bridge network is disabled")
}

func TestNetworkInvalidJSON(t *testing.T) {
	defer setupTest(t)()

	endpoints := []string{
		"/networks/create",
		"/networks/bridge/connect",
		"/networks/bridge/disconnect",
	}

	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			t.Parallel()

			res, body, err := request.Post(ep, request.RawString("{invalid json"), request.JSON)
			assert.NilError(t, err)
			assert.Equal(t, res.StatusCode, http.StatusBadRequest)

			buf, err := request.ReadBody(body)
			assert.NilError(t, err)
			assert.Check(t, is.Contains(string(buf), "invalid character 'i' looking for beginning of object key string"))

			res, body, err = request.Post(ep, request.JSON)
			assert.NilError(t, err)
			assert.Equal(t, res.StatusCode, http.StatusBadRequest)

			buf, err = request.ReadBody(body)
			assert.NilError(t, err)
			assert.Check(t, is.Contains(string(buf), "got EOF while reading request body"))
		})
	}
}

// TestNetworkList verifies that /networks returns a list of networks either
// with, or without a trailing slash (/networks/). Regression test for https://github.com/moby/moby/issues/24595
func TestNetworkList(t *testing.T) {
	defer setupTest(t)()

	endpoints := []string{
		"/networks",
		"/networks/",
	}

	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			t.Parallel()

			res, body, err := request.Get(ep, request.JSON)
			assert.NilError(t, err)
			assert.Equal(t, res.StatusCode, http.StatusOK)

			buf, err := request.ReadBody(body)
			assert.NilError(t, err)
			var nws []types.NetworkResource
			err = json.Unmarshal(buf, &nws)
			assert.NilError(t, err)
			assert.Assert(t, len(nws) > 0)
		})
	}
}

func TestHostIPv4BridgeLabel(t *testing.T) {
	skip.If(t, testEnv.OSType == "windows")
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.IsRootless, "rootless mode has different view of network")
	d := daemon.New(t)
	d.Start(t)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()
	ctx := context.Background()

	ipv4SNATAddr := "172.0.0.172"
	// Create a bridge network with --opt com.docker.network.host_ipv4=172.0.0.172
	bridgeName := "hostIPv4Bridge"
	network.CreateNoError(ctx, t, c, bridgeName,
		network.WithDriver("bridge"),
		network.WithOption("com.docker.network.host_ipv4", ipv4SNATAddr),
		network.WithOption("com.docker.network.bridge.name", bridgeName),
	)
	out, err := c.NetworkInspect(ctx, bridgeName, types.NetworkInspectOptions{Verbose: true})
	assert.NilError(t, err)
	assert.Assert(t, len(out.IPAM.Config) > 0)
	// Make sure the SNAT rule exists
	icmd.RunCommand("iptables", "-t", "nat", "-C", "POSTROUTING", "-s", out.IPAM.Config[0].Subnet, "!", "-o", bridgeName, "-j", "SNAT", "--to-source", ipv4SNATAddr).Assert(t, icmd.Success)
}

func TestSwarmNetworkFilter(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon, "cannot start daemon on remote test run")
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, testEnv.IsRootless, "rootless mode doesn't support Swarm-mode")

	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()
	ctx := context.Background()

	netName := "test_hello"
	netNamePrefix := "test"

	network.CreateNoError(ctx, t, c, netName,
		network.WithScope("swarm"), network.WithDriver("bridge"))

	// List the network by its prefix
	kvPair := filters.Arg("name", netNamePrefix)
	args := filters.NewArgs(kvPair)
	networkResources, err := c.NetworkList(ctx, types.NetworkListOptions{Filters: args})
	assert.NilError(t, err)
	// should be found by its prefix
	assert.Assert(t, len(networkResources) == 1,
		fmt.Sprintf("%s should be filtered by %s", netName, netNamePrefix))
	assert.Assert(t, networkResources[0].Name == netName)
}
