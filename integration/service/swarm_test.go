package service

import (
	"slices"
	"strings"
	"testing"
	"time"

	swarmtypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/swarm"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/poll"
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

func TestSwarmNodeRemove(t *testing.T) {
	skip.If(t, strings.HasPrefix(testEnv.FirewallBackendDriver(), "nftables"), "swarm cannot be used with nftables")
	ctx := setupTest(t)

	manager := swarm.NewSwarm(ctx, t, testEnv, daemon.WithSwarmListenAddr("127.0.0.2"))
	defer manager.Cleanup(t)
	defer manager.Stop(t)

	worker1 := daemon.New(t, daemon.WithSwarmListenAddr("127.0.0.3"))
	worker1.StartAndSwarmJoin(ctx, t, manager, false)
	defer worker1.Cleanup(t)
	defer worker1.Stop(t)

	worker2 := daemon.New(t, daemon.WithSwarmListenAddr("127.0.0.4"))
	worker2.StartAndSwarmJoin(ctx, t, manager, false)
	defer worker2.Cleanup(t)
	defer worker2.Stop(t)

	worker1NodeID := worker1.NodeID()

	apiClient := manager.NewClientT(t)

	waitForNodes := func(expectedCount int, absentNodeID string) {
		poll.WaitOn(t, func(_ poll.LogT) poll.Result {
			result, err := apiClient.NodeList(ctx, client.NodeListOptions{})
			if err != nil {
				return poll.Error(err)
			}
			if len(result.Items) != expectedCount {
				return poll.Continue("expected %d nodes, got %d", expectedCount, len(result.Items))
			}
			if absentNodeID != "" && slices.ContainsFunc(result.Items, func(node swarmtypes.Node) bool {
				return node.ID == absentNodeID
			}) {
				return poll.Continue("node %s is still present", absentNodeID)
			}
			return poll.Success()
		}, poll.WithTimeout(30*time.Second), poll.WithDelay(100*time.Millisecond))
	}

	waitForNodes(3, "")

	_, err := apiClient.NodeRemove(ctx, worker1NodeID, client.NodeRemoveOptions{Force: true})
	assert.NilError(t, err)
	waitForNodes(2, worker1NodeID)

	worker1.RestartNode(t)

	// Give the restarted worker an opportunity to rejoin before checking that it remains removed.
	time.Sleep(time.Second)

	result, err := apiClient.NodeList(ctx, client.NodeListOptions{})
	assert.NilError(t, err)
	assert.Equal(t, len(result.Items), 2)
	assert.Assert(t, !slices.ContainsFunc(result.Items, func(node swarmtypes.Node) bool {
		return node.ID == worker1NodeID
	}))
}
