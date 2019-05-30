package main

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/integration-cli/checker"
	"github.com/go-check/check"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
)

func (s *DockerSuite) TestRestartStoppedContainer(c *check.C) {
	dockerCmd(c, "run", "--name=test", "busybox", "echo", "foobar")
	cleanedContainerID := getIDByName(c, "test")

	out, _ := dockerCmd(c, "logs", cleanedContainerID)
	assert.Equal(c, out, "foobar\n")

	dockerCmd(c, "restart", cleanedContainerID)

	// Wait until the container has stopped
	err := waitInspect(cleanedContainerID, "{{.State.Running}}", "false", 20*time.Second)
	assert.NilError(c, err)

	out, _ = dockerCmd(c, "logs", cleanedContainerID)
	assert.Equal(c, out, "foobar\nfoobar\n")
}

func (s *DockerSuite) TestRestartRunningContainer(c *check.C) {
	out, _ := dockerCmd(c, "run", "-d", "busybox", "sh", "-c", "echo foobar && sleep 30 && echo 'should not print this'")

	cleanedContainerID := strings.TrimSpace(out)

	assert.NilError(c, waitRun(cleanedContainerID))

	getLogs := func(c *check.C) (interface{}, check.CommentInterface) {
		out, _ := dockerCmd(c, "logs", cleanedContainerID)
		return out, nil
	}

	// Wait 10 seconds for the 'echo' to appear in the logs
	waitAndAssert(c, 10*time.Second, getLogs, checker.Equals, "foobar\n")

	dockerCmd(c, "restart", "-t", "1", cleanedContainerID)
	assert.NilError(c, waitRun(cleanedContainerID))

	// Wait 10 seconds for first 'echo' appear (again) in the logs
	waitAndAssert(c, 10*time.Second, getLogs, checker.Equals, "foobar\nfoobar\n")
}

// Test that restarting a container with a volume does not create a new volume on restart. Regression test for #819.
func (s *DockerSuite) TestRestartWithVolumes(c *check.C) {
	prefix, slash := getPrefixAndSlashFromDaemonPlatform()
	out := runSleepingContainer(c, "-d", "-v", prefix+slash+"test")

	cleanedContainerID := strings.TrimSpace(out)
	out, err := inspectFilter(cleanedContainerID, "len .Mounts")
	assert.NilError(c, err, "failed to inspect %s: %s", cleanedContainerID, out)
	out = strings.Trim(out, " \n\r")
	assert.Equal(c, out, "1")

	source, err := inspectMountSourceField(cleanedContainerID, prefix+slash+"test")
	assert.NilError(c, err)

	dockerCmd(c, "restart", cleanedContainerID)

	out, err = inspectFilter(cleanedContainerID, "len .Mounts")
	assert.NilError(c, err, "failed to inspect %s: %s", cleanedContainerID, out)
	out = strings.Trim(out, " \n\r")
	assert.Equal(c, out, "1")

	sourceAfterRestart, err := inspectMountSourceField(cleanedContainerID, prefix+slash+"test")
	assert.NilError(c, err)
	assert.Equal(c, source, sourceAfterRestart)
}

func (s *DockerSuite) TestRestartDisconnectedContainer(c *check.C) {
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon, NotUserNamespace, NotArm)

	// Run a container on the default bridge network
	out, _ := dockerCmd(c, "run", "-d", "--name", "c0", "busybox", "top")
	cleanedContainerID := strings.TrimSpace(out)
	assert.NilError(c, waitRun(cleanedContainerID))

	// Disconnect the container from the network
	out, exitCode := dockerCmd(c, "network", "disconnect", "bridge", "c0")
	assert.Assert(c, exitCode == 0, out)

	// Restart the container
	out, exitCode = dockerCmd(c, "restart", "c0")
	assert.Assert(c, exitCode == 0, out)
}

func (s *DockerSuite) TestRestartPolicyNO(c *check.C) {
	out, _ := dockerCmd(c, "create", "--restart=no", "busybox")

	id := strings.TrimSpace(string(out))
	name := inspectField(c, id, "HostConfig.RestartPolicy.Name")
	assert.Equal(c, name, "no")
}

func (s *DockerSuite) TestRestartPolicyAlways(c *check.C) {
	out, _ := dockerCmd(c, "create", "--restart=always", "busybox")

	id := strings.TrimSpace(string(out))
	name := inspectField(c, id, "HostConfig.RestartPolicy.Name")
	assert.Equal(c, name, "always")

	MaximumRetryCount := inspectField(c, id, "HostConfig.RestartPolicy.MaximumRetryCount")

	// MaximumRetryCount=0 if the restart policy is always
	assert.Equal(c, MaximumRetryCount, "0")
}

