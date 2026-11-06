package image

import (
	"strings"
	"testing"

	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

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
	t.Cleanup(func() {
		dUserRemap.Stop(t)
		dUserRemap.Cleanup(t)
	})
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
