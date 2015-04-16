package main

import (
	"os/exec"
	"strings"
	"time"

	"github.com/go-check/check"
)

func (s *DockerSuite) TestRestartStoppedContainer(c *check.C) {

	runCmd := exec.Command(dockerBinary, "run", "-d", "busybox", "echo", "foobar")
	out, _, err := runCommandWithOutput(runCmd)
	if err != nil {
		c.Fatal(out, err)
	}

	cleanedContainerID := strings.TrimSpace(out)

	runCmd = exec.Command(dockerBinary, "wait", cleanedContainerID)
	if out, _, err = runCommandWithOutput(runCmd); err != nil {
		c.Fatal(out, err)
	}

	runCmd = exec.Command(dockerBinary, "logs", cleanedContainerID)
	out, _, err = runCommandWithOutput(runCmd)
	if err != nil {
		c.Fatal(out, err)
	}

	if out != "foobar\n" {
		c.Errorf("container should've printed 'foobar'")
	}

	runCmd = exec.Command(dockerBinary, "restart", cleanedContainerID)
	if out, _, err = runCommandWithOutput(runCmd); err != nil {
		c.Fatal(out, err)
	}

	runCmd = exec.Command(dockerBinary, "logs", cleanedContainerID)
	out, _, err = runCommandWithOutput(runCmd)
	if err != nil {
		c.Fatal(out, err)
	}

	if out != "foobar\nfoobar\n" {
		c.Errorf("container should've printed 'foobar' twice")
	}

}

func (s *DockerSuite) TestRestartRunningContainer(c *check.C) {

	runCmd := exec.Command(dockerBinary, "run", "-d", "busybox", "sh", "-c", "echo foobar && sleep 30 && echo 'should not print this'")
	out, _, err := runCommandWithOutput(runCmd)
	if err != nil {
		c.Fatal(out, err)
	}

	cleanedContainerID := strings.TrimSpace(out)

	time.Sleep(1 * time.Second)

	runCmd = exec.Command(dockerBinary, "logs", cleanedContainerID)
	out, _, err = runCommandWithOutput(runCmd)
	if err != nil {
		c.Fatal(out, err)
	}

	if out != "foobar\n" {
		c.Errorf("container should've printed 'foobar'")
	}

	runCmd = exec.Command(dockerBinary, "restart", "-t", "1", cleanedContainerID)
	if out, _, err = runCommandWithOutput(runCmd); err != nil {
		c.Fatal(out, err)
	}

	runCmd = exec.Command(dockerBinary, "logs", cleanedContainerID)
	out, _, err = runCommandWithOutput(runCmd)
	if err != nil {
		c.Fatal(out, err)
	}

	time.Sleep(1 * time.Second)

	if out != "foobar\nfoobar\n" {
		c.Errorf("container should've printed 'foobar' twice")
	}

}

// Test that restarting a container with a volume does not create a new volume on restart. Regression test for #819.
func (s *DockerSuite) TestRestartWithVolumes(c *check.C) {

	runCmd := exec.Command(dockerBinary, "run", "-d", "-v", "/test", "busybox", "top")
	out, _, err := runCommandWithOutput(runCmd)
	if err != nil {
		c.Fatal(out, err)
	}

	cleanedContainerID := strings.TrimSpace(out)

	runCmd = exec.Command(dockerBinary, "inspect", "--format", "{{ len .Volumes }}", cleanedContainerID)
	out, _, err = runCommandWithOutput(runCmd)
	if err != nil {
		c.Fatal(out, err)
	}

	if out = strings.Trim(out, " \n\r"); out != "1" {
		c.Errorf("expect 1 volume received %s", out)
	}

	runCmd = exec.Command(dockerBinary, "inspect", "--format", "{{ .Volumes }}", cleanedContainerID)
	volumes, _, err := runCommandWithOutput(runCmd)
	if err != nil {
		c.Fatal(volumes, err)
	}

	runCmd = exec.Command(dockerBinary, "restart", cleanedContainerID)
	if out, _, err = runCommandWithOutput(runCmd); err != nil {
		c.Fatal(out, err)
	}

	runCmd = exec.Command(dockerBinary, "inspect", "--format", "{{ len .Volumes }}", cleanedContainerID)
	out, _, err = runCommandWithOutput(runCmd)
	if err != nil {
		c.Fatal(out, err)
	}

	if out = strings.Trim(out, " \n\r"); out != "1" {
		c.Errorf("expect 1 volume after restart received %s", out)
	}

	runCmd = exec.Command(dockerBinary, "inspect", "--format", "{{ .Volumes }}", cleanedContainerID)
	volumesAfterRestart, _, err := runCommandWithOutput(runCmd)
	if err != nil {
		c.Fatal(volumesAfterRestart, err)
	}

	if volumes != volumesAfterRestart {
		volumes = strings.Trim(volumes, " \n\r")
		volumesAfterRestart = strings.Trim(volumesAfterRestart, " \n\r")
		c.Errorf("expected volume path: %s Actual path: %s", volumes, volumesAfterRestart)
	}

}

