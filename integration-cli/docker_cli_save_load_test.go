package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/v2/integration-cli/cli"
	"github.com/moby/moby/v2/integration-cli/cli/build"
	"github.com/moby/moby/v2/internal/testutil/specialimage"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/icmd"
	"gotest.tools/v3/skip"
)

type DockerCLISaveLoadSuite struct {
	ds *DockerSuite
}

func (s *DockerCLISaveLoadSuite) TearDownTest(ctx context.Context, t *testing.T) {
	s.ds.TearDownTest(ctx, t)
}

func (s *DockerCLISaveLoadSuite) OnTimeout(t *testing.T) {
	s.ds.OnTimeout(t)
}

// save a repo using gz compression and try to load it using stdout
func (s *DockerCLISaveLoadSuite) TestSaveXzAndLoadRepoStdout(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	name := "test-save-xz-and-load-repo-stdout"
	cli.DockerCmd(c, "run", "--name", name, "busybox", "true")

	imgRepoName := "foobar-save-load-test-xz-gz"
	out := cli.DockerCmd(c, "commit", name, imgRepoName).Combined()

	cli.DockerCmd(c, "inspect", imgRepoName)

	repoTarball, err := RunCommandPipelineWithOutput(
		exec.Command(dockerBinary, "save", imgRepoName),
		exec.Command("xz", "-c"),
		exec.Command("gzip", "-c"))
	assert.NilError(c, err, "failed to save repo: %v %v", out, err)
	deleteImages(imgRepoName)

	icmd.RunCmd(icmd.Cmd{
		Command: []string{dockerBinary, "load"},
		Stdin:   strings.NewReader(repoTarball),
	}).Assert(c, icmd.Expected{
		ExitCode: 1,
	})

	after, _, err := dockerCmdWithError("inspect", imgRepoName)
	assert.ErrorContains(c, err, "", "the repo should not exist: %v", after)
}

// save a repo using xz+gz compression and try to load it using stdout
func (s *DockerCLISaveLoadSuite) TestSaveXzGzAndLoadRepoStdout(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	name := "test-save-xz-gz-and-load-repo-stdout"
	cli.DockerCmd(c, "run", "--name", name, "busybox", "true")

	repoName := "foobar-save-load-test-xz-gz"
	cli.DockerCmd(c, "commit", name, repoName)

	cli.DockerCmd(c, "inspect", repoName)

	out, err := RunCommandPipelineWithOutput(
		exec.Command(dockerBinary, "save", repoName),
		exec.Command("xz", "-c"),
		exec.Command("gzip", "-c"))
	assert.NilError(c, err, "failed to save repo: %v %v", out, err)

	deleteImages(repoName)

	icmd.RunCmd(icmd.Cmd{
		Command: []string{dockerBinary, "load"},
		Stdin:   strings.NewReader(out),
	}).Assert(c, icmd.Expected{
		ExitCode: 1,
	})

	after, _, err := dockerCmdWithError("inspect", repoName)
	assert.ErrorContains(c, err, "", "the repo should not exist: %v", after)
}

func (s *DockerCLISaveLoadSuite) TestSaveSingleTag(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	imgRepoName := "foobar-save-single-tag-test"
	cli.DockerCmd(c, "tag", "busybox:latest", fmt.Sprintf("%v:latest", imgRepoName))

	out := cli.DockerCmd(c, "images", "-q", "--no-trunc", imgRepoName).Stdout()
	cleanedImageID := strings.TrimSpace(out)

	filesFilter := fmt.Sprintf("(^manifest.json$|%v)", cleanedImageID)
	if testEnv.UsingSnapshotter() {
		filesFilter = fmt.Sprintf("(^index.json$|^manifest.json$|%v)", cleanedImageID)
	}
	out, err := RunCommandPipelineWithOutput(
		exec.Command(dockerBinary, "save", fmt.Sprintf("%v:latest", imgRepoName)),
		exec.Command("tar", "t"),
		exec.Command("grep", "-E", filesFilter))
	assert.NilError(c, err, "failed to save repo with image ID and index files: %s, %v", out, err)
}

