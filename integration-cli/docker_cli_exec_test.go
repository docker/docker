package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration-cli/cli"
	"github.com/moby/moby/v2/integration-cli/cli/build"
	"github.com/moby/moby/v2/internal/testutil"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/icmd"
)

type DockerCLIExecSuite struct {
	ds *DockerSuite
}

func (s *DockerCLIExecSuite) TearDownTest(ctx context.Context, t *testing.T) {
	s.ds.TearDownTest(ctx, t)
}

func (s *DockerCLIExecSuite) OnTimeout(t *testing.T) {
	s.ds.OnTimeout(t)
}

func (s *DockerCLIExecSuite) TestExec(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	out := cli.DockerCmd(c, "run", "-d", "--name", "testing", "busybox", "sh", "-c", "echo test > /tmp/file && top").Stdout()
	cli.WaitRun(c, strings.TrimSpace(out))

	out = cli.DockerCmd(c, "exec", "testing", "cat", "/tmp/file").Stdout()
	assert.Equal(c, strings.Trim(out, "\r\n"), "test")
}

func (s *DockerCLIExecSuite) TestExecInteractive(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	cli.DockerCmd(c, "run", "-d", "--name", "testing", "busybox", "sh", "-c", "echo test > /tmp/file && top")

	execCmd := exec.Command(dockerBinary, "exec", "-i", "testing", "sh")
	stdin, err := execCmd.StdinPipe()
	assert.NilError(c, err)
	stdout, err := execCmd.StdoutPipe()
	assert.NilError(c, err)

	err = execCmd.Start()
	assert.NilError(c, err)
	_, err = stdin.Write([]byte("cat /tmp/file\n"))
	assert.NilError(c, err)

	r := bufio.NewReader(stdout)
	line, err := r.ReadString('\n')
	assert.NilError(c, err)
	line = strings.TrimSpace(line)
	assert.Equal(c, line, "test")
	err = stdin.Close()
	assert.NilError(c, err)
	errChan := make(chan error, 1)
	go func() {
		errChan <- execCmd.Wait()
		close(errChan)
	}()
	select {
	case err := <-errChan:
		assert.NilError(c, err)
	case <-time.After(1 * time.Second):
		c.Fatal("docker exec failed to exit on stdin close")
	}
}

func (s *DockerCLIExecSuite) TestExecAfterContainerRestart(c *testing.T) {
	cID := runSleepingContainer(c)
	cli.WaitRun(c, cID)
	cli.DockerCmd(c, "restart", cID)
	cli.WaitRun(c, cID)

	out := cli.DockerCmd(c, "exec", cID, "echo", "hello").Combined()
	assert.Equal(c, strings.TrimSpace(out), "hello")
}

func (s *DockerDaemonSuite) TestExecAfterDaemonRestart(c *testing.T) {
	ctx := testutil.GetContext(c)
	// TODO Windows CI: DockerDaemonSuite doesn't run on Windows, and requires a little work to get this ported.
	s.d.StartWithBusybox(ctx, c)

	out, err := s.d.Cmd("run", "-d", "--name", "top", "-p", "80", "busybox:latest", "top")
	assert.NilError(c, err, "Could not run top: %s", out)

	s.d.Restart(c)

	out, err = s.d.Cmd("start", "top")
	assert.NilError(c, err, "Could not start top after daemon restart: %s", out)

	out, err = s.d.Cmd("exec", "top", "echo", "hello")
	assert.NilError(c, err, "Could not exec on container top: %s", out)
	assert.Equal(c, strings.TrimSpace(out), "hello")
}

// Regression test for #9155, #9044
func (s *DockerCLIExecSuite) TestExecEnv(c *testing.T) {
	// TODO Windows CI: This one is interesting and may just end up being a feature
	// difference between Windows and Linux. On Windows, the environment is passed
	// into the process that is launched, not into the machine environment. Hence
	// a subsequent exec will not have LALA set/
	testRequires(c, DaemonIsLinux)
	runSleepingContainer(c, "-e", "LALA=value1", "-e", "LALA=value2", "-d", "--name", "testing")
	cli.WaitRun(c, "testing")

	out := cli.DockerCmd(c, "exec", "testing", "env").Stdout()
	assert.Check(c, !strings.Contains(out, "LALA=value1"))
	assert.Check(c, is.Contains(out, "LALA=value2"))
	assert.Check(c, is.Contains(out, "HOME=/root"))
}

