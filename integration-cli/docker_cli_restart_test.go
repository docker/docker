package main

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/integration-cli/checker"
	"github.com/docker/docker/integration-cli/cli"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

type DockerCLIRestartSuite struct {
	ds *DockerSuite
}

func (s *DockerCLIRestartSuite) TearDownTest(ctx context.Context, c *testing.T) {
	s.ds.TearDownTest(ctx, c)
}

func (s *DockerCLIRestartSuite) OnTimeout(c *testing.T) {
	s.ds.OnTimeout(c)
}

func (s *DockerCLIRestartSuite) TestRestartStoppedContainer(c *testing.T) {
	cID := cli.DockerCmd(c, "run", "-d", "busybox", "sh", "-c", "echo foobar && exit 0").Stdout()
	cID = strings.TrimSpace(cID)

	getLogs := func(t *testing.T) (interface{}, string) {
		out := cli.DockerCmd(t, "logs", cID).Combined()
		return out, ""
	}

	// Wait 10 seconds for the 'echo' to appear in the logs
	poll.WaitOn(c, pollCheck(c, getLogs, checker.Equals("foobar\n")), poll.WithTimeout(10*time.Second))

	// Make sure the container has stopped before we restart it.
	cli.WaitExited(c, cID, 20*time.Second)
	cli.DockerCmd(c, "restart", cID)

	poll.WaitOn(c, pollCheck(c, getLogs, checker.Equals("foobar\nfoobar\n")), poll.WithTimeout(10*time.Second))
}

func (s *DockerCLIRestartSuite) TestRestartRunningContainer(c *testing.T) {
	cID := cli.DockerCmd(c, "run", "-d", "busybox", "sh", "-c", "echo foobar && sleep 30 && echo 'should not print this'").Stdout()
	cID = strings.TrimSpace(cID)
	cli.WaitRun(c, cID)

	getLogs := func(c *testing.T) (interface{}, string) {
		out := cli.DockerCmd(c, "logs", cID).Combined()
		return out, ""
	}

	// Wait 10 seconds for the 'echo' to appear in the logs
	poll.WaitOn(c, pollCheck(c, getLogs, checker.Equals("foobar\n")), poll.WithTimeout(10*time.Second))

	cli.DockerCmd(c, "restart", "-t", "1", cID)
	cli.WaitRun(c, cID)

	// Wait 10 seconds for first 'echo' appear (again) in the logs
	poll.WaitOn(c, pollCheck(c, getLogs, checker.Equals("foobar\nfoobar\n")), poll.WithTimeout(10*time.Second))
}

// Test that restarting a container with a volume does not create a new volume on restart. Regression test for #819.
func (s *DockerCLIRestartSuite) TestRestartWithVolumes(c *testing.T) {
	prefix, slash := getPrefixAndSlashFromDaemonPlatform()
	cID := runSleepingContainer(c, "-d", "-v", prefix+slash+"test")
	out, err := inspectFilter(cID, "len .Mounts")
	assert.NilError(c, err, "failed to inspect %s: %s", cID, out)
	out = strings.Trim(out, " \n\r")
	assert.Equal(c, out, "1")

	source, err := inspectMountSourceField(cID, prefix+slash+"test")
	assert.NilError(c, err)

	cli.DockerCmd(c, "restart", cID)

	out, err = inspectFilter(cID, "len .Mounts")
	assert.NilError(c, err, "failed to inspect %s: %s", cID, out)
	out = strings.Trim(out, " \n\r")
	assert.Equal(c, out, "1")

	sourceAfterRestart, err := inspectMountSourceField(cID, prefix+slash+"test")
	assert.NilError(c, err)
	assert.Equal(c, source, sourceAfterRestart)
}

func (s *DockerCLIRestartSuite) TestRestartDisconnectedContainer(c *testing.T) {
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon, NotUserNamespace)

	// Run a container on the default bridge network
	cID := cli.DockerCmd(c, "run", "-d", "--name", "c0", "busybox", "top").Stdout()
	cID = strings.TrimSpace(cID)
	cli.WaitRun(c, cID)

	// Disconnect the container from the network
	result := cli.DockerCmd(c, "network", "disconnect", "bridge", "c0")
	assert.Assert(c, result.ExitCode == 0, result.Combined())

	// Restart the container
	result = cli.DockerCmd(c, "restart", "c0")
	assert.Assert(c, result.ExitCode == 0, result.Combined())
}

func (s *DockerCLIRestartSuite) TestRestartPolicyNO(c *testing.T) {
	cID := cli.DockerCmd(c, "create", "--restart=no", "busybox").Stdout()
	cID = strings.TrimSpace(cID)
	name := inspectField(c, cID, "HostConfig.RestartPolicy.Name")
	assert.Equal(c, name, "no")
}

