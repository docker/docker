package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration-cli/checker"
	"github.com/moby/moby/v2/integration-cli/cli"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/request"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
)

// Regression test for #9414
func (s *DockerAPISuite) TestExecAPICreateNoCmd(c *testing.T) {
	name := "exec_test"
	cli.DockerCmd(c, "run", "-d", "-t", "--name", name, "busybox", "/bin/sh")

	res, body, err := request.Post(testutil.GetContext(c), fmt.Sprintf("/containers/%s/exec", name), request.JSONBody(map[string]any{"Cmd": nil}))
	assert.NilError(c, err)
	assert.Equal(c, res.StatusCode, http.StatusBadRequest)
	b, err := request.ReadBody(body)
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(getErrorMessage(c, b), "No exec command specified"), "Expected message when creating exec command with no Cmd specified")
}

func (s *DockerAPISuite) TestExecAPICreateNoValidContentType(c *testing.T) {
	name := "exec_test"
	cli.DockerCmd(c, "run", "-d", "-t", "--name", name, "busybox", "/bin/sh")

	jsonData := bytes.NewBuffer(nil)
	if err := json.NewEncoder(jsonData).Encode(map[string]any{"Cmd": nil}); err != nil {
		c.Fatalf("Can not encode data to json %s", err)
	}

	res, body, err := request.Post(testutil.GetContext(c), fmt.Sprintf("/containers/%s/exec", name), request.RawContent(io.NopCloser(jsonData)), request.ContentType("test/plain"))
	assert.NilError(c, err)
	assert.Equal(c, res.StatusCode, http.StatusBadRequest)
	b, err := request.ReadBody(body)
	assert.NilError(c, err)
	assert.Assert(c, is.Contains(getErrorMessage(c, b), "unsupported Content-Type header (test/plain): must be 'application/json'"))
}

func (s *DockerAPISuite) TestExecAPICreateContainerPaused(c *testing.T) {
	// Not relevant on Windows as Windows containers cannot be paused
	testRequires(c, DaemonIsLinux)
	name := "exec_create_test"
	cli.DockerCmd(c, "run", "-d", "-t", "--name", name, "busybox", "/bin/sh")

	cli.DockerCmd(c, "pause", name)

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	_, err = apiClient.ExecCreate(testutil.GetContext(c), name, client.ExecCreateOptions{
		Cmd: []string{"true"},
	})
	assert.ErrorContains(c, err, "Container "+name+" is paused, unpause the container before exec", "Expected message when creating exec command with Container %s is paused", name)
}

func (s *DockerAPISuite) TestExecAPIStart(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Uses pause/unpause but bits may be salvageable to Windows to Windows CI
	cli.DockerCmd(c, "run", "-d", "--name", "test", "busybox", "top")

	id := createExec(c, "test")
	startExec(c, id, http.StatusOK)

	var execJSON struct{ PID int }
	inspectExec(testutil.GetContext(c), c, id, &execJSON)
	assert.Assert(c, execJSON.PID > 1)

	id = createExec(c, "test")
	cli.DockerCmd(c, "stop", "test")

	startExec(c, id, http.StatusNotFound)

	cli.DockerCmd(c, "start", "test")
	startExec(c, id, http.StatusNotFound)

	// make sure exec is created before pausing
	id = createExec(c, "test")
	cli.DockerCmd(c, "pause", "test")
	startExec(c, id, http.StatusConflict)
	cli.DockerCmd(c, "unpause", "test")
	startExec(c, id, http.StatusOK)
}

func (s *DockerAPISuite) TestExecAPIStartEnsureHeaders(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	cli.DockerCmd(c, "run", "-d", "--name", "test", "busybox", "top")

	id := createExec(c, "test")
	resp, _, err := request.Post(testutil.GetContext(c), fmt.Sprintf("/exec/%s/start", id), request.RawString(`{"Detach": true}`), request.JSON)
	assert.NilError(c, err)
	assert.Assert(c, resp.Header.Get("Server") != "")
}

// #19362
func (s *DockerAPISuite) TestExecAPIStartMultipleTimesError(c *testing.T) {
	runSleepingContainer(c, "-d", "--name", "test")
	execID := createExec(c, "test")
	startExec(c, execID, http.StatusOK)
	waitForExec(testutil.GetContext(c), c, execID)

	startExec(c, execID, http.StatusConflict)
}

// #20638
func (s *DockerAPISuite) TestExecAPIStartWithDetach(c *testing.T) {
	name := "foo"
	runSleepingContainer(c, "-d", "-t", "--name", name)

	ctx := testutil.GetContext(c)

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	res, err := apiClient.ExecCreate(ctx, name, client.ExecCreateOptions{
		Cmd:          []string{"true"},
		AttachStderr: true,
	})
	assert.NilError(c, err)

	_, body, err := request.Post(ctx, fmt.Sprintf("/exec/%s/start", res.ID), request.RawString(`{"Detach": true}`), request.JSON)
	assert.NilError(c, err)

	b, err := request.ReadBody(body)
	comment := fmt.Sprintf("response body: %s", b)
	assert.NilError(c, err, comment)

	resp, _, err := request.Get(ctx, "/_ping")
	assert.NilError(c, err)
	if resp.StatusCode != http.StatusOK {
		c.Fatal("daemon is down, it should alive")
	}
}

