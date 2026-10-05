package main

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

type DockerCLIPullSuite struct {
	ds *DockerSuite
}

func (s *DockerCLIPullSuite) TearDownTest(ctx context.Context, t *testing.T) {
	s.ds.TearDownTest(ctx, t)
}

func (s *DockerCLIPullSuite) OnTimeout(t *testing.T) {
	s.ds.OnTimeout(t)
}

// TestPullFromCentralRegistry pulls an image from the central registry and verifies that the client
// prints all expected output.
func (s *DockerHubPullSuite) TestPullFromCentralRegistry(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	out, err := s.CmdWithError("pull", "hello-world")
	if err != nil && strings.Contains(err.Error(), "toomanyrequests") {
		c.Skipf("XFAIL: %s", err.Error())
	}
	assert.NilError(c, err)
	defer deleteImages("hello-world")

	assert.Assert(c, strings.Contains(out, "Using default tag: latest"), "expected the 'latest' tag to be automatically assumed")
	assert.Assert(c, strings.Contains(out, "Pulling from library/hello-world"), "expected the 'library/' prefix to be automatically assumed")
	assert.Assert(c, is.Contains(out, "Downloaded newer image for hello-world:latest"))

	matches := regexp.MustCompile(`Digest: (.+)\n`).FindAllStringSubmatch(out, -1)
	assert.Equal(c, len(matches), 1, "expected exactly one image digest in the output")
	assert.Equal(c, len(matches[0]), 2, "unexpected number of submatches for the digest")
	_, err = digest.Parse(matches[0][1])
	assert.NilError(c, err, "invalid digest %q in output", matches[0][1])

	// We should have a single entry in images.
	img := strings.TrimSpace(s.Cmd(c, "images"))
	splitImg := strings.Split(img, "\n")
	assert.Equal(c, len(splitImg), 2)
	match, _ := regexp.MatchString(`hello-world\s+latest.*?`, splitImg[1])
	assert.Assert(c, match, "invalid output for `docker images` (expected image and tag name)")
}

// TestPullFromCentralRegistryImplicitRefParts pulls an image from the central registry and verifies
// that pulling the same image with different combinations of implicit elements of the image
// reference (tag, repository, central registry url, ...) doesn't trigger a new pull nor leads to
// multiple images.
func (s *DockerHubPullSuite) TestPullFromCentralRegistryImplicitRefParts(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	_, err := s.CmdWithError("image", "pull", "hello-world")
	if err != nil && strings.Contains(err.Error(), "toomanyrequests") {
		c.Skipf("XFAIL: %s", err.Error())
	}

	s.Cmd(c, "tag", "hello-world", "hello-world-backup")
	defer func() {
		s.Cmd(c, "image", "rm", "-f", "hello-world", "hello-world-backup")
	}()

	for _, ref := range []string{
		"hello-world",
		"hello-world:latest",
		"library/hello-world",
		"library/hello-world:latest",
		"docker.io/library/hello-world",
		"index.docker.io/library/hello-world",
	} {
		out, err := s.CmdWithError("image", "pull", "hello-world")
		if err != nil && strings.Contains(err.Error(), "toomanyrequests") {
			c.Skipf("XFAIL: %s", err.Error())
		}

		s.Cmd(c, "image", "rm", ref)
		s.Cmd(c, "image", "tag", "hello-world-backup", "hello-world")
		assert.Assert(c, is.Contains(out, "Image is up to date for hello-world:latest"))
	}

	s.Cmd(c, "image", "rm", "hello-world-backup")

	// We should have a single entry in images.
	img := strings.TrimSpace(s.Cmd(c, "images"))
	splitImg := strings.Split(img, "\n")
	assert.Equal(c, len(splitImg), 2)
	match, _ := regexp.MatchString(`hello-world\s+latest.*?`, splitImg[1])
	assert.Assert(c, match, "invalid output for `docker images` (expected image and tag name)")
}

// TestPullScratchNotAllowed verifies that pulling 'scratch' is rejected.
func (s *DockerHubPullSuite) TestPullScratchNotAllowed(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	out, err := s.CmdWithError("pull", "scratch")
	assert.ErrorContains(c, err, "", "expected pull of scratch to fail")
	assert.Assert(c, is.Contains(out, "'scratch' is a reserved name"))
	assert.Assert(c, !strings.Contains(out, "Pulling repository scratch"))
}

