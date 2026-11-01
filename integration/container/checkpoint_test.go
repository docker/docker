package container

import (
	"cmp"
	"encoding/json"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/checkpoint"
	"github.com/moby/moby/api/types/common"

	containertypes "github.com/moby/moby/api/types/container"
	mounttypes "github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/internal/testutil/request"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

func TestCheckpointInvalidID(t *testing.T) {
	ctx := setupTest(t)
	query := url.Values{"checkpoint": {"../outside"}}
	res, body, err := request.Post(ctx, "/containers/missing/start?"+query.Encode())
	assert.NilError(t, err)
	defer body.Close()
	assert.Equal(t, res.StatusCode, http.StatusBadRequest)

	var errorResponse common.ErrorResponse
	assert.NilError(t, json.NewDecoder(body).Decode(&errorResponse))
	assert.Check(t, is.ErrorContains(errorResponse, `invalid checkpoint ID "../outside"`))
}

func TestCheckpoint(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	skip.If(t, !testEnv.DaemonInfo.ExperimentalBuild)

	ctx := setupTest(t)

	stdoutStderr, err := exec.Command("criu", "check").CombinedOutput()
	t.Logf("%s", stdoutStderr)
	assert.NilError(t, err)

	apiClient := request.NewAPIClient(t)

	t.Log("Start a container")
	cID := container.Run(ctx, t, apiClient, container.WithMount(mounttypes.Mount{
		Type:   mounttypes.TypeTmpfs,
		Target: "/tmp",
	}))

	// FIXME: ipv6 iptables modules are not uploaded in the test environment
	stdoutStderr, err = exec.Command("bash", "-c", "set -x; "+
		"mount --bind $(type -P true) $(type -P ip6tables-restore) && "+
		"mount --bind $(type -P true) $(type -P ip6tables-save)",
	).CombinedOutput()
	t.Logf("%s", stdoutStderr)
	assert.NilError(t, err)

	defer func() {
		stdoutStderr, err = exec.Command("bash", "-c", "set -x; "+
			"umount -c -i -l $(type -P ip6tables-restore); "+
			"umount -c -i -l $(type -P ip6tables-save)",
		).CombinedOutput()
		t.Logf("%s", stdoutStderr)
		assert.NilError(t, err)
	}()

	t.Log("Do a checkpoint and leave the container running")
	_, err = apiClient.CheckpointCreate(ctx, cID, client.CheckpointCreateOptions{
		Exit:         false,
		CheckpointID: "test",
	})
	if err != nil {
		// An error can contain a path to a dump file
		t.Log(err)
		re := regexp.MustCompile("path= (.*): ")
		m := re.FindStringSubmatch(err.Error())
		if len(m) >= 2 {
			dumpLog := m[1]
			t.Logf("%s", dumpLog)
			cmd := exec.Command("cat", dumpLog)
			stdoutStderr, err = cmd.CombinedOutput()
			t.Logf("%s", stdoutStderr)
		}
	}
	assert.NilError(t, err)

	inspect, err := apiClient.ContainerInspect(ctx, cID, client.ContainerInspectOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(true, inspect.Container.State.Running))

	res, err := apiClient.CheckpointList(ctx, cID, client.CheckpointListOptions{})
	assert.NilError(t, err)
	assert.Equal(t, len(res.Items), 1)
	assert.Equal(t, res.Items[0].Name, "test")

	// Create a test file on a tmpfs mount.
	cmd := []string{"touch", "/tmp/test-file"}
	r, err := container.Exec(ctx, apiClient, cID, cmd)
	assert.NilError(t, err, "failed to exec command:", cmd)
	r.AssertSuccess(t)

	// Do a second checkpoint
	t.Log("Do a checkpoint and stop the container")
	_, err = apiClient.CheckpointCreate(ctx, cID, client.CheckpointCreateOptions{
		Exit:         true,
		CheckpointID: "test2",
	})
	assert.NilError(t, err)

	poll.WaitOn(t, container.IsInState(ctx, apiClient, cID, containertypes.StateExited))

	inspect, err = apiClient.ContainerInspect(ctx, cID, client.ContainerInspectOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(false, inspect.Container.State.Running))

	// Check that both checkpoints are listed.
	res, err = apiClient.CheckpointList(ctx, cID, client.CheckpointListOptions{})
	assert.NilError(t, err)
	assert.Equal(t, len(res.Items), 2)
	slices.SortFunc(res.Items, func(a, b checkpoint.Summary) int {
		return cmp.Compare(a.Name, b.Name)
	})
	assert.Equal(t, res.Items[0].Name, "test")
	assert.Equal(t, res.Items[1].Name, "test2")

	// Restore the container from a second checkpoint.
	t.Log("Restore the container")
	_, err = apiClient.ContainerStart(ctx, cID, client.ContainerStartOptions{
		CheckpointID: "test2",
	})
	assert.NilError(t, err)

	inspect, err = apiClient.ContainerInspect(ctx, cID, client.ContainerInspectOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(true, inspect.Container.State.Running))

	// Check that the test file has been restored.
	cmd = []string{"test", "-f", "/tmp/test-file"}
	r, err = container.Exec(ctx, apiClient, cID, cmd)
	assert.NilError(t, err, "failed to exec command:", cmd)
	r.AssertSuccess(t)

	for _, id := range []string{"test", "test2"} {
		_, err = apiClient.CheckpointRemove(ctx, cID, client.CheckpointRemoveOptions{
			CheckpointID: id,
		})
		assert.NilError(t, err)
	}
}
