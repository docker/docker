package container

import (
	"context"
	"fmt"
	"testing"
	"time"

	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/versions"
	"github.com/moby/moby/v2/integration/internal/container"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

// TestHealthCheckWorkdir verifies that health-checks inherit the containers'
// working-dir.
func TestHealthCheckWorkdir(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "FIXME")
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	cID := container.Run(ctx, t, apiClient, container.WithTty(true), container.WithWorkingDir("/foo"), func(c *container.TestContainerConfig) {
		c.Config.Healthcheck = &containertypes.HealthConfig{
			Test:     []string{"CMD-SHELL", "if [ \"$PWD\" = \"/foo\" ]; then exit 0; else exit 1; fi;"},
			Interval: 50 * time.Millisecond,
			Retries:  3,
		}
	})

	poll.WaitOn(t, pollForHealthStatus(ctx, apiClient, cID, containertypes.Healthy))
}

// GitHub #37263
// Do not stop healthchecks just because we sent a signal to the container
func TestHealthKillContainer(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "Windows only supports SIGKILL and SIGTERM? See https://github.com/moby/moby/issues/39574")
	ctx := setupTest(t)

	apiClient := testEnv.APIClient()

	id := container.Run(ctx, t, apiClient, func(c *container.TestContainerConfig) {
		cmd := `
# Set the initial HEALTH value so the healthcheck passes
HEALTH="1"
echo $HEALTH > /health

# Any time doHealth is run we flip the value
# This lets us use kill signals to determine when healtchecks have run.
doHealth() {
	case "$HEALTH" in
		"0")
			HEALTH="1"
			;;
		"1")
			HEALTH="0"
			;;
	esac
	echo $HEALTH > /health
}

trap 'doHealth' USR1

while true; do sleep 1; done
`
		c.Config.Cmd = []string{"/bin/sh", "-c", cmd}
		c.Config.Healthcheck = &containertypes.HealthConfig{
			Test:     []string{"CMD-SHELL", `[ "$(cat /health)" = "1" ]`},
			Interval: time.Second,
			Retries:  5,
		}
	})

	ctxPoll, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	poll.WaitOn(t, pollForHealthStatus(ctxPoll, apiClient, id, "healthy"))

	_, err := apiClient.ContainerKill(ctx, id, client.ContainerKillOptions{
		Signal: "SIGUSR1",
	})
	assert.NilError(t, err)

	ctxPoll, cancel = context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	poll.WaitOn(t, pollForHealthStatus(ctxPoll, apiClient, id, "unhealthy"))

	_, err = apiClient.ContainerKill(ctx, id, client.ContainerKillOptions{
		Signal: "SIGUSR1",
	})
	assert.NilError(t, err)

	ctxPoll, cancel = context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	poll.WaitOn(t, pollForHealthStatus(ctxPoll, apiClient, id, "healthy"))
}

// TestHealthCheckProcessKilled verifies that health-checks exec get killed on time-out.
func TestHealthCheckProcessKilled(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	// Note: Windows is much slower than Linux at getting the probe process to
	// the point where it has written to stdout. With a 50ms timeout the probe
	// is regularly killed before it has written anything, leaving the timeout
	// message with no output to report.
	probeTimeout := 50 * time.Millisecond
	if testEnv.DaemonInfo.OSType == "windows" {
		probeTimeout = 500 * time.Millisecond
	}

	cID := container.Run(ctx, t, apiClient, func(c *container.TestContainerConfig) {
		c.Config.Healthcheck = &containertypes.HealthConfig{
			Test:     []string{"CMD", "sh", "-c", `echo "logs1 logs2 logs3"; sleep 60`},
			Interval: 100 * time.Millisecond,
			Timeout:  probeTimeout,
			Retries:  1,
		}
	})
	poll.WaitOn(t, pollForHealthCheckLog(ctx, apiClient, cID, fmt.Sprintf("Health check exceeded timeout (%v): logs1 logs2 logs3\n", probeTimeout)))
}

