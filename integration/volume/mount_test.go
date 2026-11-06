package volume

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/versions"
	"github.com/moby/moby/v2/daemon/volume/safepath"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"github.com/moby/moby/v2/internal/testutil/fakecontext"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

func TestRunMountVolumeSubdir(t *testing.T) {
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.45"), "skip test from new feature")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	testVolumeName := setupTestVolume(t, apiClient)

	for _, tc := range []struct {
		name         string
		opts         mount.VolumeOptions
		cmd          []string
		volumeTarget string
		createErr    string
		startErr     string
		expected     string
		skipPlatform string
	}{
		{name: "subdir", opts: mount.VolumeOptions{Subpath: "subdir"}, cmd: []string{"ls", "/volume"}, expected: "hello.txt"},
		{name: "subdir link", opts: mount.VolumeOptions{Subpath: "hack/good"}, cmd: []string{"ls", "/volume"}, expected: "hello.txt"},
		{name: "subdir with copy data", opts: mount.VolumeOptions{Subpath: "bin"}, volumeTarget: "/bin", cmd: []string{"ls", "/bin/busybox"}, expected: "/bin/busybox", skipPlatform: "windows:copy not supported on Windows"},
		{name: "file", opts: mount.VolumeOptions{Subpath: "bar.txt"}, cmd: []string{"cat", "/volume"}, expected: "foo", skipPlatform: "windows:file bind mounts not supported on Windows"},
		{name: "file over existing target", opts: mount.VolumeOptions{Subpath: "file"}, volumeTarget: "/etc/localtime", cmd: []string{"cat", "/etc/localtime"}, expected: "from-volume", skipPlatform: "windows:file bind mounts not supported on Windows"},
		{name: "relative with backtracks", opts: mount.VolumeOptions{Subpath: "../../../../../../etc/passwd"}, cmd: []string{"cat", "/volume"}, createErr: "subpath must be a relative path within the volume"},
		{name: "not existing", opts: mount.VolumeOptions{Subpath: "not-existing-path"}, cmd: []string{"cat", "/volume"}, startErr: (&safepath.ErrNotAccessible{}).Error()},

		{name: "mount link", opts: mount.VolumeOptions{Subpath: filepath.Join("hack", "root")}, cmd: []string{"ls", "/volume"}, startErr: (&safepath.ErrEscapesBase{}).Error()},
		{name: "mount link link", opts: mount.VolumeOptions{Subpath: filepath.Join("hack", "bad")}, cmd: []string{"ls", "/volume"}, startErr: (&safepath.ErrEscapesBase{}).Error()}, //nolint:dupword
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.skipPlatform != "" {
				platform, reason, _ := strings.Cut(tc.skipPlatform, ":")
				if testEnv.DaemonInfo.OSType == platform {
					t.Skip(reason)
				}
			}

			cfg := containertypes.Config{
				Image: "busybox",
				Cmd:   tc.cmd,
			}
			hostCfg := containertypes.HostConfig{
				Mounts: []mount.Mount{
					{
						Type:          mount.TypeVolume,
						Source:        testVolumeName,
						Target:        "/volume",
						VolumeOptions: &tc.opts,
					},
				},
			}
			if testEnv.DaemonInfo.OSType == "windows" {
				hostCfg.Mounts[0].Target = `C:\volume`
			}
			if tc.volumeTarget != "" {
				hostCfg.Mounts[0].Target = tc.volumeTarget
			}

			ctrName := strings.ReplaceAll(t.Name(), "/", "_")
			create, creatErr := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
				Config:           &cfg,
				HostConfig:       &hostCfg,
				NetworkingConfig: &network.NetworkingConfig{},
				Name:             ctrName,
			})
			id := create.ID
			if id != "" {
				defer apiClient.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
			}

			if tc.createErr != "" {
				assert.ErrorContains(t, creatErr, tc.createErr)
				return
			}
			assert.NilError(t, creatErr, "container creation failed")

			_, startErr := apiClient.ContainerStart(ctx, id, client.ContainerStartOptions{})
			if tc.startErr != "" {
				assert.ErrorContains(t, startErr, tc.startErr)
				return
			}
			assert.NilError(t, startErr)

			output, err := container.Output(ctx, apiClient, id)
			assert.Check(t, err)

			inspect, err := apiClient.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
			if assert.Check(t, err) {
				assert.Check(t, is.Equal(inspect.Container.State.ExitCode, 0))
			}

			assert.Check(t, is.Equal(strings.TrimSpace(output.Stderr), ""))
			assert.Check(t, is.Equal(strings.TrimSpace(output.Stdout), tc.expected))
		})
	}
}