func (s *DockerSuite) TestRestartPolicyOnFailure(c *check.C) {
	out, _, err := dockerCmdWithError("create", "--restart=on-failure:-1", "busybox")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, strings.Contains(out, "maximum retry count cannot be negative"))

	out, _ = dockerCmd(c, "create", "--restart=on-failure:1", "busybox")

	id := strings.TrimSpace(string(out))
	name := inspectField(c, id, "HostConfig.RestartPolicy.Name")
	maxRetry := inspectField(c, id, "HostConfig.RestartPolicy.MaximumRetryCount")

	assert.Equal(c, name, "on-failure")
	assert.Equal(c, maxRetry, "1")

	out, _ = dockerCmd(c, "create", "--restart=on-failure:0", "busybox")

	id = strings.TrimSpace(string(out))
	name = inspectField(c, id, "HostConfig.RestartPolicy.Name")
	maxRetry = inspectField(c, id, "HostConfig.RestartPolicy.MaximumRetryCount")

	assert.Equal(c, name, "on-failure")
	assert.Equal(c, maxRetry, "0")

	out, _ = dockerCmd(c, "create", "--restart=on-failure", "busybox")

	id = strings.TrimSpace(string(out))
	name = inspectField(c, id, "HostConfig.RestartPolicy.Name")
	maxRetry = inspectField(c, id, "HostConfig.RestartPolicy.MaximumRetryCount")

	assert.Equal(c, name, "on-failure")
	assert.Equal(c, maxRetry, "0")
}

// a good container with --restart=on-failure:3
// MaximumRetryCount!=0; RestartCount=0
func (s *DockerSuite) TestRestartContainerwithGoodContainer(c *check.C) {
	out, _ := dockerCmd(c, "run", "-d", "--restart=on-failure:3", "busybox", "true")

	id := strings.TrimSpace(string(out))
	err := waitInspect(id, "{{ .State.Restarting }} {{ .State.Running }}", "false false", 30*time.Second)
	assert.NilError(c, err)

	count := inspectField(c, id, "RestartCount")
	assert.Equal(c, count, "0")

	MaximumRetryCount := inspectField(c, id, "HostConfig.RestartPolicy.MaximumRetryCount")
	assert.Equal(c, MaximumRetryCount, "3")
}

func (s *DockerSuite) TestRestartContainerSuccess(c *check.C) {
	// Skipped for Hyper-V isolated containers. Test is currently written
	// such that it assumes there is a host process to kill. In Hyper-V
	// containers, the process is inside the utility VM, not on the host.
	testRequires(c, testEnv.IsLocalDaemon, IsolationIsProcess)

	out := runSleepingContainer(c, "-d", "--restart=always")
	id := strings.TrimSpace(out)
	assert.NilError(c, waitRun(id))

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

func (s *DockerSuite) TestRestartWithPolicyUserDefinedNetwork(c *check.C) {
	// TODO Windows. This may be portable following HNS integration post TP5.
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon, NotUserNamespace, NotArm)
	dockerCmd(c, "network", "create", "-d", "bridge", "udNet")

	dockerCmd(c, "run", "-d", "--net=udNet", "--name=first", "busybox", "top")
	assert.NilError(c, waitRun("first"))

	dockerCmd(c, "run", "-d", "--restart=always", "--net=udNet", "--name=second",
		"--link=first:foo", "busybox", "top")
	assert.NilError(c, waitRun("second"))

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

func (s *DockerSuite) TestRestartPolicyAfterRestart(c *check.C) {
	// Skipped for Hyper-V isolated containers. Test is currently written
	// such that it assumes there is a host process to kill. In Hyper-V
	// containers, the process is inside the utility VM, not on the host.
	testRequires(c, testEnv.IsLocalDaemon, IsolationIsProcess)

	out := runSleepingContainer(c, "-d", "--restart=always")
	id := strings.TrimSpace(out)
	assert.NilError(c, waitRun(id))

	dockerCmd(c, "restart", id)

	assert.NilError(c, waitRun(id))

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

func (s *DockerSuite) TestRestartContainerwithRestartPolicy(c *check.C) {
	out1, _ := dockerCmd(c, "run", "-d", "--restart=on-failure:3", "busybox", "false")
	out2, _ := dockerCmd(c, "run", "-d", "--restart=always", "busybox", "false")

	id1 := strings.TrimSpace(string(out1))
	id2 := strings.TrimSpace(string(out2))
	waitTimeout := 15 * time.Second
	if testEnv.OSType == "windows" {
		waitTimeout = 150 * time.Second
	}
	err := waitInspect(id1, "{{ .State.Restarting }} {{ .State.Running }}", "false false", waitTimeout)
	assert.NilError(c, err)

	dockerCmd(c, "restart", id1)
	dockerCmd(c, "restart", id2)

	// Make sure we can stop/start (regression test from a705e166cf3bcca62543150c2b3f9bfeae45ecfa)
	dockerCmd(c, "stop", id1)
	dockerCmd(c, "stop", id2)
	dockerCmd(c, "start", id1)
	dockerCmd(c, "start", id2)

	// Kill the containers, making sure the are stopped at the end of the test
	dockerCmd(c, "kill", id1)
	dockerCmd(c, "kill", id2)
	err = waitInspect(id1, "{{ .State.Restarting }} {{ .State.Running }}", "false false", waitTimeout)
	assert.NilError(c, err)
	err = waitInspect(id2, "{{ .State.Restarting }} {{ .State.Running }}", "false false", waitTimeout)
	assert.NilError(c, err)
}

func (s *DockerSuite) TestRestartAutoRemoveContainer(c *check.C) {
	out := runSleepingContainer(c, "--rm")

	id := strings.TrimSpace(string(out))
	dockerCmd(c, "restart", id)
	err := waitInspect(id, "{{ .State.Restarting }} {{ .State.Running }}", "false true", 15*time.Second)
	assert.NilError(c, err)

	out, _ = dockerCmd(c, "ps")
	assert.Assert(c, is.Contains(out, id[:12]), "container should be restarted instead of removed: %v", out)

	// Kill the container to make sure it will be removed
	dockerCmd(c, "kill", id)
}
