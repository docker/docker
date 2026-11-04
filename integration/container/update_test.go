package container

import (
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/container"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
)

func TestUpdateRestartPolicy(t *testing.T) {
	ctx := setupTest(t)
	t.Parallel()

	apiClient := testEnv.APIClient()

	cID := container.Run(ctx, t, apiClient, container.WithCmd("sh", "-c", "sleep 1 && false"), func(c *container.TestContainerConfig) {
		c.HostConfig.RestartPolicy = containertypes.RestartPolicy{
			Name:              "on-failure",
			MaximumRetryCount: 3,
		}
	})

	_, err := apiClient.ContainerUpdate(ctx, cID, client.ContainerUpdateOptions{
		RestartPolicy: &containertypes.RestartPolicy{
			Name:              "on-failure",
			MaximumRetryCount: 5,
		},
	})
	assert.NilError(t, err)

	timeout := 60 * time.Second
	if testEnv.DaemonInfo.OSType == "windows" {
		timeout = 180 * time.Second
	}

	poll.WaitOn(t, container.IsInState(ctx, apiClient, cID, containertypes.StateExited), poll.WithTimeout(timeout))

	inspect, err := apiClient.ContainerInspect(ctx, cID, client.ContainerInspectOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(inspect.Container.RestartCount, 5))
	assert.Check(t, is.Equal(inspect.Container.HostConfig.RestartPolicy.MaximumRetryCount, 5))
}

func TestUpdateRestartWithAutoRemove(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	cID := container.Run(ctx, t, apiClient, container.WithAutoRemove)

	_, err := apiClient.ContainerUpdate(ctx, cID, client.ContainerUpdateOptions{
		RestartPolicy: &containertypes.RestartPolicy{
			Name: "always",
		},
	})
	assert.Check(t, is.ErrorContains(err, "Restart policy cannot be updated because AutoRemove is enabled for the container"))
	assert.Check(t, is.ErrorType(err, cerrdefs.IsConflict))
}