// #30311
func (s *DockerAPISuite) TestExecAPIStartValidCommand(c *testing.T) {
	name := "exec_test"
	cli.DockerCmd(c, "run", "-d", "-t", "--name", name, "busybox", "/bin/sh")

	id := createExecCmd(c, name, "true")
	startExec(c, id, http.StatusOK)

	ctx := testutil.GetContext(c)
	waitForExec(ctx, c, id)

	var inspectJSON struct{ ExecIDs []string }
	inspectContainer(ctx, c, name, &inspectJSON)

	assert.Assert(c, is.Nil(inspectJSON.ExecIDs))
}

// #30311
func (s *DockerAPISuite) TestExecAPIStartInvalidCommand(c *testing.T) {
	name := "exec_test"
	cli.DockerCmd(c, "run", "-d", "-t", "--name", name, "busybox", "/bin/sh")

	id := createExecCmd(c, name, "invalid")
	startExec(c, id, http.StatusBadRequest)
	ctx := testutil.GetContext(c)
	waitForExec(ctx, c, id)

	var inspectJSON struct{ ExecIDs []string }
	inspectContainer(ctx, c, name, &inspectJSON)

	assert.Assert(c, is.Nil(inspectJSON.ExecIDs))
}

func (s *DockerAPISuite) TestExecStateCleanup(c *testing.T) {
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon)

	// This test checks accidental regressions. Not part of stable API.

	name := "exec_cleanup"
	cid := cli.DockerCmd(c, "run", "-d", "-t", "--name", name, "busybox", "/bin/sh").Stdout()
	cid = strings.TrimSpace(cid)

	// Per-run runtime IDs preserve the first half of the Docker container ID.
	stateDirs, err := filepath.Glob("/var/run/docker/containerd/" + cid[:32] + "*")
	assert.NilError(c, err)
	assert.Assert(c, is.Len(stateDirs, 1))
	stateDir := stateDirs[0]

	checkReadDir := func(t *testing.T) (any, string) {
		fi, err := os.ReadDir(stateDir)
		assert.NilError(t, err)
		return len(fi), ""
	}

	fi, err := os.ReadDir(stateDir)
	assert.NilError(c, err)
	assert.Assert(c, len(fi) > 1)

	id := createExecCmd(c, name, "ls")
	startExec(c, id, http.StatusOK)

	ctx := testutil.GetContext(c)
	waitForExec(ctx, c, id)

	poll.WaitOn(c, pollCheck(c, checkReadDir, checker.Equals(len(fi))), poll.WithTimeout(5*time.Second))

	id = createExecCmd(c, name, "invalid")
	startExec(c, id, http.StatusBadRequest)
	waitForExec(ctx, c, id)

	poll.WaitOn(c, pollCheck(c, checkReadDir, checker.Equals(len(fi))), poll.WithTimeout(5*time.Second))

	cli.DockerCmd(c, "stop", name)
	_, err = os.Stat(stateDir)
	assert.ErrorContains(c, err, "")
	assert.Assert(c, os.IsNotExist(err))
}

func createExec(t *testing.T, name string) string {
	return createExecCmd(t, name, "true")
}

func createExecCmd(t *testing.T, name string, cmd string) string {
	_, reader, err := request.Post(testutil.GetContext(t), fmt.Sprintf("/containers/%s/exec", name), request.JSONBody(map[string]any{"Cmd": []string{cmd}}))
	assert.NilError(t, err)
	b, err := io.ReadAll(reader)
	assert.NilError(t, err)
	defer reader.Close()
	createResp := struct {
		ID string `json:"Id"`
	}{}
	assert.NilError(t, json.Unmarshal(b, &createResp), string(b))
	return createResp.ID
}

func startExec(t *testing.T, id string, code int) {
	resp, body, err := request.Post(testutil.GetContext(t), fmt.Sprintf("/exec/%s/start", id), request.RawString(`{"Detach": true}`), request.JSON)
	assert.NilError(t, err)

	b, err := request.ReadBody(body)
	assert.NilError(t, err, "response body: %s", b)
	assert.Equal(t, resp.StatusCode, code, "response body: %s", b)
}

func inspectExec(ctx context.Context, t *testing.T, id string, out any) {
	resp, body, err := request.Get(ctx, fmt.Sprintf("/exec/%s/json", id))
	assert.NilError(t, err)
	defer body.Close()
	assert.Equal(t, resp.StatusCode, http.StatusOK)
	err = json.NewDecoder(body).Decode(out)
	assert.NilError(t, err)
}

func waitForExec(ctx context.Context, t *testing.T, id string) {
	timeout := time.After(60 * time.Second)
	var execJSON struct{ Running bool }
	for {
		select {
		case <-timeout:
			t.Fatal("timeout waiting for exec to start")
		default:
		}

		inspectExec(ctx, t, id, &execJSON)
		if !execJSON.Running {
			break
		}
	}
}

func inspectContainer(ctx context.Context, t *testing.T, id string, out any) {
	resp, body, err := request.Get(ctx, "/containers/"+id+"/json")
	assert.NilError(t, err)
	defer body.Close()
	assert.Equal(t, resp.StatusCode, http.StatusOK)
	err = json.NewDecoder(body).Decode(out)
	assert.NilError(t, err)
}
