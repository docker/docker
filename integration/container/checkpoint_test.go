package container // import "github.com/docker/docker/integration/container"

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	mounttypes "github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/integration/internal/container"
	"github.com/docker/docker/internal/test/request"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/poll"
	"gotest.tools/skip"
)

func containerExec(t *testing.T, client client.APIClient, cID string, cmd []string) {
	t.Logf("Exec: %s", cmd)
	ctx := context.Background()
	r, err := container.Exec(ctx, client, cID, cmd)
	assert.NilError(t, err)
	t.Log(r.Combined())
	assert.Equal(t, r.ExitCode, 0)
}

func TestCheckpoint(t *testing.T) {
	t.Skip("TestCheckpoint is broken; see https://github.com/moby/moby/issues/38963")
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	skip.If(t, !testEnv.DaemonInfo.ExperimentalBuild)

	defer setupTest(t)()

	cmd := exec.Command("criu", "check")
	stdoutStderr, err := cmd.CombinedOutput()
	t.Logf("%s", stdoutStderr)
	assert.NilError(t, err)

	ctx := context.Background()
	client := request.NewAPIClient(t)

	mnt := mounttypes.Mount{
		Type:   mounttypes.TypeTmpfs,
		Target: "/tmp",
	}

	t.Log("Start a container")
	cID := container.Run(t, ctx, client, container.WithMount(mnt))
	poll.WaitOn(t,
		container.IsInState(ctx, client, cID, "running"),
		poll.WithDelay(100*time.Millisecond),
	)

	cptOpt := types.CheckpointCreateOptions{
		Exit:         false,
		CheckpointID: "test",
	}

	{
		// FIXME: ipv6 iptables modules are not uploaded in the test environment
		cmd := exec.Command("bash", "-c", "set -x; "+
			"mount --bind $(type -P true) $(type -P ip6tables-restore) && "+
			"mount --bind $(type -P true) $(type -P ip6tables-save)")
		stdoutStderr, err = cmd.CombinedOutput()
		t.Logf("%s", stdoutStderr)
		assert.NilError(t, err)

		defer func() {
			cmd := exec.Command("bash", "-c", "set -x; "+
				"umount -c -i -l $(type -P ip6tables-restore); "+
				"umount -c -i -l $(type -P ip6tables-save)")
			stdoutStderr, err = cmd.CombinedOutput()
			t.Logf("%s", stdoutStderr)
			assert.NilError(t, err)
		}()
	}
	t.Log("Do a checkpoint and leave the container running")
	err = client.CheckpointCreate(ctx, cID, cptOpt)
	if err != nil {
		// An error can contain a path to a dump file
		t.Logf("%s", err)
		re := regexp.MustCompile("path= (.*): ")
		m := re.FindStringSubmatch(fmt.Sprintf("%s", err))
		if len(m) >= 2 {
			dumpLog := m[1]
			t.Logf("%s", dumpLog)
			cmd := exec.Command("cat", dumpLog)
			stdoutStderr, err = cmd.CombinedOutput()
			t.Logf("%s", stdoutStderr)
		}
	}
	assert.NilError(t, err)

	inspect, err := client.ContainerInspect(ctx, cID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(true, inspect.State.Running))

	checkpoints, err := client.CheckpointList(ctx, cID, types.CheckpointListOptions{})
	assert.NilError(t, err)
	assert.Equal(t, len(checkpoints), 1)
	assert.Equal(t, checkpoints[0].Name, "test")

	// Create a test file on a tmpfs mount.
	containerExec(t, client, cID, []string{"touch", "/tmp/test-file"})

	// Do a second checkpoint
	cptOpt = types.CheckpointCreateOptions{
		Exit:         true,
		CheckpointID: "test2",
	}
	t.Log("Do a checkpoint and stop the container")
	err = client.CheckpointCreate(ctx, cID, cptOpt)
	assert.NilError(t, err)

	poll.WaitOn(t,
		container.IsInState(ctx, client, cID, "exited"),
		poll.WithDelay(100*time.Millisecond),
	)

	inspect, err = client.ContainerInspect(ctx, cID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(false, inspect.State.Running))

	// Check that both checkpoints are listed.
	checkpoints, err = client.CheckpointList(ctx, cID, types.CheckpointListOptions{})
	assert.NilError(t, err)
	assert.Equal(t, len(checkpoints), 2)
	cptNames := make([]string, 2)
	for i, c := range checkpoints {
		cptNames[i] = c.Name
	}
	sort.Strings(cptNames)
	assert.Equal(t, cptNames[0], "test")
	assert.Equal(t, cptNames[1], "test2")

	// Restore the container from a second checkpoint.
	startOpt := types.ContainerStartOptions{
		CheckpointID: "test2",
	}
	t.Log("Restore the container")
	err = client.ContainerStart(ctx, cID, startOpt)
	assert.NilError(t, err)

	inspect, err = client.ContainerInspect(ctx, cID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(true, inspect.State.Running))

	// Check that the test file has been restored.
	containerExec(t, client, cID, []string{"test", "-f", "/tmp/test-file"})

	for _, id := range []string{"test", "test2"} {
		cptDelOpt := types.CheckpointDeleteOptions{
			CheckpointID: id,
		}

		err = client.CheckpointDelete(ctx, cID, cptDelOpt)
		assert.NilError(t, err)
	}
}
