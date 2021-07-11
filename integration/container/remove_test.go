package container // import "github.com/docker/docker/integration/container"

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/integration/internal/container"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/fs"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

// dPath converts linux absolute paths to Windows absolute paths if the daemon
// is running on Windows
func dPath(path string) string {
	if testEnv.OSType == "windows" {
		return `c:` + strings.ReplaceAll(path, "/", `\`)
	}
	return path
}

// Test case for #5244: `docker rm` fails if bind dir doesn't exist anymore
func TestRemoveContainerWithRemovedVolume(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)

	defer setupTest(t)()
	ctx := context.Background()
	client := testEnv.APIClient()

	tempDir := fs.NewDir(t, "test-rm-container-with-removed-volume", fs.WithMode(0755))
	defer tempDir.Remove()

	cID := container.Run(ctx, t, client, container.WithCmd("true"), container.WithBind(tempDir.Path(), dPath("/test")))
	poll.WaitOn(t, container.IsInState(ctx, client, cID, "exited"), poll.WithDelay(100*time.Millisecond))

	err := os.RemoveAll(tempDir.Path())
	assert.NilError(t, err)

	err = client.ContainerRemove(ctx, cID, types.ContainerRemoveOptions{
		RemoveVolumes: true,
	})
	assert.NilError(t, err)

	_, _, err = client.ContainerInspectWithRaw(ctx, cID, true)
	assert.Check(t, is.ErrorContains(err, "No such container"))
}

// Test case for #2099/#2125
func TestRemoveContainerWithVolume(t *testing.T) {
	defer setupTest(t)()
	ctx := context.Background()
	client := testEnv.APIClient()

	cID := container.Run(ctx, t, client, container.WithCmd("true"), container.WithVolume(dPath("/srv")))
	poll.WaitOn(t, container.IsInState(ctx, client, cID, "exited"), poll.WithDelay(100*time.Millisecond))

	insp, _, err := client.ContainerInspectWithRaw(ctx, cID, true)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(1, len(insp.Mounts)))
	volName := insp.Mounts[0].Name

	err = client.ContainerRemove(ctx, cID, types.ContainerRemoveOptions{
		RemoveVolumes: true,
	})
	assert.NilError(t, err)

	volumes, err := client.VolumeList(ctx, filters.NewArgs(filters.Arg("name", volName)))
	assert.NilError(t, err)
	assert.Check(t, is.Equal(0, len(volumes.Volumes)))
}

func TestRemoveContainerRunning(t *testing.T) {
	defer setupTest(t)()
	ctx := context.Background()
	client := testEnv.APIClient()

	cID := container.Run(ctx, t, client)

	err := client.ContainerRemove(ctx, cID, types.ContainerRemoveOptions{})
	assert.Check(t, is.ErrorContains(err, "cannot remove a running container"))
}

func TestRemoveContainerForceRemoveRunning(t *testing.T) {
	defer setupTest(t)()
	ctx := context.Background()
	client := testEnv.APIClient()

	cID := container.Run(ctx, t, client)

	err := client.ContainerRemove(ctx, cID, types.ContainerRemoveOptions{
		Force: true,
	})
	assert.NilError(t, err)
}

func TestRemoveInvalidContainer(t *testing.T) {
	defer setupTest(t)()
	ctx := context.Background()
	client := testEnv.APIClient()

	err := client.ContainerRemove(ctx, "unknown", types.ContainerRemoveOptions{})
	assert.Check(t, is.ErrorContains(err, "No such container"))
}
