// +build !windows

package main

import (
	"io/ioutil"
	"os"
	"os/exec"

	"github.com/docker/docker/pkg/integration/checker"
	"github.com/go-check/check"
	"github.com/kr/pty"
)

// save a repo and try to load it using stdout
func (s *DockerSuite) TestSaveAndLoadRepoStdout(c *check.C) {
	name := "test-save-and-load-repo-stdout"
	dockerCmd(c, "run", "--name", name, "busybox", "true")

	repoName := "foobar-save-load-test"
	out, _ := dockerCmd(c, "commit", name, repoName)

	before, _ := dockerCmd(c, "inspect", repoName)

	tmpFile, err := ioutil.TempFile("", "foobar-save-load-test.tar")
	c.Assert(err, check.IsNil)
	defer os.Remove(tmpFile.Name())

	saveCmd := exec.Command(dockerBinary, "save", repoName)
	saveCmd.Stdout = tmpFile

	_, err = runCommand(saveCmd)
	c.Assert(err, check.IsNil)

	tmpFile, err = os.Open(tmpFile.Name())
	c.Assert(err, check.IsNil)

	deleteImages(repoName)

	loadCmd := exec.Command(dockerBinary, "load")
	loadCmd.Stdin = tmpFile

	out, _, err = runCommandWithOutput(loadCmd)
	c.Assert(err, check.IsNil, check.Commentf(out))

	after, _ := dockerCmd(c, "inspect", repoName)

	c.Assert(before, check.Equals, after) //inspect is not the same after a save / load

	deleteImages(repoName)

	pty, tty, err := pty.Open()
	c.Assert(err, check.IsNil)
	cmd := exec.Command(dockerBinary, "save", repoName)
	cmd.Stdin = tty
	cmd.Stdout = tty
	cmd.Stderr = tty
	c.Assert(cmd.Start(), check.IsNil)
	c.Assert(cmd.Wait(), check.NotNil) //did not break writing to a TTY

	buf := make([]byte, 1024)

	n, err := pty.Read(buf)
	c.Assert(err, check.IsNil) //could not read tty output
	c.Assert(string(buf[:n]), checker.Contains, "cowardly refusing", check.Commentf("help output is not being yielded", out))
}
