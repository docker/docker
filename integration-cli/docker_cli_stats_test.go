package main

import (
	"bufio"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/integration-cli/cli"
	"github.com/go-check/check"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
)

func (s *DockerSuite) TestStatsNoStream(c *check.C) {
	// Windows does not support stats
	testRequires(c, DaemonIsLinux)
	out, _ := dockerCmd(c, "run", "-d", "busybox", "top")
	id := strings.TrimSpace(out)
	assert.NilError(c, waitRun(id))

	statsCmd := exec.Command(dockerBinary, "stats", "--no-stream", id)
	type output struct {
		out []byte
		err error
	}

	ch := make(chan output)
	go func() {
		out, err := statsCmd.Output()
		ch <- output{out, err}
	}()

	select {
	case outerr := <-ch:
		assert.NilError(c, outerr.err, "Error running stats: %v", outerr.err)
		assert.Assert(c, is.Contains(string(outerr.out), id[:12]), "running container wasn't present in output")
	case <-time.After(3 * time.Second):
		statsCmd.Process.Kill()
		c.Fatalf("stats did not return immediately when not streaming")
	}
}

func (s *DockerSuite) TestStatsContainerNotFound(c *check.C) {
	// Windows does not support stats
	testRequires(c, DaemonIsLinux)

	out, _, err := dockerCmdWithError("stats", "notfound")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, is.Contains(out, "No such container: notfound"), "Expected to fail on not found container stats, got %q instead", out)

	out, _, err = dockerCmdWithError("stats", "--no-stream", "notfound")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, is.Contains(out, "No such container: notfound"), "Expected to fail on not found container stats with --no-stream, got %q instead", out)
}

func (s *DockerSuite) TestStatsAllRunningNoStream(c *check.C) {
	// Windows does not support stats
	testRequires(c, DaemonIsLinux)

	out, _ := dockerCmd(c, "run", "-d", "busybox", "top")
	id1 := strings.TrimSpace(out)[:12]
	assert.NilError(c, waitRun(id1))
	out, _ = dockerCmd(c, "run", "-d", "busybox", "top")
	id2 := strings.TrimSpace(out)[:12]
	assert.NilError(c, waitRun(id2))
	out, _ = dockerCmd(c, "run", "-d", "busybox", "top")
	id3 := strings.TrimSpace(out)[:12]
	assert.NilError(c, waitRun(id3))
	dockerCmd(c, "stop", id3)

	out, _ = dockerCmd(c, "stats", "--no-stream")
	if !strings.Contains(out, id1) || !strings.Contains(out, id2) {
		c.Fatalf("Expected stats output to contain both %s and %s, got %s", id1, id2, out)
	}
	if strings.Contains(out, id3) {
		c.Fatalf("Did not expect %s in stats, got %s", id3, out)
	}

	// check output contains real data, but not all zeros
	reg, _ := regexp.Compile("[1-9]+")
	// split output with "\n", outLines[1] is id2's output
	// outLines[2] is id1's output
	outLines := strings.Split(out, "\n")
	// check stat result of id2 contains real data
	realData := reg.Find([]byte(outLines[1][12:]))
	assert.Assert(c, realData != nil, "stat result are empty: %s", out)
	// check stat result of id1 contains real data
	realData = reg.Find([]byte(outLines[2][12:]))
	assert.Assert(c, realData != nil, "stat result are empty: %s", out)
}

func (s *DockerSuite) TestStatsAllNoStream(c *check.C) {
	// Windows does not support stats
	testRequires(c, DaemonIsLinux)

	out, _ := dockerCmd(c, "run", "-d", "busybox", "top")
	id1 := strings.TrimSpace(out)[:12]
	assert.NilError(c, waitRun(id1))
	dockerCmd(c, "stop", id1)
	out, _ = dockerCmd(c, "run", "-d", "busybox", "top")
	id2 := strings.TrimSpace(out)[:12]
	assert.NilError(c, waitRun(id2))

	out, _ = dockerCmd(c, "stats", "--all", "--no-stream")
	if !strings.Contains(out, id1) || !strings.Contains(out, id2) {
		c.Fatalf("Expected stats output to contain both %s and %s, got %s", id1, id2, out)
	}

	// check output contains real data, but not all zeros
	reg, _ := regexp.Compile("[1-9]+")
	// split output with "\n", outLines[1] is id2's output
	outLines := strings.Split(out, "\n")
	// check stat result of id2 contains real data
	realData := reg.Find([]byte(outLines[1][12:]))
	assert.Assert(c, realData != nil, "stat result of %s is empty: %s", id2, out)

	// check stat result of id1 contains all zero
	realData = reg.Find([]byte(outLines[2][12:]))
	assert.Assert(c, realData == nil, "stat result of %s should be empty : %s", id1, out)
}

func (s *DockerSuite) TestStatsAllNewContainersAdded(c *check.C) {
	// Windows does not support stats
	testRequires(c, DaemonIsLinux)

	id := make(chan string)
	addedChan := make(chan struct{})

	runSleepingContainer(c, "-d")
	statsCmd := exec.Command(dockerBinary, "stats")
	stdout, err := statsCmd.StdoutPipe()
	assert.NilError(c, err)
	assert.NilError(c, statsCmd.Start())
	go statsCmd.Wait()
	defer statsCmd.Process.Kill()

	go func() {
		containerID := <-id
		matchID := regexp.MustCompile(containerID)

		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			switch {
			case matchID.MatchString(scanner.Text()):
				close(addedChan)
				return
			}
		}
	}()

	out := runSleepingContainer(c, "-d")
	assert.NilError(c, waitRun(strings.TrimSpace(out)))
	id <- strings.TrimSpace(out)[:12]

	select {
	case <-time.After(30 * time.Second):
		c.Fatal("failed to observe new container created added to stats")
	case <-addedChan:
		// ignore, done
	}
}

func (s *DockerSuite) TestStatsFormatAll(c *check.C) {
	// Windows does not support stats
	testRequires(c, DaemonIsLinux)

	cli.DockerCmd(c, "run", "-d", "--name=RunningOne", "busybox", "top")
	cli.WaitRun(c, "RunningOne")
	cli.DockerCmd(c, "run", "-d", "--name=ExitedOne", "busybox", "top")
	cli.DockerCmd(c, "stop", "ExitedOne")
	cli.WaitExited(c, "ExitedOne", 5*time.Second)

	out := cli.DockerCmd(c, "stats", "--no-stream", "--format", "{{.Name}}").Combined()
	assert.Assert(c, is.Contains(out, "RunningOne"))
	assert.Assert(c, !strings.Contains(out, "ExitedOne"))

	out = cli.DockerCmd(c, "stats", "--all", "--no-stream", "--format", "{{.Name}}").Combined()
	assert.Assert(c, is.Contains(out, "RunningOne"))
	assert.Assert(c, is.Contains(out, "ExitedOne"))
}
