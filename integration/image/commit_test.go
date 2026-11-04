package image

import (
	"strings"
	"testing"
	"time"

	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

func TestCommitInheritsCmd(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "test requires a Linux container")
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	expectedCmd := []string{"/bin/sh", "-c", "touch /test"}
	cID := container.Run(ctx, t, apiClient, container.WithCmd(expectedCmd...))
	poll.WaitOn(t, container.IsSuccessful(ctx, apiClient, cID))

	img, err := apiClient.ContainerCommit(ctx, cID, client.ContainerCommitOptions{
		Reference: strings.ToLower(t.Name()) + ":testtag",
	})
	assert.NilError(t, err)

	imgInspect, err := apiClient.ImageInspect(ctx, img.ID)
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(imgInspect.Config.Cmd, expectedCmd))

	// Verify that the committed image contains the file created by the container.
	cID = container.Run(ctx, t, apiClient, container.WithImage(img.ID), container.WithCmd("ls", "/test"))
	poll.WaitOn(t, container.IsSuccessful(ctx, apiClient, cID))
}

func TestCommitWithLabelInConfig(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "test requires a Linux container")
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	expectedCmd := []string{"/bin/sh", "-c", "touch /test"}
	cID := container.Run(ctx, t, apiClient, container.WithCmd(expectedCmd...))
	poll.WaitOn(t, container.IsSuccessful(ctx, apiClient, cID))

	img, err := apiClient.ContainerCommit(ctx, cID, client.ContainerCommitOptions{
		Reference: strings.ToLower(t.Name()),
		Config: &containertypes.Config{
			Labels: map[string]string{"key1": "value1", "key2": "value2"},
		},
	})
	assert.NilError(t, err)

	imgInspect, err := apiClient.ImageInspect(ctx, img.ID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(imgInspect.Config.Labels["key1"], "value1"))
	assert.Check(t, is.Equal(imgInspect.Config.Labels["key2"], "value2"))
	assert.Check(t, is.DeepEqual(imgInspect.Config.Cmd, expectedCmd))

	// Verify that the committed image contains the file created by the container.
	cID = container.Run(ctx, t, apiClient, container.WithImage(img.ID), container.WithCmd("ls", "/test"))
	poll.WaitOn(t, container.IsSuccessful(ctx, apiClient, cID))
}

// TestCommitPausedContainer tests that committing a paused container leaves it paused.
func TestCommitPausedContainer(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux", "test requires a Linux container")
	skip.If(t, testEnv.DaemonInfo.CgroupDriver == "none")
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	cID := container.Run(ctx, t, apiClient)
	_, err := apiClient.ContainerPause(ctx, cID, client.ContainerPauseOptions{})
	assert.NilError(t, err)

	img, err := apiClient.ContainerCommit(ctx, cID, client.ContainerCommitOptions{})
	assert.NilError(t, err)

	_, err = apiClient.ImageInspect(ctx, img.ID)
	assert.NilError(t, err)

	inspect, err := apiClient.ContainerInspect(ctx, cID, client.ContainerInspectOptions{})
	assert.NilError(t, err)
	assert.Check(t, inspect.Container.State.Paused)
}

func TestCommitInheritsEnv(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "FIXME")
	ctx := setupTest(t)

	apiClient := testEnv.APIClient()

	cID1 := container.Create(ctx, t, apiClient)
	imgName := strings.ToLower(t.Name())

	commitResp1, err := apiClient.ContainerCommit(ctx, cID1, client.ContainerCommitOptions{
		Changes:   []string{"ENV PATH=/bin"},
		Reference: imgName,
	})
	assert.NilError(t, err)

	image1, err := apiClient.ImageInspect(ctx, commitResp1.ID)
	assert.NilError(t, err)

	expectedEnv1 := []string{"PATH=/bin"}
	assert.Check(t, is.DeepEqual(expectedEnv1, image1.Config.Env))

	cID2 := container.Create(ctx, t, apiClient, container.WithImage(image1.ID))

	commitResp2, err := apiClient.ContainerCommit(ctx, cID2, client.ContainerCommitOptions{
		Changes:   []string{"ENV PATH=/usr/bin:$PATH"},
		Reference: imgName,
	})
	assert.NilError(t, err)

	image2, err := apiClient.ImageInspect(ctx, commitResp2.ID)
	assert.NilError(t, err)
	expectedEnv2 := []string{"PATH=/usr/bin:/bin"}
	assert.Check(t, is.DeepEqual(expectedEnv2, image2.Config.Env))
}

