package main

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-check/check"
	"github.com/opencontainers/go-digest"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
)

// TestPullFromCentralRegistry pulls an image from the central registry and verifies that the client
// prints all expected output.
func (s *DockerHubPullSuite) TestPullFromCentralRegistry(c *check.C) {
	testRequires(c, DaemonIsLinux)
	out := s.Cmd(c, "pull", "hello-world")
	defer deleteImages("hello-world")

	assert.Assert(c, strings.Contains(out, "Using default tag: latest"), "expected the 'latest' tag to be automatically assumed")
	assert.Assert(c, strings.Contains(out, "Pulling from library/hello-world"), "expected the 'library/' prefix to be automatically assumed")
	assert.Assert(c, strings.Contains(out, "Downloaded newer image for hello-world:latest"))

	matches := regexp.MustCompile(`Digest: (.+)\n`).FindAllStringSubmatch(out, -1)
	assert.Equal(c, len(matches), 1, "expected exactly one image digest in the output")
	assert.Equal(c, len(matches[0]), 2, "unexpected number of submatches for the digest")
	_, err := digest.Parse(matches[0][1])
	assert.NilError(c, err, "invalid digest %q in output", matches[0][1])

	// We should have a single entry in images.
	img := strings.TrimSpace(s.Cmd(c, "images"))
	splitImg := strings.Split(img, "\n")
	assert.Equal(c, len(splitImg), 2)
	match, _ := regexp.MatchString(`hello-world\s+latest.*?`, splitImg[1])
	assert.Assert(c, match, "invalid output for `docker images` (expected image and tag name)")
}

// TestPullNonExistingImage pulls non-existing images from the central registry, with different
// combinations of implicit tag and library prefix.
func (s *DockerHubPullSuite) TestPullNonExistingImage(c *check.C) {
	testRequires(c, DaemonIsLinux)

	type entry struct {
		repo  string
		alias string
		tag   string
	}

	entries := []entry{
		{"asdfasdf", "asdfasdf", "foobar"},
		{"asdfasdf", "library/asdfasdf", "foobar"},
		{"asdfasdf", "asdfasdf", ""},
		{"asdfasdf", "asdfasdf", "latest"},
		{"asdfasdf", "library/asdfasdf", ""},
		{"asdfasdf", "library/asdfasdf", "latest"},
	}

	// The option field indicates "-a" or not.
	type record struct {
		e      entry
		option string
		out    string
		err    error
	}

	// Execute 'docker pull' in parallel, pass results (out, err) and
	// necessary information ("-a" or not, and the image name) to channel.
	var group sync.WaitGroup
	recordChan := make(chan record, len(entries)*2)
	for _, e := range entries {
		group.Add(1)
		go func(e entry) {
			defer group.Done()
			repoName := e.alias
			if e.tag != "" {
				repoName += ":" + e.tag
			}
			out, err := s.CmdWithError("pull", repoName)
			recordChan <- record{e, "", out, err}
		}(e)
		if e.tag == "" {
			// pull -a on a nonexistent registry should fall back as well
			group.Add(1)
			go func(e entry) {
				defer group.Done()
				out, err := s.CmdWithError("pull", "-a", e.alias)
				recordChan <- record{e, "-a", out, err}
			}(e)
		}
	}

	// Wait for completion
	group.Wait()
	close(recordChan)

	// Process the results (out, err).
	for record := range recordChan {
		if len(record.option) == 0 {
			assert.ErrorContains(c, record.err, "", "expected non-zero exit status when pulling non-existing image: %s", record.out)
			assert.Assert(c, strings.Contains(record.out, fmt.Sprintf("pull access denied for %s, repository does not exist or may require 'docker login'", record.e.repo)), "expected image not found error messages")
		} else {
			// pull -a on a nonexistent registry should fall back as well
			assert.ErrorContains(c, record.err, "", "expected non-zero exit status when pulling non-existing image: %s", record.out)
			assert.Assert(c, strings.Contains(record.out, fmt.Sprintf("pull access denied for %s, repository does not exist or may require 'docker login'", record.e.repo)), "expected image not found error messages")
			assert.Assert(c, !strings.Contains(record.out, "unauthorized"), `message should not contain "unauthorized"`)
		}
	}

}