func (s *DockerCLISaveLoadSuite) TestSaveImageId(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	emptyFSImage := loadSpecialImage(c, specialimage.EmptyFS)

	imgRepoName := "foobar-save-image-id-test"
	cli.DockerCmd(c, "tag", emptyFSImage, fmt.Sprintf("%v:latest", imgRepoName))

	out := cli.DockerCmd(c, "images", "-q", "--no-trunc", imgRepoName).Stdout()
	cleanedLongImageID := strings.TrimPrefix(strings.TrimSpace(out), "sha256:")

	out = cli.DockerCmd(c, "images", "-q", imgRepoName).Stdout()
	cleanedShortImageID := strings.TrimSpace(out)

	// Make sure IDs are not empty
	assert.Assert(c, cleanedLongImageID != "", "Id should not be empty.")
	assert.Assert(c, cleanedShortImageID != "", "Id should not be empty.")

	// TODO(thaJeztah): this fails with full image ID
	saveCmd := exec.Command(dockerBinary, "save", cleanedShortImageID)
	tarCmd := exec.Command("tar", "t")

	var err error
	tarCmd.Stdin, err = saveCmd.StdoutPipe()
	assert.NilError(c, err, "cannot set stdin pipe for tar: %v", err)
	tarStream, err := tarCmd.StdoutPipe()
	assert.NilError(c, err, "get tar output pipe: %v", err)

	assert.Assert(c, tarCmd.Start() == nil, "tar failed with error: %v", err)
	assert.Assert(c, saveCmd.Start() == nil, "docker save failed with error: %v", err)
	defer func() {
		saveCmd.Wait()
		tarCmd.Wait()
		cli.DockerCmd(c, "rmi", imgRepoName)
	}()

	res := icmd.RunCmd(icmd.Command("grep", "blobs/sha256/"+cleanedLongImageID), icmd.WithStdin(tarStream))
	out, err = res.Combined(), res.Error
	assert.NilError(c, err, "failed to save repo with image ID: %s, %v", out, err)
}

// save a repo and try to load it using flags
func (s *DockerCLISaveLoadSuite) TestSaveAndLoadRepoFlags(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	const name = "test-save-and-load-repo-flags"
	cli.DockerCmd(c, "run", "--name", name, "busybox", "true")

	const imgRepoName = "foobar-save-load-test"

	deleteImages(imgRepoName)
	cli.DockerCmd(c, "commit", name, imgRepoName)

	beforeStr := cli.DockerCmd(c, "inspect", imgRepoName).Stdout()

	out, err := RunCommandPipelineWithOutput(
		exec.Command(dockerBinary, "save", imgRepoName),
		exec.Command(dockerBinary, "load"))
	assert.NilError(c, err, "failed to save and load repo: %s, %v", out, err)

	afterStr := cli.DockerCmd(c, "inspect", imgRepoName).Stdout()

	var before, after []image.InspectResponse
	err = json.Unmarshal([]byte(beforeStr), &before)
	assert.NilError(c, err, "failed to parse inspect 'before' output")
	err = json.Unmarshal([]byte(afterStr), &after)
	assert.NilError(c, err, "failed to parse inspect 'after' output")

	assert.Assert(c, is.Len(before, 1))
	assert.Assert(c, is.Len(after, 1))

	if testEnv.UsingSnapshotter() {
		// Ignore LastTagTime difference with c8d.
		// It is not stored in the image archive, but in the imageStore
		// which is a graphdrivers implementation detail.
		//
		// It works because we load the image into the same daemon which saved
		// the image. It would still fail with the graphdrivers if the image
		// was loaded into a different daemon (which should be the case in a
		// real-world scenario).
		before[0].Metadata.LastTagTime = after[0].Metadata.LastTagTime
	}

	assert.Check(c, is.DeepEqual(before, after), "inspect is not the same after a save / load")
}

func (s *DockerCLISaveLoadSuite) TestSaveWithNoExistImage(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	imgName := "foobar-non-existing-image"

	out, _, err := dockerCmdWithError("save", "-o", "test-img.tar", imgName)
	assert.ErrorContains(c, err, "", "save image should fail for non-existing image")
	assert.Assert(c, is.Contains(out, "No such image: "+imgName))
}

func (s *DockerCLISaveLoadSuite) TestSaveMultipleNames(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	emptyFSImage := loadSpecialImage(c, specialimage.EmptyFS)

	const imgRepoName = "foobar-save-multi-name-test"

	oneTag := fmt.Sprintf("%v-one:latest", imgRepoName)
	twoTag := fmt.Sprintf("%v-two:latest", imgRepoName)

	cli.DockerCmd(c, "tag", emptyFSImage, oneTag)
	cli.DockerCmd(c, "tag", emptyFSImage, twoTag)

	out, err := RunCommandPipelineWithOutput(
		exec.Command(dockerBinary, "save", strings.TrimSuffix(oneTag, ":latest"), twoTag),
		exec.Command("tar", "xO", "index.json"),
	)
	assert.NilError(c, err, "failed to save multiple repos: %s, %v", out, err)

	assert.Check(c, is.Contains(out, oneTag))
	assert.Check(c, is.Contains(out, twoTag))
}