func (s *DockerCLIExecSuite) TestExecSetEnv(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	runSleepingContainer(c, "-e", "HOME=/root", "-d", "--name", "testing")
	cli.WaitRun(c, "testing")

	out := cli.DockerCmd(c, "exec", "-e", "HOME=/another", "-e", "ABC=xyz", "testing", "env").Stdout()
	assert.Check(c, !strings.Contains(out, "HOME=/root"))
	assert.Check(c, is.Contains(out, "HOME=/another"))
	assert.Check(c, is.Contains(out, "ABC=xyz"))
}

func (s *DockerCLIExecSuite) TestExecExitStatus(c *testing.T) {
	runSleepingContainer(c, "-d", "--name", "top")

	result := icmd.RunCommand(dockerBinary, "exec", "top", "sh", "-c", "exit 23")
	result.Assert(c, icmd.Expected{ExitCode: 23, Error: "exit status 23"})
}

func (s *DockerCLIExecSuite) TestExecPausedContainer(c *testing.T) {
	testRequires(c, IsPausable)

	ContainerID := runSleepingContainer(c, "-d", "--name", "testing")

	cli.DockerCmd(c, "pause", "testing")
	out, _, err := dockerCmdWithError("exec", ContainerID, "echo", "hello")
	assert.ErrorContains(c, err, "", "container should fail to exec new command if it is paused")

	expected := ContainerID + " is paused, unpause the container before exec"
	assert.Assert(c, is.Contains(out, expected), "container should not exec new command if it is paused")
}

// regression test for #9476
func (s *DockerCLIExecSuite) TestExecTTYCloseStdin(c *testing.T) {
	// TODO Windows CI: This requires some work to port to Windows.
	testRequires(c, DaemonIsLinux)
	cli.DockerCmd(c, "run", "-d", "-it", "--name", "exec_tty_stdin", "busybox")

	cmd := exec.Command(dockerBinary, "exec", "-i", "exec_tty_stdin", "cat")
	stdinRw, err := cmd.StdinPipe()
	assert.NilError(c, err)

	_, err = stdinRw.Write([]byte("test"))
	assert.Check(c, err)
	_ = stdinRw.Close()

	res := icmd.RunCmd(icmd.Cmd{
		Command: cmd.Args,
		Env:     cmd.Env,
		Dir:     cmd.Dir,
		Stdin:   cmd.Stdin,
		Stdout:  cmd.Stdout,
	})
	out, err := res.Combined(), res.Error
	assert.NilError(c, err, out)

	out = cli.DockerCmd(c, "top", "exec_tty_stdin").Combined()
	outArr := strings.Split(out, "\n")
	assert.Assert(c, len(outArr) <= 3, "exec process left running")
	assert.Assert(c, !strings.Contains(out, "nsenter-exec"))
}

// FIXME(thaJeztah) migrate to docker/cli tests
func (s *DockerCLIExecSuite) TestExecTTYWithoutStdin(c *testing.T) {
	out := cli.DockerCmd(c, "run", "-d", "-ti", "busybox").Stdout()
	id := strings.TrimSpace(out)
	cli.WaitRun(c, id)

	errChan := make(chan error, 1)
	go func() {
		defer close(errChan)

		cmd := exec.Command(dockerBinary, "exec", "-ti", id, "true")
		if _, err := cmd.StdinPipe(); err != nil {
			errChan <- err
			return
		}

		expected := "stdin is not a terminal"
		res := icmd.RunCmd(icmd.Cmd{
			Command: cmd.Args,
			Env:     cmd.Env,
			Dir:     cmd.Dir,
			Stdin:   cmd.Stdin,
			Stdout:  cmd.Stdout,
		})
		out, err := res.Combined(), res.Error
		if err == nil {
			errChan <- errors.New("exec should have failed")
			return
		} else if !strings.Contains(out, expected) {
			errChan <- fmt.Errorf("exec failed with error %q: expected %q", out, expected)
			return
		}
	}()

	select {
	case err := <-errChan:
		assert.NilError(c, err)
	case <-time.After(3 * time.Second):
		c.Fatal("exec is running but should have failed")
	}
}

