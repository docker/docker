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
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/skip"
)

// TestExecWithCloseStdin adds case for moby#37870 issue.
func TestExecWithCloseStdin(t *testing.T) {
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.39"), "broken in earlier versions")
	defer setupTest(t)()

	ctx := context.Background()
	client := testEnv.APIClient()

	// run top with detached mode
	cID := container.Run(t, ctx, client)

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
		})
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
	skip.If(t, testEnv.OSType == "windows", "FIXME. Probably needs to wait for container to be in running state.")
	defer setupTest(t)()
	ctx := context.Background()
	client := testEnv.APIClient()

	cID := container.Run(t, ctx, client, container.WithTty(true), container.WithWorkingDir("/root"))

	id, err := client.ContainerExecCreate(ctx, cID,
		types.ExecConfig{
			WorkingDir:   "/tmp",
			Env:          strslice.StrSlice([]string{"FOO=BAR"}),
			AttachStdout: true,
			Cmd:          strslice.StrSlice([]string{"sh", "-c", "env"}),
		},
	)
	assert.NilError(t, err)

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
	assert.Assert(t, is.Contains(out, "PWD=/tmp"), "exec command not running in expected /tmp working directory")
	assert.Assert(t, is.Contains(out, "FOO=BAR"), "exec command not running with expected environment variable FOO")
}

func TestExecUser(t *testing.T) {
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.39"), "broken in earlier versions")
	skip.If(t, testEnv.OSType == "windows", "FIXME. Probably needs to wait for container to be in running state.")
	defer setupTest(t)()
	ctx := context.Background()
	client := testEnv.APIClient()

	cID := container.Run(t, ctx, client, container.WithTty(true), container.WithUser("1:1"))

	result, err := container.Exec(ctx, client, cID, []string{"id"})
	assert.NilError(t, err)

	assert.Assert(t, is.Contains(result.Stdout(), "uid=1(daemon) gid=1(daemon)"), "exec command not running as uid/gid 1")
}