// Test loading a weird image where one of the layers is of zero size.
// The layer.tar file is actually zero bytes, no padding or anything else.
// See issue: https://github.com/moby/moby/issues/18170
func (s *DockerCLISaveLoadSuite) TestLoadZeroSizeLayer(c *testing.T) {
	// The "testdata/emptyLayer.tar" archive uses the legacy format, which fails
	// to import because there's no "manifest.json" included.
	//
	// This test was written for a situation where the user created an archive
	// that did contain *entries* for (layer) files, but those files were
	// deliberately empty as an optimization step (to use content already
	// present);
	//
	// - https://github.com/moby/moby/issues/18170
	// - https://github.com/moby/moby/pull/50324#issuecomment-3033482915
	//
	// Keeping the test for now as a reminder that  we may want to verify "empty
	// files in archive" cases, but we can decide to consider this an invalid
	// situation (either files must be included and non-empty, or omitted).
	c.Skip("test is using an archive using the legacy format")

	// TODO(vvoland): Create an OCI image with 0 bytes layer.
	skip.If(c, testEnv.UsingSnapshotter(), "input archive is not OCI compatible")

	// this will definitely not work if using remote daemon
	// very weird test
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon)

	cli.DockerCmd(c, "load", "-i", "testdata/emptyLayer.tar")
}

func (s *DockerCLISaveLoadSuite) TestSaveLoadParents(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	skip.If(c, testEnv.UsingSnapshotter(), "Parent image property is not supported with containerd")

	makeImage := func(from string, addfile string) string {
		id := cli.DockerCmd(c, "run", "-d", from, "touch", addfile).Stdout()
		id = strings.TrimSpace(id)

		imageID := cli.DockerCmd(c, "commit", id).Stdout()
		imageID = strings.TrimSpace(imageID)

		cli.DockerCmd(c, "rm", "-f", id)
		return imageID
	}

	idFoo := makeImage("busybox", "foo")
	idBar := makeImage(idFoo, "bar")

	tmpDir, err := os.MkdirTemp("", "save-load-parents")
	assert.NilError(c, err)
	defer os.RemoveAll(tmpDir)

	c.Log("tmpdir", tmpDir)

	outfile := filepath.Join(tmpDir, "out.tar")

	cli.DockerCmd(c, "save", "-o", outfile, idBar, idFoo)
	cli.DockerCmd(c, "rmi", idBar)
	cli.DockerCmd(c, "load", "-i", outfile)

	cmd := icmd.Command(dockerBinary, "inspect", "-f", "{{.Parent}}", idBar)
	result := icmd.RunCmd(cmd, icmd.WithEnv("DOCKER_API_VERSION=1.51"))
	assert.Equal(c, result.Stdout(), idFoo)

	cmd = icmd.Command(dockerBinary, "inspect", "-f", "{{.Parent}}", idFoo)
	result = icmd.RunCmd(cmd, icmd.WithEnv("DOCKER_API_VERSION=1.51"))
	assert.Equal(c, result.Stdout(), "")
}

func (s *DockerCLISaveLoadSuite) TestSaveLoadNoTag(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	name := "saveloadnotag"

	cli.BuildCmd(c, name, build.WithDockerfile("FROM busybox\nENV foo=bar"))
	id := inspectField(c, name, "Id")

	// Test to make sure that save w/o name just shows imageID during load
	out, err := RunCommandPipelineWithOutput(
		exec.Command(dockerBinary, "save", id),
		exec.Command(dockerBinary, "load"))
	assert.NilError(c, err, "failed to save and load repo: %s, %v", out, err)

	// Should not show 'name' but should show the image ID during the load
	assert.Assert(c, !strings.Contains(out, "Loaded image: "))
	assert.Assert(c, is.Contains(out, "Loaded image ID:"))
	assert.Assert(c, is.Contains(out, id))
	// Test to make sure that save by name shows that name during load
	out, err = RunCommandPipelineWithOutput(
		exec.Command(dockerBinary, "save", name),
		exec.Command(dockerBinary, "load"))
	assert.NilError(c, err, "failed to save and load repo: %s, %v", out, err)

	assert.Assert(c, is.Contains(out, "Loaded image: "+name+":latest"))
	assert.Assert(c, !strings.Contains(out, "Loaded image ID:"))
}