// FIXME(vdemeester) this should be a unit tests on cli/command/container package
func (s *DockerCLIExecSuite) TestExecParseError(c *testing.T) {
	// TODO Windows CI: Requires some extra work. Consider copying the
	// runSleepingContainer helper to have an exec version.
	testRequires(c, DaemonIsLinux)
	cli.DockerCmd(c, "run", "-d", "--name", "top", "busybox", "top")

	// Test normal (non-detached) case first
	icmd.RunCommand(dockerBinary, "exec", "top").Assert(c, icmd.Expected{
		ExitCode: 1,
		Error:    "exit status 1",
		Err:      "See 'docker exec --help'",
	})
}

func (s *DockerCLIExecSuite) TestExecStopNotHanging(c *testing.T) {
	// TODO Windows CI: Requires some extra work. Consider copying the
	// runSleepingContainer helper to have an exec version.
	testRequires(c, DaemonIsLinux)
	cli.DockerCmd(c, "run", "-d", "--name", "testing", "busybox", "top")

	result := icmd.StartCmd(icmd.Command(dockerBinary, "exec", "testing", "top"))
	result.Assert(c, icmd.Success)
	go icmd.WaitOnCmd(0, result)

	type dstop struct {
		out string
		err error
	}
	ch := make(chan dstop, 1)
	go func() {
		result := icmd.RunCommand(dockerBinary, "stop", "testing")
		ch <- dstop{result.Combined(), result.Error}
		close(ch)
	}()
	select {
	case <-time.After(3 * time.Second):
		c.Fatal("Container stop timed out")
	case s := <-ch:
		assert.NilError(c, s.err)
	}
}

func (s *DockerCLIExecSuite) TestExecCgroup(c *testing.T) {
	// Not applicable on Windows - using Linux specific functionality
	testRequires(c, NotUserNamespace)
	testRequires(c, DaemonIsLinux)
	cli.DockerCmd(c, "run", "-d", "--name", "testing", "busybox", "top")

	out := cli.DockerCmd(c, "exec", "testing", "cat", "/proc/1/cgroup").Stdout()
	containerCgroups := strings.Split(out, "\n")

	var wg sync.WaitGroup
	var mu sync.Mutex
	var execCgroups [][]string
	errChan := make(chan error, 5)
	// exec a few times concurrently to get consistent failure
	for range 5 {
		wg.Go(func() {
			out, _, err := dockerCmdWithError("exec", "testing", "cat", "/proc/self/cgroup")
			if err != nil {
				errChan <- err
				return
			}

			mu.Lock()
			execCgroups = append(execCgroups, strings.Split(out, "\n"))
			mu.Unlock()
		})
	}
	wg.Wait()
	close(errChan)

	for err := range errChan {
		assert.NilError(c, err)
	}

	for _, cg := range execCgroups {
		assert.DeepEqual(c, cg, containerCgroups)
	}
}

func (s *DockerCLIExecSuite) TestExecInspectID(c *testing.T) {
	id := runSleepingContainer(c, "-d")

	out := inspectField(c, id, "ExecIDs")
	assert.Equal(c, out, "[]", "ExecIDs should be empty, got: %s", out)

	// Start an exec, have it block waiting so we can do some checking
	cmd := exec.Command(dockerBinary, "exec", id, "sh", "-c",
		"while ! test -e /execid1; do sleep 1; done")

	err := cmd.Start()
	assert.NilError(c, err, "failed to start the exec cmd")

	// Give the exec 10 chances/seconds to start then give up and stop the test
	tries := 10
	for i := range tries {
		// Since its still running we should see exec as part of the container
		out = strings.TrimSpace(inspectField(c, id, "ExecIDs"))

		if out != "[]" && out != "<no value>" {
			break
		}
		assert.Check(c, i+1 != tries, "ExecIDs still empty after 10 second")
		time.Sleep(1 * time.Second)
	}

	// Save execID for later
	execID, err := inspectFilter(id, "index .ExecIDs 0")
	assert.NilError(c, err, "failed to get the exec id")

	// End the exec by creating the missing file
	err = exec.Command(dockerBinary, "exec", id, "sh", "-c", "touch /execid1").Run()
	assert.NilError(c, err, "failed to run the 2nd exec cmd")

	// Wait for 1st exec to complete
	cmd.Wait()

	// Give the exec 10 chances/seconds to stop then give up and stop the test
	for i := range tries {
		// Since its still running we should see exec as part of the container
		out = strings.TrimSpace(inspectField(c, id, "ExecIDs"))

		if out == "[]" {
			break
		}
		assert.Check(c, i+1 != tries, "ExecIDs still empty after 10 second")
		time.Sleep(1 * time.Second)
	}

	// But we should still be able to query the execID
	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	_, err = apiClient.ExecInspect(testutil.GetContext(c), execID, client.ExecInspectOptions{})
	assert.NilError(c, err)

	// Now delete the container and then an 'inspect' on the exec should
	// result in a 404 (not 'container not running')
	res := cli.DockerCmd(c, "rm", "-f", id)
	assert.Equal(c, res.ExitCode, 0, "error removing container: %s", res.Combined())

	_, err = apiClient.ExecInspect(testutil.GetContext(c), execID, client.ExecInspectOptions{})
	assert.ErrorContains(c, err, "No such exec instance")
}