// Verify that files created are owned by the remapped user even after a commit
func TestUsernsCommit(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, testEnv.IsRemoteDaemon())
	skip.If(t, !testEnv.IsUserNamespaceInKernel())
	skip.If(t, testEnv.IsRootless())

	t.Parallel()

	ctx := t.Context()
	dUserRemap := daemon.New(t, daemon.WithUserNsRemap("default"))
	dUserRemap.StartWithBusybox(ctx, t, "--iptables=false", "--ip6tables=false")
	clientUserRemap := dUserRemap.NewClientT(t)
	defer clientUserRemap.Close()

	cID := container.Run(ctx, t, clientUserRemap, container.WithName(t.Name()), container.WithImage("busybox"), container.WithCmd("sh", "-c", "echo hello world > /hello.txt && chown 1000:1000 /hello.txt"))
	poll.WaitOn(t, container.IsStopped(ctx, clientUserRemap, cID))
	img, err := clientUserRemap.ContainerCommit(ctx, t.Name(), client.ContainerCommitOptions{})
	assert.NilError(t, err)

	res := container.RunAttach(ctx, t, clientUserRemap, container.WithImage(img.ID), container.WithCmd("sh", "-c", "stat -c %u:%g /hello.txt"))
	assert.Check(t, is.Equal(res.ExitCode, 0))
	assert.Check(t, is.Equal(res.Stderr.String(), ""))
	assert.Assert(t, is.Equal(strings.TrimSpace(res.Stdout.String()), "1000:1000"))
}

func TestCommitChangeLabels(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	// Start with a label that will be overridden in the committed image.
	cID := container.Run(ctx, t, apiClient,
		container.WithCmd("true"),
		func(c *container.TestContainerConfig) {
			c.Config.Labels = map[string]string{"some": "label"}
		},
	)
	var pollOpts []poll.SettingOp
	if testEnv.DaemonInfo.OSType == "windows" {
		pollOpts = append(pollOpts, poll.WithTimeout(75*time.Second))
	}
	poll.WaitOn(t, container.IsSuccessful(ctx, apiClient, cID), pollOpts...)

	img, err := apiClient.ContainerCommit(ctx, cID, client.ContainerCommitOptions{
		Changes: []string{"LABEL some=label2"},
	})
	assert.NilError(t, err)

	// The committed image should contain the replacement label.
	imgInspect, err := apiClient.ImageInspect(ctx, img.ID)
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(
		imgInspect.Config.Labels,
		map[string]string{"some": "label2"},
	))

	// Changing the image label must not change the source container.
	source := container.Inspect(ctx, t, apiClient, cID)
	assert.Check(t, is.DeepEqual(
		source.Config.Labels,
		map[string]string{"some": "label"},
	))
}

func TestCommitChange(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	// Wait for the container to finish before committing its changes.
	cID := container.Run(ctx, t, apiClient, container.WithCmd("true"))
	var pollOpts []poll.SettingOp
	if testEnv.DaemonInfo.OSType == "windows" {
		pollOpts = append(pollOpts, poll.WithTimeout(75*time.Second))
	}
	poll.WaitOn(t, container.IsSuccessful(ctx, apiClient, cID), pollOpts...)

	img, err := apiClient.ContainerCommit(ctx, cID, client.ContainerCommitOptions{
		Changes: []string{
			"EXPOSE 8080",
			"ENV DEBUG true",
			"ENV test 1",
			"ENV PATH /foo",
			"LABEL foo bar",
			`CMD ["/bin/sh"]`,
			"WORKDIR /opt",
			`ENTRYPOINT ["/bin/sh"]`,
			"USER testuser",
			"VOLUME /var/lib/docker",
			"ONBUILD /usr/local/bin/python-build --dir /app/src",
		},
	})
	assert.NilError(t, err)

	imgInspect, err := apiClient.ImageInspect(ctx, img.ID)
	assert.NilError(t, err)
	assert.Assert(t, imgInspect.Config != nil)
	config := imgInspect.Config

	expectedEnv := []string{"PATH=/foo", "DEBUG=true", "test=1"}
	expectedWorkingDir := "/opt"
	if testEnv.DaemonInfo.OSType == "windows" {
		// Windows has no inherited PATH, and normalizes WORKDIR to a drive path.
		expectedEnv = []string{"DEBUG=true", "test=1", "PATH=/foo"}
		expectedWorkingDir = `C:\opt`
	}

	// Check all nine configuration fields covered by the legacy test.
	assert.Check(t, is.DeepEqual(config.ExposedPorts, map[string]struct{}{"8080/tcp": {}}))
	assert.Check(t, is.DeepEqual(config.Env, expectedEnv))
	assert.Check(t, is.DeepEqual(config.Labels, map[string]string{"foo": "bar"}))
	assert.Check(t, is.DeepEqual(config.Cmd, []string{"/bin/sh"}))
	assert.Check(t, is.Equal(config.WorkingDir, expectedWorkingDir))
	assert.Check(t, is.DeepEqual(config.Entrypoint, []string{"/bin/sh"}))
	assert.Check(t, is.Equal(config.User, "testuser"))
	assert.Check(t, is.DeepEqual(config.Volumes, map[string]struct{}{"/var/lib/docker": {}}))
	assert.Check(t, is.DeepEqual(config.OnBuild, []string{"/usr/local/bin/python-build --dir /app/src"}))
}
