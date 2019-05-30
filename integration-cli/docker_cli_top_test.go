package main

import (
	"strings"

	"github.com/go-check/check"
	"gotest.tools/assert"
	"gotest.tools/icmd"
)

func (s *DockerSuite) TestTopMultipleArgs(c *check.C) {
	out := runSleepingContainer(c, "-d")
	cleanedContainerID := strings.TrimSpace(out)

	var expected icmd.Expected
	switch testEnv.OSType {
	case "windows":
		expected = icmd.Expected{ExitCode: 1, Err: "Windows does not support arguments to top"}
	default:
		expected = icmd.Expected{Out: "PID"}
	}
	result := dockerCmdWithResult("top", cleanedContainerID, "-o", "pid")
	result.Assert(c, expected)
}

func (s *DockerSuite) TestTopNonPrivileged(c *check.C) {
	out := runSleepingContainer(c, "-d")
	cleanedContainerID := strings.TrimSpace(out)

	out1, _ := dockerCmd(c, "top", cleanedContainerID)
	out2, _ := dockerCmd(c, "top", cleanedContainerID)
	dockerCmd(c, "kill", cleanedContainerID)

	// Windows will list the name of the launched executable which in this case is busybox.exe, without the parameters.
	// Linux will display the command executed in the container
	var lookingFor string
	if testEnv.OSType == "windows" {
		lookingFor = "busybox.exe"
	} else {
		lookingFor = "top"
	}

	assert.Assert(c, strings.Contains(out1, lookingFor), "top should've listed `%s` in the process list, but failed the first time", lookingFor)
	assert.Assert(c, strings.Contains(out2, lookingFor), "top should've listed `%s` in the process list, but failed the second time", lookingFor)
}

// TestTopWindowsCoreProcesses validates that there are lines for the critical
// processes which are found in a Windows container. Note Windows is architecturally
// very different to Linux in this regard.
func (s *DockerSuite) TestTopWindowsCoreProcesses(c *check.C) {
	testRequires(c, DaemonIsWindows)
	out := runSleepingContainer(c, "-d")
	cleanedContainerID := strings.TrimSpace(out)
	out1, _ := dockerCmd(c, "top", cleanedContainerID)
	lookingFor := []string{"smss.exe", "csrss.exe", "wininit.exe", "services.exe", "lsass.exe", "CExecSvc.exe"}
	for i, s := range lookingFor {
		assert.Assert(c, strings.Contains(out1, s), "top should've listed `%s` in the process list, but failed. Test case %d", s, i)
	}
}

func (s *DockerSuite) TestTopPrivileged(c *check.C) {
	// Windows does not support --privileged
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	out, _ := dockerCmd(c, "run", "--privileged", "-i", "-d", "busybox", "top")
	cleanedContainerID := strings.TrimSpace(out)

	out1, _ := dockerCmd(c, "top", cleanedContainerID)
	out2, _ := dockerCmd(c, "top", cleanedContainerID)
	dockerCmd(c, "kill", cleanedContainerID)

	assert.Assert(c, strings.Contains(out1, "top"), "top should've listed `top` in the process list, but failed the first time")
	assert.Assert(c, strings.Contains(out2, "top"), "top should've listed `top` in the process list, but failed the second time")
}