func (s *DockerCLIRestartSuite) TestRestartPolicyAlways(c *testing.T) {
	id := cli.DockerCmd(c, "create", "--restart=always", "busybox").Stdout()
	id = strings.TrimSpace(id)
	name := inspectField(c, id, "HostConfig.RestartPolicy.Name")
	assert.Equal(c, name, "always")

	MaximumRetryCount := inspectField(c, id, "HostConfig.RestartPolicy.MaximumRetryCount")

	// MaximumRetryCount=0 if the restart policy is always
	assert.Equal(c, MaximumRetryCount, "0")
}

func (s *DockerCLIRestartSuite) TestRestartPolicyOnFailure(c *testing.T) {
	out, _, err := dockerCmdWithError("create", "--restart=on-failure:-1", "busybox")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, strings.Contains(out, "maximum retry count cannot be negative"))

	id := cli.DockerCmd(c, "create", "--restart=on-failure:1", "busybox").Stdout()
	id = strings.TrimSpace(id)
	name := inspectField(c, id, "HostConfig.RestartPolicy.Name")
	maxRetry := inspectField(c, id, "HostConfig.RestartPolicy.MaximumRetryCount")
	assert.Equal(c, name, "on-failure")
	assert.Equal(c, maxRetry, "1")

	id = cli.DockerCmd(c, "create", "--restart=on-failure:0", "busybox").Stdout()
	id = strings.TrimSpace(id)
	name = inspectField(c, id, "HostConfig.RestartPolicy.Name")
	maxRetry = inspectField(c, id, "HostConfig.RestartPolicy.MaximumRetryCount")
	assert.Equal(c, name, "on-failure")
	assert.Equal(c, maxRetry, "0")

	id = cli.DockerCmd(c, "create", "--restart=on-failure", "busybox").Stdout()
	id = strings.TrimSpace(id)
	name = inspectField(c, id, "HostConfig.RestartPolicy.Name")
	maxRetry = inspectField(c, id, "HostConfig.RestartPolicy.MaximumRetryCount")
	assert.Equal(c, name, "on-failure")
	assert.Equal(c, maxRetry, "0")
}

// a good container with --restart=on-failure:3
// MaximumRetryCount!=0; RestartCount=0
func (s *DockerCLIRestartSuite) TestRestartContainerwithGoodContainer(c *testing.T) {
	id := cli.DockerCmd(c, "run", "-d", "--restart=on-failure:3", "busybox", "true").Stdout()
	id = strings.TrimSpace(id)
	err := waitInspect(id, "{{ .State.Restarting }} {{ .State.Running }}", "false false", 30*time.Second)
	assert.NilError(c, err)

	count := inspectField(c, id, "RestartCount")
	assert.Equal(c, count, "0")

	MaximumRetryCount := inspectField(c, id, "HostConfig.RestartPolicy.MaximumRetryCount")
	assert.Equal(c, MaximumRetryCount, "3")
}

func (s *DockerCLIRestartSuite) TestRestartContainerSuccess(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)
	// Skipped for Hyper-V isolated containers. Test is currently written
	// such that it assumes there is a host process to kill. In Hyper-V
	// containers, the process is inside the utility VM, not on the host.
	if DaemonIsWindows() {
		skip.If(c, testEnv.GitHubActions())
		testRequires(c, testEnv.DaemonInfo.Isolation.IsProcess)
	}

	id := runSleepingContainer(c, "-d", "--restart=always")
	cli.WaitRun(c, id)

	pidStr := inspectField(c, id, "State.Pid")

	pid, err := strconv.Atoi(pidStr)
	assert.NilError(c, err)

	p, err := os.FindProcess(pid)
	assert.NilError(c, err)
	assert.Assert(c, p != nil)

	err = p.Kill()
	assert.NilError(c, err)

	err = waitInspect(id, "{{.RestartCount}}", "1", 30*time.Second)
	assert.NilError(c, err)

	err = waitInspect(id, "{{.State.Status}}", "running", 30*time.Second)
	assert.NilError(c, err)
}