func (s *DockerCLIExecSuite) TestLinksPingLinkedContainersOnRename(c *testing.T) {
	// Problematic on Windows as Windows does not support links
	testRequires(c, DaemonIsLinux)
	out := cli.DockerCmd(c, "run", "-d", "--name", "container1", "busybox", "top").Stdout()
	idA := strings.TrimSpace(out)
	assert.Assert(c, idA != "", "%s, id should not be nil", out)
	out = cli.DockerCmd(c, "run", "-d", "--link", "container1:alias1", "--name", "container2", "busybox", "top").Stdout()
	idB := strings.TrimSpace(out)
	assert.Assert(c, idB != "", "%s, id should not be nil", out)

	cli.DockerCmd(c, "exec", "container2", "ping", "-c", "1", "alias1", "-W", "1")
	cli.DockerCmd(c, "rename", "container1", "container_new")
	cli.DockerCmd(c, "exec", "container2", "ping", "-c", "1", "alias1", "-W", "1")
}

func (s *DockerCLIExecSuite) TestRunMutableNetworkFiles(c *testing.T) {
	// Not applicable on Windows to Windows CI.
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)
	for _, fn := range []string{"resolv.conf", "hosts"} {
		containers := cli.DockerCmd(c, "ps", "-q", "-a").Combined()
		if containers != "" {
			cli.DockerCmd(c, append([]string{"rm", "-fv"}, strings.Split(strings.TrimSpace(containers), "\n")...)...)
		}

		content := runCommandAndReadContainerFile(c, fn, dockerBinary, "run", "-d", "--name", "c1", "busybox", "sh", "-c", fmt.Sprintf("echo success >/etc/%s && top", fn))

		assert.Equal(c, strings.TrimSpace(string(content)), "success", "Content was not what was modified in the container", string(content))

		out := cli.DockerCmd(c, "run", "-d", "--name", "c2", "busybox", "top").Stdout()
		contID := strings.TrimSpace(out)
		netFilePath := containerStorageFile(contID, fn)

		f, err := os.OpenFile(netFilePath, os.O_WRONLY|os.O_SYNC|os.O_APPEND, 0o644)
		assert.NilError(c, err)

		if _, err := f.Seek(0, 0); err != nil {
			_ = f.Close()
			c.Fatal(err)
		}

		if err := f.Truncate(0); err != nil {
			_ = f.Close()
			c.Fatal(err)
		}

		if _, err := f.WriteString("success2\n"); err != nil {
			_ = f.Close()
			c.Fatal(err)
		}
		_ = f.Close()

		res := cli.DockerCmd(c, "exec", contID, "cat", "/etc/"+fn).Stdout()
		assert.Equal(c, res, "success2\n")
	}
}

func (s *DockerCLIExecSuite) TestExecWithUser(c *testing.T) {
	// TODO Windows CI: This may be fixable in the future once Windows
	// supports users
	testRequires(c, DaemonIsLinux)
	cli.DockerCmd(c, "run", "-d", "--name", "parent", "busybox", "top")

	out := cli.DockerCmd(c, "exec", "-u", "1", "parent", "id").Stdout()
	assert.Assert(c, is.Contains(out, "uid=1(daemon) gid=1(daemon)"))

	out = cli.DockerCmd(c, "exec", "-u", "root", "parent", "id").Stdout()
	assert.Assert(c, strings.Contains(out, "uid=0(root) gid=0(root)"), "exec with user by id expected daemon user got %s", out)
}