func TestRunMountImage(t *testing.T) {
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.48"), "skip test from new feature")
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "image mounts not supported on Windows")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	for _, tc := range []struct {
		name         string
		opts         mount.ImageOptions
		cmd          []string
		target       string
		createErr    string
		startErr     string
		expected     string
		skipPlatform string
	}{
		{name: "image", cmd: []string{"cat", "/image/foo"}, expected: "bar"},
		{name: "image_tag", cmd: []string{"cat", "/image/foo"}, expected: "bar"},

		{name: "subdir", opts: mount.ImageOptions{Subpath: "subdir"}, cmd: []string{"ls", "/image"}, expected: "hello"},
		{name: "subdir link", opts: mount.ImageOptions{Subpath: "hack/good"}, cmd: []string{"ls", "/image"}, expected: "hello"},
		{name: "subdir link outside context", opts: mount.ImageOptions{Subpath: "hack/bad"}, cmd: []string{"ls", "/image"}, startErr: (&safepath.ErrEscapesBase{}).Error()},
		{name: "file", opts: mount.ImageOptions{Subpath: "subdir/hello"}, cmd: []string{"cat", "/image"}, expected: "world"},
		{name: "file_over_existing_target", opts: mount.ImageOptions{Subpath: "subdir/hello"}, cmd: []string{"cat", "/etc/localtime"}, target: "/etc/localtime", expected: "world"},
		{name: "relative with backtracks", opts: mount.ImageOptions{Subpath: "../../../../../../etc/passwd"}, cmd: []string{"cat", "/image"}, createErr: "subpath must be a relative path within the volume"},
		{name: "not existing", opts: mount.ImageOptions{Subpath: "not-existing-path"}, cmd: []string{"cat", "/image"}, startErr: (&safepath.ErrNotAccessible{}).Error()},

		{name: "image_remove", cmd: []string{"cat", "/image/foo"}, expected: "bar"},
		// Expected is duplicated because the container runs twice
		{name: "image_remove_force", cmd: []string{"cat", "/image/foo"}, expected: "barbar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testImage := setupTestImage(t, ctx, apiClient, tc.name)
			if testImage != "" {
				defer func() {
					_, _ = apiClient.ImageRemove(ctx, testImage, client.ImageRemoveOptions{Force: true})
				}()
			}

			cfg := containertypes.Config{
				Image: "busybox",
				Cmd:   tc.cmd,
			}

			target := tc.target
			if target == "" {
				target = "/image"
			}

			hostCfg := containertypes.HostConfig{
				Mounts: []mount.Mount{
					{
						Type:         mount.TypeImage,
						Source:       testImage,
						Target:       target,
						ImageOptions: &tc.opts,
					},
				},
			}

			startContainer := func(id string) {
				_, startErr := apiClient.ContainerStart(ctx, id, client.ContainerStartOptions{})
				if tc.startErr != "" {
					assert.ErrorContains(t, startErr, tc.startErr)
					return
				}
				assert.NilError(t, startErr)
			}

			ctrName := strings.ReplaceAll(t.Name(), "/", "_")
			create, creatErr := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
				Config:           &cfg,
				HostConfig:       &hostCfg,
				NetworkingConfig: &network.NetworkingConfig{},
				Name:             ctrName,
			})
			id := create.ID
			if id != "" {
				defer container.Remove(ctx, t, apiClient, id, client.ContainerRemoveOptions{Force: true})
			}

			if tc.createErr != "" {
				assert.ErrorContains(t, creatErr, tc.createErr)
				return
			}
			assert.NilError(t, creatErr, "container creation failed")
			startContainer(id)

			// Test image mounted is in use logic
			if tc.name == "image_remove" {
				img, _ := apiClient.ImageInspect(ctx, testImage)
				imgId := strings.Split(img.ID, ":")[1]
				_, removeErr := apiClient.ImageRemove(ctx, testImage, client.ImageRemoveOptions{})
				assert.ErrorContains(t, removeErr, fmt.Sprintf(`container %s is using its referenced image %s`, id[:12], imgId[:12]))
			}

			// Test that the container survives a restart when mounted image is removed
			if tc.name == "image_remove_force" {
				_, stopErr := apiClient.ContainerStop(ctx, id, client.ContainerStopOptions{})
				assert.NilError(t, stopErr)

				_, removeErr := apiClient.ImageRemove(ctx, testImage, client.ImageRemoveOptions{Force: true})
				assert.NilError(t, removeErr)

				startContainer(id)
			}

			output, err := container.Output(ctx, apiClient, id)
			assert.Check(t, err)

			inspect, err := apiClient.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
			if tc.startErr == "" && assert.Check(t, err) {
				assert.Check(t, is.Equal(inspect.Container.State.ExitCode, 0))
			}

			assert.Check(t, is.Equal(strings.TrimSpace(output.Stderr), ""))
			assert.Check(t, is.Equal(strings.TrimSpace(output.Stdout), tc.expected))
		})
	}
}

