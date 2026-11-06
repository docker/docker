package service

import (
	"testing"

	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/swarm"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestAPISwarmListNodes(t *testing.T) {
	ctx := setupTest(t)

	d1 := swarm.NewSwarm(ctx, t, testEnv)
	defer d1.Cleanup(t)
	defer d1.Stop(t)

	d2 := daemon.New(t, daemon.WithSwarmPort(daemon.DefaultSwarmPort+1))
	d2.StartAndSwarmJoin(ctx, t, d1, false)
	defer d2.Cleanup(t)
	defer d2.Stop(t)

	d3 := daemon.New(t, daemon.WithSwarmPort(daemon.DefaultSwarmPort+2))
	d3.StartAndSwarmJoin(ctx, t, d1, false)
	defer d3.Cleanup(t)
	defer d3.Stop(t)

	apiClient := d1.NewClientT(t)
	result, err := apiClient.NodeList(ctx, client.NodeListOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Len(result.Items, 3))

	nodeIDs := map[string]struct{}{
		d1.NodeID(): {},
		d2.NodeID(): {},
		d3.NodeID(): {},
	}
	resultNodeIDs := make(map[string]struct{}, len(result.Items))
	for _, node := range result.Items {
		resultNodeIDs[node.ID] = struct{}{}
	}
	assert.DeepEqual(t, nodeIDs, resultNodeIDs)
}
