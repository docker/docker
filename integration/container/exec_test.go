package container

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/common"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/build"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/internal/testutil/fakecontext"
	req "github.com/moby/moby/v2/internal/testutil/request"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

// TestExecWithCloseStdin adds case for moby#37870 issue.
func TestExecWithCloseStdin(t *testing.T) {
	skip.If(t, testEnv.RuntimeIsWindowsContainerd(), "FIXME. Hang on Windows + containerd combination")
	ctx := setupTest(t)

	apiClient := testEnv.APIClient()

	// run top with detached mode
	cID := container.Run(ctx, t, apiClient)

	const expected = "closeIO"
	res, err := apiClient.ExecCreate(ctx, cID, client.ExecCreateOptions{
		AttachStdin:  true,
		AttachStdout: true,
		Cmd:          []string{"sh", "-c", "cat && echo " + expected},
	})
	assert.NilError(t, err)

	resp, err := apiClient.ExecAttach(ctx, res.ID, client.ExecAttachOptions{})
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
		r, err := io.ReadAll(resp.Reader)

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
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	cID := container.Run(ctx, t, apiClient, container.WithTty(true), container.WithWorkingDir("/root"))

	res, err := apiClient.ExecCreate(ctx, cID, client.ExecCreateOptions{
		WorkingDir:   "/tmp",
		Env:          []string{"FOO=BAR"},
		AttachStdout: true,
		Cmd:          []string{"sh", "-c", "env"},
	})
	assert.NilError(t, err)

	inspect, err := apiClient.ExecInspect(ctx, res.ID, client.ExecInspectOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(inspect.ID, res.ID))

	resp, err := apiClient.ExecAttach(ctx, res.ID, client.ExecAttachOptions{})
	assert.NilError(t, err)
	defer resp.Close()
	r, err := io.ReadAll(resp.Reader)
	assert.NilError(t, err)
	out := string(r)
	assert.NilError(t, err)
	expected := "PWD=/tmp"
	if testEnv.DaemonInfo.OSType == "windows" {
		expected = "PWD=C:/tmp"
	}
	assert.Check(t, is.Contains(out, expected), "exec command not running in expected /tmp working directory")
	assert.Check(t, is.Contains(out, "FOO=BAR"), "exec command not running with expected environment variable FOO")
}

func TestExecResizeStress(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "windows")

	for i := range 100 {
		t.Run(strconv.Itoa(i), TestExecResize)
	}
}

