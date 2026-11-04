package main

import (
	"context"
	"strings"
	"testing"

	"github.com/moby/moby/v2/integration-cli/cli"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/skip"
)

type DockerCLICommitSuite struct {
	ds *DockerSuite
}

func (s *DockerCLICommitSuite) TearDownTest(ctx context.Context, t *testing.T) {
	s.ds.TearDownTest(ctx, t)
}

func (s *DockerCLICommitSuite) OnTimeout(t *testing.T) {
	s.ds.OnTimeout(t)
}

func (s *DockerCLICommitSuite) TestCommitAfterContainerIsDone(c *testing.T) {
	skip.If(c, RuntimeIsWindowsContainerd(), "FIXME: Broken on Windows + containerd combination")
	cID := cli.DockerCmd(c, "run", "-d", "busybox", "echo", c.Name()).Combined()
	cID = strings.TrimSpace(cID)
	imageID := cli.DockerCmd(c, "commit", cID).Combined()
	imageID = strings.TrimSpace(imageID)
	cli.DockerCmd(c, "inspect", imageID)
}

func (s *DockerCLICommitSuite) TestCommitWithoutPause(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	cID := cli.DockerCmd(c, "run", "-dit", "busybox").Combined()
	cID = strings.TrimSpace(cID)
	imageID := cli.DockerCmd(c, "commit", "-p=false", cID).Combined()
	imageID = strings.TrimSpace(imageID)
	cli.DockerCmd(c, "inspect", imageID)
}

func (s *DockerCLICommitSuite) TestCommitNewFile(c *testing.T) {
	cli.DockerCmd(c, "run", "--name", "committer", "busybox", "/bin/sh", "-c", "echo koye > /foo")

	imageID := cli.DockerCmd(c, "commit", "committer").Stdout()
	imageID = strings.TrimSpace(imageID)

	out := cli.DockerCmd(c, "run", imageID, "cat", "/foo").Combined()
	actual := strings.TrimSpace(out)
	assert.Equal(c, actual, "koye")
}

func (s *DockerCLICommitSuite) TestCommitHardlink(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	firstOutput := cli.DockerCmd(c, "run", "-t", "--name", "hardlinks", "busybox", "sh", "-c", "touch file1 && ln file1 file2 && ls -di file1 file2").Combined()

	chunks := strings.Split(strings.TrimSpace(firstOutput), " ")
	inode := chunks[0]
	chunks = strings.SplitAfterN(strings.TrimSpace(firstOutput), " ", 2)
	assert.Assert(c, strings.Contains(chunks[1], chunks[0]), "Failed to create hardlink in a container. Expected to find %q in %q", inode, chunks[1:])
	imageID := cli.DockerCmd(c, "commit", "hardlinks", "hardlinks").Stdout()
	imageID = strings.TrimSpace(imageID)

	secondOutput := cli.DockerCmd(c, "run", "-t", imageID, "ls", "-di", "file1", "file2").Combined()

	chunks = strings.Split(strings.TrimSpace(secondOutput), " ")
	inode = chunks[0]
	chunks = strings.SplitAfterN(strings.TrimSpace(secondOutput), " ", 2)
	assert.Assert(c, strings.Contains(chunks[1], chunks[0]), "Failed to create hardlink in a container. Expected to find %q in %q", inode, chunks[1:])
}

func (s *DockerCLICommitSuite) TestCommitTTY(c *testing.T) {
	cli.DockerCmd(c, "run", "-t", "--name", "tty", "busybox", "/bin/ls")

	imageID := cli.DockerCmd(c, "commit", "tty", "ttytest").Stdout()
	imageID = strings.TrimSpace(imageID)

	cli.DockerCmd(c, "run", imageID, "/bin/ls")
}

func (s *DockerCLICommitSuite) TestCommitWithHostBindMount(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	cli.DockerCmd(c, "run", "--name", "bind-commit", "-v", "/dev/null:/winning", "busybox", "true")

	imageID := cli.DockerCmd(c, "commit", "bind-commit", "bindtest").Stdout()
	imageID = strings.TrimSpace(imageID)

	cli.DockerCmd(c, "run", imageID, "true")
}
