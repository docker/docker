package service

import (
	"strings"
	"testing"

	swarmTypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/swarm"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/skip"
)

func TestSwarmCAHash(t *testing.T) {
	skip.If(t, strings.HasPrefix(testEnv.FirewallBackendDriver(), "nftables"), "swarm cannot be used with nftables")
	ctx := setupTest(t)

	d1 := swarm.NewSwarm(ctx, t, testEnv)
	defer d1.Stop(t)
	d2 := daemon.New(t)
	d2.Start(t)
	defer d2.Stop(t)

	splitToken := strings.Split(d1.JoinTokens(t).Worker, "-")
	splitToken[2] = "1kxftv4ofnc6mt30lmgipg6ngf9luhwqopfk1tz6bdmnkubg0e"
	replacementToken := strings.Join(splitToken, "-")
	c2 := d2.NewClientT(t)
	defer c2.Close()

	_, err := c2.SwarmJoin(ctx, client.SwarmJoinOptions{
		ListenAddr:  d2.SwarmListenAddr(),
		JoinToken:   replacementToken,
		RemoteAddrs: []string{d1.SwarmListenAddr()},
	})
	assert.ErrorContains(t, err, "remote CA does not match fingerprint")
}

func TestSwarmInit(t *testing.T) {
	skip.If(t, strings.HasPrefix(testEnv.FirewallBackendDriver(), "nftables"), "swarm cannot be used with nftables")
	ctx := setupTest(t)

	d1 := swarm.NewSwarm(ctx, t, testEnv)
	defer d1.Stop(t)

	// TODO: Should still find a better way to verify that components are running than /info
	info := d1.SwarmInfo(ctx, t)
	assert.Equal(t, info.ControlAvailable, true)
	assert.Equal(t, info.LocalNodeState, swarmTypes.LocalNodeStateActive)
	assert.Equal(t, info.Cluster.RootRotationInProgress, false)

	d2 := daemon.New(t)
	d2.StartAndSwarmJoin(ctx, t, d1, false)
	defer d2.Stop(t)

	info = d2.SwarmInfo(ctx, t)
	assert.Equal(t, info.ControlAvailable, false)
	assert.Equal(t, info.LocalNodeState, swarmTypes.LocalNodeStateActive)

	// Leaving swarm cluster
	assert.NilError(t, d2.SwarmLeave(ctx, t, false))

	info = d2.SwarmInfo(ctx, t)
	assert.Equal(t, info.ControlAvailable, false)
	assert.Equal(t, info.LocalNodeState, swarmTypes.LocalNodeStateInactive)

	// Rejoining cluster
	d2.SwarmJoin(ctx, t, swarmTypes.JoinRequest{
		ListenAddr:  d1.SwarmListenAddr(),
		JoinToken:   d1.JoinTokens(t).Worker,
		RemoteAddrs: []string{d1.SwarmListenAddr()},
	})

	info = d2.SwarmInfo(ctx, t)
	assert.Equal(t, info.ControlAvailable, false)
	assert.Equal(t, info.LocalNodeState, swarmTypes.LocalNodeStateActive)

	// Restarting and restoring states
	d1.Stop(t)
	d2.Stop(t)

	d1.StartNode(t)
	d2.StartNode(t)

	info = d1.SwarmInfo(ctx, t)
	assert.Equal(t, info.ControlAvailable, true)
	assert.Equal(t, info.LocalNodeState, swarmTypes.LocalNodeStateActive)

	info = d2.SwarmInfo(ctx, t)
	assert.Equal(t, info.ControlAvailable, false)
	assert.Equal(t, info.LocalNodeState, swarmTypes.LocalNodeStateActive)
}
