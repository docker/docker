package container // import "github.com/docker/docker/integration/container"

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/integration/internal/container"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

// TestHealthCheckWorkdir verifies that health-checks inherit the containers'
// working-dir.
func TestHealthCheckWorkdir(t *testing.T) {
	skip.If(t, testEnv.OSType == "windows", "FIXME")
	defer setupTest(t)()
	ctx := context.Background()
	client := testEnv.APIClient()

	cID := container.Run(ctx, t, client, container.WithTty(true), container.WithWorkingDir("/foo"), func(c *container.TestContainerConfig) {
		c.Config.Healthcheck = &containertypes.HealthConfig{
			Test:     []string{"CMD-SHELL", "if [ \"$PWD\" = \"/foo\" ]; then exit 0; else exit 1; fi;"},
			Interval: 50 * time.Millisecond,
			Retries:  3,
		}
	})

	poll.WaitOn(t, pollForHealthStatus(ctx, client, cID, types.Healthy), poll.WithDelay(100*time.Millisecond))
}

// GitHub #37263
// Do not stop healthchecks just because we sent a signal to the container
func TestHealthKillContainer(t *testing.T) {
	skip.If(t, testEnv.OSType == "windows", "Windows only supports SIGKILL and SIGTERM? See https://github.com/moby/moby/issues/39574")
	defer setupTest(t)()

	ctx := context.Background()
	client := testEnv.APIClient()

	id := container.Run(ctx, t, client, func(c *container.TestContainerConfig) {
		c.Config.Healthcheck = &containertypes.HealthConfig{
			Test:     []string{"CMD-SHELL", "sleep 1"},
			Interval: time.Second,
			Retries:  5,
		}
	})

	ctxPoll, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	poll.WaitOn(t, pollForHealthStatus(ctxPoll, client, id, "healthy"), poll.WithDelay(100*time.Millisecond))

	err := client.ContainerKill(ctx, id, "SIGUSR1")
	assert.NilError(t, err)

	ctxPoll, cancel = context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	poll.WaitOn(t, pollForHealthStatus(ctxPoll, client, id, "healthy"), poll.WithDelay(100*time.Millisecond))
}

func TestHealthStartInterval(t *testing.T) {
	defer setupTest(t)()
	ctx := context.Background()
	client := testEnv.APIClient()

	id := container.Run(ctx, t, client, func(c *container.TestContainerConfig) {
		c.Config.Healthcheck = &containertypes.HealthConfig{
			Test:          []string{"CMD-SHELL", `count="$(cat /tmp/health)"; if [ -z "${count}" ]; then let count=0; fi; let count=${count}+1; echo -n ${count} | tee /tmp/health; if [ ${count} -lt 3 ]; then exit 1; fi`},
			Interval:      30 * time.Second,
			StartInterval: time.Second,
			StartPeriod:   time.Minute,
		}
	})

	ctxPoll, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	poll.WaitOn(t, func(log poll.LogT) poll.Result {
		if ctxPoll.Err() != nil {
			return poll.Error(ctxPoll.Err())
		}
		inspect, err := client.ContainerInspect(ctxPoll, id)
		if err != nil {
			return poll.Error(err)
		}
		if inspect.State.Health.Status != "healthy" {
			return poll.Continue("waiting on container to be ready")
		}
		return poll.Success()
	}, poll.WithDelay(100*time.Millisecond))
	cancel()

	ctxPoll, cancel = context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	dl, _ := ctxPoll.Deadline()

	poll.WaitOn(t, func(log poll.LogT) poll.Result {
		inspect, err := client.ContainerInspect(ctxPoll, id)
		if err != nil {
			return poll.Error(err)
		}

		hLen := len(inspect.State.Health.Log)
		if hLen < 2 {
			return poll.Continue("waiting for more healthcheck results")
		}

		h1 := inspect.State.Health.Log[hLen-1]
		h2 := inspect.State.Health.Log[hLen-2]
		if h1.Start.Sub(h2.Start) >= inspect.Config.Healthcheck.Interval {
			return poll.Success()
		}
		return poll.Continue("waiting for health check interval to switch from the start interval")
	}, poll.WithDelay(time.Second), poll.WithTimeout(dl.Sub(time.Now())))
}

func pollForHealthStatus(ctx context.Context, client client.APIClient, containerID string, healthStatus string) func(log poll.LogT) poll.Result {
	return func(log poll.LogT) poll.Result {
		inspect, err := client.ContainerInspect(ctx, containerID)

		switch {
		case err != nil:
			return poll.Error(err)
		case inspect.State.Health.Status == healthStatus:
			return poll.Success()
		default:
			return poll.Continue("waiting for container to become %s", healthStatus)
		}
	}
}
