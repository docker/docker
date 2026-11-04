package container

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/containerd/platforms"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/internal/testutil/request"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

func TestInspectAnnotations(t *testing.T) {
	ctx := setupTest(t)
	apiClient := request.NewAPIClient(t)

	annotations := map[string]string{
		"hello": "world",
		"foo":   "bar",
	}

	name := strings.ToLower(t.Name())
	id := container.Create(ctx, t, apiClient,
		container.WithName(name),
		container.WithCmd("true"),
		func(c *container.TestContainerConfig) {
			c.HostConfig.Annotations = annotations
		},
	)

	inspect, err := apiClient.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(inspect.Container.HostConfig.Annotations, annotations))
}

// TestNetworkAliasesAreEmpty verifies that network-scoped aliases are not set
// for non-custom networks (network-scoped aliases are only supported for
// custom networks, except for the "Default Switch" network on Windows).
func TestNetworkAliasesAreEmpty(t *testing.T) {
	ctx := setupTest(t)
	apiClient := request.NewAPIClient(t)

	netModes := []string{"host", "bridge", "none"}
	if runtime.GOOS == "windows" {
		netModes = []string{"nat", "none"}
	}

	for _, nwMode := range netModes {
		t.Run(nwMode, func(t *testing.T) {
			skip.If(t, nwMode == "host" && testEnv.IsUserNamespace(), "host network mode is incompatible with user namespaces")
			ctr := container.Create(ctx, t, apiClient,
				container.WithName("ctr-"+nwMode),
				container.WithImage("busybox:latest"),
				container.WithNetworkMode(nwMode),
			)
			defer container.Remove(ctx, t, apiClient, ctr, client.ContainerRemoveOptions{Force: true})

			inspect := container.Inspect(ctx, t, apiClient, ctr)
			netAliases := inspect.NetworkSettings.Networks[nwMode].Aliases

			assert.Check(t, is.Nil(netAliases))
		})
	}
}

func TestInspectImageManifestPlatform(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, !testEnv.UsingSnapshotter())

	tests := []struct {
		name             string
		image            string
		skipIf           func() bool
		expectedPlatform platforms.Platform
	}{
		{
			name:  "amd64 only on any host",
			image: "busybox:latest",
			expectedPlatform: platforms.Platform{
				OS:           "linux",
				Architecture: "amd64",
				Variant:      "",
			},
		},
		{
			skipIf: func() bool { return runtime.GOARCH != "amd64" },
			name:   "amd64 image on non-amd64 host",

			image: "hello-world:amd64",
			expectedPlatform: platforms.Platform{
				OS:           "linux",
				Architecture: "amd64",
			},
		},
		{
			name:   "arm64 image on non-arm64 host",
			skipIf: func() bool { return runtime.GOARCH != "arm64" },
			image:  "hello-world:arm64",

			expectedPlatform: platforms.Platform{
				OS:           "linux",
				Architecture: "arm64",
				Variant:      "",
			},
		},
	}

	for _, tc := range tests {
		if tc.skipIf != nil && tc.skipIf() {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			ctx := setupTest(t)
			apiClient := request.NewAPIClient(t)

			ctr := container.Create(ctx, t, apiClient, container.WithImage(tc.image))
			defer apiClient.ContainerRemove(ctx, ctr, client.ContainerRemoveOptions{Force: true})

			img, err := apiClient.ImageInspect(ctx, tc.image)
			assert.NilError(t, err)

			hostPlatform := platforms.Platform{
				OS:           img.Os,
				Architecture: img.Architecture,
				Variant:      img.Variant,
			}
			inspect := container.Inspect(ctx, t, apiClient, ctr)
			assert.Assert(t, inspect.ImageManifestDescriptor != nil)
			assert.Check(t, is.DeepEqual(*inspect.ImageManifestDescriptor.Platform, hostPlatform))

			t.Run("pre 1.48", func(t *testing.T) {
				oldClient := request.NewAPIClient(t, client.WithAPIVersion("1.47"))
				inspect := container.Inspect(ctx, t, oldClient, ctr)
				assert.Check(t, is.Nil(inspect.ImageManifestDescriptor))
			})
		})
	}
}

