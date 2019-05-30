// +build !windows,!test_no_exec

package main

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/go-check/check"
	"github.com/kr/pty"
	"gotest.tools/assert"
)

// regression test for #12546
func (s *DockerSuite) TestExecInteractiveStdinClose(c *check.C) {
	testRequires(c, DaemonIsLinux)
	out, _ := dockerCmd(c, "run", "-itd", "busybox", "/bin/cat")
	contID := strings.TrimSpace(out)

	cmd := exec.Command(dockerBinary, "exec", "-i", contID, "echo", "-n", "hello")
	p, err := pty.Start(cmd)
	assert.NilError(c, err)

	b := bytes.NewBuffer(nil)

	ch := make(chan error)
	go func() { ch <- cmd.Wait() }()

	select {
	case err := <-ch:
		assert.NilError(c, err)
		io.Copy(b, p)
		p.Close()
		bs := b.Bytes()
		bs = bytes.Trim(bs, "\x00")
		output := string(bs[:])
		assert.Equal(c, strings.TrimSpace(output), "hello")
	case <-time.After(5 * time.Second):
		p.Close()
		c.Fatal("timed out running docker exec")
	}
}

func (s *DockerSuite) TestExecTTY(c *check.C) {
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon)
	dockerCmd(c, "run", "-d", "--name=test", "busybox", "sh", "-c", "echo hello > /foo && top")

	cmd := exec.Command(dockerBinary, "exec", "-it", "test", "sh")
	p, err := pty.Start(cmd)
	assert.NilError(c, err)
	defer p.Close()

	_, err = p.Write([]byte("cat /foo && exit\n"))
	assert.NilError(c, err)

	chErr := make(chan error)
	go func() {
		chErr <- cmd.Wait()
	}()
	select {
	case err := <-chErr:
		assert.NilError(c, err)
	case <-time.After(3 * time.Second):
		c.Fatal("timeout waiting for exec to exit")
	}

	buf := make([]byte, 256)
	read, err := p.Read(buf)
	assert.NilError(c, err)
	assert.Assert(c, bytes.Contains(buf, []byte("hello")), string(buf[:read]))
}

// Test the TERM env var is set when -t is provided on exec
func (s *DockerSuite) TestExecWithTERM(c *check.C) {
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon)
	out, _ := dockerCmd(c, "run", "-id", "busybox", "/bin/cat")
	contID := strings.TrimSpace(out)
	cmd := exec.Command(dockerBinary, "exec", "-t", contID, "sh", "-c", "if [ -z $TERM ]; then exit 1; else exit 0; fi")
	if err := cmd.Run(); err != nil {
		assert.NilError(c, err)
	}
}

// Test that the TERM env var is not set on exec when -t is not provided, even if it was set
// on run
func (s *DockerSuite) TestExecWithNoTERM(c *check.C) {
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon)
	out, _ := dockerCmd(c, "run", "-itd", "busybox", "/bin/cat")
	contID := strings.TrimSpace(out)
	cmd := exec.Command(dockerBinary, "exec", contID, "sh", "-c", "if [ -z $TERM ]; then exit 0; else exit 1; fi")
	if err := cmd.Run(); err != nil {
		assert.NilError(c, err)
	}
}