func (s *DockerCLIExecSuite) TestExecWithPrivileged(c *testing.T) {
	// Not applicable on Windows
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	// Start main loop which attempts mknod repeatedly
	cli.DockerCmd(c, "run", "-d", "--name", "parent", "--cap-drop=ALL", "busybox", "sh", "-c", `while (true); do if [ -e /exec_priv ]; then cat /exec_priv && mknod /tmp/sda b 8 0 && echo "Success"; else echo "Privileged exec has not run yet"; fi; usleep 10000; done`)

	// Check exec mknod doesn't work
	icmd.RunCommand(dockerBinary, "exec", "parent", "sh", "-c", "mknod /tmp/sdb b 8 16").Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "Operation not permitted",
	})

	// Check exec mknod does work with --privileged
	result := icmd.RunCommand(dockerBinary, "exec", "--privileged", "parent", "sh", "-c", `echo "Running exec --privileged" > /exec_priv && mknod /tmp/sdb b 8 16 && usleep 50000 && echo "Finished exec --privileged" > /exec_priv && echo ok`)
	result.Assert(c, icmd.Success)

	actual := strings.TrimSpace(result.Combined())
	assert.Equal(c, actual, "ok", "exec mknod in --cap-drop=ALL container with --privileged failed, output: %q", result.Combined())

	// Check subsequent unprivileged exec cannot mknod
	icmd.RunCommand(dockerBinary, "exec", "parent", "sh", "-c", "mknod /tmp/sdc b 8 32").Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "Operation not permitted",
	})
	// Confirm at no point was mknod allowed
	result = icmd.RunCommand(dockerBinary, "logs", "parent")
	result.Assert(c, icmd.Success)
	assert.Assert(c, !strings.Contains(result.Combined(), "Success"))
}

func (s *DockerCLIExecSuite) TestExecWithImageUser(c *testing.T) {
	// Not applicable on Windows
	testRequires(c, DaemonIsLinux)
	const name = "testbuilduser"
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
		RUN echo 'dockerio:x:1001:1001::/bin:/bin/false' >> /etc/passwd
		USER dockerio`))
	cli.DockerCmd(c, "run", "-d", "--name", "dockerioexec", name, "top")

	out := cli.DockerCmd(c, "exec", "dockerioexec", "whoami").Stdout()
	assert.Assert(c, strings.Contains(out, "dockerio"), "exec with user by id expected dockerio user got %s", out)
}

func (s *DockerCLIExecSuite) TestExecOnReadonlyContainer(c *testing.T) {
	// Windows does not support read-only
	// --read-only + userns has remount issues
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	cli.DockerCmd(c, "run", "-d", "--read-only", "--name", "parent", "busybox", "top")
	cli.DockerCmd(c, "exec", "parent", "true")
}

func (s *DockerCLIExecSuite) TestExecUlimits(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	const name = "testexeculimits"
	runSleepingContainer(c, "-d", "--ulimit", "nofile=511:511", "--name", name)
	cli.WaitRun(c, name)

	out, _, err := dockerCmdWithError("exec", name, "sh", "-c", "ulimit -n")
	assert.NilError(c, err)
	assert.Equal(c, strings.TrimSpace(out), "511")
}

// #15750
func (s *DockerCLIExecSuite) TestExecStartFails(c *testing.T) {
	const name = "exec-15750"
	runSleepingContainer(c, "-d", "--name", name)
	cli.WaitRun(c, name)

	out, _, err := dockerCmdWithError("exec", name, "no-such-cmd")
	assert.ErrorContains(c, err, "", out)

	expectedMsg := "executable file not found"
	if DaemonIsWindows() {
		expectedMsg = "The system cannot find the file specified"
	}
	assert.Assert(c, is.Contains(out, expectedMsg))
}

// Fix regression in https://github.com/moby/moby/pull/26461#issuecomment-250287297
func (s *DockerCLIExecSuite) TestExecWindowsPathNotWiped(c *testing.T) {
	testRequires(c, DaemonIsWindows)
	out := cli.DockerCmd(c, "run", "-d", "--name", "testing", minimalBaseImage(), "powershell", "start-sleep", "60").Stdout()
	cli.WaitRun(c, strings.TrimSpace(out))

	out = cli.DockerCmd(c, "exec", "testing", "powershell", "write-host", "$env:PATH").Stdout()
	out = strings.ToLower(strings.Trim(out, "\r\n"))
	assert.Assert(c, is.Contains(out, `windowspowershell\v1.0`))
}
