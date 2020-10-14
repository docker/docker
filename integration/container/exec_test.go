package container // import "github.com/docker/docker/integration/container"

import (
	"context"
	"io/ioutil"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/integration/internal/container"
	"github.com/docker/docker/daemon/exec"
	"github.com/docker/docker/pkg/signal"
	"github.com/docker/docker/testutil/daemon"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

// TestExecWithCloseStdin adds case for moby#37870 issue.
func TestExecWithCloseStdin(t *testing.T) {
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.39"), "broken in earlier versions")
	defer setupTest(t)()

	ctx := context.Background()
	client := testEnv.APIClient()

	// run top with detached mode
	cID := container.Run(ctx, t, client)

	expected := "closeIO"
	execResp, err := client.ContainerExecCreate(ctx, cID,
		types.ExecConfig{
			AttachStdin:  true,
			AttachStdout: true,
			Cmd:          strslice.StrSlice([]string{"sh", "-c", "cat && echo " + expected}),
		},
	)
	assert.NilError(t, err)

	resp, err := client.ContainerExecAttach(ctx, execResp.ID,
		types.ExecStartCheck{
			Detach: false,
			Tty:    false,
		},
	)
	assert.NilError(t, err)
	defer resp.Close()

	// close stdin to send EOF to cat
	assert.NilError(t, resp.CloseWrite())

	var (
		waitCh = make(chan struct{})
		resCh  = make(chan struct {
			content string
			err     error
		}, 1)
	)

	go func() {
		close(waitCh)
		defer close(resCh)
		r, err := ioutil.ReadAll(resp.Reader)

		resCh <- struct {
			content string
			err     error
		}{
			content: string(r),
			err:     err,
		}
	}()

	<-waitCh
	select {
	case <-time.After(3 * time.Second):
		t.Fatal("failed to read the content in time")
	case got := <-resCh:
		assert.NilError(t, got.err)

		// NOTE: using Contains because no-tty's stream contains UX information
		// like size, stream type.
		assert.Assert(t, is.Contains(got.content, expected))
	}
}

func TestExec(t *testing.T) {
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.35"), "broken in earlier versions")
	defer setupTest(t)()
	ctx := context.Background()
	client := testEnv.APIClient()

	cID := container.Run(ctx, t, client, container.WithTty(true), container.WithWorkingDir("/root"))

	id, err := client.ContainerExecCreate(ctx, cID,
		types.ExecConfig{
			WorkingDir:   "/tmp",
			Env:          strslice.StrSlice([]string{"FOO=BAR"}),
			AttachStdout: true,
			Cmd:          strslice.StrSlice([]string{"sh", "-c", "env"}),
		},
	)
	assert.NilError(t, err)

	inspect, err := client.ContainerExecInspect(ctx, id.ID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(inspect.ExecID, id.ID))

	resp, err := client.ContainerExecAttach(ctx, id.ID,
		types.ExecStartCheck{
			Detach: false,
			Tty:    false,
		},
	)
	assert.NilError(t, err)
	defer resp.Close()
	r, err := ioutil.ReadAll(resp.Reader)
	assert.NilError(t, err)
	out := string(r)
	assert.NilError(t, err)
	expected := "PWD=/tmp"
	if testEnv.OSType == "windows" {
		expected = "PWD=C:/tmp"
	}
	assert.Assert(t, is.Contains(out, expected), "exec command not running in expected /tmp working directory")
	assert.Assert(t, is.Contains(out, "FOO=BAR"), "exec command not running with expected environment variable FOO")
}

func TestExecUser(t *testing.T) {
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.39"), "broken in earlier versions")
	skip.If(t, testEnv.OSType == "windows", "FIXME. Probably needs to wait for container to be in running state.")
	defer setupTest(t)()
	ctx := context.Background()
	client := testEnv.APIClient()

	cID := container.Run(ctx, t, client, container.WithTty(true), container.WithUser("1:1"))

	result, err := container.Exec(ctx, client, cID, []string{"id"})
	assert.NilError(t, err)

	assert.Assert(t, is.Contains(result.Stdout(), "uid=1(daemon) gid=1(daemon)"), "exec command not running as uid/gid 1")
}

// Borrowed from daemon/util_test.go
type MockContainerdClient struct {
}
func (c *MockContainerdClient) CloseStdin(ctx context.Context, containerID, processID string) error {
	return nil
}


type mockContainerd struct {
	MockContainerdClient
	calledCtx         *context.Context
	calledContainerID *string
	calledID          *string
	calledSig         *int
}

func (cd *mockContainerd) SignalProcess(ctx context.Context, containerID, id string, sig int) error {
	cd.calledCtx = &ctx
	cd.calledContainerID = &containerID
	cd.calledID = &id
	cd.calledSig = &sig
	return nil
}

func TestContainerExecKillNoSuchExec(t *testing.T) {
	mock := mockContainerd{}
	ctx := context.Background()
	// d := &Daemon{
	// 	execCommands: exec.NewStore(),
	// 	containerd:   &mock,
	// }
	d := daemon.New(t)

	err := d.ContainerExecKill(ctx, "nil", uint64(signal.SignalMap["TERM"]))
	assert.ErrorContains(t, err, "No such exec instance")
	assert.Assert(t, is.Nil(mock.calledCtx))
	assert.Assert(t, is.Nil(mock.calledContainerID))
	assert.Assert(t, is.Nil(mock.calledID))
	assert.Assert(t, is.Nil(mock.calledSig))
}

func TestContainerExecKill(t *testing.T) {
	mock := mockContainerd{}
	ctx := context.Background()
	c := &container.Container{
		ExecCommands: exec.NewStore(),
		State:        &container.State{Running: true},
	}
	ec := &exec.Config{
		ID:          "exec",
		ContainerID: "container",
		Started:     make(chan struct{}),
	}
	// d := &Daemon{
	// 	execCommands: exec.NewStore(),
	// 	containers:   container.NewMemoryStore(),
	// 	containerd:   &mock,
	// }
	d := daemon.New(t)
	d.containers.Add("container", c)
	d.registerExecCommand(c, ec)

	err := d.ContainerExecKill(ctx, "exec", uint64(signal.SignalMap["TERM"]))
	assert.NilError(t, err)
	assert.Equal(t, *mock.calledCtx, ctx)
	assert.Equal(t, *mock.calledContainerID, "container")
	assert.Equal(t, *mock.calledID, "exec")
	assert.Equal(t, *mock.calledSig, int(signal.SignalMap["TERM"]))
}
