package bridge

import (
	"errors"
	"testing"

	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge"
	"github.com/moby/moby/v2/daemon/libnetwork/nlwrap"
	"github.com/moby/moby/v2/integration/internal/network"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"github.com/vishvananda/netlink"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

// TestNetworkInitError checks that, if the default bridge network can't be restored on startup,
// it doesn't prevent the daemon from starting once the underlying problem is resolved.
// Regression test for https://github.com/moby/moby/issues/49291
func TestNetworkInitErrorDocker0(t *testing.T) {
	d := daemon.New(t)
	d.Start(t)
	defer func() {
		_ = d.StopWithError()
	}()

	const brName = "docker0"
	d.SetEnvVar("DOCKER_TEST_BRIDGE_INIT_ERROR", brName)
	err := d.RestartWithError()
	assert.Assert(t, is.ErrorContains(err, "daemon exited during startup"))

	d.SetEnvVar("DOCKER_TEST_BRIDGE_INIT_ERROR", "")
	d.Start(t)
}

// TestNetworkInitErrorUserDefined is equivalent to TestNetworkInitErrorDocker0, for a
// user-defined network. But, the daemon doesn't try to delete a user-defined network
// and the daemon will still start if it can't be restored on startup. So, try to
// delete the network when it's failed to initialise, and check that it can be
// re-created when the initialisation problem has been resolved.
func TestNetworkInitErrorUserDefined(t *testing.T) {
	ctx := setupTest(t)
	d := daemon.New(t)
	d.Start(t)
	defer func() {
		_ = d.StopWithError()
	}()

	apiClient := d.NewClientT(t)

	const netName = "testnet"
	const brName = "br-" + netName
	network.CreateNoError(ctx, t, apiClient, netName,
		network.WithOption(bridge.BridgeName, brName),
	)
	defer network.RemoveNoError(ctx, t, apiClient, netName)

	d.SetEnvVar("DOCKER_TEST_BRIDGE_INIT_ERROR", brName)
	d.Restart(t)

	_, err := apiClient.NetworkRemove(ctx, netName, client.NetworkRemoveOptions{})
	assert.NilError(t, err)

	d.SetEnvVar("DOCKER_TEST_BRIDGE_INIT_ERROR", "")
	d.Restart(t)
	network.CreateNoError(ctx, t, apiClient, netName,
		network.WithOption(bridge.BridgeName, brName),
	)
}

// TestNetworkCreateErrorNoBridge checks that no bridge device gets left around
// when there's an error during network creation.
func TestNetworkCreateErrorNoBridge(t *testing.T) {
	ctx := setupTest(t)
	d := daemon.New(t)

	const netName = "testnet"
	const brName = "br-" + netName

	d.SetEnvVar("DOCKER_TEST_BRIDGE_INIT_ERROR", brName)
	d.Start(t)
	defer d.Stop(t)

	apiClient := d.NewClientT(t)

	_, err := network.Create(ctx, apiClient, netName, network.WithOption(bridge.BridgeName, brName))
	if err == nil {
		defer network.RemoveNoError(ctx, t, apiClient, brName)
		t.Fatalf("expected an error creating the network")
	}
	assert.Check(t, is.ErrorContains(err, "DOCKER_TEST_BRIDGE_INIT_ERROR"))

	// Check there's no bridge device.
	_, err = nlwrap.LinkByName(brName)
	assert.Check(t, errors.As(err, &netlink.LinkNotFoundError{}), "expected LinkNotFoundError, got: %v", err)
}