func (s *DockerSuite) TestRestartPolicyNO(c *check.C) {

	cmd := exec.Command(dockerBinary, "run", "-d", "--restart=no", "busybox", "false")
	out, _, err := runCommandWithOutput(cmd)
	if err != nil {
		c.Fatal(err, out)
	}

	id := strings.TrimSpace(string(out))
	name, err := inspectField(id, "HostConfig.RestartPolicy.Name")
	if err != nil {
		c.Fatal(err, out)
	}
	if name != "no" {
		c.Fatalf("Container restart policy name is %s, expected %s", name, "no")
	}

}

func (s *DockerSuite) TestRestartPolicyAlways(c *check.C) {

	cmd := exec.Command(dockerBinary, "run", "-d", "--restart=always", "busybox", "false")
	out, _, err := runCommandWithOutput(cmd)
	if err != nil {
		c.Fatal(err, out)
	}

	id := strings.TrimSpace(string(out))
	name, err := inspectField(id, "HostConfig.RestartPolicy.Name")
	if err != nil {
		c.Fatal(err, out)
	}
	if name != "always" {
		c.Fatalf("Container restart policy name is %s, expected %s", name, "always")
	}

	MaximumRetryCount, err := inspectField(id, "HostConfig.RestartPolicy.MaximumRetryCount")
	if err != nil {
		c.Fatal(err)
	}

	// MaximumRetryCount=0 if the restart policy is always
	if MaximumRetryCount != "0" {
		c.Fatalf("Container Maximum Retry Count is %s, expected %s", MaximumRetryCount, "0")
	}

}

func (s *DockerSuite) TestRestartPolicyOnFailure(c *check.C) {

	cmd := exec.Command(dockerBinary, "run", "-d", "--restart=on-failure:1", "busybox", "false")
	out, _, err := runCommandWithOutput(cmd)
	if err != nil {
		c.Fatal(err, out)
	}

	id := strings.TrimSpace(string(out))
	name, err := inspectField(id, "HostConfig.RestartPolicy.Name")
	if err != nil {
		c.Fatal(err, out)
	}
	if name != "on-failure" {
		c.Fatalf("Container restart policy name is %s, expected %s", name, "on-failure")
	}

}

// a good container with --restart=on-failure:3
// MaximumRetryCount!=0; RestartCount=0
func (s *DockerSuite) TestContainerRestartwithGoodContainer(c *check.C) {
	out, err := exec.Command(dockerBinary, "run", "-d", "--restart=on-failure:3", "busybox", "true").CombinedOutput()
	if err != nil {
		c.Fatal(string(out), err)
	}
	id := strings.TrimSpace(string(out))
	if err := waitInspect(id, "{{ .State.Restarting }} {{ .State.Running }}", "false false", 5); err != nil {
		c.Fatal(err)
	}
	count, err := inspectField(id, "RestartCount")
	if err != nil {
		c.Fatal(err)
	}
	if count != "0" {
		c.Fatalf("Container was restarted %s times, expected %d", count, 0)
	}
	MaximumRetryCount, err := inspectField(id, "HostConfig.RestartPolicy.MaximumRetryCount")
	if err != nil {
		c.Fatal(err)
	}
	if MaximumRetryCount != "3" {
		c.Fatalf("Container Maximum Retry Count is %s, expected %s", MaximumRetryCount, "3")
	}

}

func (s *DockerSuite) TestCreateStartRestartStopStartKillRm(c *check.C) {
	defer deleteAllContainers()

	runCmd := exec.Command(dockerBinary, "run", "-d", "busybox", "top")
	out, _, err := runCommandWithOutput(runCmd)
	if err != nil {
		c.Fatal(out, err)
	}

	cleanedContainerID := strings.TrimSpace(out)

	out, _ = dockerCmd(c, "ps", "-aq")
	if strings.HasPrefix(cleanedContainerID, out) {
		c.Fatal("Containers id is not listed")
	}

	dockerCmd(c, "restart", cleanedContainerID)

	dockerCmd(c, "stop", cleanedContainerID)

	dockerCmd(c, "start", cleanedContainerID)

	dockerCmd(c, "kill", cleanedContainerID)

	dockerCmd(c, "rm", cleanedContainerID)

	dockerCmd(c, "ps", "-aq")

}
