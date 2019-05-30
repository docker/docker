package main

import (
	"fmt"
	"strings"

	"github.com/go-check/check"
	"gotest.tools/assert"
)

// search for repos named  "registry" on the central registry
func (s *DockerSuite) TestSearchOnCentralRegistry(c *check.C) {
	testRequires(c, Network, DaemonIsLinux)

	out, _ := dockerCmd(c, "search", "busybox")
	assert.Assert(c, strings.Contains(out, "Busybox base image."), "couldn't find any repository named (or containing) 'Busybox base image.'")
}

func (s *DockerSuite) TestSearchStarsOptionWithWrongParameter(c *check.C) {
	out, _, err := dockerCmdWithError("search", "--filter", "stars=a", "busybox")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, strings.Contains(out, "Invalid filter"), "couldn't find the invalid filter warning")

	out, _, err = dockerCmdWithError("search", "-f", "stars=a", "busybox")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, strings.Contains(out, "Invalid filter"), "couldn't find the invalid filter warning")

	out, _, err = dockerCmdWithError("search", "-f", "is-automated=a", "busybox")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, strings.Contains(out, "Invalid filter"), "couldn't find the invalid filter warning")

	out, _, err = dockerCmdWithError("search", "-f", "is-official=a", "busybox")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, strings.Contains(out, "Invalid filter"), "couldn't find the invalid filter warning")

	// -s --stars deprecated since Docker 1.13
	out, _, err = dockerCmdWithError("search", "--stars=a", "busybox")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, strings.Contains(out, "invalid syntax"), "couldn't find the invalid value warning")

	// -s --stars deprecated since Docker 1.13
	out, _, err = dockerCmdWithError("search", "-s=-1", "busybox")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, strings.Contains(out, "invalid syntax"), "couldn't find the invalid value warning")
}

// search for repos which start with "ubuntu-" on the central registry
func (s *DockerSuite) TestSearchOnCentralRegistryWithDash(c *check.C) {
	testRequires(c, Network, DaemonIsLinux)

	dockerCmd(c, "search", "ubuntu-")
}

// test case for #23055
func (s *DockerSuite) TestSearchWithLimit(c *check.C) {
	testRequires(c, Network, DaemonIsLinux)

	limit := 10
	out, _, err := dockerCmdWithError("search", fmt.Sprintf("--limit=%d", limit), "docker")
	assert.NilError(c, err)
	outSlice := strings.Split(out, "\n")
	assert.Equal(c, len(outSlice), limit+2) // 1 header, 1 carriage return

	limit = 50
	out, _, err = dockerCmdWithError("search", fmt.Sprintf("--limit=%d", limit), "docker")
	assert.NilError(c, err)
	outSlice = strings.Split(out, "\n")
	assert.Equal(c, len(outSlice), limit+2) // 1 header, 1 carriage return

	limit = 100
	out, _, err = dockerCmdWithError("search", fmt.Sprintf("--limit=%d", limit), "docker")
	assert.NilError(c, err)
	outSlice = strings.Split(out, "\n")
	assert.Equal(c, len(outSlice), limit+2) // 1 header, 1 carriage return

	limit = 0
	_, _, err = dockerCmdWithError("search", fmt.Sprintf("--limit=%d", limit), "docker")
	assert.ErrorContains(c, err, "")

	limit = 200
	_, _, err = dockerCmdWithError("search", fmt.Sprintf("--limit=%d", limit), "docker")
	assert.ErrorContains(c, err, "")
}