func TestExecResize(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	cID := container.Run(ctx, t, apiClient, container.WithTty(true))
	defer container.Remove(ctx, t, apiClient, cID, client.ContainerRemoveOptions{Force: true})

	cmd := []string{"top"}
	if runtime.GOOS == "windows" {
		cmd = []string{"sleep", "240"}
	}
	res, err := apiClient.ExecCreate(ctx, cID, client.ExecCreateOptions{
		TTY: true, // Windows requires a TTY for the resize to work, otherwise fails with "is not a tty: failed precondition", see https://github.com/moby/moby/pull/48665#issuecomment-2412530345
		Cmd: cmd,
	})
	assert.NilError(t, err)
	execID := res.ID
	assert.NilError(t, err)
	_, err = apiClient.ExecStart(ctx, execID, client.ExecStartOptions{
		Detach: true,
	})
	assert.NilError(t, err)

	if runtime.GOOS == "windows" {
		// Try to fix flakiness on Windows 2025, which often fails:
		//
		//	=== FAIL: github.com/docker/docker/integration/container TestExecResize/success (0.01s)
		//		exec_test.go:144: assertion failed: error is not nil: Error response from daemon: NotFound: exec: '9c19c467436132df24d8b606b0c462b1110dacfbbd13b63e5b42579eda76d7fc' in task: '7d1f371218285a0c653ae77024a1ab3f5d61a5d097c651ddf7df97364fafb454' not found: not found
		poll.WaitOn(t, func(poll.LogT) poll.Result {
			i, err := apiClient.ContainerExecInspect(ctx, execID)
			if cerrdefs.IsNotFound(err) {
				return poll.Continue("waiting for exec %s to exist", execID)
			}
			if !i.Running {
				return poll.Continue("waiting for exec %s to be running", execID)
			}
			return poll.Success()
		})
	}

	t.Run("success", func(t *testing.T) {
		_, err := apiClient.ExecResize(ctx, execID, client.ExecResizeOptions{
			Height: 40,
			Width:  40,
		})
		if runtime.GOOS == "windows" && err != nil {
			// FIXME(thaJeztah): temporarily allowing test to fail on Windows: see https://github.com/moby/moby/issues/50402
			t.Log("XFAIL:", err)
			t.Skip("XFAIL: flaky test on Windows: see https://github.com/moby/moby/issues/50402")
			return
		}
		assert.NilError(t, err)

		// TODO(thaJeztah): also check if the resize happened
		//
		// Note: container inspect shows the initial size that was
		// set when creating the container. Actual resize happens in
		// containerd, and currently does not update the container's
		// config after running (but does send a "resize" event).
	})

	t.Run("invalid size", func(t *testing.T) {
		const valueNotSet = "unset"

		sizes := []struct {
			doc, height, width, expErr string
		}{
			{
				doc:    "unset height",
				height: valueNotSet,
				width:  "100",
				expErr: `invalid resize height "": invalid syntax`,
			},
			{
				doc:    "unset width",
				height: "100",
				width:  valueNotSet,
				expErr: `invalid resize width "": invalid syntax`,
			},
			{
				doc:    "empty height",
				width:  "100",
				expErr: `invalid resize height "": invalid syntax`,
			},
			{
				doc:    "empty width",
				height: "100",
				expErr: `invalid resize width "": invalid syntax`,
			},
			{
				doc:    "non-numeric height",
				height: "not-a-number",
				width:  "100",
				expErr: `invalid resize height "not-a-number": invalid syntax`,
			},
			{
				doc:    "non-numeric width",
				height: "100",
				width:  "not-a-number",
				expErr: `invalid resize width "not-a-number": invalid syntax`,
			},
			{
				doc:    "negative height",
				height: "-100",
				width:  "100",
				expErr: `invalid resize height "-100": value out of range`,
			},
			{
				doc:    "negative width",
				height: "100",
				width:  "-100",
				expErr: `invalid resize width "-100": value out of range`,
			},
			{
				doc:    "out of range height",
				height: "4294967296", // math.MaxUint32+1
				width:  "100",
				expErr: `invalid resize height "4294967296": value out of range`,
			},
			{
				doc:    "out of range width",
				height: "100",
				width:  "4294967296", // math.MaxUint32+1
				expErr: `invalid resize width "4294967296": value out of range`,
			},
		}
		for _, tc := range sizes {
			t.Run(tc.doc, func(t *testing.T) {
				// Manually creating a request here, as the APIClient would invalidate
				// these values before they're sent.
				vals := url.Values{}
				if tc.height != valueNotSet {
					vals.Add("h", tc.height)
				}
				if tc.width != valueNotSet {
					vals.Add("w", tc.width)
				}
				res, _, err := req.Post(ctx, "/exec/"+execID+"/resize?"+vals.Encode())
				assert.NilError(t, err)
				assert.Check(t, is.Equal(http.StatusBadRequest, res.StatusCode))

				var errorResponse common.ErrorResponse
				err = json.NewDecoder(res.Body).Decode(&errorResponse)
				assert.NilError(t, err)
				assert.Check(t, is.ErrorContains(errorResponse, tc.expErr))
			})
		}
	})

	t.Run("unknown execID", func(t *testing.T) {
		_, err = apiClient.ExecResize(ctx, "no-such-exec-id", client.ExecResizeOptions{
			Height: 40,
			Width:  40,
		})
		assert.Check(t, is.ErrorType(err, cerrdefs.IsNotFound))
		assert.Check(t, is.ErrorContains(err, "No such exec instance: no-such-exec-id"))
	})

	t.Run("invalid state", func(t *testing.T) {
		// FIXME(thaJeztah): Windows + builtin returns a NotFound instead of a Conflict error
		//
		// When using the builtin runtime, stopping the container causes
		// the exec-resize to return a "NotFound" error, whereas with containerd
		// as runtime, it returns the expected "Conflict" error. This could be
		// either a limitation of the "builtin" runtime, or there's a bug to
		// be fixed.
		//
		// See https://github.com/moby/moby/pull/48665#issuecomment-2412579701
		//
		//  === RUN   TestExecResize/invalid_state
		//      exec_test.go:234: assertion failed: error is Error response from daemon: No such exec instance: cc728a332d3f594249fb7ee9adb3bb12a59a5d1776f8f6dedc56355364361711 (errdefs.errNotFound), not errdefs.IsConflict
		//      exec_test.go:235: assertion failed: expected error to contain "is not running", got "Error response from daemon: No such exec instance: cc728a332d3f594249fb7ee9adb3bb12a59a5d1776f8f6dedc56355364361711"
		//          Error response from daemon: No such exec instance: cc728a332d3f594249fb7ee9adb3bb12a59a5d1776f8f6dedc56355364361711
		skip.If(t, testEnv.DaemonInfo.OSType == "windows" && !testEnv.RuntimeIsWindowsContainerd(), "FIXME. Windows + builtin returns a NotFound instead of a Conflict error")

		_, err := apiClient.ContainerKill(ctx, cID, client.ContainerKillOptions{})
		assert.NilError(t, err)

		_, err = apiClient.ExecResize(ctx, execID, client.ExecResizeOptions{
			Height: 40,
			Width:  40,
		})
		assert.Check(t, is.ErrorType(err, cerrdefs.IsConflict))
		assert.Check(t, is.ErrorContains(err, "is not running"))
	})
}

