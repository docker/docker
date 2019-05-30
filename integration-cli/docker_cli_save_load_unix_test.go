// +build !windows

package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/docker/docker/integration-cli/cli/build"
	"github.com/go-check/check"
	"github.com/kr/pty"
	"gotest.tools/assert"
	"gotest.tools/icmd"
)

// save a repo and try to load it using stdout
func (s *DockerSuite) TestSaveAndLoadRepoStdout(c *check.C) {
	name := "test-save-and-load-repo-stdout"
	dockerCmd(c, "run", "--name", name, "busybox", "true")

	repoName := "foobar-save-load-test"
	before, _ := dockerCmd(c, "commit", name, repoName)
	before = strings.TrimRight(before, "\n")

	tmpFile, err := ioutil.TempFile("", "foobar-save-load-test.tar")
	assert.NilError(c, err)
	defer os.Remove(tmpFile.Name())

	icmd.RunCmd(icmd.Cmd{
		Command: []string{dockerBinary, "save", repoName},
		Stdout:  tmpFile,
	}).Assert(c, icmd.Success)

	tmpFile, err = os.Open(tmpFile.Name())
	assert.NilError(c, err)
	defer tmpFile.Close()

	deleteImages(repoName)

	icmd.RunCmd(icmd.Cmd{
		Command: []string{dockerBinary, "load"},
		Stdin:   tmpFile,
	}).Assert(c, icmd.Success)

	after := inspectField(c, repoName, "Id")
	after = strings.TrimRight(after, "\n")

	assert.Equal(c, after, before, "inspect is not the same after a save / load")

	deleteImages(repoName)

	pty, tty, err := pty.Open()
	assert.NilError(c, err)
	cmd := exec.Command(dockerBinary, "save", repoName)
	cmd.Stdin = tty
	cmd.Stdout = tty
	cmd.Stderr = tty
	assert.NilError(c, cmd.Start())
	assert.ErrorContains(c, cmd.Wait(), "", "did not break writing to a TTY")

	buf := make([]byte, 1024)

	n, err := pty.Read(buf)
	assert.NilError(c, err) //could not read tty output
	assert.Assert(c, strings.Contains(string(buf[:n]), "cowardly refusing"), "help output is not being yielded")
}

func (s *DockerSuite) TestSaveAndLoadWithProgressBar(c *check.C) {
	name := "test-load"
	buildImageSuccessfully(c, name, build.WithDockerfile(`FROM busybox
	RUN touch aa
	`))

	tmptar := name + ".tar"
	dockerCmd(c, "save", "-o", tmptar, name)
	defer os.Remove(tmptar)

	dockerCmd(c, "rmi", name)
	dockerCmd(c, "tag", "busybox", name)
	out, _ := dockerCmd(c, "load", "-i", tmptar)
	expected := fmt.Sprintf("The image %s:latest already exists, renaming the old one with ID", name)
	assert.Assert(c, strings.Contains(out, expected))
}

// fail because load didn't receive data from stdin
func (s *DockerSuite) TestLoadNoStdinFail(c *check.C) {
	pty, tty, err := pty.Open()
	assert.NilError(c, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, dockerBinary, "load")
	cmd.Stdin = tty
	cmd.Stdout = tty
	cmd.Stderr = tty
	assert.ErrorContains(c, cmd.Run(), "", "docker-load should fail")

	buf := make([]byte, 1024)

	n, err := pty.Read(buf)
	assert.NilError(c, err) //could not read tty output
	assert.Assert(c, strings.Contains(string(buf[:n]), "requested load from stdin, but stdin is empty"))
}
