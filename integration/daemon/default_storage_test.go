package daemon

import (
	"testing"

	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/storage"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

func TestDefaultStorageDriver(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "Windows does not support running sub-daemons")
	skip.If(t, testEnv.IsUserNamespace(), "containerd snapshotters are disabled with user namespace remapping")
	t.Setenv("DOCKER_DRIVER", "")
	t.Setenv("DOCKER_GRAPHDRIVER", "")
	t.Setenv("TEST_INTEGRATION_USE_GRAPHDRIVER", "")
	_ = testutil.StartSpan(baseContext, t)

	d := daemon.New(t)
	defer d.Stop(t)

	d.Start(t, "--iptables=false", "--ip6tables=false")

	info := d.Info(t)
	assert.Check(t, is.Equal(info.DriverStatus[0][1], "io.containerd.snapshotter.v1"))
}

// TestGraphDriverPersistence tests that when a daemon starts with graphdrivers,
// pulls images and creates containers, then is restarted without explicit
// graphdriver configuration, it continues to use graphdrivers instead of
// migrating to containerd snapshotters automatically.
func TestGraphDriverPersistence(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "Windows does not support running sub-daemons")
	t.Setenv("DOCKER_DRIVER", "")
	t.Setenv("DOCKER_GRAPHDRIVER", "")
	t.Setenv("TEST_INTEGRATION_USE_GRAPHDRIVER", "")

	ctx := testutil.StartSpan(baseContext, t)

	// Phase 1: Start daemon with explicit graphdriver (overlay2)
	d := daemon.New(t)
	t.Cleanup(func() {
		d.Stop(t)
	})

	const testImage = "busybox:latest"

	d.StartWithBusybox(ctx, t, "--iptables=false", "--ip6tables=false", "--storage-driver=overlay2")
	c := d.NewClientT(t)

	// Verify we're using graphdriver
	info := d.Info(t)
	if len(info.DriverStatus) > 0 {
		assert.Check(t, info.DriverStatus[0][1] != "io.containerd.snapshotter.v1")
	}
	prevDriver := info.Driver

	containerResp, err := c.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &containertypes.Config{
			Image: testImage,
			Cmd:   []string{"echo", "test"},
		},
		Name: "test-container",
	})
	assert.NilError(t, err, "Failed to create container")

	containerID := containerResp.ID

	d.Stop(t)

	// Phase 2: Start daemon again WITHOUT explicit graphdriver configuration
	d.Start(t, "--iptables=false", "--ip6tables=false")

	// Verify daemon still uses graphdriver (not containerd snapshotter)
	// Verify we're using graphdriver
	info = d.Info(t)
	if len(info.DriverStatus) > 0 {
		assert.Check(t, info.DriverStatus[0][1] != "io.containerd.snapshotter.v1")
	}
	assert.Check(t, is.Equal(info.Driver, prevDriver))

	// Verify our image is still there
	imageInspect, err := c.ImageInspect(ctx, testImage)
	assert.NilError(t, err, "Test image should still be available after daemon restart")
	assert.Check(t, imageInspect.GraphDriver != nil, "GraphDriver should be set for graphdriver backend")
	assert.Check(t, is.Equal(imageInspect.GraphDriver.Name, prevDriver), "Image graphdriver data should match")

	// Verify our container is still there
	inspect, err := c.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	assert.NilError(t, err, "Test container should still exist after daemon restart")
	assert.Check(t, inspect.Container.GraphDriver != nil, "GraphDriver should be set for graphdriver backend")
	assert.Check(t, is.Equal(inspect.Container.GraphDriver.Name, prevDriver), "Container graphdriver data should match")
}

// TestInspectGraphDriverAPIBC checks API backward compatibility of the GraphDriver field in image/container inspect.
func TestInspectGraphDriverAPIBC(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "Windows does not support running sub-daemons")
	t.Setenv("DOCKER_DRIVER", "")
	t.Setenv("DOCKER_GRAPHDRIVER", "")
	t.Setenv("TEST_INTEGRATION_USE_GRAPHDRIVER", "")
	ctx := testutil.StartSpan(baseContext, t)

	tests := []struct {
		name          string
		apiVersion    string
		storageDriver string

		expContainerdSnapshotter bool
		expGraphDriver           string
		expRootFSStorage         bool
	}{
		{
			name:                     "vCurrent/containerd",
			expContainerdSnapshotter: true,
			expRootFSStorage:         true,
		},
		{
			name:                     "v1.51/containerd",
			apiVersion:               "v1.51",
			expContainerdSnapshotter: true,
			expGraphDriver:           "overlayfs",
		},
		{
			name:           "vCurrent/graphdriver",
			storageDriver:  "vfs",
			expGraphDriver: "vfs",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			skip.If(t, tc.expContainerdSnapshotter && testEnv.IsUserNamespace(), "containerd snapshotters are disabled with user namespace remapping")

			d := daemon.New(t)
			defer d.Stop(t)
			d.StartWithBusybox(ctx, t, "--iptables=false", "--ip6tables=false", "--storage-driver="+tc.storageDriver)
			c := d.NewClientT(t, client.WithAPIVersion(tc.apiVersion))

			// Check selection of containerd / storage-driver worked.
			info := d.Info(t)
			if tc.expContainerdSnapshotter {
				assert.Check(t, is.Equal(info.Driver, "overlayfs"))
				assert.Check(t, is.Equal(info.DriverStatus[0][0], "driver-type"))
				assert.Check(t, is.Equal(info.DriverStatus[0][1], "io.containerd.snapshotter.v1"))
			} else {
				assert.Check(t, is.Equal(info.Driver, "vfs"))
				assert.Check(t, is.Len(info.DriverStatus, 0))
			}

			const testImage = "busybox:latest"
			ctr, err := c.ContainerCreate(ctx, client.ContainerCreateOptions{Image: testImage, Name: "test-container"})
			assert.NilError(t, err)
			defer func() { _, _ = c.ContainerRemove(ctx, ctr.ID, client.ContainerRemoveOptions{Force: true}) }()

			if imageInspect, err := c.ImageInspect(ctx, testImage); assert.Check(t, err) {
				if tc.expGraphDriver != "" {
					if assert.Check(t, imageInspect.GraphDriver != nil) {
						assert.Check(t, is.Equal(imageInspect.GraphDriver.Name, tc.expGraphDriver))
					}
				} else {
					assert.Check(t, is.Nil(imageInspect.GraphDriver))
				}
			}

			if inspect, err := c.ContainerInspect(ctx, ctr.ID, client.ContainerInspectOptions{}); assert.Check(t, err) {
				if tc.expGraphDriver != "" {
					if assert.Check(t, inspect.Container.GraphDriver != nil) {
						assert.Check(t, is.Equal(inspect.Container.GraphDriver.Name, tc.expGraphDriver))
					}
				} else {
					assert.Check(t, is.Nil(inspect.Container.GraphDriver))
				}
				if tc.expRootFSStorage {
					assert.DeepEqual(t, inspect.Container.Storage, &storage.Storage{
						RootFS: &storage.RootFSStorage{Snapshot: &storage.RootFSStorageSnapshot{Name: "overlayfs"}},
					})
				} else {
					assert.Check(t, is.Nil(inspect.Container.Storage))
				}
			}
		})
	}
}
