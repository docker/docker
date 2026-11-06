package main

import (
	"context"
	"strings"
	"testing"

	"github.com/moby/moby/v2/integration-cli/cli"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

// GH14530. Validates combinations of --net= with other options

// stringCheckPS is how the output of PS starts in order to validate that
// the command executed in a container did really run PS correctly.
const stringCheckPS = "PID   USER"

type DockerCLINetmodeSuite struct {
	ds *DockerSuite
}

func (s *DockerCLINetmodeSuite) TearDownTest(ctx context.Context, t *testing.T) {
	s.ds.TearDownTest(ctx, t)
}

func (s *DockerCLINetmodeSuite) OnTimeout(t *testing.T) {
	s.ds.OnTimeout(t)
}

// DockerCmdWithFail executes a docker command that is supposed to fail and returns
// the output. If the command returns a Nil error, it will fail and stop the tests.
func dockerCmdWithFail(t *testing.T, args ...string) string {
	t.Helper()
	out, _, err := dockerCmdWithError(args...)
	assert.Assert(t, err != nil, "%v", out)
	return out
}

func (s *DockerCLINetmodeSuite) TestNetHostnameWithNetHost(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace)

	out := cli.DockerCmd(c, "run", "--net=host", "busybox", "ps").Stdout()
	assert.Assert(c, is.Contains(out, stringCheckPS))
}

func (s *DockerCLINetmodeSuite) TestNetHostname(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	out := cli.DockerCmd(c, "run", "-h=name", "busybox", "ps").Stdout()
	assert.Assert(c, is.Contains(out, stringCheckPS))
	out = cli.DockerCmd(c, "run", "-h=name", "--net=bridge", "busybox", "ps").Stdout()
	assert.Assert(c, is.Contains(out, stringCheckPS))
	out = cli.DockerCmd(c, "run", "-h=name", "--net=none", "busybox", "ps").Stdout()
	assert.Assert(c, is.Contains(out, stringCheckPS))
	out = dockerCmdWithFail(c, "run", "-h=name", "--net=container:other", "busybox", "ps")
	assert.Assert(c, is.Contains(out, "conflicting options: hostname and the network mode"))
	out = dockerCmdWithFail(c, "run", "--net=container", "busybox", "ps")
	assert.Assert(c, is.Contains(out, "invalid container format container:<name|id>"))
	out = dockerCmdWithFail(c, "run", "--net=weird", "busybox", "ps")
	assert.Assert(c, is.Contains(strings.ToLower(out), "not found"))
}

func (s *DockerCLINetmodeSuite) TestConflictContainerNetworkAndLinks(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	out := dockerCmdWithFail(c, "run", "--net=container:other", "--link=zip:zap", "busybox", "ps")
	oneOf := []string{
		"conflicting options: container type network can't be used with links", // current CLI, daemon-side validation
		"links are only supported for user-defined networks",                   // older CLI, client-side validation
	}
	if !strings.Contains(out, oneOf[0]) && !strings.Contains(out, oneOf[1]) {
		c.Errorf("expected one of %v: %s", oneOf, out)
	}
}

func (s *DockerCLINetmodeSuite) TestConflictContainerNetworkHostAndLinks(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace)

	out := dockerCmdWithFail(c, "run", "--net=host", "--link=zip:zap", "busybox", "ps")
	oneOf := []string{
		"conflicting options: host type networking can't be used with links", // current CLI, daemon-side validation
		"links are only supported for user-defined networks",                 // older CLI, client-side validation
	}
	if !strings.Contains(out, oneOf[0]) && !strings.Contains(out, oneOf[1]) {
		c.Errorf("expected one of %v: %s", oneOf, out)
	}
}

func (s *DockerCLINetmodeSuite) TestConflictNetworkModeNetHostAndOptions(c *testing.T) {
	c.Skip("FIXME(thaJeztah): no daemon-side validation for this case!")
	testRequires(c, DaemonIsLinux, NotUserNamespace)

	// This doesn't produce an error:
	// 	docker run --rm --net=host --mac-address=92:d0:c6:0a:29:33 busybox
	out := dockerCmdWithFail(c, "run", "--net=host", "--mac-address=92:d0:c6:0a:29:33", "busybox", "ps")
	assert.Assert(c, is.Contains(out, "conflicting options: mac-address and the network mode"))
}

func (s *DockerCLINetmodeSuite) TestConflictNetworkModeAndOptions(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	out := dockerCmdWithFail(c, "run", "--net=container:other", "--dns=8.8.8.8", "busybox", "ps")
	assert.Assert(c, is.Contains(out, "conflicting options: dns and the network mode"))
	out = dockerCmdWithFail(c, "run", "--net=container:other", "--add-host=name:8.8.8.8", "busybox", "ps")
	assert.Assert(c, is.Contains(out, "conflicting options: custom host-to-IP mapping and the network mode"))
	out = dockerCmdWithFail(c, "run", "--net=container:other", "-P", "busybox", "ps")
	assert.Assert(c, is.Contains(out, "conflicting options: port publishing and the container type network mode"))
	out = dockerCmdWithFail(c, "run", "--net=container:other", "-p", "8080", "busybox", "ps")
	assert.Assert(c, is.Contains(out, "conflicting options: port publishing and the container type network mode"))
	out = dockerCmdWithFail(c, "run", "--net=container:other", "--expose", "8000-9000", "busybox", "ps")
	assert.Assert(c, is.Contains(out, "conflicting options: port exposing and the container type network mode"))
}