func TestHealthStartInterval(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "The shell commands used in the test healthcheck do not work on Windows")

	ctx := setupTest(t)
	t.Parallel()

	apiClient := testEnv.APIClient()

	id := container.Run(ctx, t, apiClient, func(c *container.TestContainerConfig) {
		c.Config.Healthcheck = &containertypes.HealthConfig{
			Test:          []string{"CMD-SHELL", `count="$(cat /tmp/health)"; if [ -z "${count}" ]; then let count=0; fi; let count=${count}+1; echo -n ${count} | tee /tmp/health; if [ ${count} -lt 3 ]; then exit 1; fi`},
			Interval:      5 * time.Second,
			StartInterval: 200 * time.Millisecond,
			StartPeriod:   5 * time.Second,
		}
	})

	ctxPoll, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	dl, _ := ctxPoll.Deadline()

	poll.WaitOn(t, func(log poll.LogT) poll.Result {
		if ctxPoll.Err() != nil {
			return poll.Error(ctxPoll.Err())
		}
		inspect, err := apiClient.ContainerInspect(ctxPoll, id, client.ContainerInspectOptions{})
		if err != nil {
			return poll.Error(err)
		}
		if inspect.Container.State.Health.Status != containertypes.Healthy {
			var out string
			if len(inspect.Container.State.Health.Log) > 0 {
				out = inspect.Container.State.Health.Log[len(inspect.Container.State.Health.Log)-1].Output
			}
			return poll.Continue("waiting on container to be ready (%s): %s", inspect.Container.ID, out)
		}
		return poll.Success()
	}, poll.WithTimeout(time.Until(dl)))
	cancel()

	ctxPoll, cancel = context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dl, _ = ctxPoll.Deadline()

	poll.WaitOn(t, func(log poll.LogT) poll.Result {
		inspect, err := apiClient.ContainerInspect(ctxPoll, id, client.ContainerInspectOptions{})
		if err != nil {
			return poll.Error(err)
		}

		hLen := len(inspect.Container.State.Health.Log)
		if hLen < 2 {
			return poll.Continue("waiting for more healthcheck results")
		}

		h1 := inspect.Container.State.Health.Log[hLen-1]
		h2 := inspect.Container.State.Health.Log[hLen-2]
		if h1.Start.Sub(h2.Start) >= inspect.Container.Config.Healthcheck.Interval {
			return poll.Success()
		}
		return poll.Continue("waiting for health check interval to switch from the start interval: %s", h1.Start.Sub(h2.Start))
	}, poll.WithDelay(time.Second), poll.WithTimeout(time.Until(dl)))
}

func pollForHealthCheckLog(ctx context.Context, apiClient client.APIClient, containerID string, expected string) func(log poll.LogT) poll.Result {
	return func(log poll.LogT) poll.Result {
		inspect, err := apiClient.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
		if err != nil {
			return poll.Error(err)
		}
		healthChecksTotal := len(inspect.Container.State.Health.Log)
		if healthChecksTotal > 0 {
			output := inspect.Container.State.Health.Log[healthChecksTotal-1].Output
			if output == expected {
				return poll.Success()
			}
			return poll.Error(fmt.Errorf("expected %q, got %q", expected, output))
		}
		return poll.Continue("waiting for container healthcheck logs")
	}
}

func pollForHealthStatus(ctx context.Context, apiClient client.APIClient, containerID string, healthStatus containertypes.HealthStatus) func(log poll.LogT) poll.Result {
	return func(log poll.LogT) poll.Result {
		inspect, err := apiClient.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})

		switch {
		case err != nil:
			return poll.Error(err)
		case inspect.Container.State.Health.Status == healthStatus:
			return poll.Success()
		default:
			return poll.Continue("waiting for container to become %s", healthStatus)
		}
	}
}

func TestHealthCheckUmask(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "Windows does not support umask")
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.56"), "requires API v1.56")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	users := []string{"", "1000", "nobody"}
	prettyUser := func(s string) string {
		if s == "" {
			return "unspecified"
		}
		return s
	}

	tests := []struct {
		umask    uint32
		expected string
	}{
		{
			umask:    0o000,
			expected: "0000",
		},
		{
			umask:    0o777,
			expected: "0777",
		},
	}
	prettyUmask := func(u uint32) string {
		return fmt.Sprintf("%04o", u)
	}

	for _, user := range users {
		for _, tc := range tests {
			t.Run(fmt.Sprintf("user_%s_umask_%s", prettyUser(user), prettyUmask(tc.umask)), func(t *testing.T) {
				cID := container.Run(ctx, t, apiClient, container.WithUser(user), func(c *container.TestContainerConfig) {
					c.HostConfig.Umask = &tc.umask
					c.Config.Healthcheck = &containertypes.HealthConfig{
						Test:     []string{"CMD-SHELL", "umask"},
						Interval: time.Millisecond,
						Retries:  1,
					}
				})
				poll.WaitOn(t, pollForHealthCheckLog(ctx, apiClient, cID, tc.expected+"\n"), poll.WithTimeout(3*time.Second))
			})
		}
	}
}
