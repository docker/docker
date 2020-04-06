// +build !windows

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/integration-cli/cli"
	"github.com/docker/docker/integration-cli/cli/build"
	"github.com/docker/docker/testutil/fakecontext"
	"github.com/docker/go-units"
	"gotest.tools/assert"
	"gotest.tools/icmd"
)

func (s *DockerSuite) TestBuildResourceConstraintsAreUsed(c *testing.T) {
	testRequires(c, cpuCfsQuota)
	name := "testbuildresourceconstraints"
	buildLabel := "DockerSuite.TestBuildResourceConstraintsAreUsed"

	ctx := fakecontext.New(c, "", fakecontext.WithDockerfile(`
	FROM hello-world:frozen
	RUN ["/hello"]
	`))
	cli.Docker(
		cli.Args("build", "--no-cache", "--rm=false", "--memory=64m", "--memory-swap=-1", "--cpuset-cpus=0", "--cpuset-mems=0", "--cpu-shares=100", "--cpu-quota=8000", "--ulimit", "nofile=42", "--label="+buildLabel, "-t", name, "."),
		cli.InDir(ctx.Dir),
	).Assert(c, icmd.Success)

	out := cli.DockerCmd(c, "ps", "-lq", "--filter", "label="+buildLabel).Combined()
	cID := strings.TrimSpace(out)

	type hostConfig struct {
		Memory     int64
		MemorySwap int64
		CpusetCpus string
		CpusetMems string
		CPUShares  int64
		CPUQuota   int64
		Ulimits    []*units.Ulimit
	}

	cfg := inspectFieldJSON(c, cID, "HostConfig")

	var c1 hostConfig
	err := json.Unmarshal([]byte(cfg), &c1)
	assert.Assert(c, err == nil, cfg)

	assert.Equal(c, c1.Memory, int64(64*1024*1024), "resource constraints not set properly for Memory")
	assert.Equal(c, c1.MemorySwap, int64(-1), "resource constraints not set properly for MemorySwap")
	assert.Equal(c, c1.CpusetCpus, "0", "resource constraints not set properly for CpusetCpus")
	assert.Equal(c, c1.CpusetMems, "0", "resource constraints not set properly for CpusetMems")
	assert.Equal(c, c1.CPUShares, int64(100), "resource constraints not set properly for CPUShares")
	assert.Equal(c, c1.CPUQuota, int64(8000), "resource constraints not set properly for CPUQuota")
	assert.Equal(c, c1.Ulimits[0].Name, "nofile", "resource constraints not set properly for Ulimits")
	assert.Equal(c, c1.Ulimits[0].Hard, int64(42), "resource constraints not set properly for Ulimits")

	// Make sure constraints aren't saved to image
	cli.DockerCmd(c, "run", "--name=test", name)

	cfg = inspectFieldJSON(c, "test", "HostConfig")

	var c2 hostConfig
	err = json.Unmarshal([]byte(cfg), &c2)
	assert.Assert(c, err == nil, cfg)

	assert.Assert(c, c2.Memory != int64(64*1024*1024), "resource leaked from build for Memory")
	assert.Assert(c, c2.MemorySwap != int64(-1), "resource leaked from build for MemorySwap")
	assert.Assert(c, c2.CpusetCpus != "0", "resource leaked from build for CpusetCpus")
	assert.Assert(c, c2.CpusetMems != "0", "resource leaked from build for CpusetMems")
	assert.Assert(c, c2.CPUShares != int64(100), "resource leaked from build for CPUShares")
	assert.Assert(c, c2.CPUQuota != int64(8000), "resource leaked from build for CPUQuota")
	assert.Assert(c, c2.Ulimits == nil, "resource leaked from build for Ulimits")
}

