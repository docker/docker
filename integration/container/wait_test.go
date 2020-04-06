package container // import "github.com/docker/docker/integration/container"

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/integration/internal/container"
	"github.com/docker/docker/testutil/request"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/poll"
	"gotest.tools/skip"
)

func TestWaitNonBlocked(t *testing.T) {
	defer setupTest(t)()
	cli := request.NewAPIClient(t)

	testCases := []struct {
		doc          string
		cmd          string
		expectedCode int64
	}{
		{
			doc:          "wait-nonblocking-exit-0",
			cmd:          "exit 0",
			expectedCode: 0,
		},
		{
			doc:          "wait-nonblocking-exit-random",
			cmd:          "exit 99",
			expectedCode: 99,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.doc, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			containerID := container.Run(ctx, t, cli, container.WithCmd("sh", "-c", tc.cmd))
			poll.WaitOn(t, container.IsInState(ctx, cli, containerID, "exited"), poll.WithTimeout(30*time.Second), poll.WithDelay(100*time.Millisecond))

			waitresC, errC := cli.ContainerWait(ctx, containerID, "")
			select {
			case err := <-errC:
				assert.NilError(t, err)
			case waitres := <-waitresC:
				assert.Check(t, is.Equal(tc.expectedCode, waitres.StatusCode))
			}
		})
	}
}

func TestWaitBlocked(t *testing.T) {
	// Windows busybox does not support trap in this way, not sleep with sub-second
	// granularity. It will always exit 0x40010004.
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	defer setupTest(t)()
	cli := request.NewAPIClient(t)

	testCases := []struct {
		doc          string
		cmd          string
		expectedCode int64
	}{
		{
			doc:          "test-wait-blocked-exit-zero",
			cmd:          "trap 'exit 0' TERM; while true; do usleep 10; done",
			expectedCode: 0,
		},
		{
			doc:          "test-wait-blocked-exit-random",
			cmd:          "trap 'exit 99' TERM; while true; do usleep 10; done",
			expectedCode: 99,
		},
	}
	for _, tc := range testCases {
		tc := tc
		t.Run(tc.doc, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			containerID := container.Run(ctx, t, cli, container.WithCmd("sh", "-c", tc.cmd))
			poll.WaitOn(t, container.IsInState(ctx, cli, containerID, "running"), poll.WithTimeout(30*time.Second), poll.WithDelay(100*time.Millisecond))

			waitresC, errC := cli.ContainerWait(ctx, containerID, "")

			err := cli.ContainerStop(ctx, containerID, nil)
			assert.NilError(t, err)

			select {
			case err := <-errC:
				assert.NilError(t, err)
			case waitres := <-waitresC:
				assert.Check(t, is.Equal(tc.expectedCode, waitres.StatusCode))
			case <-time.After(2 * time.Second):
				t.Fatal("timeout waiting for `docker wait`")
			}
		})
	}
}
