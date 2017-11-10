package swarm

import (
	"testing"

	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/integration/util/request"
	"github.com/gotestyourself/gotestyourself/skip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/context"
)

func TestSwarmUpdate(t *testing.T) {
	skip.IfCondition(t, !testEnv.IsLocalDaemon())
	defer setupTest(t)()

	client := request.NewAPIClient(t)
	ctx := context.Background()

	_, err := client.SwarmInit(ctx, swarm.InitRequest{ListenAddr: "127.0.0.1:2478", AdvertiseAddr: "127.0.0.1"})
	require.NoError(t, err)

	swarmInspect, err := client.SwarmInspect(ctx)
	require.NoError(t, err)
	err = client.SwarmUpdate(ctx, swarmInspect.Version, swarm.Spec{Annotations: swarm.Annotations{Name: "whoops"}}, swarm.UpdateFlags{})
	assert.EqualError(t, err, `Error response from daemon: invalid Name "whoops": swarm spec must be named "default"`)
}