func (s *DockerCLIRestartSuite) TestRestartWithPolicyUserDefinedNetwork(c *testing.T) {
	// TODO Windows. This may be portable following HNS integration post TP5.
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon, NotUserNamespace)
	cli.DockerCmd(c, "network", "create", "-d", "bridge", "udNet")

	cli.DockerCmd(c, "run", "-d", "--net=udNet", "--name=first", "busybox", "top")
	cli.WaitRun(c, "first")

	cli.DockerCmd(c, "run", "-d", "--restart=always", "--net=udNet", "--name=second", "--link=first:foo", "busybox", "top")
	cli.WaitRun(c, "second")

	// ping to first and its alias foo must succeed
	_, _, err := dockerCmdWithError("exec", "second", "ping", "-c", "1", "first")
	assert.NilError(c, err)
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "foo")
	assert.NilError(c, err)

	// Now kill the second container and let the restart policy kick in
	pidStr := inspectField(c, "second", "State.Pid")

	pid, err := strconv.Atoi(pidStr)
	assert.NilError(c, err)

	p, err := os.FindProcess(pid)
	assert.NilError(c, err)
	assert.Assert(c, p != nil)

	err = p.Kill()
	assert.NilError(c, err)

	err = waitInspect("second", "{{.RestartCount}}", "1", 5*time.Second)
	assert.NilError(c, err)

	err = waitInspect("second", "{{.State.Status}}", "running", 5*time.Second)
	assert.NilError(c, err)

	// ping to first and its alias foo must still succeed
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "first")
	assert.NilError(c, err)
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "foo")
	assert.NilError(c, err)
}

func (s *DockerCLIRestartSuite) TestRestartPolicyAfterRestart(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)
	// Skipped for Hyper-V isolated containers. Test is currently written
	// such that it assumes there is a host process to kill. In Hyper-V
	// containers, the process is inside the utility VM, not on the host.
	if DaemonIsWindows() {
		skip.If(c, testEnv.GitHubActions())
		testRequires(c, testEnv.DaemonInfo.Isolation.IsProcess)
	}

	id := runSleepingContainer(c, "-d", "--restart=always")
	cli.WaitRun(c, id)

	cli.DockerCmd(c, "restart", id)
	cli.WaitRun(c, id)

	pidStr := inspectField(c, id, "State.Pid")

	pid, err := strconv.Atoi(pidStr)
	assert.NilError(c, err)

	p, err := os.FindProcess(pid)
	assert.NilError(c, err)
	assert.Assert(c, p != nil)

	err = p.Kill()
	assert.NilError(c, err)

	err = waitInspect(id, "{{.RestartCount}}", "1", 30*time.Second)
	assert.NilError(c, err)

	err = waitInspect(id, "{{.State.Status}}", "running", 30*time.Second)
	assert.NilError(c, err)
}

func (s *DockerCLIRestartSuite) TestRestartContainerwithRestartPolicy(c *testing.T) {
	id1 := cli.DockerCmd(c, "run", "-d", "--restart=on-failure:3", "busybox", "false").Stdout()
	id1 = strings.TrimSpace(id1)
	id2 := cli.DockerCmd(c, "run", "-d", "--restart=always", "busybox", "false").Stdout()
	id2 = strings.TrimSpace(id2)

	waitTimeout := 15 * time.Second
	if testEnv.DaemonInfo.OSType == "windows" {
		waitTimeout = 150 * time.Second
	}
	err := waitInspect(id1, "{{ .State.Restarting }} {{ .State.Running }}", "false false", waitTimeout)
	assert.NilError(c, err)

	cli.DockerCmd(c, "restart", id1)
	cli.DockerCmd(c, "restart", id2)

	// Make sure we can stop/start (regression test from a705e166cf3bcca62543150c2b3f9bfeae45ecfa)
	cli.DockerCmd(c, "stop", id1)
	cli.DockerCmd(c, "stop", id2)
	cli.DockerCmd(c, "start", id1)
	cli.DockerCmd(c, "start", id2)

	// Kill the containers, making sure they are stopped at the end of the test
	cli.DockerCmd(c, "kill", id1)
	cli.DockerCmd(c, "kill", id2)
	err = waitInspect(id1, "{{ .State.Restarting }} {{ .State.Running }}", "false false", waitTimeout)
	assert.NilError(c, err)
	err = waitInspect(id2, "{{ .State.Restarting }} {{ .State.Running }}", "false false", waitTimeout)
	assert.NilError(c, err)
}

func (s *DockerCLIRestartSuite) TestRestartAutoRemoveContainer(c *testing.T) {
	id := runSleepingContainer(c, "--rm")
	cli.DockerCmd(c, "restart", id)
	err := waitInspect(id, "{{ .State.Restarting }} {{ .State.Running }}", "false true", 15*time.Second)
	assert.NilError(c, err)

	out := cli.DockerCmd(c, "ps").Stdout()
	assert.Assert(c, is.Contains(out, id[:12]), "container should be restarted instead of removed: %v", out)

	// Kill the container to make sure it will be removed
	cli.DockerCmd(c, "kill", id)
}