func (s *DockerSuite) TestBuildAddChangeOwnership(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	name := "testbuildaddown"

	ctx := func() *fakecontext.Fake {
		dockerfile := `
			FROM busybox
			ADD foo /bar/
			RUN [ $(stat -c %U:%G "/bar") = 'root:root' ]
			RUN [ $(stat -c %U:%G "/bar/foo") = 'root:root' ]
			`
		tmpDir, err := ioutil.TempDir("", "fake-context")
		assert.NilError(c, err)
		testFile, err := os.Create(filepath.Join(tmpDir, "foo"))
		if err != nil {
			c.Fatalf("failed to create foo file: %v", err)
		}
		defer testFile.Close()

		icmd.RunCmd(icmd.Cmd{
			Command: []string{"chown", "daemon:daemon", "foo"},
			Dir:     tmpDir,
		}).Assert(c, icmd.Success)

		if err := ioutil.WriteFile(filepath.Join(tmpDir, "Dockerfile"), []byte(dockerfile), 0644); err != nil {
			c.Fatalf("failed to open destination dockerfile: %v", err)
		}
		return fakecontext.New(c, tmpDir)
	}()

	defer ctx.Close()

	buildImageSuccessfully(c, name, build.WithExternalBuildContext(ctx))
}

// Test that an infinite sleep during a build is killed if the client disconnects.
// This test is fairly hairy because there are lots of ways to race.
// Strategy:
// * Monitor the output of docker events starting from before
// * Run a 1-year-long sleep from a docker build.
// * When docker events sees container start, close the "docker build" command
// * Wait for docker events to emit a dying event.
//
// TODO(buildkit): this test needs to be rewritten for buildkit.
// It has been manually tested positive. Confirmed issue: docker build output parsing.
// Potential issue: newEventObserver uses docker events, which is not hooked up to buildkit.
func (s *DockerSuite) TestBuildCancellationKillsSleep(c *testing.T) {
	testRequires(c, DaemonIsLinux, TODOBuildkit)
	name := "testbuildcancellation"

	observer, err := newEventObserver(c)
	assert.NilError(c, err)
	err = observer.Start()
	assert.NilError(c, err)
	defer observer.Stop()

	// (Note: one year, will never finish)
	ctx := fakecontext.New(c, "", fakecontext.WithDockerfile("FROM busybox\nRUN sleep 31536000"))
	defer ctx.Close()

	buildCmd := exec.Command(dockerBinary, "build", "-t", name, ".")
	buildCmd.Dir = ctx.Dir

	stdoutBuild, err := buildCmd.StdoutPipe()
	assert.NilError(c, err)

	if err := buildCmd.Start(); err != nil {
		c.Fatalf("failed to run build: %s", err)
	}
	// always clean up
	defer func() {
		buildCmd.Process.Kill()
		buildCmd.Wait()
	}()

	matchCID := regexp.MustCompile("Running in (.+)")
	scanner := bufio.NewScanner(stdoutBuild)

	outputBuffer := new(bytes.Buffer)
	var buildID string
	for scanner.Scan() {
		line := scanner.Text()
		outputBuffer.WriteString(line)
		outputBuffer.WriteString("\n")
		if matches := matchCID.FindStringSubmatch(line); len(matches) > 0 {
			buildID = matches[1]
			break
		}
	}

	if buildID == "" {
		c.Fatalf("Unable to find build container id in build output:\n%s", outputBuffer.String())
	}

	testActions := map[string]chan bool{
		"start": make(chan bool, 1),
		"die":   make(chan bool, 1),
	}

	matcher := matchEventLine(buildID, "container", testActions)
	processor := processEventMatch(testActions)
	go observer.Match(matcher, processor)

	select {
	case <-time.After(10 * time.Second):
		observer.CheckEventError(c, buildID, "start", matcher)
	case <-testActions["start"]:
		// ignore, done
	}

	// Send a kill to the `docker build` command.
	// Causes the underlying build to be cancelled due to socket close.
	if err := buildCmd.Process.Kill(); err != nil {
		c.Fatalf("error killing build command: %s", err)
	}

	// Get the exit status of `docker build`, check it exited because killed.
	if err := buildCmd.Wait(); err != nil && !isKilled(err) {
		c.Fatalf("wait failed during build run: %T %s", err, err)
	}

	select {
	case <-time.After(10 * time.Second):
		observer.CheckEventError(c, buildID, "die", matcher)
	case <-testActions["die"]:
		// ignore, done
	}
}

func isKilled(err error) bool {
	if exitErr, ok := err.(*exec.ExitError); ok {
		status, ok := exitErr.Sys().(syscall.WaitStatus)
		if !ok {
			return false
		}
		// status.ExitStatus() is required on Windows because it does not
		// implement Signal() nor Signaled(). Just check it had a bad exit
		// status could mean it was killed (and in tests we do kill)
		return (status.Signaled() && status.Signal() == os.Kill) || status.ExitStatus() != 0
	}
	return false
}
