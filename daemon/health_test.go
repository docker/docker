package daemon

import (
	"errors"
	"testing"
	"time"

	containertypes "github.com/moby/moby/api/types/container"
	eventtypes "github.com/moby/moby/api/types/events"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/events"
	"gotest.tools/v3/assert"
)

func TestHealthcheckNone(t *testing.T) {
	c := &container.Container{
		ID:   "container_id",
		Name: "container_name",
		Config: &containertypes.Config{
			Image: "image_name",
			Healthcheck: &containertypes.HealthConfig{
				Test: []string{"NONE"},
			},
		},
		State: &container.State{},
	}
	store, err := container.NewViewDB()
	assert.NilError(t, err)
	daemon := &Daemon{containersReplica: store}
	daemon.initHealthMonitor(c)
	assert.Assert(t, c.State.Health == nil)
}

func TestHealthStates(t *testing.T) {
	tests := []struct {
		name        string
		retries     int
		startPeriod time.Duration
		test        func(t *testing.T, c *container.Container, handleResult func(time.Duration, int), expectEvent func(eventtypes.Action))
	}{
		{
			name:    "status transitions",
			retries: 1,
			test: func(t *testing.T, c *container.Container, handleResult func(time.Duration, int), expectEvent func(eventtypes.Action)) {
				handleResult(time.Second, 1)
				expectEvent(eventtypes.ActionHealthStatusUnhealthy)

				handleResult(2*time.Second, 0)
				expectEvent(eventtypes.ActionHealthStatusHealthy)

				handleResult(3*time.Second, 1)
				expectEvent(eventtypes.ActionHealthStatusUnhealthy)
			},
		},
		{
			name:    "retries",
			retries: 3,
			test: func(t *testing.T, c *container.Container, handleResult func(time.Duration, int), expectEvent func(eventtypes.Action)) {
				handleResult(20*time.Second, 1)
				handleResult(40*time.Second, 1)

				assert.Equal(t, c.State.Health.Status(), containertypes.Starting)
				assert.Equal(t, c.State.Health.FailingStreak, 2)

				handleResult(60*time.Second, 1)
				expectEvent(eventtypes.ActionHealthStatusUnhealthy)

				handleResult(80*time.Second, 0)
				expectEvent(eventtypes.ActionHealthStatusHealthy)

				assert.Equal(t, c.State.Health.FailingStreak, 0)
			},
		},
		{
			name:        "start period",
			retries:     2,
			startPeriod: 30 * time.Second,
			test: func(t *testing.T, c *container.Container, handleResult func(time.Duration, int), expectEvent func(eventtypes.Action)) {
				handleResult(20*time.Second, 1)

				assert.Equal(t, c.State.Health.Status(), containertypes.Starting)
				assert.Equal(t, c.State.Health.FailingStreak, 0)

				handleResult(50*time.Second, 1)

				assert.Equal(t, c.State.Health.Status(), containertypes.Starting)
				assert.Equal(t, c.State.Health.FailingStreak, 1)

				handleResult(80*time.Second, 0)
				expectEvent(eventtypes.ActionHealthStatusHealthy)

				assert.Equal(t, c.State.Health.FailingStreak, 0)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := events.New()
			_, listener, cancel := e.Subscribe()
			t.Cleanup(cancel)

			store, err := container.NewViewDB()
			assert.NilError(t, err)

			c := &container.Container{
				ID:   "container_id",
				Name: "container_name",
				Config: &containertypes.Config{
					Image: "image_name",
					Healthcheck: &containertypes.HealthConfig{
						Retries:     tc.retries,
						StartPeriod: tc.startPeriod,
					},
				},
				State: &container.State{
					Health: &container.Health{
						Health: containertypes.Health{
							Status: containertypes.Starting,
						},
					},
				},
			}

			daemon := &Daemon{
				EventsService:     e,
				containersReplica: store,
			}

			handleResult := func(sinceStart time.Duration, exitCode int) {
				start := c.State.StartedAt.Add(sinceStart)
				handleProbeResult(daemon, c, &containertypes.HealthcheckResult{
					Start:    start,
					End:      start,
					ExitCode: exitCode,
				}, nil)
			}

			// expectEvent verifies that the next emitted event has the expected action.
			expectEvent := func(expected eventtypes.Action) {
				t.Helper()

				select {
				case event := <-listener:
					ev, ok := event.(eventtypes.Message)
					assert.Assert(t, ok)
					assert.Equal(t, ev.Action, expected)
				default:
					t.Fatalf("expected event %v, got none", expected)
				}
			}
			tc.test(t, c, handleResult, expectEvent)
		})
	}
}

func TestHealthcheckEmptyCommand(t *testing.T) {
	c := &container.Container{
		ID: "container_id",
		Config: &containertypes.Config{
			Healthcheck: &containertypes.HealthConfig{
				Test: []string{"CMD"},
			},
		},
	}

	p := &cmdProbe{}
	_, err := p.run(t.Context(), nil, c)
	assert.ErrorContains(t, err, "has no command")
}

func TestWaitForExecStart(t *testing.T) {
	t.Run("exec already finished", func(t *testing.T) {
		// ContainerExecStart closes Started before returning, so a fast
		// probe leaves both ready and either case can win the select.
		// Both outcomes must leave the caller able to proceed: finished
		// true, or the result still waiting in execErr. Consuming the
		// result without reporting it deadlocks the probe: the next wait
		// blocks on a value that will never arrive again.
		for range 50 {
			started := make(chan struct{})
			close(started)
			execErr := make(chan error, 1)
			execErr <- nil

			finished, err := waitForExecStart(started, execErr, 2*time.Second)
			assert.NilError(t, err)
			if finished {
				continue
			}
			select {
			case err := <-execErr:
				assert.NilError(t, err)
			case <-time.After(2 * time.Second):
				t.Fatal("exec result consumed but not reported as finished")
			}
		}
	})

	t.Run("exec started", func(t *testing.T) {
		started := make(chan struct{})
		close(started)
		execErr := make(chan error, 1)

		finished, err := waitForExecStart(started, execErr, time.Second)
		assert.NilError(t, err)
		assert.Assert(t, !finished)
	})

	t.Run("exec failed to start", func(t *testing.T) {
		started := make(chan struct{})
		execErr := make(chan error, 1)
		execErr <- errors.New("exec failed")

		_, err := waitForExecStart(started, execErr, time.Second)
		assert.ErrorContains(t, err, "exec failed")
	})

	t.Run("start timed out", func(t *testing.T) {
		started := make(chan struct{})
		execErr := make(chan error, 1)

		_, err := waitForExecStart(started, execErr, 50*time.Millisecond)
		assert.Assert(t, errors.Is(err, errExecStartTimeout))
	})
}
