package container

import (
	"context"
	"runtime"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
)

// TestContainerConfig holds container configuration struct that
// are used in api calls.
type TestContainerConfig struct {
	Name             string
	Config           *container.Config
	HostConfig       *container.HostConfig
	NetworkingConfig *network.NetworkingConfig
	Platform         *specs.Platform
}

// create creates a container with the specified options
func create(ctx context.Context, t *testing.T, client client.APIClient, ops ...func(*TestContainerConfig)) (container.ContainerCreateCreatedBody, error) {
	t.Helper()
	cmd := []string{"top"}
	if runtime.GOOS == "windows" {
		cmd = []string{"sleep", "240"}
	}
	config := &TestContainerConfig{
		Config: &container.Config{
			Image: "busybox",
			Cmd:   cmd,
		},
		HostConfig:       &container.HostConfig{},
		NetworkingConfig: &network.NetworkingConfig{},
	}

	for _, op := range ops {
		op(config)
	}

	return client.ContainerCreate(ctx, config.Config, config.HostConfig, config.NetworkingConfig, config.Platform, config.Name)
}

// Create creates a container with the specified options, asserting that there was no error
func Create(ctx context.Context, t *testing.T, client client.APIClient, ops ...func(*TestContainerConfig)) string {
	t.Helper()
	c, err := create(ctx, t, client, ops...)
	assert.NilError(t, err)

	return c.ID
}

// CreateExpectingErr creates a container, expecting an error with the specified message
func CreateExpectingErr(ctx context.Context, t *testing.T, client client.APIClient, errMsg string, ops ...func(*TestContainerConfig)) {
	_, err := create(ctx, t, client, ops...)
	assert.ErrorContains(t, err, errMsg)
}

// Run creates and start a container with the specified options
func Run(ctx context.Context, t *testing.T, client client.APIClient, ops ...func(*TestContainerConfig)) string {
	t.Helper()
	id := Create(ctx, t, client, ops...)

	err := client.ContainerStart(ctx, id, types.ContainerStartOptions{})
	assert.NilError(t, err)

	return id
}