func TestExecUser(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "FIXME. Probably needs to wait for container to be in running state.")
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	ctrOpts := []func(*container.TestContainerConfig){
		container.WithTty(true),
		container.WithUser("1:1"),
	}
	withoutEtcGroups := container.WithImage(build.Do(ctx, t, apiClient, fakecontext.New(t, "", fakecontext.WithDockerfile("FROM busybox\nRUN rm /etc/group")), client.ImageBuildOptions{}))
	withoutEtcPasswd := container.WithImage(build.Do(ctx, t, apiClient, fakecontext.New(t, "", fakecontext.WithDockerfile("FROM busybox\nRUN rm /etc/passwd")), client.ImageBuildOptions{}))

	withUser := func(user string) func(options *client.ExecCreateOptions) {
		return func(options *client.ExecCreateOptions) { options.User = user }
	}

	tests := []struct {
		doc         string
		user        string
		ctrOpts     []func(*container.TestContainerConfig)
		expectedErr string
		expectedOut string
	}{
		{
			doc:         "default user",
			expectedOut: "uid=1(daemon) gid=1(daemon)",
		},
		{
			doc:         "uid",
			user:        "0",
			expectedOut: "uid=0(root) gid=0(root) groups=0(root)",
		},
		{
			doc:         "uid gid",
			user:        "0:0",
			expectedOut: "uid=0(root) gid=0(root) groups=0(root)",
		},
		{
			doc:         "username groupname",
			user:        "root:root",
			expectedOut: "uid=0(root) gid=0(root) groups=0(root)",
		},
		{
			doc:         "unknown user",
			user:        "no-such-user",
			expectedErr: `Error response from daemon: unable to find user no-such-user: no matching entries in passwd file`,
		},
		{
			doc:         "unknown user with gid",
			user:        "no-such-user:1",
			expectedErr: `Error response from daemon: unable to find user no-such-user: no matching entries in passwd file`,
		},
		{
			doc:         "unknown group",
			user:        "1:no-such-group",
			expectedErr: `Error response from daemon: unable to find group no-such-group: no matching entries in group file`,
		},
		{
			doc:     "missing etc/group",
			ctrOpts: []func(*container.TestContainerConfig){withoutEtcGroups},
		},
		{
			doc:     "uid:gid and missing etc/group",
			user:    "0:0",
			ctrOpts: []func(*container.TestContainerConfig){withoutEtcGroups},
		},
		{
			doc:     "user and missing etc/group",
			user:    "root",
			ctrOpts: []func(*container.TestContainerConfig){withoutEtcGroups},
		},
		{
			doc:         "user:gid and missing etc/group",
			user:        "root;0",
			ctrOpts:     []func(*container.TestContainerConfig){withoutEtcGroups},
			expectedErr: `Error response from daemon: unable to find user root;0: no matching entries in passwd file`,
		},
		{
			doc:         "group and missing etc/group",
			user:        "0:root",
			ctrOpts:     []func(*container.TestContainerConfig){withoutEtcGroups},
			expectedErr: `Error response from daemon: unable to find group root: no matching entries in group file`,
		},
		{
			doc:     "missing etc/passwd",
			ctrOpts: []func(*container.TestContainerConfig){withoutEtcPasswd},
		},
		{
			doc:     "uid:gid and missing etc/passwd",
			user:    "0:0",
			ctrOpts: []func(*container.TestContainerConfig){withoutEtcPasswd},
		},
		{
			doc:         "user and missing etc/passwd",
			user:        "root",
			ctrOpts:     []func(*container.TestContainerConfig){withoutEtcPasswd},
			expectedErr: `Error response from daemon: unable to find user root: no matching entries in passwd file`,
		},
		{
			doc:         "user:gid and missing etc/passwd",
			user:        "root;0",
			ctrOpts:     []func(*container.TestContainerConfig){withoutEtcPasswd},
			expectedErr: `Error response from daemon: unable to find user root;0: no matching entries in passwd file`,
		},
		{
			doc:     "group and missing etc/passwd",
			user:    "0:root",
			ctrOpts: []func(*container.TestContainerConfig){withoutEtcPasswd},
		},
	}

	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			cID := container.Run(ctx, t, apiClient, append(ctrOpts, tc.ctrOpts...)...)
			result, err := container.Exec(ctx, apiClient, cID, []string{"id"}, withUser(tc.user))
			if tc.expectedErr != "" {
				assert.Check(t, is.Error(err, tc.expectedErr))
				assert.Check(t, is.ErrorType(err, cerrdefs.IsInvalidArgument))
				assert.Check(t, is.Equal(result.Stdout(), "<nil>"))
				assert.Check(t, is.Equal(result.Stderr(), "<nil>"))
			} else {
				assert.Check(t, err)
				assert.Check(t, is.Contains(result.Stdout(), tc.expectedOut))
				assert.Check(t, is.Equal(result.Stderr(), ""))
			}
		})
	}
}

// Test that additional groups set with `--group-add` are kept on exec when the container
// also has a user set.
// (regression test for https://github.com/moby/moby/issues/46712)
func TestExecWithGroupAdd(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "FIXME. Probably needs to wait for container to be in running state.")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	cID := container.Run(ctx, t, apiClient, container.WithTty(true), container.WithUser("root:root"), container.WithAdditionalGroups("staff", "wheel", "audio", "777"), container.WithCmd("sleep", "5"))

	result, err := container.Exec(ctx, apiClient, cID, []string{"id"})
	assert.NilError(t, err)

	const expected = "uid=0(root) gid=0(root) groups=0(root),10(wheel),29(audio),50(staff),777"
	assert.Check(t, is.Equal(strings.TrimSpace(result.Stdout()), expected), "exec command not keeping additional groups w/ user")
}
