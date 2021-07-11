package service // import "github.com/docker/docker/integration/service"

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/integration/internal/container"
	net "github.com/docker/docker/integration/internal/network"
	"github.com/docker/docker/integration/internal/swarm"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

func TestDockerNetworkConnectAlias(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	client := d.NewClientT(t)
	defer client.Close()
	ctx := context.Background()

	name := t.Name() + "test-alias"
	net.CreateNoError(ctx, t, client, name,
		net.WithDriver("overlay"),
		net.WithAttachable(),
	)

	cID1 := container.Create(ctx, t, client, func(c *container.TestContainerConfig) {
		c.NetworkingConfig = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				name: {},
			},
		}
	})

	err := client.NetworkConnect(ctx, name, cID1, &network.EndpointSettings{
		Aliases: []string{
			"aaa",
		},
	})
	assert.NilError(t, err)

	err = client.ContainerStart(ctx, cID1, types.ContainerStartOptions{})
	assert.NilError(t, err)

	ng1, err := client.ContainerInspect(ctx, cID1)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(ng1.NetworkSettings.Networks[name].Aliases), 2))
	assert.Check(t, is.Equal(ng1.NetworkSettings.Networks[name].Aliases[0], "aaa"))

	cID2 := container.Create(ctx, t, client, func(c *container.TestContainerConfig) {
		c.NetworkingConfig = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				name: {},
			},
		}
	})

	err = client.NetworkConnect(ctx, name, cID2, &network.EndpointSettings{
		Aliases: []string{
			"bbb",
		},
	})
	assert.NilError(t, err)

	err = client.ContainerStart(ctx, cID2, types.ContainerStartOptions{})
	assert.NilError(t, err)

	ng2, err := client.ContainerInspect(ctx, cID2)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(ng2.NetworkSettings.Networks[name].Aliases), 2))
	assert.Check(t, is.Equal(ng2.NetworkSettings.Networks[name].Aliases[0], "bbb"))
}

func TestDockerNetworkReConnect(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	client := d.NewClientT(t)
	defer client.Close()
	ctx := context.Background()

	name := t.Name() + "dummyNet"
	net.CreateNoError(ctx, t, client, name,
		net.WithDriver("overlay"),
		net.WithAttachable(),
	)

	c1 := container.Create(ctx, t, client, func(c *container.TestContainerConfig) {
		c.NetworkingConfig = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				name: {},
			},
		}
	})

	err := client.NetworkConnect(ctx, name, c1, &network.EndpointSettings{})
	assert.NilError(t, err)

	err = client.ContainerStart(ctx, c1, types.ContainerStartOptions{})
	assert.NilError(t, err)

	n1, err := client.ContainerInspect(ctx, c1)
	assert.NilError(t, err)

	err = client.NetworkConnect(ctx, name, c1, &network.EndpointSettings{})
	assert.ErrorContains(t, err, "is already attached to network")

	n2, err := client.ContainerInspect(ctx, c1)
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(n1, n2))
}

func TestDockerRestartWithAttachbleNetwork(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	client := d.NewClientT(t)
	defer client.Close()
	ctx := context.Background()

	name := t.Name() + "dummyNet"
	net.CreateNoError(ctx, t, client, name,
		net.WithDriver("overlay"),
		net.WithAttachable(),
	)

	c1 := container.Create(ctx, t, client, func(c *container.TestContainerConfig) {
		c.NetworkingConfig = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				name: {},
			},
		}
	})

	err := client.ContainerStart(ctx, c1, types.ContainerStartOptions{})
	assert.NilError(t, err)

	var timeout time.Duration = 20 * time.Second
	err = client.ContainerRestart(ctx, c1, &timeout)
	assert.NilError(t, err)

}