// setupTestVolume sets up a volume with:
// .
// |-- bar.txt                        (file with "foo")
// |-- bin                            (directory)
// |-- file                           (file with "from-volume")
// |-- subdir                         (directory)
// |   |-- hello.txt                  (file with "world")
// |-- hack                           (directory)
// |   |-- root                       (symlink to /)
// |   |-- good                       (symlink to ../subdir)
// |   |-- bad                        (symlink to root)
func setupTestVolume(t *testing.T, apiClient client.APIClient) string {
	t.Helper()
	ctx := t.Context()

	volumeName := t.Name() + "-volume"

	_, err := apiClient.VolumeRemove(ctx, volumeName, client.VolumeRemoveOptions{Force: true})
	assert.NilError(t, err, "failed to clean volume")

	_, err = apiClient.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name: volumeName,
	})
	assert.NilError(t, err, "failed to setup volume")

	rootFs := "/"
	target := "/volume"
	if testEnv.DaemonInfo.OSType == "windows" {
		rootFs = `C:`
		target = `C:\volume`
	}

	initCmd := "echo foo > /volume/bar.txt && " +
		"echo from-volume > /volume/file && " +
		"mkdir /volume/bin && " +
		"mkdir /volume/subdir && " +
		"echo world > /volume/subdir/hello.txt && " +
		"mkdir /volume/hack && " +
		"ln -s " + rootFs + " /volume/hack/root && " +
		"ln -s ../subdir /volume/hack/good && " +
		"ln -s root /volume/hack/bad &&" +
		"mkdir /volume/hack/iwanttobehackedwithtoctou"

	opts := []func(*container.TestContainerConfig){
		container.WithMount(mount.Mount{
			Type:   mount.TypeVolume,
			Source: volumeName,
			Target: target,
		}),
		container.WithCmd("sh", "-c", initCmd+"; ls -lah /volume /volume/hack/"),
	}
	if testEnv.DaemonInfo.OSType == "windows" {
		// Can't create symlinks under HyperV isolation
		opts = append(opts, container.WithIsolation(containertypes.IsolationProcess))
	}

	cid := container.Run(ctx, t, apiClient, opts...)
	defer container.Remove(ctx, t, apiClient, cid, client.ContainerRemoveOptions{Force: true})
	output, err := container.Output(ctx, apiClient, cid)

	assert.NilError(t, err)
	assert.Assert(t, is.Equal(output.Stderr, ""))

	inspect, err := apiClient.ContainerInspect(ctx, cid, client.ContainerInspectOptions{})
	assert.NilError(t, err)
	assert.Assert(t, is.Equal(inspect.Container.State.ExitCode, 0))

	return volumeName
}

func setupTestImage(t *testing.T, ctx context.Context, apiClient client.APIClient, test string) string {
	imgName := "test-image"

	if test == "image_tag" {
		imgName += ":foo"
	}

	//nolint:dupword // ignore "Duplicate words (subdir) found (dupword)"
	dockerfile := `
		FROM busybox as symlink
		RUN mkdir /hack \
			&& ln -s "../subdir" /hack/good \
			&& ln -s ../../../../../ /hack/bad
		#--
		FROM scratch
		COPY foo /
		COPY subdir subdir
		COPY --from=symlink /hack /hack
		`

	source := fakecontext.New(
		t,
		"",
		fakecontext.WithDockerfile(dockerfile),
		fakecontext.WithFile("foo", "bar"),
		fakecontext.WithFile("subdir/hello", "world"),
	)
	defer source.Close()

	resp, err := apiClient.ImageBuild(ctx, source.AsTarReader(t), client.ImageBuildOptions{
		Remove:      false,
		ForceRemove: false,
		Tags:        []string{imgName},
	})
	assert.NilError(t, err)

	out := bytes.NewBuffer(nil)
	_, err = io.Copy(out, resp.Body)
	assert.Check(t, resp.Body.Close())
	assert.NilError(t, err)

	_, err = apiClient.ImageInspect(ctx, imgName)
	if err != nil {
		t.Log(out)
	}
	assert.NilError(t, err)

	return imgName
}

