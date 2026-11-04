package build

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/pkg/stdcopy"
	buildtypes "github.com/moby/moby/api/types/build"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/jsonmessage"
	"github.com/moby/moby/v2/integration/internal/build"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"github.com/moby/moby/v2/internal/testutil/fakecontext"
	"github.com/moby/moby/v2/internal/testutil/fixtures/load"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

// Implements a test for https://github.com/moby/moby/issues/41723
// Images built in a user-namespaced daemon should have capabilities serialised in
// VFS_CAP_REVISION_2 (no user-namespace root uid) format rather than V3 (that includes
// the root uid).
func TestBuildUserNamespaceValidateCapabilitiesAreV2(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, testEnv.IsRemoteDaemon())
	skip.If(t, !testEnv.IsUserNamespaceInKernel())
	skip.If(t, testEnv.IsRootless())
	t.Parallel()

	ctx := testutil.StartSpan(baseContext, t)

	const imageTag = "capabilities:1.0"

	tmpDir := t.TempDir()

	dUserRemap := daemon.New(t, daemon.WithUserNsRemap("default"))
	dUserRemap.Start(t)
	clientUserRemap := dUserRemap.NewClientT(t)
	defer clientUserRemap.Close()

	err := load.FrozenImagesLinux(ctx, clientUserRemap, "debian:trixie-slim")
	assert.NilError(t, err)

	dUserRemapRunning := true
	defer func() {
		if dUserRemapRunning {
			dUserRemap.Stop(t)
			dUserRemap.Cleanup(t)
		}
	}()

	dockerfile := `
		FROM debian:trixie-slim
		RUN apt-get update && apt-get install -y libcap2-bin --no-install-recommends
		RUN setcap CAP_NET_BIND_SERVICE=+eip /bin/sleep
	`

	source := fakecontext.New(t, "", fakecontext.WithDockerfile(dockerfile))
	defer source.Close()

	resp, err := clientUserRemap.ImageBuild(ctx, source.AsTarReader(t), client.ImageBuildOptions{
		Tags: []string{imageTag},
	})
	assert.NilError(t, err)
	defer resp.Body.Close()

	buf := bytes.NewBuffer(nil)
	err = jsonmessage.DisplayStream(resp.Body, buf)
	assert.NilError(t, err)

	reader, err := clientUserRemap.ImageSave(ctx, []string{imageTag})
	assert.NilError(t, err, "failed to download capabilities image")
	defer func() { _ = reader.Close() }()

	tar, err := os.Create(filepath.Join(tmpDir, "image.tar"))
	assert.NilError(t, err, "failed to create image tar file")
	defer tar.Close()

	_, err = io.Copy(tar, reader)
	assert.NilError(t, err, "failed to write image tar file")

	dUserRemap.Stop(t)
	dUserRemap.Cleanup(t)
	dUserRemapRunning = false

	dNoUserRemap := daemon.New(t)
	dNoUserRemap.Start(t)
	defer func() {
		dNoUserRemap.Stop(t)
		dNoUserRemap.Cleanup(t)
	}()

	clientNoUserRemap := dNoUserRemap.NewClientT(t)
	defer clientNoUserRemap.Close()

	tarFile, err := os.Open(tmpDir + "/image.tar")
	assert.NilError(t, err, "failed to open image tar file")
	defer tarFile.Close()

	tarReader := bufio.NewReader(tarFile)
	loadResp, err := clientNoUserRemap.ImageLoad(ctx, tarReader)
	assert.NilError(t, err, "failed to load image tar file")
	defer loadResp.Close()
	var buf2 bytes.Buffer
	err = jsonmessage.DisplayStream(loadResp, &buf2)
	assert.NilError(t, err)

	cid := container.Run(ctx, t, clientNoUserRemap,
		container.WithImage(imageTag),
		container.WithCmd("/sbin/getcap", "-n", "/bin/sleep"),
	)

	poll.WaitOn(t, container.IsStopped(ctx, clientNoUserRemap, cid))
	logReader, err := clientNoUserRemap.ContainerLogs(ctx, cid, client.ContainerLogsOptions{
		ShowStdout: true,
	})
	assert.NilError(t, err)
	defer logReader.Close()

	actualStdout := new(bytes.Buffer)
	actualStderr := io.Discard
	_, err = stdcopy.StdCopy(actualStdout, actualStderr, logReader)
	assert.NilError(t, err)
	if strings.TrimSpace(actualStdout.String()) != "/bin/sleep cap_net_bind_service=eip" {
		t.Fatalf("run produced invalid output: %q, expected %q", actualStdout.String(), "/bin/sleep cap_net_bind_service=eip")
	}
}

func TestBuildUserNamespaceRemap(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, testEnv.IsRootless())
	skip.If(t, testEnv.UsingSnapshotter(), "TODO: Broken with containerd")

	ctx := testutil.StartSpan(baseContext, t)

	d := daemon.New(t, daemon.WithUserNsRemap("default"))
	d.Start(t)
	defer func() {
		d.Stop(t)
		d.Cleanup(t)
	}()

	apiClient := d.NewClientT(t)
	defer apiClient.Close()

	_, err := apiClient.Info(ctx, client.InfoOptions{})
	assert.NilError(t, err)

	err = load.FrozenImagesLinux(ctx, apiClient, "busybox:latest")
	assert.NilError(t, err)

	const dockerfile = `FROM busybox
RUN echo "hello world"
`

	source := fakecontext.New(t, "", fakecontext.WithDockerfile(dockerfile))
	defer source.Close()

	assert.Check(t, build.Do(ctx, t, apiClient, source, client.ImageBuildOptions{
		Version: buildtypes.BuilderBuildKit,
	}) != "")
}