// TestPullAllTagsFromCentralRegistry pulls using `all-tags` for a given image and verifies that it
// results in more images than a naked pull.
func (s *DockerHubPullSuite) TestPullAllTagsFromCentralRegistry(c *testing.T) {
	// See https://github.com/moby/moby/issues/46632
	skip.If(c, testEnv.UsingSnapshotter, "The image dockercore/engine-pull-all-test-fixture is a hand-made image that contains an error in the manifest, the size is reported as 424 but its real size is 524, containerd fails to pull it because it checks that the sizes reported are right")
	testRequires(c, DaemonIsLinux)
	_, err := s.CmdWithError("pull", "dockercore/engine-pull-all-test-fixture")
	if err != nil && strings.Contains(err.Error(), "toomanyrequests") {
		c.Skipf("XFAIL: %s", err.Error())
	}
	assert.NilError(c, err)
	outImageCmd := s.Cmd(c, "images", "dockercore/engine-pull-all-test-fixture")
	splitOutImageCmd := strings.Split(strings.TrimSpace(outImageCmd), "\n")
	assert.Equal(c, len(splitOutImageCmd), 2)

	s.Cmd(c, "pull", "--all-tags=true", "dockercore/engine-pull-all-test-fixture")
	outImageAllTagCmd := s.Cmd(c, "images", "dockercore/engine-pull-all-test-fixture")
	linesCount := strings.Count(outImageAllTagCmd, "\n")
	assert.Assert(c, linesCount > 2, "pulling all tags should provide more than two images, got %s", outImageAllTagCmd)

	// Verify that the line for 'dockercore/engine-pull-all-test-fixture:latest' is left unchanged.
	var latestLine string
	for line := range strings.SplitSeq(outImageAllTagCmd, "\n") {
		if strings.HasPrefix(line, "dockercore/engine-pull-all-test-fixture") && strings.Contains(line, "latest") {
			latestLine = line
			break
		}
	}
	assert.Assert(c, latestLine != "", "no entry for dockercore/engine-pull-all-test-fixture:latest found after pulling all tags")

	splitLatest := strings.Fields(latestLine)
	splitCurrent := strings.Fields(splitOutImageCmd[1])

	// Clear relative creation times, since these can easily change between
	// two invocations of "docker images". Without this, the test can fail
	// like this:
	// ... obtained []string = []string{"busybox", "latest", "d9551b4026f0", "27", "minutes", "ago", "1.113", "MB"}
	// ... expected []string = []string{"busybox", "latest", "d9551b4026f0", "26", "minutes", "ago", "1.113", "MB"}
	splitLatest[3] = ""
	splitLatest[4] = ""
	splitLatest[5] = ""
	splitCurrent[3] = ""
	splitCurrent[4] = ""
	splitCurrent[5] = ""

	assert.Assert(c, is.DeepEqual(splitLatest, splitCurrent), "dockercore/engine-pull-all-test-fixture:latest was changed after pulling all tags")
}

// TestPullClientDisconnect kills the client during a pull operation and verifies that the operation
// gets cancelled.
//
// Ref: docker/docker#15589
func (s *DockerHubPullSuite) TestPullClientDisconnect(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	const imgRepo = "hello-world:latest"

	pullCmd := s.MakeCmd("pull", imgRepo)
	stdout, err := pullCmd.StdoutPipe()
	assert.NilError(c, err)
	err = pullCmd.Start()
	assert.NilError(c, err)
	go pullCmd.Wait()

	// Cancel as soon as we get some output.
	buf := make([]byte, 10)
	_, err = stdout.Read(buf)
	assert.NilError(c, err)

	err = pullCmd.Process.Kill()
	assert.NilError(c, err)

	time.Sleep(2 * time.Second)
	_, err = s.CmdWithError("inspect", imgRepo)
	assert.ErrorContains(c, err, "", "image was pulled after client disconnected")
}

// Regression test for https://github.com/moby/moby/issues/26429
func (s *DockerCLIPullSuite) TestPullLinuxImageFailsOnWindows(c *testing.T) {
	testRequires(c, DaemonIsWindows, Network)
	_, _, err := dockerCmdWithError("pull", "ubuntu")

	assert.ErrorContains(c, err, "no matching manifest for windows")
}

// Regression test for https://github.com/moby/moby/issues/28892
func (s *DockerCLIPullSuite) TestPullWindowsImageFailsOnLinux(c *testing.T) {
	testRequires(c, DaemonIsLinux, Network)
	_, _, err := dockerCmdWithError("pull", "mcr.microsoft.com/windows/servercore:ltsc2022")

	assert.ErrorContains(c, err, "no matching manifest for linux")
}
