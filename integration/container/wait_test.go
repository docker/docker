package container // import "github.com/docker/docker/integration/container"

import (
	"testing"
	"time"

	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/integration/internal/container"
	"github.com/docker/docker/testutil"
	"github.com/docker/docker/testutil/request"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

func TestWaitNonBlocked(t *testing.T) {
	ctx := setupTest(t)

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

			ctx := testutil.StartSpan(ctx, t)
			containerID := container.Run(ctx, t, cli, container.WithCmd("sh", "-c", tc.cmd))
			poll.WaitOn(t, container.IsInState(ctx, cli, containerID, "exited"), poll.WithTimeout(30*time.Second), poll.WithDelay(100*time.Millisecond))

			waitResC, errC := cli.ContainerWait(ctx, containerID, "")
			select {
			case err := <-errC:
				assert.NilError(t, err)
			case waitRes := <-waitResC:
				assert.Check(t, is.Equal(tc.expectedCode, waitRes.StatusCode))
			}
		})
	}
}

func TestWaitBlocked(t *testing.T) {
	// Windows busybox does not support trap in this way, not sleep with sub-second
	// granularity. It will always exit 0x40010004.
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	ctx := setupTest(t)
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
			// TODO(vvoland): Verify why this helps for flakiness
			// t.Parallel()
			ctx := testutil.StartSpan(ctx, t)
			containerID := container.Run(ctx, t, cli, container.WithCmd("sh", "-c", tc.cmd))
			waitResC, errC := cli.ContainerWait(ctx, containerID, "")

			err := cli.ContainerStop(ctx, containerID, containertypes.StopOptions{})
			assert.NilError(t, err)

			select {
			case err := <-errC:
				assert.NilError(t, err)
			case waitRes := <-waitResC:
				assert.Check(t, is.Equal(tc.expectedCode, waitRes.StatusCode))
			case <-time.After(2 * time.Second):
				t.Fatal("timeout waiting for `docker wait`")
			}
		})
	}
}

func TestWaitConditions(t *testing.T) {
	ctx := setupTest(t)
	cli := request.NewAPIClient(t)

	testCases := []struct {
		doc      string
		waitCond containertypes.WaitCondition
		runOpts  []func(*container.TestContainerConfig)
	}{
		{
			doc: "default",
		},
		{
			doc:      "not-running",
			waitCond: containertypes.WaitConditionNotRunning,
		},
		{
			doc:      "next-exit",
			waitCond: containertypes.WaitConditionNextExit,
		},
		{
			doc:      "removed",
			waitCond: containertypes.WaitConditionRemoved,
			runOpts:  []func(*container.TestContainerConfig){container.WithAutoRemove},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.doc, func(t *testing.T) {
			// TODO(vvoland): Verify why this helps for flakiness
			// t.Parallel()
			ctx := testutil.StartSpan(ctx, t)
			opts := append([]func(*container.TestContainerConfig){
				container.WithCmd("sh", "-c", "read -r; exit 99"),
				func(tcc *container.TestContainerConfig) {
					tcc.Config.AttachStdin = true
					tcc.Config.OpenStdin = true
				},
			}, tc.runOpts...)
			containerID := container.Create(ctx, t, cli, opts...)
			t.Logf("ContainerID = %v", containerID)

			streams, err := cli.ContainerAttach(ctx, containerID, containertypes.AttachOptions{Stream: true, Stdin: true})
			assert.NilError(t, err)
			defer streams.Close()

			assert.NilError(t, cli.ContainerStart(ctx, containerID, containertypes.StartOptions{}))
			waitResC, errC := cli.ContainerWait(ctx, containerID, tc.waitCond)
			select {
			case err := <-errC:
				t.Fatalf("ContainerWait() err = %v", err)
			case res := <-waitResC:
				t.Fatalf("ContainerWait() sent exit code (%v) before ContainerStart()", res)
			default:
			}

			info, _ := cli.ContainerInspect(ctx, containerID)
			assert.Equal(t, "running", info.State.Status)

			_, err = streams.Conn.Write([]byte("\n"))
			assert.NilError(t, err)

			select {
			case err := <-errC:
				assert.NilError(t, err)
			case waitRes := <-waitResC:
				assert.Check(t, is.Equal(int64(99), waitRes.StatusCode))
			case <-time.After(StopContainerWindowsPollTimeout):
				info, _ := cli.ContainerInspect(ctx, containerID)
				t.Fatalf("Timed out waiting for container exit code (status = %q)", info.State.Status)
			}
		})
	}
}

func TestWaitRestartedContainer(t *testing.T) {
	ctx := setupTest(t)
	cli := request.NewAPIClient(t)

	testCases := []struct {
		doc      string
		waitCond containertypes.WaitCondition
	}{
		{
			doc: "default",
		},
		{
			doc:      "not-running",
			waitCond: containertypes.WaitConditionNotRunning,
		},
		{
			doc:      "next-exit",
			waitCond: containertypes.WaitConditionNextExit,
		},
	}

	// We can't catch the SIGTERM in the Windows based busybox image
	isWindowDaemon := testEnv.DaemonInfo.OSType == "windows"

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.doc, func(t *testing.T) {
			// TODO(vvoland): Verify why this helps for flakiness
			// t.Parallel()
			ctx := testutil.StartSpan(ctx, t)
			containerID := container.Run(ctx, t, cli,
				container.WithCmd("sh", "-c", "trap 'exit 5' SIGTERM; while true; do sleep 0.1; done"),
			)
			defer cli.ContainerRemove(ctx, containerID, containertypes.RemoveOptions{Force: true})

			// Container is running now, wait for exit
			waitResC, errC := cli.ContainerWait(ctx, containerID, tc.waitCond)

			timeout := 10
			// On Windows it will always timeout, because our process won't receive SIGTERM
			// Skip to force killing immediately
			if isWindowDaemon {
				timeout = 0
			}

			err := cli.ContainerRestart(ctx, containerID, containertypes.StopOptions{Timeout: &timeout, Signal: "SIGTERM"})
			assert.NilError(t, err)

			select {
			case err := <-errC:
				t.Fatalf("Unexpected error: %v", err)
			case <-time.After(time.Second * 3):
				t.Fatalf("Wait should end after restart")
			case waitRes := <-waitResC:
				expectedCode := int64(5)

				if !isWindowDaemon {
					assert.Check(t, is.Equal(expectedCode, waitRes.StatusCode))
				}
			}
		})
	}
}
