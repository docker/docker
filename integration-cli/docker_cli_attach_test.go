package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/v2/integration-cli/cli"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/icmd"
)

const attachWait = 5 * time.Second

type DockerCLIAttachSuite struct {
	ds *DockerSuite
}

func (s *DockerCLIAttachSuite) TearDownTest(ctx context.Context, t *testing.T) {
	s.ds.TearDownTest(ctx, t)
}

func (s *DockerCLIAttachSuite) OnTimeout(t *testing.T) {
	s.ds.OnTimeout(t)
}

func (s *DockerCLIAttachSuite) TestAttachMultipleAndRestart(c *testing.T) {
	endGroup := &sync.WaitGroup{}
	startGroup := &sync.WaitGroup{}
	endGroup.Add(3)
	startGroup.Add(3)

	cli.DockerCmd(c, "run", "--name", "attacher", "-d", "busybox", "/bin/sh", "-c", "while true; do sleep 1; echo hello; done")
	cli.WaitRun(c, "attacher")

	startDone := make(chan struct{})
	endDone := make(chan struct{})

	go func() {
		endGroup.Wait()
		close(endDone)
	}()

	go func() {
		startGroup.Wait()
		close(startDone)
	}()

	for range 3 {
		go func() {
			cmd := exec.Command(dockerBinary, "attach", "attacher")

			defer func() {
				cmd.Wait()
				endGroup.Done()
			}()

			out, err := cmd.StdoutPipe()
			if err != nil {
				c.Error(err)
			}
			defer out.Close()

			if err := cmd.Start(); err != nil {
				c.Error(err)
			}

			buf := make([]byte, 1024)

			if _, err := out.Read(buf); err != nil && err != io.EOF {
				c.Error(err)
			}

			startGroup.Done()

			if !strings.Contains(string(buf), "hello") {
				c.Errorf("unexpected output %s expected hello\n", string(buf))
			}
		}()
	}

	select {
	case <-startDone:
	case <-time.After(attachWait):
		c.Fatalf("Attaches did not initialize properly")
	}

	cli.DockerCmd(c, "kill", "attacher")

	select {
	case <-endDone:
	case <-time.After(attachWait):
		c.Fatalf("Attaches did not finish properly")
	}
}

// FIXME(thaJeztah) migrate to docker/cli tests
func (s *DockerCLIAttachSuite) TestAttachTTYWithoutStdin(c *testing.T) {
	// TODO: Figure out how to get this running again reliable on Windows.
	// It works by accident at the moment. Sometimes. I've gone back to v1.13.0 and see the same.
	// On Windows, docker run -d -ti busybox causes the container to exit immediately.
	// Obviously a year back when I updated the test, that was not the case. However,
	// with this, and the test racing with the tear-down which panic's, sometimes CI
	// will just fail and `MISS` all the other tests. For now, disabling it. Will
	// open an issue to track re-enabling this and root-causing the problem.
	testRequires(c, DaemonIsLinux)
	out := cli.DockerCmd(c, "run", "-d", "-ti", "busybox").Stdout()
	id := strings.TrimSpace(out)
	cli.WaitRun(c, id)

	done := make(chan error, 1)
	go func() {
		defer close(done)

		cmd := exec.Command(dockerBinary, "attach", id)
		if _, err := cmd.StdinPipe(); err != nil {
			done <- err
			return
		}

		expected := "stdin is not a terminal"
		result := icmd.RunCmd(icmd.Cmd{
			Command: cmd.Args,
			Env:     cmd.Env,
			Dir:     cmd.Dir,
			Stdin:   cmd.Stdin,
			Stdout:  cmd.Stdout,
		})
		if result.Error == nil {
			done <- errors.New("attach should have failed")
			return
		} else if !strings.Contains(result.Combined(), expected) {
			done <- fmt.Errorf("attach failed with error %q: expected %q", out, expected)
			return
		}
	}()

	select {
	case err := <-done:
		assert.NilError(c, err)
	case <-time.After(attachWait):
		c.Fatal("attach is running but should have failed")
	}
}

func (s *DockerCLIAttachSuite) TestAttachDisconnect(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	out := cli.DockerCmd(c, "run", "-di", "busybox", "/bin/cat").Stdout()
	id := strings.TrimSpace(out)

	cmd := exec.Command(dockerBinary, "attach", id)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		c.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	assert.NilError(c, err)
	defer stdout.Close()
	assert.NilError(c, cmd.Start())
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()

	_, err = stdin.Write([]byte("hello\n"))
	assert.NilError(c, err)
	out, err = bufio.NewReader(stdout).ReadString('\n')
	assert.NilError(c, err)
	assert.Equal(c, strings.TrimSpace(out), "hello")

	assert.NilError(c, stdin.Close())

	// Expect container to still be running after stdin is closed
	running := inspectField(c, id, "State.Running")
	assert.Equal(c, running, "true")
}

func (s *DockerCLIAttachSuite) TestAttachPausedContainer(c *testing.T) {
	testRequires(c, IsPausable)
	runSleepingContainer(c, "-d", "--name=test")
	cli.DockerCmd(c, "pause", "test")

	result := cli.Docker(cli.Args("attach", "test"))
	result.Assert(c, icmd.Expected{
		Error:    "exit status 1",
		ExitCode: 1,
		Err:      "cannot attach to a paused container",
	})
}
