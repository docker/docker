package network

import (
	"context"
	"fmt"
	"io/ioutil"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/integration/internal/container"
	"github.com/docker/docker/internal/test/request"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/poll"
)

func TestDockerNetworkLinkAlias(t *testing.T) {
	defer setupTest(t)()
	client := request.NewAPIClient(t)
	ctx := context.Background()

	mynet, err := client.NetworkCreate(ctx, "mynet", types.NetworkCreate{})
	assert.NilError(t, err)

	nws, err := client.NetworkList(ctx, types.NetworkListOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(true, containsNetwork(nws, mynet.ID)), "failed to create network mynet")

	fooID := container.Run(t, ctx, client, container.WithNetworkMode("mynet"), container.WithName("foo"))

	// These are the test cases of full id,partial id, and container name.
	aliases := []string{fooID, fooID[:5], "foo"}
	for _, l := range aliases {
		barID := container.Run(t, ctx, client, container.WithCmd("ping", "-c", "3", "server"), func(c *container.TestContainerConfig) {
			c.HostConfig.NetworkMode = "mynet"
			c.HostConfig.Links = []string{fmt.Sprintf("%s:server", l)}
			c.NetworkingConfig = &network.NetworkingConfig{
				EndpointsConfig: map[string]*network.EndpointSettings{
					"mynet": {
						Links: []string{fmt.Sprintf("%s:server", l)},
					},
				},
			}
		})
		poll.WaitOn(t, container.IsStopped(ctx, client, barID), poll.WithDelay(100*time.Millisecond))

		body, err := client.ContainerLogs(ctx, barID, types.ContainerLogsOptions{
			ShowStdout: true,
		})
		assert.NilError(t, err)

		log, err := ioutil.ReadAll(body)
		assert.NilError(t, err)
		assert.Check(t, is.Contains(string(log), "bytes from "))
	}
}
