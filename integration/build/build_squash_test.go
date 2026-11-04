package build

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/build"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"github.com/moby/moby/v2/internal/testutil/fakecontext"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

func TestBuildSquashParent(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")
	skip.If(t, testEnv.UsingSnapshotter(), "squash is not implemented for containerd image store")
	t.Parallel()

	ctx := testutil.StartSpan(baseContext, t)

	var apiClient client.APIClient
	if !testEnv.DaemonInfo.ExperimentalBuild {
		skip.If(t, testEnv.IsRemoteDaemon, "cannot run daemon when remote daemon")

		d := daemon.New(t, daemon.WithExperimental())
		d.StartWithBusybox(ctx, t)
		defer d.Stop(t)
		apiClient = d.NewClientT(t)
	} else {
		apiClient = testEnv.APIClient()
	}

	dockerfile := `
		FROM busybox
		RUN echo hello > /hello
		RUN echo world >> /hello
		RUN echo hello > /remove_me
		ENV HELLO world
		RUN rm /remove_me
		`

	// build and get the ID that we can use later for history comparison
	source := fakecontext.New(t, "", fakecontext.WithDockerfile(dockerfile))
	defer source.Close()

	name := strings.ToLower(t.Name())
	build.Do(ctx, t, apiClient, source, client.ImageBuildOptions{
		Remove:      true,
		ForceRemove: true,
		Tags:        []string{name},
	})

	inspect, err := apiClient.ImageInspect(ctx, name)
	assert.NilError(t, err)
	origID := inspect.ID

	// build with squash
	build.Do(ctx, t, apiClient, source, client.ImageBuildOptions{
		Remove:      true,
		ForceRemove: true,
		Squash:      true,
		Tags:        []string{name},
	})

	cid := container.Run(ctx, t, apiClient,
		container.WithImage(name),
		container.WithCmd("/bin/sh", "-c", "cat /hello"),
	)

	poll.WaitOn(t, container.IsStopped(ctx, apiClient, cid))
	reader, err := apiClient.ContainerLogs(ctx, cid, client.ContainerLogsOptions{
		ShowStdout: true,
	})
	assert.NilError(t, err)

	actualStdout := new(bytes.Buffer)
	actualStderr := io.Discard
	_, err = stdcopy.StdCopy(actualStdout, actualStderr, reader)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(strings.TrimSpace(actualStdout.String()), "hello\nworld"))

	container.Run(ctx, t, apiClient,
		container.WithImage(name),
		container.WithCmd("/bin/sh", "-c", "[ ! -f /remove_me ]"),
	)
	container.Run(ctx, t, apiClient,
		container.WithImage(name),
		container.WithCmd("/bin/sh", "-c", `[ "$(echo $HELLO)" = "world" ]`),
	)

	origHistory, err := apiClient.ImageHistory(ctx, origID)
	assert.NilError(t, err)
	testHistory, err := apiClient.ImageHistory(ctx, name)
	assert.NilError(t, err)

	inspect, err = apiClient.ImageInspect(ctx, name)
	assert.NilError(t, err)
	assert.Check(t, is.Len(testHistory.Items, len(origHistory.Items)+1))
	assert.Check(t, is.Len(inspect.RootFS.Layers, 2))
}
