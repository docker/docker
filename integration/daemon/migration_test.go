package daemon

import (
	"bytes"
	"io"
	"runtime"
	"testing"

	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"github.com/moby/moby/v2/internal/testutil/fixtures/load"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/skip"
)

func TestMigrateOverlaySnapshotter(t *testing.T) {
	testMigrateSnapshotter(t, "overlay2", "overlayfs")
}

func TestMigrateNativeSnapshotter(t *testing.T) {
	testMigrateSnapshotter(t, "vfs", "native")
}

func testMigrateSnapshotter(t *testing.T, graphdriver, snapshotter string) {
	skip.If(t, runtime.GOOS != "linux")
	skip.If(t, testEnv.IsUserNamespace(), "containerd migration is not supported with user namespace remapping")

	t.Setenv("DOCKER_MIGRATE_SNAPSHOTTER_THRESHOLD", "200M")
	t.Setenv("DOCKER_DRIVER", "")

	ctx := testutil.StartSpan(baseContext, t)

	d := daemon.New(t)
	defer d.Stop(t)

	d.Start(t, "--iptables=false", "--ip6tables=false", "-s", graphdriver)
	info := d.Info(t)
	id := info.ID
	assert.Check(t, id != "")
	assert.Equal(t, info.Containers, 0)
	assert.Equal(t, info.Images, 0)
	assert.Equal(t, info.Driver, graphdriver)

	load.FrozenImagesLinux(ctx, d.NewClientT(t), "busybox:latest")

	info = d.Info(t)
	allImages := info.Images
	assert.Check(t, allImages > 0)

	apiClient := d.NewClientT(t)

	containerID := container.Run(ctx, t, apiClient, func(c *container.TestContainerConfig) {
		c.Name = "Migration-1-" + snapshotter
		c.Config.Image = "busybox:latest"
		c.Config.Cmd = []string{"top"}
	})

	d.Stop(t)

	// Start with migration feature but with a container which will prevent migration
	d.Start(t, "--iptables=false", "--ip6tables=false", "-s", graphdriver, "--feature", "containerd-migration")
	info = d.Info(t)
	assert.Equal(t, info.ID, id)
	assert.Equal(t, info.Driver, graphdriver)
	assert.Equal(t, info.Containers, 1)
	assert.Equal(t, info.Images, allImages)
	container.Remove(ctx, t, apiClient, containerID, client.ContainerRemoveOptions{
		Force: true,
	})

	d.Stop(t)

	d.Start(t, "--iptables=false", "--ip6tables=false", "-s", graphdriver, "--feature", "containerd-migration")
	info = d.Info(t)
	assert.Equal(t, info.ID, id)
	assert.Equal(t, info.Containers, 0)
	assert.Equal(t, info.Driver, snapshotter, "expected migrate to switch from %s to %s", graphdriver, snapshotter)
	assert.Equal(t, info.Images, allImages)

	result := container.RunAttach(ctx, t, apiClient, func(c *container.TestContainerConfig) {
		c.Name = "Migration-2-" + snapshotter
		c.Config.Image = "busybox:latest"
		c.Config.Cmd = []string{"echo", "hello"}
	})
	assert.Equal(t, result.ExitCode, 0)
	container.Remove(ctx, t, apiClient, result.ContainerID, client.ContainerRemoveOptions{})
}

func TestMigrateSaveLoad(t *testing.T) {
	skip.If(t, runtime.GOOS != "linux")
	skip.If(t, testEnv.IsUserNamespace(), "containerd migration is not supported with user namespace remapping")

	t.Setenv("DOCKER_MIGRATE_SNAPSHOTTER_THRESHOLD", "200M")
	t.Setenv("DOCKER_DRIVER", "")

	var (
		ctx         = testutil.StartSpan(baseContext, t)
		d           = daemon.New(t)
		graphdriver = "overlay2"
		snapshotter = "overlayfs"
	)
	defer d.Stop(t)

	d.Start(t, "--iptables=false", "--ip6tables=false", "-s", graphdriver)
	info := d.Info(t)
	id := info.ID
	assert.Check(t, id != "")
	assert.Equal(t, info.Containers, 0)
	assert.Equal(t, info.Images, 0)
	assert.Equal(t, info.Driver, graphdriver)

	load.FrozenImagesLinux(ctx, d.NewClientT(t), "busybox:latest")

	info = d.Info(t)
	allImages := info.Images
	assert.Check(t, allImages > 0)

	d.Stop(t)

	d.Start(t, "--iptables=false", "--ip6tables=false", "-s", graphdriver, "--feature", "containerd-migration")
	info = d.Info(t)
	assert.Equal(t, info.ID, id)
	assert.Equal(t, info.Containers, 0)
	assert.Equal(t, info.Driver, snapshotter, "expected migrate to switch from %s to %s", graphdriver, snapshotter)
	assert.Equal(t, info.Images, allImages)

	apiClient := d.NewClientT(t)

	// Save image to buffer
	rdr, err := apiClient.ImageSave(ctx, []string{"busybox:latest"})
	assert.NilError(t, err)
	buf := bytes.NewBuffer(nil)
	_, _ = io.Copy(buf, rdr)
	defer func() { _ = rdr.Close() }()

	// Delete all images
	list, err := apiClient.ImageList(ctx, client.ImageListOptions{})
	assert.NilError(t, err)
	for _, i := range list.Items {
		_, err = apiClient.ImageRemove(ctx, i.ID, client.ImageRemoveOptions{Force: true})
		assert.NilError(t, err)
	}

	// Check zero images
	info = d.Info(t)
	assert.Equal(t, info.Images, 0)

	// Import
	resp, err := apiClient.ImageLoad(ctx, bytes.NewReader(buf.Bytes()), client.ImageLoadWithQuiet(true))
	assert.NilError(t, err)
	_, _ = io.Copy(io.Discard, resp)
	_ = resp.Close()

	result := container.RunAttach(ctx, t, apiClient, func(c *container.TestContainerConfig) {
		c.Name = "Migration-save-load-" + snapshotter
		c.Config.Image = "busybox:latest"
		c.Config.Cmd = []string{"echo", "hello"}
	})
	assert.Equal(t, result.ExitCode, 0)
	container.Remove(ctx, t, apiClient, result.ContainerID, client.ContainerRemoveOptions{})
}
