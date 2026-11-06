package service

import (
	"strings"
	"testing"

	swarmtypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/swarm"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"gotest.tools/v3/assert"
)

func TestSwarmCAHash(t *testing.T) {
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

func TestSwarmJoinToken(t *testing.T) {
	skip.If(t, strings.HasPrefix(testEnv.FirewallBackendDriver(), "nftables"), "swarm cannot be used with nftables")
	ctx := setupTest(t)

	d1 := swarm.NewSwarm(ctx, t, testEnv)
	defer d1.Stop(t)

	d2 := daemon.New(t)
	d2.StartNode(t)
	defer d2.Stop(t)

	c2 := d2.NewClientT(t)
	defer c2.Close()

	// todo: error message differs depending if some components of token are valid

	_, err := c2.SwarmJoin(ctx, client.SwarmJoinOptions{
		ListenAddr:  d2.SwarmListenAddr(),
		RemoteAddrs: []string{d1.SwarmListenAddr()},
	})
	assert.ErrorContains(t, err, "join token")
	assert.Equal(t, d2.SwarmInfo(ctx, t).LocalNodeState, swarmtypes.LocalNodeStateInactive)

	_, err = c2.SwarmJoin(ctx, client.SwarmJoinOptions{
		JoinToken:   "foobaz",
		ListenAddr:  d2.SwarmListenAddr(),
		RemoteAddrs: []string{d1.SwarmListenAddr()},
	})
	assert.ErrorContains(t, err, "invalid join token")
	assert.Equal(t, d2.SwarmInfo(ctx, t).LocalNodeState, swarmtypes.LocalNodeStateInactive)

	workerToken := d1.JoinTokens(t).Worker
	d2.SwarmJoin(ctx, t, swarmtypes.JoinRequest{
		JoinToken:   workerToken,
		RemoteAddrs: []string{d1.SwarmListenAddr()},
	})
	assert.Equal(t, d2.SwarmInfo(ctx, t).LocalNodeState, swarmtypes.LocalNodeStateActive)
	assert.NilError(t, d2.SwarmLeave(ctx, t, false))
	assert.Equal(t, d2.SwarmInfo(ctx, t).LocalNodeState, swarmtypes.LocalNodeStateInactive)

	// change tokens
	d1.RotateTokens(t)

	_, err = c2.SwarmJoin(ctx, client.SwarmJoinOptions{
		JoinToken:   workerToken,
		ListenAddr:  d2.SwarmListenAddr(),
		RemoteAddrs: []string{d1.SwarmListenAddr()},
	})
	assert.ErrorContains(t, err, "join token is necessary")
	assert.Equal(t, d2.SwarmInfo(ctx, t).LocalNodeState, swarmtypes.LocalNodeStateInactive)

	workerToken = d1.JoinTokens(t).Worker
	d2.SwarmJoin(ctx, t, swarmtypes.JoinRequest{
		JoinToken:   workerToken,
		RemoteAddrs: []string{d1.SwarmListenAddr()},
	})
	assert.Equal(t, d2.SwarmInfo(ctx, t).LocalNodeState, swarmtypes.LocalNodeStateActive)
	assert.NilError(t, d2.SwarmLeave(ctx, t, false))
	assert.Equal(t, d2.SwarmInfo(ctx, t).LocalNodeState, swarmtypes.LocalNodeStateInactive)

	// change spec, don't change tokens
	d1.UpdateSwarm(t, func(*swarmtypes.Spec) {})

	_, err = c2.SwarmJoin(ctx, client.SwarmJoinOptions{
		ListenAddr:  d2.SwarmListenAddr(),
		RemoteAddrs: []string{d1.SwarmListenAddr()},
	})
	assert.ErrorContains(t, err, "join token")
	assert.Equal(t, d2.SwarmInfo(ctx, t).LocalNodeState, swarmtypes.LocalNodeStateInactive)

	d2.SwarmJoin(ctx, t, swarmtypes.JoinRequest{
		JoinToken:   workerToken,
		RemoteAddrs: []string{d1.SwarmListenAddr()},
	})
	assert.Equal(t, d2.SwarmInfo(ctx, t).LocalNodeState, swarmtypes.LocalNodeStateActive)
	assert.NilError(t, d2.SwarmLeave(ctx, t, false))
	assert.Equal(t, d2.SwarmInfo(ctx, t).LocalNodeState, swarmtypes.LocalNodeStateInactive)
}