func TestContainerInspectWithRaw(t *testing.T) {
	ctx := setupTest(t)
	apiClient := request.NewAPIClient(t)

	ctrID := container.Create(ctx, t, apiClient, container.WithCmd("true"), func(c *container.TestContainerConfig) {
		c.HostConfig.VolumeDriver = "local"
	})
	defer apiClient.ContainerRemove(ctx, ctrID, client.ContainerRemoveOptions{Force: true})

	tests := []struct {
		doc      string
		withSize bool
	}{
		{
			doc:      "no size",
			withSize: false,
		},
		{
			doc:      "with size",
			withSize: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			inspect, err := apiClient.ContainerInspect(ctx, ctrID, client.ContainerInspectOptions{
				Size: tc.withSize,
			})
			assert.NilError(t, err)
			assert.Check(t, is.Equal(inspect.Container.ID, ctrID))

			var rawInspect map[string]any
			err = json.Unmarshal(inspect.Raw, &rawInspect)
			assert.NilError(t, err, "Should produce valid JSON")

			if tc.withSize {
				if testEnv.DaemonInfo.OSType == "windows" {
					// FIXME(thaJeztah): Windows does not support size at all, so values would always be 0.
					// See https://github.com/moby/moby/blob/2837112c8ead55cdad36eaac61bafc713b4f669a/daemon/images/image_windows.go#L12-L16
					t.Log("skip checking SizeRw, SizeRootFs on windows as it's not yet implemented")
				} else {
					if assert.Check(t, inspect.Container.SizeRw != nil) {
						// RW-layer size can be zero.
						assert.Check(t, *inspect.Container.SizeRw >= 0, "Should have a size: %d", *inspect.Container.SizeRw)
					}
					if assert.Check(t, inspect.Container.SizeRootFs != nil) {
						assert.Check(t, *inspect.Container.SizeRootFs > 0, "Should have a size: %d", *inspect.Container.SizeRootFs)
					}
				}

				_, ok := rawInspect["SizeRw"]
				assert.Check(t, ok)
				_, ok = rawInspect["SizeRootFs"]
				assert.Check(t, ok)
			} else {
				assert.Check(t, is.Nil(inspect.Container.SizeRw))
				assert.Check(t, is.Nil(inspect.Container.SizeRootFs))
				_, ok := rawInspect["SizeRw"]
				assert.Check(t, !ok, "Should not contain SizeRw:\n%s", string(inspect.Raw))
				_, ok = rawInspect["SizeRootFs"]
				assert.Check(t, !ok, "Should not contain SizeRootFs:\n%s", string(inspect.Raw))
			}
		})
	}

	t.Run("API 1.24 compatibility", func(t *testing.T) {
		// Older API versions include GraphDriver even when using a snapshotter.
		oldClient := request.NewAPIClient(t, client.WithAPIVersion("1.24"))
		inspect, err := oldClient.ContainerInspect(ctx, ctrID, client.ContainerInspectOptions{})
		assert.NilError(t, err)

		var rawInspect map[string]any

		err = json.Unmarshal(inspect.Raw, &rawInspect)
		assert.NilError(t, err)

		for _, key := range []string{
			"Id", "State", "Created", "Path", "Args", "Config", "Image", "NetworkSettings",
			"ResolvConfPath", "HostnamePath", "HostsPath", "LogPath", "Name", "Driver",
			"MountLabel", "ProcessLabel", "GraphDriver", "Mounts",
		} {
			_, ok := rawInspect[key]
			assert.Check(t, ok, "%s does not exist in response", key)
		}

		// Issue #6830: type not properly converted to JSON/back
		assert.Check(t, is.Equal(rawInspect["Path"], "true"), "Path should be the string \"true\", not a boolean")

		config, ok := rawInspect["Config"].(map[string]any)
		assert.Assert(t, ok, "Unable to find 'Config' object")
		_, ok = config["VolumeDriver"]
		assert.Check(t, !ok, "Expected to not include VolumeDriver in 'Config'")

		hostConfig, ok := rawInspect["HostConfig"].(map[string]any)
		assert.Assert(t, ok, "Unable to find 'HostConfig' object")
		assert.Check(t, is.Equal(hostConfig["VolumeDriver"], "local"), "Expected to include VolumeDriver in 'HostConfig'")
	})
}