// TestPullFromCentralRegistryImplicitRefParts pulls an image from the central registry and verifies
// that pulling the same image with different combinations of implicit elements of the image
// reference (tag, repository, central registry url, ...) doesn't trigger a new pull nor leads to
// multiple images.
func (s *DockerHubPullSuite) TestPullFromCentralRegistryImplicitRefParts(c *check.C) {
	testRequires(c, DaemonIsLinux)

	// Pull hello-world from v2
	pullFromV2 := func(ref string) (int, string) {
		out := s.Cmd(c, "pull", "hello-world")
		v1Retries := 0
		for strings.Contains(out, "this image was pulled from a legacy registry") {
			// Some network errors may cause fallbacks to the v1
			// protocol, which would violate the test's assumption
			// that it will get the same images. To make the test
			// more robust against these network glitches, allow a
			// few retries if we end up with a v1 pull.

			if v1Retries > 2 {
				c.Fatalf("too many v1 fallback incidents when pulling %s", ref)
			}

			s.Cmd(c, "rmi", ref)
			out = s.Cmd(c, "pull", ref)

			v1Retries++
		}

		return v1Retries, out
	}

	pullFromV2("hello-world")
	defer deleteImages("hello-world")

	s.Cmd(c, "tag", "hello-world", "hello-world-backup")

	for _, ref := range []string{
		"hello-world",
		"hello-world:latest",
		"library/hello-world",
		"library/hello-world:latest",
		"docker.io/library/hello-world",
		"index.docker.io/library/hello-world",
	} {
		var out string
		for {
			var v1Retries int
			v1Retries, out = pullFromV2(ref)

			// Keep repeating the test case until we don't hit a v1
			// fallback case. We won't get the right "Image is up
			// to date" message if the local image was replaced
			// with one pulled from v1.
			if v1Retries == 0 {
				break
			}
			s.Cmd(c, "rmi", ref)
			s.Cmd(c, "tag", "hello-world-backup", "hello-world")
		}
		assert.Assert(c, strings.Contains(out, "Image is up to date for hello-world:latest"))
	}

	s.Cmd(c, "rmi", "hello-world-backup")

	// We should have a single entry in images.
	img := strings.TrimSpace(s.Cmd(c, "images"))
	splitImg := strings.Split(img, "\n")
	assert.Equal(c, len(splitImg), 2)
	match, _ := regexp.MatchString(`hello-world\s+latest.*?`, splitImg[1])
	assert.Assert(c, match, "invalid output for `docker images` (expected image and tag name)")
}

// TestPullScratchNotAllowed verifies that pulling 'scratch' is rejected.
func (s *DockerHubPullSuite) TestPullScratchNotAllowed(c *check.C) {
	testRequires(c, DaemonIsLinux)
	out, err := s.CmdWithError("pull", "scratch")
	assert.ErrorContains(c, err, "", "expected pull of scratch to fail")
	assert.Assert(c, strings.Contains(out, "'scratch' is a reserved name"))
	assert.Assert(c, !strings.Contains(out, "Pulling repository scratch"))
}

// TestPullAllTagsFromCentralRegistry pulls using `all-tags` for a given image and verifies that it
// results in more images than a naked pull.
func (s *DockerHubPullSuite) TestPullAllTagsFromCentralRegistry(c *check.C) {
	testRequires(c, DaemonIsLinux)
	s.Cmd(c, "pull", "dockercore/engine-pull-all-test-fixture")
	outImageCmd := s.Cmd(c, "images", "dockercore/engine-pull-all-test-fixture")
	splitOutImageCmd := strings.Split(strings.TrimSpace(outImageCmd), "\n")
	assert.Equal(c, len(splitOutImageCmd), 2)

	s.Cmd(c, "pull", "--all-tags=true", "dockercore/engine-pull-all-test-fixture")
	outImageAllTagCmd := s.Cmd(c, "images", "dockercore/engine-pull-all-test-fixture")
	linesCount := strings.Count(outImageAllTagCmd, "\n")
	assert.Assert(c, linesCount > 2, "pulling all tags should provide more than two images, got %s", outImageAllTagCmd)

	// Verify that the line for 'dockercore/engine-pull-all-test-fixture:latest' is left unchanged.
	var latestLine string
	for _, line := range strings.Split(outImageAllTagCmd, "\n") {
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
func (s *DockerHubPullSuite) TestPullClientDisconnect(c *check.C) {
	testRequires(c, DaemonIsLinux)
	repoName := "hello-world:latest"

	pullCmd := s.MakeCmd("pull", repoName)
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
	_, err = s.CmdWithError("inspect", repoName)
	assert.ErrorContains(c, err, "", "image was pulled after client disconnected")
}

// Regression test for https://github.com/docker/docker/issues/26429
func (s *DockerSuite) TestPullLinuxImageFailsOnWindows(c *check.C) {
	testRequires(c, DaemonIsWindows, Network)
	_, _, err := dockerCmdWithError("pull", "ubuntu")
	assert.ErrorContains(c, err, "no matching manifest for windows")
}

// Regression test for https://github.com/docker/docker/issues/28892
func (s *DockerSuite) TestPullWindowsImageFailsOnLinux(c *check.C) {
	testRequires(c, DaemonIsLinux, Network)
	_, _, err := dockerCmdWithError("pull", "mcr.microsoft.com/windows/servercore:ltsc2019")
	assert.ErrorContains(c, err, "no matching manifest for linux")
}
