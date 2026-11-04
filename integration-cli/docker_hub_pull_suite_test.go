package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/moby/moby/v2/integration-cli/daemon"
	testdaemon "github.com/moby/moby/v2/internal/testutil/daemon"
	"gotest.tools/v3/assert"
)

// DockerHubPullSuite provides an isolated daemon that doesn't have all the
// images that are baked into our 'global' test environment daemon (e.g.,
// busybox, httpserver, ...).
//
// We use it for push/pull tests where we want to start fresh, and measure the
// relative impact of each individual operation. As part of this suite, all
// images are removed after each test.
type DockerHubPullSuite struct {
	d  *daemon.Daemon
	ds *DockerSuite
}

// newDockerHubPullSuite returns a new instance of a DockerHubPullSuite.
func newDockerHubPullSuite() *DockerHubPullSuite {
	return &DockerHubPullSuite{
		ds: &DockerSuite{},
	}
}

// SetUpSuite starts the suite daemon.
func (s *DockerHubPullSuite) SetUpSuite(ctx context.Context, t *testing.T) {
	testRequires(t, DaemonIsLinux, testEnv.IsLocalDaemon)
	t.Logf("SETTING UP TEST")
	s.d = daemon.New(t, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
	s.d.Start(t)
}

// TearDownSuite stops the suite daemon.
func (s *DockerHubPullSuite) TearDownSuite(ctx context.Context, t *testing.T) {
	if s.d != nil {
		s.d.Stop(t)
	}
}

// SetUpTest declares that all tests of this suite require network.
func (s *DockerHubPullSuite) SetUpTest(ctx context.Context, t *testing.T) {
	testRequires(t, Network)
}

// TearDownTest removes all images from the suite daemon.
func (s *DockerHubPullSuite) TearDownTest(ctx context.Context, t *testing.T) {
	// TODO(thaJeztah): this may be redundant if the daemon is already calling `Cleanup()`
	if out, _ := s.CmdWithError(t, "images", "-aq"); out != "" {
		images := strings.Split(out, "\n")
		images = append([]string{"rmi", "-f"}, images...)
		_, _ = s.d.Cmd(images...)
	}
	s.ds.TearDownTest(ctx, t)
}

// Cmd executes a command against the suite daemon and returns the combined
// output. The function fails the test when the command returns an error.
func (s *DockerHubPullSuite) Cmd(t *testing.T, name string, arg ...string) string {
	t.Helper()
	args := append([]string{"--host", s.d.Sock(), name}, arg...)
	out, err := exec.CommandContext(t.Context(), dockerBinary, args...).CombinedOutput()
	assert.Assert(t, err == nil, "%q failed with errors: %s %s, %v", dockerBinary, strings.Join(args, " "), string(out), err)
	return string(out)
}

// CmdWithError executes a command against the suite daemon and returns the
// combined output as well as any error.
func (s *DockerHubPullSuite) CmdWithError(t *testing.T, name string, arg ...string) (string, error) {
	t.Helper()
	args := append([]string{"--host", s.d.Sock(), name}, arg...)
	out, err := exec.CommandContext(t.Context(), dockerBinary, args...).CombinedOutput()
	return string(out), err
}
