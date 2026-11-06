package main

import (
	"context"
	"strings"
	"testing"

	"github.com/moby/moby/v2/integration-cli/cli"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
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
	imageID := cli.DockerCmd(c, "commit", "--no-pause", cID).Combined()
	imageID = strings.TrimSpace(imageID)
	cli.DockerCmd(c, "inspect", imageID)
}

// TestCommitPausedContainer tests that a paused container is not unpaused after being committed
func (s *DockerCLICommitSuite) TestCommitPausedContainer(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	cID := cli.DockerCmd(c, "run", "-dit", "busybox").Combined()
	cID = strings.TrimSpace(cID)
	cli.DockerCmd(c, "pause", cID)

	imageID := cli.DockerCmd(c, "commit", cID).Combined()
	imageID = strings.TrimSpace(imageID)
	cli.DockerCmd(c, "inspect", imageID)

	// commit should not unpause a paused container
	out := inspectField(c, cID, "State.Paused")
	assert.Assert(c, is.Contains(out, "true"))
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

func (s *DockerCLICommitSuite) TestCommitChange(c *testing.T) {
	cli.DockerCmd(c, "run", "--name", "test", "busybox", "true")

	imageID := cli.DockerCmd(c, "commit",
		"--change", `EXPOSE 8080`,
		"--change", `ENV DEBUG true`,
		"--change", `ENV test 1`,
		"--change", `ENV PATH /foo`,
		"--change", `LABEL foo bar`,
		"--change", `CMD ["/bin/sh"]`,
		"--change", `WORKDIR /opt`,
		"--change", `ENTRYPOINT ["/bin/sh"]`,
		"--change", `USER testuser`,
		"--change", `VOLUME /var/lib/docker`,
		"--change", `ONBUILD /usr/local/bin/python-build --dir /app/src`,
		"test", "test-commit",
	).Stdout()
	imageID = strings.TrimSpace(imageID)

	expectedEnv := "[DEBUG=true test=1 PATH=/foo]"
	if testEnv.DaemonInfo.OSType != "windows" {
		// The ordering here is due to `PATH` being overridden from the container's
		// ENV.  On windows, the container doesn't have a `PATH` ENV variable so
		// the ordering is the same as the cli.
		expectedEnv = "[PATH=/foo DEBUG=true test=1]"
	}

	prefix, slash := getPrefixAndSlashFromDaemonPlatform()
	prefix = strings.ToUpper(prefix) // Force C: as that's how WORKDIR is normalized on Windows
	expected := map[string]string{
		"Config.ExposedPorts": "map[8080/tcp:{}]",
		"Config.Env":          expectedEnv,
		"Config.Labels":       "map[foo:bar]",
		"Config.Cmd":          "[/bin/sh]",
		"Config.WorkingDir":   prefix + slash + "opt",
		"Config.Entrypoint":   "[/bin/sh]",
		"Config.User":         "testuser",
		"Config.Volumes":      "map[/var/lib/docker:{}]",
		"Config.OnBuild":      "[/usr/local/bin/python-build --dir /app/src]",
	}

	for conf, value := range expected {
		res := inspectField(c, imageID, conf)
		if res != value {
			c.Errorf("%s('%s'), expected %s", conf, res, value)
		}
	}
}

func (s *DockerCLICommitSuite) TestCommitChangeLabels(c *testing.T) {
	cli.DockerCmd(c, "run", "--name", "test", "--label", "some=label", "busybox", "true")

	imageID := cli.DockerCmd(c, "commit", "--change", "LABEL some=label2", "test", "test-commit").Stdout()
	imageID = strings.TrimSpace(imageID)

	assert.Equal(c, inspectField(c, imageID, "Config.Labels"), "map[some:label2]")
	// check that container labels didn't change
	assert.Equal(c, inspectField(c, "test", "Config.Labels"), "map[some:label]")
}