// TestRunMountImageMultipleTimes tests mounting the same image multiple times to different destinations
// Regression test for: https://github.com/moby/moby/issues/50122
func TestRunMountImageMultipleTimes(t *testing.T) {
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.48"), "skip test from new feature")
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	basePath := "/etc"
	if testEnv.DaemonInfo.OSType == "windows" {
		basePath = `C:\`
	}

	// hello-world image is a small image that only has /hello executable binary file
	testImage := "hello-world:frozen"
	id := container.Run(ctx, t, apiClient,
		container.WithImage("busybox:latest"),
		container.WithCmd("top"),
		container.WithMount(mount.Mount{
			Type:   mount.TypeImage,
			Source: testImage,
			Target: filepath.Join(basePath, "foo"),
		}),
		container.WithMount(mount.Mount{
			Type:   mount.TypeImage,
			Source: testImage,
			Target: filepath.Join(basePath, "bar"),
		}),
	)

	defer apiClient.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})

	inspect, err := apiClient.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	assert.NilError(t, err)

	assert.Equal(t, len(inspect.Container.Mounts), 2)
	var hasFoo, hasBar bool
	for _, mnt := range inspect.Container.Mounts {
		if mnt.Destination == "/etc/foo" {
			hasFoo = true
		}
		if mnt.Destination == "/etc/bar" {
			hasBar = true
		}
	}

	assert.Check(t, hasFoo, "Expected mount at /etc/foo")
	assert.Check(t, hasBar, "Expected mount at /etc/bar")

	t.Run("mounted foo", func(t *testing.T) {
		res, err := container.Exec(ctx, apiClient, id, []string{"stat", "/etc/foo/hello"})
		assert.NilError(t, err)
		assert.Check(t, is.Equal(res.ExitCode, 0))
	})

	t.Run("mounted bar", func(t *testing.T) {
		res, err := container.Exec(ctx, apiClient, id, []string{"stat", "/etc/bar/hello"})
		assert.NilError(t, err)
		assert.Check(t, is.Equal(res.ExitCode, 0))
	})
}

// TestRunMountImageSubpathDaemonRestart tests that a container with an image mount
// and subpath can be restarted after a daemon restart.
// Regression test for: https://github.com/moby/moby/issues/50999
func TestRunMountImageSubpathDaemonRestart(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon, "cannot start daemon on remote test run")
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "image mounts not supported on Windows")

	skip.If(t, testEnv.IsRootless, "FIXME: https://github.com/moby/moby/issues/50999")

	t.Parallel()

	ctx := testutil.StartSpan(baseContext, t)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t, "--iptables=false", "--ip6tables=false")
	defer d.Stop(t)

	apiClient := d.NewClientT(t)
	defer apiClient.Close()

	// Create container with image mount and subpath
	// Use a long-running command so we can exec into it after daemon restart
	cfg := containertypes.Config{
		Image: "busybox",
		Cmd:   []string{"sleep", "infinity"},
	}
	hostCfg := containertypes.HostConfig{
		RestartPolicy: containertypes.RestartPolicy{
			Name: containertypes.RestartPolicyAlways,
		},
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeImage,
				Source: "busybox:latest",
				Target: "/image",
				ImageOptions: &mount.ImageOptions{
					Subpath: "bin",
				},
			},
		},
	}

	ctrName := strings.ReplaceAll(t.Name(), "/", "_")
	create, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           &cfg,
		HostConfig:       &hostCfg,
		NetworkingConfig: &network.NetworkingConfig{},
		Name:             ctrName,
	})
	assert.NilError(t, err, "container creation failed")
	ctrID := create.ID
	defer apiClient.ContainerRemove(ctx, ctrID, client.ContainerRemoveOptions{Force: true})

	_, err = apiClient.ContainerStart(ctx, ctrID, client.ContainerStartOptions{})
	assert.NilError(t, err, "container start failed before daemon restart")

	res, err := container.Exec(ctx, apiClient, ctrID, []string{"ls", "/image"})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(res.ExitCode, 0))
	assert.Check(t, is.Contains(res.Stdout(), "busybox"))

	d.Restart(t, "--iptables=false", "--ip6tables=false")

	inspect, err := apiClient.ContainerInspect(ctx, ctrID, client.ContainerInspectOptions{})
	assert.NilError(t, err, "container inspect failed after daemon restart")

	assert.Assert(t, is.Len(inspect.Container.Mounts, 1), "expected 1 mount")
	assert.Check(t, is.Equal(inspect.Container.Mounts[0].Type, mount.TypeImage))
	assert.Check(t, is.Equal(inspect.Container.Mounts[0].Destination, "/image"))

	assert.Assert(t, inspect.Container.State.Running, "container should be running after daemon restart")

	res, err = container.Exec(ctx, apiClient, ctrID, []string{"ls", "/bin"})
	assert.NilError(t, err, "exec failed after daemon restart")
	assert.Assert(t, is.Equal(res.ExitCode, 0))
	assert.Check(t, is.Contains(res.Stdout(), "busybox"))
}
