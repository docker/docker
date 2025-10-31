package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/stringid"
	"github.com/moby/moby/v2/integration-cli/cli"
	"github.com/moby/moby/v2/integration-cli/cli/build"
	"github.com/moby/moby/v2/integration-cli/daemon"
	"github.com/moby/moby/v2/internal/testutil"
	testdaemon "github.com/moby/moby/v2/internal/testutil/daemon"
	"github.com/moby/moby/v2/internal/testutil/fakecontext"
	"github.com/moby/moby/v2/internal/testutil/specialimage"
	"github.com/moby/sys/mountinfo"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/icmd"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

type DockerCLIRunSuite struct {
	ds *DockerSuite
}

func (s *DockerCLIRunSuite) TearDownTest(ctx context.Context, t *testing.T) {
	s.ds.TearDownTest(ctx, t)
}

func (s *DockerCLIRunSuite) OnTimeout(t *testing.T) {
	s.ds.OnTimeout(t)
}

// "test123" should be printed by docker run
func (s *DockerCLIRunSuite) TestRunEchoStdout(c *testing.T) {
	out := cli.DockerCmd(c, "run", "busybox", "echo", "test123").Combined()
	if out != "test123\n" {
		c.Fatalf("container should've printed 'test123', got '%s'", out)
	}
}

// "test" should be printed
func (s *DockerCLIRunSuite) TestRunEchoNamedContainer(c *testing.T) {
	out := cli.DockerCmd(c, "run", "--name", "testfoonamedcontainer", "busybox", "echo", "test").Combined()
	if out != "test\n" {
		c.Errorf("container should've printed 'test'")
	}
}

// docker run should not leak file descriptors. This test relies on Unix
// specific functionality and cannot run on Windows.
func (s *DockerCLIRunSuite) TestRunLeakyFileDescriptors(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	out := cli.DockerCmd(c, "run", "busybox", "ls", "-C", "/proc/self/fd").Combined()

	// normally, we should only get 0, 1, and 2, but 3 gets created by "ls" when it does "opendir" on the "fd" directory
	if out != "0  1  2  3\n" {
		c.Errorf("container should've printed '0  1  2  3', not: %s", out)
	}
}

// it should be possible to lookup Google DNS
// this will fail when Internet access is unavailable
func (s *DockerCLIRunSuite) TestRunLookupGoogleDNS(c *testing.T) {
	testRequires(c, Network)
	if testEnv.DaemonInfo.OSType == "windows" {
		// nslookup isn't present in Windows busybox. Is built-in. Further,
		// nslookup isn't present in nanoserver. Hence just use PowerShell...
		cli.DockerCmd(c, "run", testEnv.PlatformDefaults.BaseImage, "powershell", "Resolve-DNSName", "google.com")
	} else {
		cli.DockerCmd(c, "run", "busybox", "nslookup", "google.com")
	}
}

// the exit code should be 0
func (s *DockerCLIRunSuite) TestRunExitCodeZero(c *testing.T) {
	cli.DockerCmd(c, "run", "busybox", "true")
}

// the exit code should be 1
func (s *DockerCLIRunSuite) TestRunExitCodeOne(c *testing.T) {
	_, exitCode, err := dockerCmdWithError("run", "busybox", "false")
	assert.ErrorContains(c, err, "")
	assert.Equal(c, exitCode, 1)
}

// it should be possible to pipe in data via stdin to a process running in a container
func (s *DockerCLIRunSuite) TestRunStdinPipe(c *testing.T) {
	// TODO Windows: This needs some work to make compatible.
	testRequires(c, DaemonIsLinux)
	c.Skip("FIXME(thaJeztah): broken on current CLI versions due to change in behavior (does not stay attached or print container ID)")
	result := icmd.RunCmd(icmd.Cmd{
		Command: []string{dockerBinary, "run", "-i", "-a", "stdin", "busybox", "cat"},
		Stdin:   strings.NewReader("blahblah"),
	})
	result.Assert(c, icmd.Success)
	out := result.Stdout()

	out = strings.TrimSpace(out)
	cli.DockerCmd(c, "wait", out)

	containerLogs := cli.DockerCmd(c, "logs", out).Combined()
	containerLogs = strings.TrimSpace(containerLogs)
	if containerLogs != "blahblah" {
		c.Errorf("logs didn't print the container's logs %s", containerLogs)
	}

	cli.DockerCmd(c, "rm", out)
}

// the container's ID should be printed when starting a container in detached mode
func (s *DockerCLIRunSuite) TestRunDetachedContainerIDPrinting(c *testing.T) {
	id := cli.DockerCmd(c, "run", "-d", "busybox", "true").Stdout()
	id = strings.TrimSpace(id)
	cli.DockerCmd(c, "wait", id)

	rmOut := cli.DockerCmd(c, "rm", id).Stdout()
	rmOut = strings.TrimSpace(rmOut)
	if rmOut != id {
		c.Errorf("rm didn't print the container ID %s %s", id, rmOut)
	}
}

// the working directory should be set correctly
func (s *DockerCLIRunSuite) TestRunWorkingDirectory(c *testing.T) {
	dir := "/root"
	const imgName = "busybox"
	if testEnv.DaemonInfo.OSType == "windows" {
		dir = `C:/Windows`
	}

	// First with -w
	out := cli.DockerCmd(c, "run", "-w", dir, imgName, "pwd").Stdout()
	if strings.TrimSpace(out) != dir {
		c.Errorf("-w failed to set working directory")
	}

	// Then with --workdir
	out = cli.DockerCmd(c, "run", "--workdir", dir, imgName, "pwd").Stdout()
	if strings.TrimSpace(out) != dir {
		c.Errorf("--workdir failed to set working directory")
	}
}

// pinging Google's DNS resolver should fail when we disable the networking
func (s *DockerCLIRunSuite) TestRunWithoutNetworking(c *testing.T) {
	count := "-c"
	imgName := "busybox"
	if testEnv.DaemonInfo.OSType == "windows" {
		count = "-n"
		imgName = testEnv.PlatformDefaults.BaseImage
	}

	// First using the long form --net
	out, exitCode, err := dockerCmdWithError("run", "--net=none", imgName, "ping", count, "1", "8.8.8.8")
	if err != nil && exitCode != 1 {
		c.Fatal(out, err)
	}
	if exitCode != 1 {
		c.Errorf("--net=none should've disabled the network; the container shouldn't have been able to ping 8.8.8.8")
	}
}

// test --link use container name to link target
func (s *DockerCLIRunSuite) TestRunLinksContainerWithContainerName(c *testing.T) {
	// TODO Windows: This test cannot run on a Windows daemon as the networking
	// settings are not populated back yet on inspect.
	testRequires(c, DaemonIsLinux)
	cli.DockerCmd(c, "run", "-i", "-t", "-d", "--name", "parent", "busybox")

	ip := inspectField(c, "parent", "NetworkSettings.Networks.bridge.IPAddress")

	out := cli.DockerCmd(c, "run", "--link", "parent:test", "busybox", "/bin/cat", "/etc/hosts").Combined()
	if !strings.Contains(out, ip+"	test") {
		c.Fatalf("use a container name to link target failed")
	}
}

// test --link use container id to link target
func (s *DockerCLIRunSuite) TestRunLinksContainerWithContainerID(c *testing.T) {
	// TODO Windows: This test cannot run on a Windows daemon as the networking
	// settings are not populated back yet on inspect.
	testRequires(c, DaemonIsLinux)
	cID := cli.DockerCmd(c, "run", "-i", "-t", "-d", "busybox").Stdout()
	cID = strings.TrimSpace(cID)
	ip := inspectField(c, cID, "NetworkSettings.Networks.bridge.IPAddress")

	out := cli.DockerCmd(c, "run", "--link", cID+":test", "busybox", "/bin/cat", "/etc/hosts").Combined()
	if !strings.Contains(out, ip+"	test") {
		c.Fatalf("use a container id to link target failed")
	}
}

func (s *DockerCLIRunSuite) TestUserDefinedNetworkLinks(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	cli.DockerCmd(c, "network", "create", "-d", "bridge", "udlinkNet")

	cli.DockerCmd(c, "run", "-d", "--net=udlinkNet", "--name=first", "busybox", "top")
	cli.WaitRun(c, "first")

	// run a container in user-defined network udlinkNet with a link for an existing container
	// and a link for a container that doesn't exist
	cli.DockerCmd(c, "run", "-d", "--net=udlinkNet", "--name=second", "--link=first:foo", "--link=third:bar", "busybox", "top")
	cli.WaitRun(c, "second")

	// ping to first and its alias foo must succeed
	_, _, err := dockerCmdWithError("exec", "second", "ping", "-c", "1", "first")
	assert.NilError(c, err)
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "foo")
	assert.NilError(c, err)

	// ping to third and its alias must fail
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "third")
	assert.ErrorContains(c, err, "")
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "bar")
	assert.ErrorContains(c, err, "")

	// start third container now
	cli.DockerCmd(c, "run", "-d", "--net=udlinkNet", "--name=third", "busybox", "top")
	cli.WaitRun(c, "third")

	// ping to third and its alias must succeed now
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "third")
	assert.NilError(c, err)
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "bar")
	assert.NilError(c, err)
}

func (s *DockerCLIRunSuite) TestUserDefinedNetworkLinksWithRestart(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	cli.DockerCmd(c, "network", "create", "-d", "bridge", "udlinkNet")

	cli.DockerCmd(c, "run", "-d", "--net=udlinkNet", "--name=first", "busybox", "top")
	cli.WaitRun(c, "first")

	cli.DockerCmd(c, "run", "-d", "--net=udlinkNet", "--name=second", "--link=first:foo", "busybox", "top")
	cli.WaitRun(c, "second")

	// ping to first and its alias foo must succeed
	_, _, err := dockerCmdWithError("exec", "second", "ping", "-c", "1", "first")
	assert.NilError(c, err)
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "foo")
	assert.NilError(c, err)

	// Restart first container
	cli.DockerCmd(c, "restart", "first")
	cli.WaitRun(c, "first")

	// ping to first and its alias foo must still succeed
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "first")
	assert.NilError(c, err)
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "foo")
	assert.NilError(c, err)

	// Restart second container
	cli.DockerCmd(c, "restart", "second")
	cli.WaitRun(c, "second")

	// ping to first and its alias foo must still succeed
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "first")
	assert.NilError(c, err)
	_, _, err = dockerCmdWithError("exec", "second", "ping", "-c", "1", "foo")
	assert.NilError(c, err)
}

func (s *DockerCLIRunSuite) TestRunWithNetAliasOnDefaultNetworks(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace)

	defaults := []string{"bridge", "host", "none"}
	for _, nw := range defaults {
		c.Run(nw, func(t *testing.T) {
			out, _, err := dockerCmdWithError("run", "-d", "--net", nw, "--net-alias", "alias_"+nw, "busybox", "top")
			assert.ErrorContains(t, err, "")

			// TODO(thaJeztah): this validation should be on the daemon side (and already is?): https://github.com/moby/moby/blob/5856ec5348ccacf430f8b17fe8a6e30c579a7817/daemon/container_operations.go#L528-L539
			assert.Assert(t, is.Contains(out, "network-scoped aliases are only supported for user-defined networks"))
		})
	}
}

func (s *DockerCLIRunSuite) TestUserDefinedNetworkAlias(c *testing.T) {
	t := c
	testRequires(t, DaemonIsLinux, NotUserNamespace)
	cli.DockerCmd(t, "network", "create", "-d", "bridge", "net1")

	cid1 := cli.DockerCmd(t, "run", "-d", "--net=net1", "--name=first", "--net-alias=foo1", "--net-alias=foo2", "busybox:glibc", "top").Stdout()
	cid1 = strings.TrimSpace(cid1)
	cli.WaitRun(t, "first")

	// Check if default short-id alias is added automatically
	aliases := cli.Docker(
		cli.Args("container", "inspect", "--format", "{{.NetworkSettings.Networks.net1.Aliases}}", cid1),
		// API versions < v1.45 included the short-id in aliases. Newer versions only include it in DNSNames.
		cli.WithEnvironmentVariables("DOCKER_API_VERSION=1.44"),
	).Assert(t, icmd.Success).Stdout()
	assert.Assert(t, is.Contains(aliases, stringid.TruncateID(cid1)))
	assert.Assert(t, is.Contains(aliases, "foo1"))
	assert.Assert(t, is.Contains(aliases, "foo2"))

	cid2 := cli.DockerCmd(t, "run", "-d", "--net=net1", "--name=second", "busybox:glibc", "top").Stdout()
	cid2 = strings.TrimSpace(cid2)
	cli.WaitRun(t, "second")

	// Check if default short-id alias is added automatically
	aliases = cli.Docker(
		cli.Args("container", "inspect", "--format", "{{.NetworkSettings.Networks.net1.Aliases}}", cid2),
		// API versions < v1.45 included the short-id in aliases. Newer versions only include it in DNSNames.
		cli.WithEnvironmentVariables("DOCKER_API_VERSION=1.44"),
	).Assert(t, icmd.Success).Stdout()
	assert.Assert(t, is.Contains(aliases, stringid.TruncateID(cid2)))

	// ping to first and its network-scoped aliases
	cli.DockerCmd(t, "exec", "second", "ping", "-c", "1", "first")
	cli.DockerCmd(t, "exec", "second", "ping", "-c", "1", "foo1")
	cli.DockerCmd(t, "exec", "second", "ping", "-c", "1", "foo2")
	// ping first container's short-id alias
	cli.DockerCmd(t, "exec", "second", "ping", "-c", "1", stringid.TruncateID(cid1))

	// Restart first container
	cli.DockerCmd(t, "restart", "first")
	cli.WaitRun(t, "first")

	// ping to first and its network-scoped aliases must succeed
	cli.DockerCmd(t, "exec", "second", "ping", "-c", "1", "first")
	cli.DockerCmd(t, "exec", "second", "ping", "-c", "1", "foo1")
	cli.DockerCmd(t, "exec", "second", "ping", "-c", "1", "foo2")
	// ping first container's short-id alias
	cli.DockerCmd(t, "exec", "second", "ping", "-c", "1", stringid.TruncateID(cid1))
}

// Issue 9677.
func (s *DockerCLIRunSuite) TestRunWithDaemonFlags(c *testing.T) {
	out, _, err := dockerCmdWithError("--exec-opt", "foo=bar", "run", "-i", "busybox", "true")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, is.Contains(out, "unknown flag: --exec-opt"))
}

// Regression test for #4979
func (s *DockerCLIRunSuite) TestRunWithVolumesFromExited(c *testing.T) {
	var result *icmd.Result

	// Create a file in a volume
	if testEnv.DaemonInfo.OSType == "windows" {
		result = cli.DockerCmd(c, "run", "--name", "test-data", "--volume", `c:\some\dir`, testEnv.PlatformDefaults.BaseImage, "cmd", "/c", `echo hello > c:\some\dir\file`)
	} else {
		result = cli.DockerCmd(c, "run", "--name", "test-data", "--volume", "/some/dir", "busybox", "touch", "/some/dir/file")
	}
	if result.ExitCode != 0 {
		c.Fatal("1", result.Combined(), result.ExitCode)
	}

	// Read the file from another container using --volumes-from to access the volume in the second container
	if testEnv.DaemonInfo.OSType == "windows" {
		result = cli.DockerCmd(c, "run", "--volumes-from", "test-data", testEnv.PlatformDefaults.BaseImage, "cmd", "/c", `type c:\some\dir\file`)
	} else {
		result = cli.DockerCmd(c, "run", "--volumes-from", "test-data", "busybox", "cat", "/some/dir/file")
	}
	if result.ExitCode != 0 {
		c.Fatal("2", result.Combined(), result.ExitCode)
	}
}

// Volume path is a symlink which also exists on the host, and the host side is a file not a dir
// But the volume call is just a normal volume, not a bind mount
func (s *DockerCLIRunSuite) TestRunCreateVolumesInSymlinkDir(c *testing.T) {
	var (
		dockerFile    string
		containerPath string
		cmd           string
	)
	// This test cannot run on a Windows daemon as
	// Windows does not support symlinks inside a volume path
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)
	name := "test-volume-symlink"

	dir, err := os.MkdirTemp("", name)
	if err != nil {
		c.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// In the case of Windows to Windows CI, if the machine is setup so that
	// the temp directory is not the C: drive, this test is invalid and will
	// not work.
	if testEnv.DaemonInfo.OSType == "windows" && strings.ToLower(dir[:1]) != "c" {
		c.Skip("Requires TEMP to point to C: drive")
	}

	f, err := os.OpenFile(filepath.Join(dir, "test"), os.O_CREATE, 0o600)
	if err != nil {
		c.Fatal(err)
	}
	f.Close()

	if testEnv.DaemonInfo.OSType == "windows" {
		dockerFile = fmt.Sprintf("FROM %s\nRUN mkdir %s\nRUN mklink /D c:\\test %s", testEnv.PlatformDefaults.BaseImage, dir, dir)
		containerPath = `c:\test\test`
		cmd = "tasklist"
	} else {
		dockerFile = fmt.Sprintf("FROM busybox\nRUN mkdir -p %s\nRUN ln -s %s /test", dir, dir)
		containerPath = "/test/test"
		cmd = "true"
	}
	cli.BuildCmd(c, name, build.WithDockerfile(dockerFile))
	cli.DockerCmd(c, "run", "-v", containerPath, name, cmd)
}

// Volume path is a symlink in the container
func (s *DockerCLIRunSuite) TestRunCreateVolumesInSymlinkDir2(c *testing.T) {
	var (
		dockerFile    string
		containerPath string
		cmd           string
	)
	// This test cannot run on a Windows daemon as
	// Windows does not support symlinks inside a volume path
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)
	name := "test-volume-symlink2"

	if testEnv.DaemonInfo.OSType == "windows" {
		dockerFile = fmt.Sprintf("FROM %s\nRUN mkdir c:\\%s\nRUN mklink /D c:\\test c:\\%s", testEnv.PlatformDefaults.BaseImage, name, name)
		containerPath = `c:\test\test`
		cmd = "tasklist"
	} else {
		dockerFile = fmt.Sprintf("FROM busybox\nRUN mkdir -p /%s\nRUN ln -s /%s /test", name, name)
		containerPath = "/test/test"
		cmd = "true"
	}
	cli.BuildCmd(c, name, build.WithDockerfile(dockerFile))
	cli.DockerCmd(c, "run", "-v", containerPath, name, cmd)
}

func (s *DockerCLIRunSuite) TestRunVolumesMountedAsReadonly(c *testing.T) {
	if _, code, err := dockerCmdWithError("run", "-v", "/test:/test:ro", "busybox", "touch", "/test/somefile"); err == nil || code == 0 {
		c.Fatalf("run should fail because volume is ro: exit code %d", code)
	}
}

func (s *DockerCLIRunSuite) TestRunVolumesFromInReadonlyModeFails(c *testing.T) {
	var (
		volumeDir string
		fileInVol string
	)
	if testEnv.DaemonInfo.OSType == "windows" {
		volumeDir = `c:/test` // Forward-slash as using busybox
		fileInVol = `c:/test/file`
	} else {
		testRequires(c, DaemonIsLinux)
		volumeDir = "/test"
		fileInVol = `/test/file`
	}
	cli.DockerCmd(c, "run", "--name", "parent", "-v", volumeDir, "busybox", "true")

	if _, code, err := dockerCmdWithError("run", "--volumes-from", "parent:ro", "busybox", "touch", fileInVol); err == nil || code == 0 {
		c.Fatalf("run should fail because volume is ro: exit code %d", code)
	}
}

// Regression test for #1201
func (s *DockerCLIRunSuite) TestRunVolumesFromInReadWriteMode(c *testing.T) {
	var (
		volumeDir string
		fileInVol string
	)
	if testEnv.DaemonInfo.OSType == "windows" {
		volumeDir = `c:/test` // Forward-slash as using busybox
		fileInVol = `c:/test/file`
	} else {
		volumeDir = "/test"
		fileInVol = "/test/file"
	}

	cli.DockerCmd(c, "run", "--name", "parent", "-v", volumeDir, "busybox", "true")
	cli.DockerCmd(c, "run", "--volumes-from", "parent:rw", "busybox", "touch", fileInVol)

	if out, _, err := dockerCmdWithError("run", "--volumes-from", "parent:bar", "busybox", "touch", fileInVol); err == nil || !strings.Contains(out, `invalid mode: bar`) {
		c.Fatalf("running --volumes-from parent:bar should have failed with invalid mode: %q", out)
	}

	cli.DockerCmd(c, "run", "--volumes-from", "parent", "busybox", "touch", fileInVol)
}

func (s *DockerCLIRunSuite) TestVolumesFromGetsProperMode(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)
	hostpath := RandomTmpDirPath("test", testEnv.DaemonInfo.OSType)
	if err := os.MkdirAll(hostpath, 0o755); err != nil {
		c.Fatalf("Failed to create %s: %q", hostpath, err)
	}
	defer os.RemoveAll(hostpath)

	cli.DockerCmd(c, "run", "--name", "parent", "-v", hostpath+":"+dPath("/test")+":ro", "busybox", "true")

	// Expect this "rw" mode to be ignored since the inherited volume is "ro"
	if _, _, err := dockerCmdWithError("run", "--volumes-from", "parent:rw", "busybox", "touch", dPath("/test/file")); err == nil {
		c.Fatal("Expected volumes-from to inherit read-only volume even when passing in `rw`")
	}

	cli.DockerCmd(c, "run", "--name", "parent2", "-v", hostpath+":"+dPath("/test")+":ro", "busybox", "true")

	// Expect this to be read-only since both are "ro"
	if _, _, err := dockerCmdWithError("run", "--volumes-from", "parent2:ro", "busybox", "touch", dPath("/test/file")); err == nil {
		c.Fatal("Expected volumes-from to inherit read-only volume even when passing in `ro`")
	}
}

// Test for GH#10618
func (s *DockerCLIRunSuite) TestRunNoDupVolumes(c *testing.T) {
	path1 := RandomTmpDirPath("test1", testEnv.DaemonInfo.OSType)
	path2 := RandomTmpDirPath("test2", testEnv.DaemonInfo.OSType)

	someplace := ":/someplace"
	if testEnv.DaemonInfo.OSType == "windows" {
		// Windows requires that the source directory exists before calling HCS
		testRequires(c, testEnv.IsLocalDaemon)
		someplace = `:c:\someplace`
		if err := os.MkdirAll(path1, 0o755); err != nil {
			c.Fatalf("Failed to create %s: %q", path1, err)
		}
		defer os.RemoveAll(path1)
		if err := os.MkdirAll(path2, 0o755); err != nil {
			c.Fatalf("Failed to create %s: %q", path1, err)
		}
		defer os.RemoveAll(path2)
	}
	mountstr1 := path1 + someplace
	mountstr2 := path2 + someplace

	if out, _, err := dockerCmdWithError("run", "-v", mountstr1, "-v", mountstr2, "busybox", "true"); err == nil {
		c.Fatal("Expected error about duplicate mount definitions")
	} else {
		if !strings.Contains(out, "Duplicate mount point") {
			c.Fatalf("Expected 'duplicate mount point' error, got %v", out)
		}
	}

	// Test for https://github.com/moby/moby/issues/22093
	volumename1 := "test1"
	volumename2 := "test2"
	volume1 := volumename1 + someplace
	volume2 := volumename2 + someplace
	if out, _, err := dockerCmdWithError("run", "-v", volume1, "-v", volume2, "busybox", "true"); err == nil {
		c.Fatal("Expected error about duplicate mount definitions")
	} else {
		if !strings.Contains(out, "Duplicate mount point") {
			c.Fatalf("Expected 'duplicate mount point' error, got %v", out)
		}
	}
	// create failed should have create volume volumename1 or volumename2
	// we should remove volumename2 or volumename2 successfully
	out := cli.DockerCmd(c, "volume", "ls").Stdout()
	if strings.Contains(out, volumename1) {
		cli.DockerCmd(c, "volume", "rm", volumename1)
	} else {
		cli.DockerCmd(c, "volume", "rm", volumename2)
	}
}

// Test for #1351
func (s *DockerCLIRunSuite) TestRunApplyVolumesFromBeforeVolumes(c *testing.T) {
	prefix := ""
	if testEnv.DaemonInfo.OSType == "windows" {
		prefix = `c:`
	}
	cli.DockerCmd(c, "run", "--name", "parent", "-v", prefix+"/test", "busybox", "touch", prefix+"/test/foo")
	cli.DockerCmd(c, "run", "--volumes-from", "parent", "-v", prefix+"/test", "busybox", "cat", prefix+"/test/foo")
}

func (s *DockerCLIRunSuite) TestRunMultipleVolumesFrom(c *testing.T) {
	prefix := ""
	if testEnv.DaemonInfo.OSType == "windows" {
		prefix = `c:`
	}
	cli.DockerCmd(c, "run", "--name", "parent1", "-v", prefix+"/test", "busybox", "touch", prefix+"/test/foo")
	cli.DockerCmd(c, "run", "--name", "parent2", "-v", prefix+"/other", "busybox", "touch", prefix+"/other/bar")
	cli.DockerCmd(c, "run", "--volumes-from", "parent1", "--volumes-from", "parent2", "busybox", "sh", "-c", "cat /test/foo && cat /other/bar")
}

// this tests verifies the ID format for the container
func (s *DockerCLIRunSuite) TestRunVerifyContainerID(c *testing.T) {
	out, exit, err := dockerCmdWithError("run", "-d", "busybox", "true")
	if err != nil {
		c.Fatal(err)
	}
	if exit != 0 {
		c.Fatalf("expected exit code 0 received %d", exit)
	}

	match, err := regexp.MatchString("^[0-9a-f]{64}$", strings.TrimSuffix(out, "\n"))
	if err != nil {
		c.Fatal(err)
	}
	if !match {
		c.Fatalf("Invalid container ID: %s", out)
	}
}

// Test that creating a container with a volume doesn't crash. Regression test for #995.
func (s *DockerCLIRunSuite) TestRunCreateVolume(c *testing.T) {
	prefix := ""
	if testEnv.DaemonInfo.OSType == "windows" {
		prefix = `c:`
	}
	cli.DockerCmd(c, "run", "-v", prefix+"/var/lib/data", "busybox", "true")
}

// Test that creating a volume with a symlink in its path works correctly. Test for #5152.
// Note that this bug happens only with symlinks with a target that starts with '/'.
func (s *DockerCLIRunSuite) TestRunCreateVolumeWithSymlink(c *testing.T) {
	// Cannot run on Windows as relies on Linux-specific functionality (sh -c mount...)
	testRequires(c, DaemonIsLinux)
	workingDirectory, err := os.MkdirTemp("", "TestRunCreateVolumeWithSymlink")
	assert.NilError(c, err)
	const imgName = "docker-test-createvolumewithsymlink"

	buildCmd := exec.Command(dockerBinary, "build", "-t", imgName, "-")
	buildCmd.Stdin = strings.NewReader(`FROM busybox
		RUN ln -s home /bar`)
	buildCmd.Dir = workingDirectory
	err = buildCmd.Run()
	if err != nil {
		c.Fatalf("could not build '%s': %v", imgName, err)
	}

	_, exitCode, err := dockerCmdWithError("run", "-v", "/bar/foo", "--name", "test-createvolumewithsymlink", imgName, "sh", "-c", "mount | grep -q /home/foo")
	if err != nil || exitCode != 0 {
		c.Fatalf("[run] err: %v, exitcode: %d", err, exitCode)
	}

	mnt, err := inspectMountPoint("test-createvolumewithsymlink", "/bar/foo")
	assert.NilError(c, err)

	_, exitCode, err = dockerCmdWithError("rm", "-v", "test-createvolumewithsymlink")
	if err != nil || exitCode != 0 {
		c.Fatalf("[rm] err: %v, exitcode: %d", err, exitCode)
	}

	_, err = os.Stat(mnt.Source)
	if !os.IsNotExist(err) {
		c.Fatalf("[open] (expecting 'file does not exist' error) err: %v, mnt.Source: %s", err, mnt.Source)
	}
}

// Tests that a volume path that has a symlink exists in a container mounting it with `--volumes-from`.
func (s *DockerCLIRunSuite) TestRunVolumesFromSymlinkPath(c *testing.T) {
	// This test cannot run on a Windows daemon as
	// Windows does not support symlinks inside a volume path
	testRequires(c, DaemonIsLinux)

	workingDirectory, err := os.MkdirTemp("", "TestRunVolumesFromSymlinkPath")
	assert.NilError(c, err)
	name := "docker-test-volumesfromsymlinkpath"
	prefix := ""
	dfContents := `FROM busybox
		RUN ln -s home /foo
		VOLUME ["/foo/bar"]`

	if testEnv.DaemonInfo.OSType == "windows" {
		prefix = `c:`
		dfContents = `FROM ` + testEnv.PlatformDefaults.BaseImage + `
	    RUN mkdir c:\home
		RUN mklink /D c:\foo c:\home
		VOLUME ["c:/foo/bar"]
		ENTRYPOINT c:\windows\system32\cmd.exe`
	}

	buildCmd := exec.Command(dockerBinary, "build", "-t", name, "-")
	buildCmd.Stdin = strings.NewReader(dfContents)
	buildCmd.Dir = workingDirectory
	err = buildCmd.Run()
	if err != nil {
		c.Fatalf("could not build 'docker-test-volumesfromsymlinkpath': %v", err)
	}

	out, exitCode, err := dockerCmdWithError("run", "--name", "test-volumesfromsymlinkpath", name)
	if err != nil || exitCode != 0 {
		c.Fatalf("[run] (volume) err: %v, exitcode: %d, out: %s", err, exitCode, out)
	}

	_, exitCode, err = dockerCmdWithError("run", "--volumes-from", "test-volumesfromsymlinkpath", "busybox", "sh", "-c", "ls "+prefix+"/foo | grep -q bar")
	if err != nil || exitCode != 0 {
		c.Fatalf("[run] err: %v, exitcode: %d", err, exitCode)
	}
}

func (s *DockerCLIRunSuite) TestRunExitCode(c *testing.T) {
	var (
		exit int
		err  error
	)

	_, exit, err = dockerCmdWithError("run", "busybox", "/bin/sh", "-c", "exit 72")

	if err == nil {
		c.Fatal("should not have a non nil error")
	}
	if exit != 72 {
		c.Fatalf("expected exit code 72 received %d", exit)
	}
}

func (s *DockerCLIRunSuite) TestRunUserDefaults(c *testing.T) {
	expected := "uid=0(root) gid=0(root)"
	if testEnv.DaemonInfo.OSType == "windows" {
		expected = "uid=0(root) gid=0(root) groups=0(root)"
	}
	out := cli.DockerCmd(c, "run", "busybox", "id").Stdout()
	if !strings.Contains(out, expected) {
		c.Fatalf("expected '%s' got %s", expected, out)
	}
}

func (s *DockerCLIRunSuite) TestRunUserByName(c *testing.T) {
	// TODO Windows: This test cannot run on a Windows daemon as Windows does
	// not support the use of -u
	testRequires(c, DaemonIsLinux)
	out := cli.DockerCmd(c, "run", "-u", "root", "busybox", "id").Stdout()
	if !strings.Contains(out, "uid=0(root) gid=0(root)") {
		c.Fatalf("expected root user got %s", out)
	}
}

func (s *DockerCLIRunSuite) TestRunUserByID(c *testing.T) {
	// TODO Windows: This test cannot run on a Windows daemon as Windows does
	// not support the use of -u
	testRequires(c, DaemonIsLinux)
	out := cli.DockerCmd(c, "run", "-u", "1", "busybox", "id").Stdout()
	if !strings.Contains(out, "uid=1(daemon) gid=1(daemon)") {
		c.Fatalf("expected daemon user got %s", out)
	}
}

func (s *DockerCLIRunSuite) TestRunUserByIDBig(c *testing.T) {
	// TODO Windows: This test cannot run on a Windows daemon as Windows does
	// not support the use of -u
	testRequires(c, DaemonIsLinux)
	out, _, err := dockerCmdWithError("run", "-u", "2147483648", "busybox", "id")
	if err == nil {
		c.Fatal("No error, but must be.", out)
	}
	if !strings.Contains(strings.ToLower(out), "uids and gids must be in range") {
		c.Fatalf("expected error about uids range, got %s", out)
	}
}

func (s *DockerCLIRunSuite) TestRunUserByIDNegative(c *testing.T) {
	// TODO Windows: This test cannot run on a Windows daemon as Windows does
	// not support the use of -u
	testRequires(c, DaemonIsLinux)
	out, _, err := dockerCmdWithError("run", "-u", "-1", "busybox", "id")
	if err == nil {
		c.Fatal("No error, but must be.", out)
	}
	if !strings.Contains(strings.ToLower(out), "uids and gids must be in range") {
		c.Fatalf("expected error about uids range, got %s", out)
	}
}

func (s *DockerCLIRunSuite) TestRunUserByIDZero(c *testing.T) {
	// TODO Windows: This test cannot run on a Windows daemon as Windows does
	// not support the use of -u
	testRequires(c, DaemonIsLinux)
	out, _, err := dockerCmdWithError("run", "-u", "0", "busybox", "id")
	if err != nil {
		c.Fatal(err, out)
	}
	if !strings.Contains(out, "uid=0(root) gid=0(root) groups=0(root),10(wheel)") {
		c.Fatalf("expected daemon user got %s", out)
	}
}

func (s *DockerCLIRunSuite) TestRunUserNotFound(c *testing.T) {
	// TODO Windows: This test cannot run on a Windows daemon as Windows does
	// not support the use of -u
	testRequires(c, DaemonIsLinux)
	_, _, err := dockerCmdWithError("run", "-u", "notme", "busybox", "id")
	if err == nil {
		c.Fatal("unknown user should cause container to fail")
	}
}

func (s *DockerCLIRunSuite) TestRunTwoConcurrentContainers(c *testing.T) {
	sleepTime := "2"
	group := sync.WaitGroup{}
	group.Add(2)

	errChan := make(chan error, 2)
	for range 2 {
		go func() {
			defer group.Done()
			_, _, err := dockerCmdWithError("run", "busybox", "sleep", sleepTime)
			errChan <- err
		}()
	}

	group.Wait()
	close(errChan)

	for err := range errChan {
		assert.NilError(c, err)
	}
}

func (s *DockerCLIRunSuite) TestRunEnvironment(c *testing.T) {
	// TODO Windows: Environment handling is different between Linux and
	// Windows and this test relies currently on unix functionality.
	testRequires(c, DaemonIsLinux)
	result := icmd.RunCmd(icmd.Cmd{
		Command: []string{dockerBinary, "run", "-h", "testing", "-e=FALSE=true", "-e=TRUE", "-e=TRICKY", "-e=HOME=", "busybox", "env"},
		Env: append(os.Environ(),
			"TRUE=false",
			"TRICKY=tri\ncky\n",
		),
	})
	result.Assert(c, icmd.Success)

	actualEnv := strings.Split(strings.TrimSuffix(result.Stdout(), "\n"), "\n")
	goodEnv := []string{
		// The first two should not be tested here, those are "inherent" environment variable. This test validates
		// the -e behavior, not the default environment variable (that could be subject to change)
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOSTNAME=testing",
		"FALSE=true",
		"TRUE=false",
		"TRICKY=tri",
		"cky",
		"",
		"HOME=/root",
	}
	assert.DeepEqual(c, actualEnv, goodEnv, cmpopts.SortSlices(func(a, b string) bool {
		return a < b
	}))
}

func (s *DockerCLIRunSuite) TestRunEnvironmentErase(c *testing.T) {
	// TODO Windows: Environment handling is different between Linux and
	// Windows and this test relies currently on unix functionality.
	testRequires(c, DaemonIsLinux)

	// Test to make sure that when we use -e on env vars that are
	// not set in our local env that they're removed (if present) in
	// the container

	result := icmd.RunCmd(icmd.Cmd{
		Command: []string{dockerBinary, "run", "-e", "FOO", "-e", "HOSTNAME", "busybox", "env"},
		Env:     appendBaseEnv(true),
	})
	result.Assert(c, icmd.Success)

	actualEnv := strings.Split(strings.TrimSpace(result.Combined()), "\n")
	goodEnv := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
	}
	assert.DeepEqual(c, actualEnv, goodEnv, cmpopts.SortSlices(func(a, b string) bool {
		return a < b
	}))
}

func (s *DockerCLIRunSuite) TestRunEnvironmentOverride(c *testing.T) {
	// TODO Windows: Environment handling is different between Linux and
	// Windows and this test relies currently on unix functionality.
	testRequires(c, DaemonIsLinux)

	// Test to make sure that when we use -e on env vars that are
	// already in the env that we're overriding them

	result := icmd.RunCmd(icmd.Cmd{
		Command: []string{dockerBinary, "run", "-e", "HOSTNAME", "-e", "HOME=/root2", "busybox", "env"},
		Env:     appendBaseEnv(true, "HOSTNAME=bar"),
	})
	result.Assert(c, icmd.Success)

	actualEnv := strings.Split(strings.TrimSpace(result.Combined()), "\n")
	goodEnv := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root2",
		"HOSTNAME=bar",
	}
	assert.DeepEqual(c, actualEnv, goodEnv, cmpopts.SortSlices(func(a, b string) bool {
		return a < b
	}))
}

func (s *DockerCLIRunSuite) TestRunContainerNetwork(c *testing.T) {
	if testEnv.DaemonInfo.OSType == "windows" {
		// Windows busybox does not have ping. Use built in ping instead.
		cli.DockerCmd(c, "run", testEnv.PlatformDefaults.BaseImage, "ping", "-n", "1", "127.0.0.1")
	} else {
		cli.DockerCmd(c, "run", "busybox", "ping", "-c", "1", "127.0.0.1")
	}
}

func (s *DockerCLIRunSuite) TestRunNetHostNotAllowedWithLinks(c *testing.T) {
	// TODO Windows: This is Linux specific as --link is not supported and
	// this will be deprecated in favor of container networking model.
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	cli.DockerCmd(c, "run", "--name", "linked", "busybox", "true")

	_, _, err := dockerCmdWithError("run", "--net=host", "--link", "linked:linked", "busybox", "true")
	if err == nil {
		c.Fatal("Expected error")
	}
}

// #7851 hostname outside container shows FQDN, inside only shortname
// For testing purposes it is not required to set host's hostname directly
// and use "--net=host" (as the original issue submitter did), as the same
// codepath is executed with "docker run -h <hostname>".  Both were manually
// tested, but this testcase takes the simpler path of using "run -h .."
func (s *DockerCLIRunSuite) TestRunFullHostnameSet(c *testing.T) {
	// TODO Windows: -h is not yet functional.
	testRequires(c, DaemonIsLinux)
	out := cli.DockerCmd(c, "run", "-h", "foo.bar.baz", "busybox", "hostname").Combined()
	if actual := strings.Trim(out, "\r\n"); actual != "foo.bar.baz" {
		c.Fatalf("expected hostname 'foo.bar.baz', received %s", actual)
	}
}

func (s *DockerCLIRunSuite) TestRunPrivilegedCanMknod(c *testing.T) {
	// Not applicable for Windows as Windows daemon does not support
	// the concept of --privileged, and mknod is a Unix concept.
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	out := cli.DockerCmd(c, "run", "--privileged", "busybox", "sh", "-c", "mknod /tmp/sda b 8 0 && echo ok").Combined()
	if actual := strings.Trim(out, "\r\n"); actual != "ok" {
		c.Fatalf("expected output ok received %s", actual)
	}
}

func (s *DockerCLIRunSuite) TestRunUnprivilegedCanMknod(c *testing.T) {
	// Not applicable for Windows as Windows daemon does not support
	// the concept of --privileged, and mknod is a Unix concept.
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	out := cli.DockerCmd(c, "run", "busybox", "sh", "-c", "mknod /tmp/sda b 8 0 && echo ok").Combined()
	if actual := strings.Trim(out, "\r\n"); actual != "ok" {
		c.Fatalf("expected output ok received %s", actual)
	}
}

func (s *DockerCLIRunSuite) TestRunCapDropInvalid(c *testing.T) {
	// Not applicable for Windows as there is no concept of --cap-drop
	testRequires(c, DaemonIsLinux)
	out, _, err := dockerCmdWithError("run", "--cap-drop=CHPASS", "busybox", "ls")
	if err == nil {
		c.Fatal(err, out)
	}
}

func (s *DockerCLIRunSuite) TestRunCapDropCannotMknod(c *testing.T) {
	// Not applicable for Windows as there is no concept of --cap-drop or mknod
	testRequires(c, DaemonIsLinux)
	out, _, err := dockerCmdWithError("run", "--cap-drop=MKNOD", "busybox", "sh", "-c", "mknod /tmp/sda b 8 0 && echo ok")

	if err == nil {
		c.Fatal(err, out)
	}
	if actual := strings.Trim(out, "\r\n"); actual == "ok" {
		c.Fatalf("expected output not ok received %s", actual)
	}
}

func (s *DockerCLIRunSuite) TestRunCapDropCannotMknodLowerCase(c *testing.T) {
	// Not applicable for Windows as there is no concept of --cap-drop or mknod
	testRequires(c, DaemonIsLinux)
	out, _, err := dockerCmdWithError("run", "--cap-drop=mknod", "busybox", "sh", "-c", "mknod /tmp/sda b 8 0 && echo ok")

	if err == nil {
		c.Fatal(err, out)
	}
	if actual := strings.Trim(out, "\r\n"); actual == "ok" {
		c.Fatalf("expected output not ok received %s", actual)
	}
}

func (s *DockerCLIRunSuite) TestRunCapDropALLCannotMknod(c *testing.T) {
	// Not applicable for Windows as there is no concept of --cap-drop or mknod
	testRequires(c, DaemonIsLinux)
	out, _, err := dockerCmdWithError("run", "--cap-drop=ALL", "--cap-add=SETGID", "busybox", "sh", "-c", "mknod /tmp/sda b 8 0 && echo ok")
	if err == nil {
		c.Fatal(err, out)
	}
	if actual := strings.Trim(out, "\r\n"); actual == "ok" {
		c.Fatalf("expected output not ok received %s", actual)
	}
}

func (s *DockerCLIRunSuite) TestRunCapDropALLAddMknodCanMknod(c *testing.T) {
	// Not applicable for Windows as there is no concept of --cap-drop or mknod
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	out := cli.DockerCmd(c, "run", "--cap-drop=ALL", "--cap-add=MKNOD", "--cap-add=SETGID", "busybox", "sh", "-c", "mknod /tmp/sda b 8 0 && echo ok").Combined()

	if actual := strings.Trim(out, "\r\n"); actual != "ok" {
		c.Fatalf("expected output ok received %s", actual)
	}
}

func (s *DockerCLIRunSuite) TestRunCapAddInvalid(c *testing.T) {
	// Not applicable for Windows as there is no concept of --cap-add
	testRequires(c, DaemonIsLinux)
	out, _, err := dockerCmdWithError("run", "--cap-add=CHPASS", "busybox", "ls")
	if err == nil {
		c.Fatal(err, out)
	}
}

func (s *DockerCLIRunSuite) TestRunCapAddCanDownInterface(c *testing.T) {
	// Not applicable for Windows as there is no concept of --cap-add
	testRequires(c, DaemonIsLinux)
	out := cli.DockerCmd(c, "run", "--cap-add=NET_ADMIN", "busybox", "sh", "-c", "ip link set eth0 down && echo ok").Combined()

	if actual := strings.Trim(out, "\r\n"); actual != "ok" {
		c.Fatalf("expected output ok received %s", actual)
	}
}

func (s *DockerCLIRunSuite) TestRunCapAddALLCanDownInterface(c *testing.T) {
	// Not applicable for Windows as there is no concept of --cap-add
	testRequires(c, DaemonIsLinux)
	out := cli.DockerCmd(c, "run", "--cap-add=ALL", "busybox", "sh", "-c", "ip link set eth0 down && echo ok").Combined()

	if actual := strings.Trim(out, "\r\n"); actual != "ok" {
		c.Fatalf("expected output ok received %s", actual)
	}
}

func (s *DockerCLIRunSuite) TestRunCapAddALLDropNetAdminCanDownInterface(c *testing.T) {
	// Not applicable for Windows as there is no concept of --cap-add
	testRequires(c, DaemonIsLinux)
	out, _, err := dockerCmdWithError("run", "--cap-add=ALL", "--cap-drop=NET_ADMIN", "busybox", "sh", "-c", "ip link set eth0 down && echo ok")
	if err == nil {
		c.Fatal(err, out)
	}
	if actual := strings.Trim(out, "\r\n"); actual == "ok" {
		c.Fatalf("expected output not ok received %s", actual)
	}
}

func (s *DockerCLIRunSuite) TestRunGroupAdd(c *testing.T) {
	// Not applicable for Windows as there is no concept of --group-add
	testRequires(c, DaemonIsLinux)
	out := cli.DockerCmd(c, "run", "--group-add=audio", "--group-add=staff", "--group-add=777", "busybox", "sh", "-c", "id").Combined()

	groupsList := "uid=0(root) gid=0(root) groups=0(root),10(wheel),29(audio),50(staff),777"
	if actual := strings.Trim(out, "\r\n"); actual != groupsList {
		c.Fatalf("expected output %s received %s", groupsList, actual)
	}
}

func (s *DockerCLIRunSuite) TestRunPrivilegedCanMount(c *testing.T) {
	// Not applicable for Windows as there is no concept of --privileged
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	out := cli.DockerCmd(c, "run", "--privileged", "busybox", "sh", "-c", "mount -t tmpfs none /tmp && echo ok").Combined()

	if actual := strings.Trim(out, "\r\n"); actual != "ok" {
		c.Fatalf("expected output ok received %s", actual)
	}
}

func (s *DockerCLIRunSuite) TestRunUnprivilegedCannotMount(c *testing.T) {
	// Not applicable for Windows as there is no concept of unprivileged
	testRequires(c, DaemonIsLinux)
	out, _, err := dockerCmdWithError("run", "busybox", "sh", "-c", "mount -t tmpfs none /tmp && echo ok")

	if err == nil {
		c.Fatal(err, out)
	}
	if actual := strings.Trim(out, "\r\n"); actual == "ok" {
		c.Fatalf("expected output not ok received %s", actual)
	}
}

func (s *DockerCLIRunSuite) TestRunSysNotWritableInNonPrivilegedContainers(c *testing.T) {
	// Not applicable for Windows as there is no concept of unprivileged
	testRequires(c, DaemonIsLinux)
	if _, code, err := dockerCmdWithError("run", "busybox", "touch", "/sys/kernel/profiling"); err == nil || code == 0 {
		c.Fatal("sys should not be writable in a non privileged container")
	}
}

func (s *DockerCLIRunSuite) TestRunSysWritableInPrivilegedContainers(c *testing.T) {
	// Not applicable for Windows as there is no concept of unprivileged
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	if _, code, err := dockerCmdWithError("run", "--privileged", "busybox", "touch", "/sys/kernel/profiling"); err != nil || code != 0 {
		c.Fatalf("sys should be writable in privileged container")
	}
}

func (s *DockerCLIRunSuite) TestRunProcNotWritableInNonPrivilegedContainers(c *testing.T) {
	// Not applicable for Windows as there is no concept of unprivileged
	testRequires(c, DaemonIsLinux)
	if _, code, err := dockerCmdWithError("run", "busybox", "touch", "/proc/sysrq-trigger"); err == nil || code == 0 {
		c.Fatal("proc should not be writable in a non privileged container")
	}
}

func (s *DockerCLIRunSuite) TestRunProcWritableInPrivilegedContainers(c *testing.T) {
	// Not applicable for Windows as there is no concept of --privileged
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	if result := cli.DockerCmd(c, "run", "--privileged", "busybox", "sh", "-c", "touch /proc/sysrq-trigger"); result.ExitCode != 0 {
		c.Fatalf("proc should be writable in privileged container")
	}
}

func (s *DockerCLIRunSuite) TestRunDeviceNumbers(c *testing.T) {
	// Not applicable on Windows as /dev/ is a Unix specific concept
	// TODO: NotUserNamespace could be removed here if "root" "root" is replaced w user
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	out := cli.DockerCmd(c, "run", "busybox", "sh", "-c", "ls -l /dev/null").Combined()
	deviceLineFields := strings.Fields(out)
	deviceLineFields[6] = ""
	deviceLineFields[7] = ""
	deviceLineFields[8] = ""
	expected := []string{"crw-rw-rw-", "1", "root", "root", "1,", "3", "", "", "", "/dev/null"}

	if !(reflect.DeepEqual(deviceLineFields, expected)) {
		c.Fatalf("expected output\ncrw-rw-rw- 1 root root 1, 3 May 24 13:29 /dev/null\n received\n %s\n", out)
	}
}

func (s *DockerCLIRunSuite) TestRunThatCharacterDevicesActLikeCharacterDevices(c *testing.T) {
	// Not applicable on Windows as /dev/ is a Unix specific concept
	testRequires(c, DaemonIsLinux)
	out := cli.DockerCmd(c, "run", "busybox", "sh", "-c", "dd if=/dev/zero of=/zero bs=1k count=5 2> /dev/null ; du -h /zero").Combined()
	if actual := strings.Trim(out, "\r\n"); actual[0] == '0' {
		c.Fatalf("expected a new file called /zero to be create that is greater than 0 bytes long, but du says: %s", actual)
	}
}

func (s *DockerCLIRunSuite) TestRunUnprivilegedWithChroot(c *testing.T) {
	// Not applicable on Windows as it does not support chroot
	testRequires(c, DaemonIsLinux)
	cli.DockerCmd(c, "run", "busybox", "chroot", "/", "true")
}

func (s *DockerCLIRunSuite) TestRunAddingOptionalDevices(c *testing.T) {
	// Not applicable on Windows as Windows does not support --device
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	out := cli.DockerCmd(c, "run", "--device", "/dev/zero:/dev/nulo", "busybox", "sh", "-c", "ls /dev/nulo").Combined()
	if actual := strings.Trim(out, "\r\n"); actual != "/dev/nulo" {
		c.Fatalf("expected output /dev/nulo, received %s", actual)
	}
}

func (s *DockerCLIRunSuite) TestRunAddingOptionalDevicesNoSrc(c *testing.T) {
	// Not applicable on Windows as Windows does not support --device
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	out := cli.DockerCmd(c, "run", "--device", "/dev/zero:rw", "busybox", "sh", "-c", "ls /dev/zero").Combined()
	if actual := strings.Trim(out, "\r\n"); actual != "/dev/zero" {
		c.Fatalf("expected output /dev/zero, received %s", actual)
	}
}

func (s *DockerCLIRunSuite) TestRunAddingOptionalDevicesInvalidMode(c *testing.T) {
	// Not applicable on Windows as Windows does not support --device
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	_, _, err := dockerCmdWithError("run", "--device", "/dev/zero:ro", "busybox", "sh", "-c", "ls /dev/zero")
	if err == nil {
		c.Fatalf("run container with device mode ro should fail")
	}
}

func (s *DockerCLIRunSuite) TestRunModeHostname(c *testing.T) {
	// Not applicable on Windows as Windows does not support -h
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux, NotUserNamespace)

	out := cli.DockerCmd(c, "run", "-h=testhostname", "busybox", "cat", "/etc/hostname").Combined()

	if actual := strings.Trim(out, "\r\n"); actual != "testhostname" {
		c.Fatalf("expected 'testhostname', but says: %q", actual)
	}

	out = cli.DockerCmd(c, "run", "--net=host", "busybox", "cat", "/etc/hostname").Combined()

	hostname, err := os.Hostname()
	if err != nil {
		c.Fatal(err)
	}
	if actual := strings.Trim(out, "\r\n"); actual != hostname {
		c.Fatalf("expected %q, but says: %q", hostname, actual)
	}
}

func (s *DockerCLIRunSuite) TestRunRootWorkdir(c *testing.T) {
	out := cli.DockerCmd(c, "run", "--workdir", "/", "busybox", "pwd").Combined()
	expected := "/\n"
	if testEnv.DaemonInfo.OSType == "windows" {
		expected = "C:" + expected
	}
	if out != expected {
		c.Fatalf("pwd returned %q (expected %s)", out, expected)
	}
}

func (s *DockerCLIRunSuite) TestRunAllowBindMountingRoot(c *testing.T) {
	if testEnv.DaemonInfo.OSType == "windows" {
		// Windows busybox will fail with Permission Denied on items such as pagefile.sys
		cli.DockerCmd(c, "run", "-v", `c:\:c:\host`, testEnv.PlatformDefaults.BaseImage, "cmd", "-c", "dir", `c:\host`)
	} else {
		cli.DockerCmd(c, "run", "-v", "/:/host", "busybox", "ls", "/host")
	}
}

func (s *DockerCLIRunSuite) TestRunDisallowBindMountingRootToRoot(c *testing.T) {
	mount := "/:/"
	targetDir := "/host"
	if testEnv.DaemonInfo.OSType == "windows" {
		mount = `c:\:c\`
		targetDir = "c:/host" // Forward slash as using busybox
	}
	out, _, err := dockerCmdWithError("run", "-v", mount, "busybox", "ls", targetDir)
	if err == nil {
		c.Fatal(out, err)
	}
}

// Verify that a container gets default DNS when only localhost resolvers exist
func (s *DockerCLIRunSuite) TestRunDNSDefaultOptions(c *testing.T) {
	// Not applicable on Windows as this is testing Unix specific functionality
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	// preserve original resolv.conf for restoring after test
	origResolvConf, err := os.ReadFile("/etc/resolv.conf")
	if os.IsNotExist(err) {
		c.Fatalf("/etc/resolv.conf does not exist")
	}
	// defer restored original conf
	defer func() {
		if err := os.WriteFile("/etc/resolv.conf", origResolvConf, 0o644); err != nil {
			c.Fatal(err)
		}
	}()

	// test 3 cases: standard IPv4 localhost, commented out localhost, and IPv6 localhost
	// 2 are removed from the file at container start, and the 3rd (commented out) one is ignored by
	// GetNameservers(), leading to a replacement of nameservers with the default set
	tmpResolvConf := []byte("nameserver 127.0.0.1\n#nameserver 127.0.2.1\nnameserver ::1")
	if err := os.WriteFile("/etc/resolv.conf", tmpResolvConf, 0o644); err != nil {
		c.Fatal(err)
	}

	actual := cli.DockerCmd(c, "run", "busybox", "cat", "/etc/resolv.conf").Combined()
	actual = regexp.MustCompile("(?m)^#.*$").ReplaceAllString(actual, "")
	actual = strings.ReplaceAll(strings.Trim(actual, "\r\n"), "\n", " ")
	// NOTE: if we ever change the defaults from google dns, this will break
	expected := "nameserver 8.8.8.8 nameserver 8.8.4.4"

	if actual != expected {
		c.Fatalf("expected resolv.conf be: %q, but was: %q", expected, actual)
	}
}

func (s *DockerCLIRunSuite) TestRunDNSOptions(c *testing.T) {
	// Not applicable on Windows as Windows does not support --dns*, or
	// the Unix-specific functionality of resolv.conf.
	testRequires(c, DaemonIsLinux)
	result := cli.DockerCmd(c, "run", "--dns=127.0.0.1", "--dns-search=mydomain", "--dns-opt=ndots:9", "busybox", "cat", "/etc/resolv.conf")

	// The client will get a warning on stderr when setting DNS to a localhost address; verify this:
	//
	// FIXME(thaJeztah): localhost DNS no longer produces a warning: remove this test?
	// if !strings.Contains(result.Stderr(), "Localhost DNS setting") {
	// 	c.Fatalf("Expected warning on stderr about localhost resolver, but got %q", result.Stderr())
	// }

	actual := regexp.MustCompile("(?m)^#.*$").ReplaceAllString(result.Stdout(), "")
	actual = strings.ReplaceAll(strings.Trim(actual, "\r\n"), "\n", " ")
	if actual != "nameserver 127.0.0.1 search mydomain options ndots:9" {
		c.Fatalf("nameserver 127.0.0.1 expected 'search mydomain options ndots:9', but says: %q", actual)
	}

	out := cli.DockerCmd(c, "run", "--dns=1.1.1.1", "--dns-search=.", "--dns-opt=ndots:3", "busybox", "cat", "/etc/resolv.conf").Combined()

	actual = regexp.MustCompile("(?m)^#.*$").ReplaceAllString(out, "")
	actual = strings.ReplaceAll(strings.Trim(strings.Trim(actual, "\r\n"), " "), "\n", " ")
	if actual != "nameserver 1.1.1.1 options ndots:3" {
		c.Fatalf("expected 'nameserver 1.1.1.1 options ndots:3', but says: %q", actual)
	}
}

func (s *DockerCLIRunSuite) TestRunDNSRepeatOptions(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	out := cli.DockerCmd(c, "run", "--dns=1.1.1.1", "--dns=2.2.2.2", "--dns-search=mydomain", "--dns-search=mydomain2", "--dns-opt=ndots:9", "--dns-opt=timeout:3", "busybox", "cat", "/etc/resolv.conf").Stdout()

	actual := regexp.MustCompile("(?m)^#.*$").ReplaceAllString(out, "")
	actual = strings.ReplaceAll(strings.Trim(actual, "\r\n"), "\n", " ")
	if actual != "nameserver 1.1.1.1 nameserver 2.2.2.2 search mydomain mydomain2 options ndots:9 timeout:3" {
		c.Fatalf("expected 'nameserver 1.1.1.1 nameserver 2.2.2.2 search mydomain mydomain2 options ndots:9 timeout:3', but says: %q", actual)
	}
}

// Test to see if a non-root user can resolve a DNS name. Also
// check if the container resolv.conf file has at least 0644 perm.
func (s *DockerCLIRunSuite) TestRunNonRootUserResolvName(c *testing.T) {
	// Not applicable on Windows as Windows does not support --user
	testRequires(c, testEnv.IsLocalDaemon, Network, DaemonIsLinux)

	cli.DockerCmd(c, "run", "--name=testperm", "--user=nobody", "busybox", "nslookup", "example.com")

	cID := getIDByName(c, "testperm")

	fmode := (os.FileMode)(0o644)
	finfo, err := os.Stat(containerStorageFile(cID, "resolv.conf"))
	if err != nil {
		c.Fatal(err)
	}

	if (finfo.Mode() & fmode) != fmode {
		c.Fatalf("Expected container resolv.conf mode to be at least %s, instead got %s", fmode.String(), finfo.Mode().String())
	}
}

// Test if container resolv.conf gets updated the next time it restarts
// if host /etc/resolv.conf has changed. This only applies if the container
// uses the host's /etc/resolv.conf and does not have any dns options provided.
func (s *DockerCLIRunSuite) TestRunResolvconfUpdate(c *testing.T) {
	// Not applicable on Windows as testing unix specific functionality
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)
	c.Skip("Unstable test, to be re-activated once #19937 is resolved")

	tmpResolvConf := []byte("search pommesfrites.fr\nnameserver 12.34.56.78\n")
	tmpLocalhostResolvConf := []byte("nameserver 127.0.0.1")

	// take a copy of resolv.conf for restoring after test completes
	resolvConfSystem, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		c.Fatal(err)
	}

	// This test case is meant to test monitoring resolv.conf when it is
	// a regular file not a bind mount. So we unmount resolv.conf and replace
	// it with a file containing the original settings.
	mounted, err := mountinfo.Mounted("/etc/resolv.conf")
	if err != nil {
		c.Fatal(err)
	}
	if mounted {
		icmd.RunCommand("umount", "/etc/resolv.conf").Assert(c, icmd.Success)
	}

	// cleanup
	defer func() {
		if err := os.WriteFile("/etc/resolv.conf", resolvConfSystem, 0o644); err != nil {
			c.Fatal(err)
		}
	}()

	// 1. test that a restarting container gets an updated resolv.conf
	cli.DockerCmd(c, "run", "--name=first", "busybox", "true")
	containerID1 := getIDByName(c, "first")

	// replace resolv.conf with our temporary copy
	if err := os.WriteFile("/etc/resolv.conf", tmpResolvConf, 0o644); err != nil {
		c.Fatal(err)
	}

	// start the container again to pickup changes
	cli.DockerCmd(c, "start", "first")

	// check for update in container
	containerResolv := readContainerFile(c, containerID1, "resolv.conf")
	if !bytes.Equal(containerResolv, tmpResolvConf) {
		c.Fatalf("Restarted container does not have updated resolv.conf; expected %q, got %q", tmpResolvConf, string(containerResolv))
	}

	/*	// make a change to resolv.conf (in this case replacing our tmp copy with orig copy)
		if err := os.WriteFile("/etc/resolv.conf", resolvConfSystem, 0644); err != nil {
						c.Fatal(err)
								} */
	// 2. test that a restarting container does not receive resolv.conf updates
	//   if it modified the container copy of the starting point resolv.conf
	cli.DockerCmd(c, "run", "--name=second", "busybox", "sh", "-c", "echo 'search mylittlepony.com' >>/etc/resolv.conf")
	containerID2 := getIDByName(c, "second")

	// make a change to resolv.conf (in this case replacing our tmp copy with orig copy)
	if err := os.WriteFile("/etc/resolv.conf", resolvConfSystem, 0o644); err != nil {
		c.Fatal(err)
	}

	// start the container again
	cli.DockerCmd(c, "start", "second")

	// check for update in container
	containerResolv = readContainerFile(c, containerID2, "resolv.conf")
	if bytes.Equal(containerResolv, resolvConfSystem) {
		c.Fatalf("Container's resolv.conf should not have been updated with host resolv.conf: %q", string(containerResolv))
	}

	// 3. test that a running container's resolv.conf is not modified while running
	runningContainerID := cli.DockerCmd(c, "run", "-d", "busybox", "top").Stdout()
	runningContainerID = strings.TrimSpace(runningContainerID)

	// replace resolv.conf
	if err := os.WriteFile("/etc/resolv.conf", tmpResolvConf, 0o644); err != nil {
		c.Fatal(err)
	}

	// check for update in container
	containerResolv = readContainerFile(c, runningContainerID, "resolv.conf")
	if bytes.Equal(containerResolv, tmpResolvConf) {
		c.Fatalf("Running container should not have updated resolv.conf; expected %q, got %q", string(resolvConfSystem), string(containerResolv))
	}

	// 4. test that a running container's resolv.conf is updated upon restart
	//   (the above container is still running..)
	cli.DockerCmd(c, "restart", runningContainerID)

	// check for update in container
	containerResolv = readContainerFile(c, runningContainerID, "resolv.conf")
	if !bytes.Equal(containerResolv, tmpResolvConf) {
		c.Fatalf("Restarted container should have updated resolv.conf; expected %q, got %q", string(tmpResolvConf), string(containerResolv))
	}

	// 5. test that additions of a localhost resolver are cleaned from
	//   host resolv.conf before updating container's resolv.conf copies

	// replace resolv.conf with a localhost-only nameserver copy
	if err = os.WriteFile("/etc/resolv.conf", tmpLocalhostResolvConf, 0o644); err != nil {
		c.Fatal(err)
	}

	// start the container again to pickup changes
	cli.DockerCmd(c, "start", "first")

	// our first exited container ID should have been updated, but with default DNS
	// after the cleanup of resolv.conf found only a localhost nameserver:
	containerResolv = readContainerFile(c, containerID1, "resolv.conf")
	expected := "\nnameserver 8.8.8.8\nnameserver 8.8.4.4\n"
	if !bytes.Equal(containerResolv, []byte(expected)) {
		c.Fatalf("Container does not have cleaned/replaced DNS in resolv.conf; expected %q, got %q", expected, string(containerResolv))
	}

	// 6. Test that replacing (as opposed to modifying) resolv.conf triggers an update
	//   of containers' resolv.conf.

	// Restore the original resolv.conf
	if err := os.WriteFile("/etc/resolv.conf", resolvConfSystem, 0o644); err != nil {
		c.Fatal(err)
	}

	// Run the container so it picks up the old settings
	cli.DockerCmd(c, "run", "--name=third", "busybox", "true")
	containerID3 := getIDByName(c, "third")

	// Create a modified resolv.conf.aside and override resolv.conf with it
	if err := os.WriteFile("/etc/resolv.conf.aside", tmpResolvConf, 0o644); err != nil {
		c.Fatal(err)
	}

	err = os.Rename("/etc/resolv.conf.aside", "/etc/resolv.conf")
	if err != nil {
		c.Fatal(err)
	}

	// start the container again to pickup changes
	cli.DockerCmd(c, "start", "third")

	// check for update in container
	containerResolv = readContainerFile(c, containerID3, "resolv.conf")
	if !bytes.Equal(containerResolv, tmpResolvConf) {
		c.Fatalf("Stopped container does not have updated resolv.conf; expected\n%q\n got\n%q", tmpResolvConf, string(containerResolv))
	}

	// cleanup, restore original resolv.conf happens in defer func()
}

func (s *DockerCLIRunSuite) TestRunAddHost(c *testing.T) {
	// Not applicable on Windows as it does not support --add-host
	testRequires(c, DaemonIsLinux)
	out := cli.DockerCmd(c, "run", "--add-host=extra:86.75.30.9", "busybox", "grep", "extra", "/etc/hosts").Combined()

	actual := strings.Trim(out, "\r\n")
	if actual != "86.75.30.9\textra" {
		c.Fatalf("expected '86.75.30.9\textra', but says: %q", actual)
	}
}

// Regression test for #6983
func (s *DockerCLIRunSuite) TestRunAttachStdErrOnlyTTYMode(c *testing.T) {
	exitCode := cli.DockerCmd(c, "run", "-t", "-a", "stderr", "busybox", "true").ExitCode
	if exitCode != 0 {
		c.Fatalf("Container should have exited with error code 0")
	}
}

// Regression test for #6983
func (s *DockerCLIRunSuite) TestRunAttachStdOutOnlyTTYMode(c *testing.T) {
	exitCode := cli.DockerCmd(c, "run", "-t", "-a", "stdout", "busybox", "true").ExitCode
	if exitCode != 0 {
		c.Fatalf("Container should have exited with error code 0")
	}
}

// Regression test for #6983
func (s *DockerCLIRunSuite) TestRunAttachStdOutAndErrTTYMode(c *testing.T) {
	exitCode := cli.DockerCmd(c, "run", "-t", "-a", "stdout", "-a", "stderr", "busybox", "true").ExitCode
	if exitCode != 0 {
		c.Fatalf("Container should have exited with error code 0")
	}
}

// Test for #10388 - this will run the same test as TestRunAttachStdOutAndErrTTYMode
// but using --attach instead of -a to make sure we read the flag correctly
func (s *DockerCLIRunSuite) TestRunAttachWithDetach(c *testing.T) {
	result := icmd.RunCommand(dockerBinary, "run", "-d", "--attach", "stdout", "busybox", "true")
	result.Assert(c, icmd.Expected{
		ExitCode: 1,
	})
	out := strings.ToLower(result.Combined())
	assert.Check(c, is.Contains(out, "conflicting options"))
}

func (s *DockerCLIRunSuite) TestRunState(c *testing.T) {
	// TODO Windows: This needs some rework as Windows busybox does not support top
	testRequires(c, DaemonIsLinux)
	id := cli.DockerCmd(c, "run", "-d", "busybox", "top").Stdout()
	id = strings.TrimSpace(id)

	state := inspectField(c, id, "State.Running")
	if state != "true" {
		c.Fatal("Container state is 'not running'")
	}
	pid1 := inspectField(c, id, "State.Pid")
	if pid1 == "0" {
		c.Fatal("Container state Pid 0")
	}

	cli.DockerCmd(c, "stop", id)
	state = inspectField(c, id, "State.Running")
	if state != "false" {
		c.Fatal("Container state is 'running'")
	}
	pid2 := inspectField(c, id, "State.Pid")
	if pid2 == pid1 {
		c.Fatalf("Container state Pid %s, but expected %s", pid2, pid1)
	}

	cli.DockerCmd(c, "start", id)
	state = inspectField(c, id, "State.Running")
	if state != "true" {
		c.Fatal("Container state is 'not running'")
	}
	pid3 := inspectField(c, id, "State.Pid")
	if pid3 == pid1 {
		c.Fatalf("Container state Pid %s, but expected %s", pid2, pid1)
	}
}

// Test for #1737
func (s *DockerCLIRunSuite) TestRunCopyVolumeUIDGID(c *testing.T) {
	// Not applicable on Windows as it does not support uid or gid in this way
	testRequires(c, DaemonIsLinux)
	name := "testrunvolumesuidgid"
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
		RUN echo 'dockerio:x:1001:1001::/bin:/bin/false' >> /etc/passwd
		RUN echo 'dockerio:x:1001:' >> /etc/group
		RUN mkdir -p /hello && touch /hello/test && chown dockerio.dockerio /hello`))

	// Test that the uid and gid is copied from the image to the volume
	out := cli.DockerCmd(c, "run", "--rm", "-v", "/hello", name, "sh", "-c", `ls -l / | grep hello | awk '{print $3":"$4}'`).Combined()
	out = strings.TrimSpace(out)
	if out != "dockerio:dockerio" {
		c.Fatalf("Wrong /hello ownership: %s, expected dockerio:dockerio", out)
	}
}

// Test for #1582
func (s *DockerCLIRunSuite) TestRunCopyVolumeContent(c *testing.T) {
	// TODO Windows, post RS1. Windows does not yet support volume functionality
	// that copies from the image to the volume.
	testRequires(c, DaemonIsLinux)
	name := "testruncopyvolumecontent"
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
		RUN mkdir -p /hello/local && echo hello > /hello/local/world`))

	// Test that the content is copied from the image to the volume
	out := cli.DockerCmd(c, "run", "--rm", "-v", "/hello", name, "find", "/hello").Combined()
	if !strings.Contains(out, "/hello/local/world") || !strings.Contains(out, "/hello/local") {
		c.Fatal("Container failed to transfer content to volume")
	}
}

func (s *DockerCLIRunSuite) TestRunCleanupCmdOnEntrypoint(c *testing.T) {
	name := "testrunmdcleanuponentrypoint"
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
		ENTRYPOINT ["echo"]
		CMD ["testingpoint"]`))

	result := cli.DockerCmd(c, "run", "--entrypoint", "whoami", name)
	out := strings.TrimSpace(result.Combined())
	if result.ExitCode != 0 {
		c.Fatalf("expected exit code 0 received %d, out: %q", result.ExitCode, out)
	}
	expected := "root"
	if testEnv.DaemonInfo.OSType == "windows" {
		if strings.Contains(testEnv.PlatformDefaults.BaseImage, "servercore") {
			expected = `user manager\containeradministrator`
		} else {
			expected = `ContainerAdministrator` // nanoserver
		}
	}
	if out != expected {
		c.Fatalf("Expected output %s, got %q. %s", expected, out, testEnv.PlatformDefaults.BaseImage)
	}
}

// TestRunWorkdirExistsAndIsFile checks that if 'docker run -w' with existing file can be detected
func (s *DockerCLIRunSuite) TestRunWorkdirExistsAndIsFile(c *testing.T) {
	existingFile := "/bin/cat"
	expected := "not a directory"
	if testEnv.DaemonInfo.OSType == "windows" {
		existingFile = `\windows\system32\ntdll.dll`
		expected = `The directory name is invalid.`
	}

	out, exitCode, err := dockerCmdWithError("run", "-w", existingFile, "busybox")
	if err == nil || exitCode != 125 || !strings.Contains(out, expected) {
		c.Fatalf("Existing binary as a directory should error out with exitCode 125; we got: %s, exitCode: %d", out, exitCode)
	}
}

func (s *DockerCLIRunSuite) TestRunExitOnStdinClose(c *testing.T) {
	name := "testrunexitonstdinclose"

	meow := "/bin/cat"
	delay := 60
	if testEnv.DaemonInfo.OSType == "windows" {
		meow = "cat"
	}
	runCmd := exec.Command(dockerBinary, "run", "--name", name, "-i", "busybox", meow)

	stdin, err := runCmd.StdinPipe()
	if err != nil {
		c.Fatal(err)
	}
	stdout, err := runCmd.StdoutPipe()
	if err != nil {
		c.Fatal(err)
	}

	if err := runCmd.Start(); err != nil {
		c.Fatal(err)
	}
	if _, err := stdin.Write([]byte("hello\n")); err != nil {
		c.Fatal(err)
	}

	r := bufio.NewReader(stdout)
	line, err := r.ReadString('\n')
	if err != nil {
		c.Fatal(err)
	}
	line = strings.TrimSpace(line)
	if line != "hello" {
		c.Fatalf("Output should be 'hello', got '%q'", line)
	}
	if err := stdin.Close(); err != nil {
		c.Fatal(err)
	}
	finish := make(chan error, 1)
	go func() {
		finish <- runCmd.Wait()
		close(finish)
	}()
	select {
	case err := <-finish:
		assert.NilError(c, err)
	case <-time.After(time.Duration(delay) * time.Second):
		c.Fatal("docker run failed to exit on stdin close")
	}
	state := inspectField(c, name, "State.Running")

	if state != "false" {
		c.Fatal("Container must be stopped after stdin closing")
	}
}

// Test run -i --restart xxx doesn't hang
func (s *DockerCLIRunSuite) TestRunInteractiveWithRestartPolicy(c *testing.T) {
	name := "test-inter-restart"

	result := icmd.RunCmd(icmd.Cmd{
		Command: []string{dockerBinary, "run", "-i", "--name", name, "--restart=always", "busybox", "sh"},
		Stdin:   bytes.NewBufferString("exit 11"),
	})
	defer func() {
		cli.Docker(cli.Args("stop", name)).Assert(c, icmd.Success)
	}()

	result.Assert(c, icmd.Expected{ExitCode: 11})
}

// Test for #2267
func (s *DockerCLIRunSuite) TestRunWriteSpecialFilesAndNotCommit(c *testing.T) {
	// Cannot run on Windows as this files are not present in Windows
	testRequires(c, DaemonIsLinux)

	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "writehosts", path: "/etc/hosts"},
		{name: "writehostname", path: "/etc/hostname"},
		{name: "writeresolv", path: "/etc/resolv.conf"},
	} {
		c.Run(tc.name, func(t *testing.T) {
			command := fmt.Sprintf("echo test2267 >> %s && cat %s", tc.path, tc.path)
			out := cli.DockerCmd(t, "run", "--name", tc.name, "busybox", "sh", "-c", command).Combined()
			if !strings.Contains(out, "test2267") {
				t.Fatalf("%s should contain 'test2267'", tc.path)
			}

			out = cli.DockerCmd(t, "diff", tc.name).Combined()
			if strings.Trim(out, "\r\n") == "" {
				return
			}

			baseName := "eqToBaseDiff" + testutil.RandomAlpha(32)
			cli.DockerCmd(t, "run", "--name", baseName, "busybox", "echo", "hello")

			baseID := getIDByName(t, baseName)
			baseDiff := strings.Split(cli.DockerCmd(t, "diff", baseID).Combined(), "\n")
			actualDiff := strings.Split(out, "\n")

			assert.DeepEqual(t, actualDiff, baseDiff, cmpopts.SortSlices(func(a, b string) bool {
				return a < b
			}))
		})
	}
}

func (s *DockerCLIRunSuite) TestRunWithBadDevice(c *testing.T) {
	// Cannot run on Windows as Windows does not support --device
	testRequires(c, DaemonIsLinux)
	name := "baddevice"
	out, _, err := dockerCmdWithError("run", "--name", name, "--device", "/etc", "busybox", "true")

	if err == nil {
		c.Fatal("Run should fail with bad device")
	}
	expected := `"/etc": not a device node`
	if !strings.Contains(out, expected) {
		c.Fatalf("Output should contain %q, actual out: %q", expected, out)
	}
}

func (s *DockerCLIRunSuite) TestRunEntrypoint(c *testing.T) {
	const name = "entrypoint"
	const expected = "foobar"

	out := cli.DockerCmd(c, "run", "--name", name, "--entrypoint", "echo", "busybox", "-n", "foobar").Combined()
	if out != expected {
		c.Fatalf("Output should be %q, actual out: %q", expected, out)
	}
}

func (s *DockerCLIRunSuite) TestRunBindMounts(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)
	if testEnv.DaemonInfo.OSType == "linux" {
		testRequires(c, DaemonIsLinux, NotUserNamespace)
	}

	tmpDir, err := os.MkdirTemp("", "docker-test-container")
	if err != nil {
		c.Fatal(err)
	}

	defer os.RemoveAll(tmpDir)
	writeFile(path.Join(tmpDir, "touch-me"), "", c)

	// Test reading from a read-only bind mount
	out := cli.DockerCmd(c, "run", "-v", fmt.Sprintf("%s:%s:ro", tmpDir, dPath("/tmpx")), "busybox", "ls", dPath("/tmpx")).Combined()
	if !strings.Contains(out, "touch-me") {
		c.Fatal("Container failed to read from bind mount")
	}

	// test writing to bind mount
	if testEnv.DaemonInfo.OSType == "windows" {
		cli.DockerCmd(c, "run", "-v", tmpDir+`:c:\tmp:rw`, "busybox", "touch", "c:/tmp/holla")
	} else {
		cli.DockerCmd(c, "run", "-v", tmpDir+`:/tmp:rw`, "busybox", "touch", "/tmp/holla")
	}

	readFile(path.Join(tmpDir, "holla"), c) // Will fail if the file doesn't exist

	// test mounting to an illegal destination directory
	_, _, err = dockerCmdWithError("run", "-v", tmpDir+":.", "busybox", "ls", ".")
	if err == nil {
		c.Fatal("Container bind mounted illegal directory")
	}

	// Windows does not (and likely never will) support mounting a single file
	if testEnv.DaemonInfo.OSType != "windows" {
		// test mount a file
		cli.DockerCmd(c, "run", "-v", tmpDir+"/holla:/tmp/holla:rw", "busybox", "sh", "-c", "echo -n 'yotta' > /tmp/holla")
		content := readFile(path.Join(tmpDir, "holla"), c) // Will fail if the file doesn't exist
		expected := "yotta"
		if content != expected {
			c.Fatalf("Output should be %q, actual out: %q", expected, content)
		}
	}
}

// Ensure that CIDFile gets deleted if it's empty
// Perform this test by making `docker run` fail
func (s *DockerCLIRunSuite) TestRunCidFileCleanupIfEmpty(c *testing.T) {
	// Skip on Windows. Base image on Windows has a CMD set in the image.
	testRequires(c, DaemonIsLinux)

	tmpDir, err := os.MkdirTemp("", "TestRunCidFile")
	if err != nil {
		c.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	tmpCidFile := path.Join(tmpDir, "cid")

	// This must be an image that has no CMD or ENTRYPOINT set
	imgRef := loadSpecialImage(c, specialimage.EmptyFS)

	out, _, err := dockerCmdWithError("run", "--cidfile", tmpCidFile, imgRef)
	if err == nil {
		c.Fatalf("Run without command must fail. out=%s", out)
	} else if !strings.Contains(out, "no command specified") {
		c.Fatalf("Run without command failed with wrong output. out=%s\nerr=%v", out, err)
	}

	if _, err := os.Stat(tmpCidFile); err == nil {
		c.Fatalf("empty CIDFile %q should've been deleted", tmpCidFile)
	}
}

// #2098 - Docker cidFiles only contain short version of the containerId
// sudo docker run --cidfile /tmp/docker_tesc.cid ubuntu echo "test"
// TestRunCidFile tests that run --cidfile returns the longid
func (s *DockerCLIRunSuite) TestRunCidFileCheckIDLength(c *testing.T) {
	tmpDir, err := os.MkdirTemp("", "TestRunCidFile")
	if err != nil {
		c.Fatal(err)
	}
	tmpCidFile := path.Join(tmpDir, "cid")
	defer os.RemoveAll(tmpDir)

	id := cli.DockerCmd(c, "run", "-d", "--cidfile", tmpCidFile, "busybox", "true").Stdout()
	id = strings.TrimSpace(id)
	buffer, err := os.ReadFile(tmpCidFile)
	if err != nil {
		c.Fatal(err)
	}
	cid := string(buffer)
	if len(cid) != 64 {
		c.Fatalf("--cidfile should be a long id, not %q", id)
	}
	if cid != id {
		c.Fatalf("cid must be equal to %s, got %s", id, cid)
	}
}

func (s *DockerCLIRunSuite) TestRunSetMacAddress(c *testing.T) {
	skip.If(c, RuntimeIsWindowsContainerd(), "FIXME: Broken on Windows + containerd combination")
	mac := "12:34:56:78:9a:bc"
	var out string
	if testEnv.DaemonInfo.OSType == "windows" {
		out = cli.DockerCmd(c, "run", "-i", "--rm", "--mac-address="+mac, "busybox", "sh", "-c", "ipconfig /all | grep 'Physical Address' | awk '{print $12}'").Combined()
		mac = strings.ReplaceAll(strings.ToUpper(mac), ":", "-") // To Windows-style MACs
	} else {
		out = cli.DockerCmd(c, "run", "-i", "--rm", "--mac-address="+mac, "busybox", "/bin/sh", "-c", "ip link show eth0 | tail -1 | awk '{print $2}'").Combined()
	}

	actualMac := strings.TrimSpace(out)
	if actualMac != mac {
		c.Fatalf("Set MAC address with --mac-address failed. The container has an incorrect MAC address: %q, expected: %q", actualMac, mac)
	}
}

func (s *DockerCLIRunSuite) TestRunInspectMacAddress(c *testing.T) {
	// TODO Windows. Network settings are not propagated back to inspect.
	testRequires(c, DaemonIsLinux)
	const mac = "12:34:56:78:9a:bc"
	out := cli.DockerCmd(c, "run", "-d", "--mac-address="+mac, "busybox", "top").Combined()

	id := strings.TrimSpace(out)
	inspectedMac := inspectField(c, id, "NetworkSettings.Networks.bridge.MacAddress")
	if inspectedMac != mac {
		c.Fatalf("docker inspect outputs wrong MAC address: %q, should be: %q", inspectedMac, mac)
	}
}

// test docker run use an invalid mac address
func (s *DockerCLIRunSuite) TestRunWithInvalidMacAddress(c *testing.T) {
	out, _, err := dockerCmdWithError("run", "--mac-address", "92:d0:c6:0a:29", "busybox")
	// use an invalid mac address should with an error out
	if err == nil || !strings.Contains(out, "is not a valid mac address") {
		c.Fatalf("run with an invalid --mac-address should with error out")
	}
}

func (s *DockerCLIRunSuite) TestRunPortInUse(c *testing.T) {
	// TODO Windows. The duplicate NAT message returned by Windows will be
	// changing as is currently completely undecipherable. Does need modifying
	// to run sh rather than top though as top isn't in Windows busybox.
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	port := "1234"
	cli.DockerCmd(c, "run", "-d", "-p", port+":80", "busybox", "top")

	out, _, err := dockerCmdWithError("run", "-d", "-p", port+":80", "busybox", "top")
	if err == nil {
		c.Fatalf("Binding on used port must fail")
	}
	if !strings.Contains(out, "port is already allocated") {
		c.Fatalf(`Out must be about "port is already allocated", got %s`, out)
	}
}

// https://github.com/moby/moby/issues/12148
func (s *DockerCLIRunSuite) TestRunAllocatePortInReservedRange(c *testing.T) {
	// TODO Windows. -P is not yet supported
	testRequires(c, DaemonIsLinux)
	// allocate a dynamic port to get the most recent
	id := cli.DockerCmd(c, "run", "-d", "-P", "-p", "80", "busybox", "top").Stdout()
	id = strings.TrimSpace(id)

	out := cli.DockerCmd(c, "inspect", "--format", `{{index .NetworkSettings.Ports "80/tcp" 0 "HostPort" }}`, id).Stdout()
	out = strings.TrimSpace(out)
	port, err := strconv.ParseInt(out, 10, 64)
	if err != nil {
		c.Fatalf("invalid port, got: %s, error: %s", out, err)
	}

	// allocate a static port and a dynamic port together, with static port
	// takes the next recent port in dynamic port range.
	cli.DockerCmd(c, "run", "-d", "-P", "-p", "80", "-p", fmt.Sprintf("%d:8080", port+1), "busybox", "top")
}

// Regression test for #7792
func (s *DockerCLIRunSuite) TestRunMountOrdering(c *testing.T) {
	// TODO Windows: Post RS1. Windows does not support nested mounts.
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux, NotUserNamespace)

	tmpDir, err := os.MkdirTemp("", "docker_nested_mount_test")
	if err != nil {
		c.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	tmpDir2, err := os.MkdirTemp("", "docker_nested_mount_test2")
	if err != nil {
		c.Fatal(err)
	}
	defer os.RemoveAll(tmpDir2)

	// Create a temporary tmpfs mount.
	fooDir := filepath.Join(tmpDir, "foo")
	if err := os.MkdirAll(filepath.Join(tmpDir, "foo"), 0o755); err != nil {
		c.Fatalf("failed to mkdir at %s - %s", fooDir, err)
	}

	if err := os.WriteFile(filepath.Join(fooDir, "touch-me"), []byte{}, 0o644); err != nil {
		c.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(tmpDir, "touch-me"), []byte{}, 0o644); err != nil {
		c.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(tmpDir2, "touch-me"), []byte{}, 0o644); err != nil {
		c.Fatal(err)
	}

	cli.DockerCmd(c, "run",
		"-v", tmpDir+":"+dPath("/tmp"),
		"-v", fooDir+":"+dPath("/tmp/foo"),
		"-v", tmpDir2+":"+dPath("/tmp/tmp2"),
		"-v", fooDir+":"+dPath("/tmp/tmp2/foo"),
		"busybox:latest", "sh", "-c",
		"ls "+dPath("/tmp/touch-me")+" "+dPath("/tmp/foo/touch-me")+" "+dPath("/tmp/tmp2/touch-me")+" "+dPath("/tmp/tmp2/foo/touch-me"),
	)
}

// Regression test for https://github.com/moby/moby/issues/8259
func (s *DockerCLIRunSuite) TestRunReuseBindVolumeThatIsSymlink(c *testing.T) {
	// Not applicable on Windows as Windows does not support volumes
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux, NotUserNamespace)

	tmpDir, err := os.MkdirTemp(os.TempDir(), "testlink")
	if err != nil {
		c.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	linkPath := os.TempDir() + "/testlink2"
	if err := os.Symlink(tmpDir, linkPath); err != nil {
		c.Fatal(err)
	}
	defer os.RemoveAll(linkPath)

	// Create first container
	cli.DockerCmd(c, "run", "-v", linkPath+":"+dPath("/tmp/test"), "busybox", "ls", dPath("/tmp/test"))

	// Create second container with same symlinked path
	// This will fail if the referenced issue is hit with a "Volume exists" error
	cli.DockerCmd(c, "run", "-v", linkPath+":"+dPath("/tmp/test"), "busybox", "ls", dPath("/tmp/test"))
}

// GH#10604: Test an "/etc" volume doesn't overlay special bind mounts in container
func (s *DockerCLIRunSuite) TestRunCreateVolumeEtc(c *testing.T) {
	// While Windows supports volumes, it does not support --add-host hence
	// this test is not applicable on Windows.
	testRequires(c, DaemonIsLinux)
	out := cli.DockerCmd(c, "run", "--dns=127.0.0.1", "-v", "/etc", "busybox", "cat", "/etc/resolv.conf").Stdout()
	if !strings.Contains(out, "nameserver 127.0.0.1") {
		c.Fatal("/etc volume mount hides /etc/resolv.conf")
	}

	out = cli.DockerCmd(c, "run", "-h=test123", "-v", "/etc", "busybox", "cat", "/etc/hostname").Stdout()
	if !strings.Contains(out, "test123") {
		c.Fatal("/etc volume mount hides /etc/hostname")
	}

	out = cli.DockerCmd(c, "run", "--add-host=test:192.168.0.1", "-v", "/etc", "busybox", "cat", "/etc/hosts").Stdout()
	out = strings.ReplaceAll(out, "\n", " ")
	if !strings.Contains(out, "192.168.0.1\ttest") || !strings.Contains(out, "127.0.0.1\tlocalhost") {
		c.Fatal("/etc volume mount hides /etc/hosts")
	}
}

func (s *DockerCLIRunSuite) TestVolumesNoCopyData(c *testing.T) {
	// TODO Windows (Post RS1). Windows does not support volumes which
	// are pre-populated such as is built in the dockerfile used in this test.
	testRequires(c, DaemonIsLinux)
	cli.BuildCmd(c, "dataimage", build.WithDockerfile(`FROM busybox
		RUN ["mkdir", "-p", "/foo"]
		RUN ["touch", "/foo/bar"]`))
	cli.DockerCmd(c, "run", "--name", "test", "-v", dPath("/foo"), "busybox")

	if out, _, err := dockerCmdWithError("run", "--volumes-from", "test", "dataimage", "ls", "-lh", "/foo/bar"); err == nil || !strings.Contains(out, "No such file or directory") {
		c.Fatalf("Data was copied on volumes-from but shouldn't be:\n%q", out)
	}

	tmpDir := RandomTmpDirPath("docker_test_bind_mount_copy_data", testEnv.DaemonInfo.OSType)
	if out, _, err := dockerCmdWithError("run", "-v", tmpDir+":/foo", "dataimage", "ls", "-lh", "/foo/bar"); err == nil || !strings.Contains(out, "No such file or directory") {
		c.Fatalf("Data was copied on bind mount but shouldn't be:\n%q", out)
	}
}

func (s *DockerCLIRunSuite) TestRunNoOutputFromPullInStdout(c *testing.T) {
	// just run with unknown image
	cmd := exec.Command(dockerBinary, "run", "asdfsg")
	stdout := bytes.NewBuffer(nil)
	cmd.Stdout = stdout
	if err := cmd.Run(); err == nil {
		c.Fatal("Run with unknown image should fail")
	}
	if stdout.Len() != 0 {
		c.Fatalf("Stdout contains output from pull: %s", stdout)
	}
}

func (s *DockerCLIRunSuite) TestRunVolumesCleanPaths(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)

	// use forward slashes in Dockerfile
	prefix, _ := getPrefixAndSlashFromDaemonPlatform()
	cli.BuildCmd(c, "run_volumes_clean_paths", build.WithDockerfile(`FROM busybox
		VOLUME `+prefix+`/foo/`))
	cli.DockerCmd(c, "run", "-v", dPath("/foo"), "-v", dPath("/bar/"), "--name", "dark_helmet", "run_volumes_clean_paths")

	p := dPath("/foo/")
	mnt, err := inspectMountPoint("dark_helmet", p)
	if !errors.Is(err, errMountNotFound) {
		c.Fatalf("Found unexpected volume entry for '%s' in volumes\n%q", p, mnt.Source)
	}

	p = dPath("/foo")
	mnt, err = inspectMountPoint("dark_helmet", p)
	assert.NilError(c, err)
	if !strings.Contains(strings.ToLower(mnt.Source), strings.ToLower(testEnv.PlatformDefaults.VolumesConfigPath)) {
		c.Fatalf("Volume was not defined for %s\n%q", p, mnt.Source)
	}

	p = dPath("/bar/")
	mnt, err = inspectMountPoint("dark_helmet", p)
	if !errors.Is(err, errMountNotFound) {
		c.Fatalf("Found unexpected volume entry for '%s' in volumes\n%q", p, mnt.Source)
	}

	p = dPath("/bar")
	mnt, err = inspectMountPoint("dark_helmet", p)
	assert.NilError(c, err)
	if !strings.Contains(strings.ToLower(mnt.Source), strings.ToLower(testEnv.PlatformDefaults.VolumesConfigPath)) {
		c.Fatalf("Volume was not defined for %s\n%q", p, mnt.Source)
	}
}

// Regression test for #3631
func (s *DockerCLIRunSuite) TestRunSlowStdoutConsumer(c *testing.T) {
	// TODO Windows: This should be able to run on Windows if can find an
	// alternate to /dev/zero and /dev/stdout.
	testRequires(c, DaemonIsLinux)

	args := []string{"run", "--rm", "busybox", "/bin/sh", "-c", "dd if=/dev/zero of=/dev/stdout bs=1024 count=2000 | cat -v"}
	cont := exec.Command(dockerBinary, args...)

	stdout, err := cont.StdoutPipe()
	if err != nil {
		c.Fatal(err)
	}

	if err := cont.Start(); err != nil {
		c.Fatal(err)
	}
	defer func() { go cont.Wait() }()
	n, err := ConsumeWithSpeed(stdout, 10000, 5*time.Millisecond, nil)
	if err != nil {
		c.Fatal(err)
	}

	expected := 2 * 1024 * 2000
	if n != expected {
		c.Fatalf("Expected %d, got %d", expected, n)
	}
}

func (s *DockerCLIRunSuite) TestRunAllowPortRangeThroughExpose(c *testing.T) {
	// TODO Windows: -P is not currently supported. Also network
	// settings are not propagated back.
	testRequires(c, DaemonIsLinux)
	id := cli.DockerCmd(c, "run", "-d", "--expose", "3000-3003", "-P", "busybox", "top").Stdout()
	id = strings.TrimSpace(id)

	portstr := inspectFieldJSON(c, id, "NetworkSettings.Ports")
	var ports network.PortMap
	if err := json.Unmarshal([]byte(portstr), &ports); err != nil {
		c.Fatal(err)
	}
	for port, binding := range ports {
		if port.Num() < 3000 || port.Num() > 3003 {
			c.Fatalf("Port %d is out of range", port.Num())
		}
		if len(binding) == 0 || binding[0].HostPort == "" {
			c.Fatalf("Port is not mapped for the port %s", port)
		}
	}
}

func (s *DockerCLIRunSuite) TestRunExposePort(c *testing.T) {
	out, _, err := dockerCmdWithError("run", "--expose", "80000", "busybox")
	assert.Assert(c, err != nil, "--expose with an invalid port should error out")
	assert.Assert(c, is.Contains(out, "invalid range format for --expose"))
}

func (s *DockerCLIRunSuite) TestRunModeIpcHost(c *testing.T) {
	// Not applicable on Windows as uses Unix-specific capabilities
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux, NotUserNamespace)

	hostIpc, err := os.Readlink("/proc/1/ns/ipc")
	if err != nil {
		c.Fatal(err)
	}

	out := cli.DockerCmd(c, "run", "--ipc=host", "busybox", "readlink", "/proc/self/ns/ipc").Combined()
	out = strings.Trim(out, "\n")
	if hostIpc != out {
		c.Fatalf("IPC different with --ipc=host %s != %s\n", hostIpc, out)
	}

	out = cli.DockerCmd(c, "run", "busybox", "readlink", "/proc/self/ns/ipc").Combined()
	out = strings.Trim(out, "\n")
	if hostIpc == out {
		c.Fatalf("IPC should be different without --ipc=host %s == %s\n", hostIpc, out)
	}
}

func (s *DockerCLIRunSuite) TestRunModeIpcContainerNotExists(c *testing.T) {
	// Not applicable on Windows as uses Unix-specific capabilities
	testRequires(c, DaemonIsLinux)
	out, _, err := dockerCmdWithError("run", "-d", "--ipc", "container:abcd1234", "busybox", "top")
	if !strings.Contains(out, "abcd1234") || err == nil {
		c.Fatalf("run IPC from a non exists container should with correct error out")
	}
}

func (s *DockerCLIRunSuite) TestRunModeIpcContainerNotRunning(c *testing.T) {
	// Not applicable on Windows as uses Unix-specific capabilities
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	id := cli.DockerCmd(c, "create", "busybox").Stdout()
	id = strings.TrimSpace(id)

	out, _, err := dockerCmdWithError("run", "--ipc=container:"+id, "busybox")
	if err == nil {
		c.Fatalf("Run container with ipc mode container should fail with non running container: %s\n%s", out, err)
	}
}

func (s *DockerCLIRunSuite) TestRunModePIDContainer(c *testing.T) {
	// Not applicable on Windows as uses Unix-specific capabilities
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	id := cli.DockerCmd(c, "run", "-d", "busybox", "sh", "-c", "top").Stdout()
	id = strings.TrimSpace(id)

	state := inspectField(c, id, "State.Running")
	if state != "true" {
		c.Fatal("Container state is 'not running'")
	}
	pid1 := inspectField(c, id, "State.Pid")

	parentContainerPid, err := os.Readlink(path.Join("/proc", pid1, "ns/pid"))
	if err != nil {
		c.Fatal(err)
	}

	out := cli.DockerCmd(c, "run", "--pid=container:"+id, "busybox", "readlink", "/proc/self/ns/pid").Combined()
	out = strings.Trim(out, "\n")
	if parentContainerPid != out {
		c.Fatalf("PID different with --pid=container:%s %s != %s\n", id, parentContainerPid, out)
	}
}

func (s *DockerCLIRunSuite) TestRunModePIDContainerNotExists(c *testing.T) {
	// Not applicable on Windows as uses Unix-specific capabilities
	testRequires(c, DaemonIsLinux)
	out, _, err := dockerCmdWithError("run", "-d", "--pid", "container:abcd1234", "busybox", "top")
	if !strings.Contains(out, "abcd1234") || err == nil {
		c.Fatalf("run PID from a non exists container should with correct error out")
	}
}

func (s *DockerCLIRunSuite) TestRunModePIDContainerNotRunning(c *testing.T) {
	// Not applicable on Windows as uses Unix-specific capabilities
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	id := cli.DockerCmd(c, "create", "busybox").Stdout()
	id = strings.TrimSpace(id)

	out, _, err := dockerCmdWithError("run", "--pid=container:"+id, "busybox")
	if err == nil {
		c.Fatalf("Run container with pid mode container should fail with non running container: %s\n%s", out, err)
	}
}

func (s *DockerCLIRunSuite) TestRunMountShmMqueueFromHost(c *testing.T) {
	// Not applicable on Windows as uses Unix-specific capabilities
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux, NotUserNamespace)

	cli.DockerCmd(c, "run", "-d", "--name", "shmfromhost", "-v", "/dev/shm:/dev/shm", "-v", "/dev/mqueue:/dev/mqueue", "busybox", "sh", "-c", "echo -n test > /dev/shm/test && touch /dev/mqueue/toto && top")
	c.Cleanup(func() {
		err := os.Remove("/dev/shm/test")
		assert.Check(c, err == nil || errors.Is(err, os.ErrNotExist))
		err = os.Remove("/dev/mqueue/toto")
		assert.Check(c, err == nil || errors.Is(err, os.ErrNotExist))
	})
	mnt, err := inspectMountPoint("shmfromhost", "/dev/shm")
	assert.NilError(c, err)
	assert.Equal(c, mnt.Source, "/dev/shm")

	out := cli.DockerCmd(c, "run", "--name", "ipchost", "--ipc", "host", "busybox", "cat", "/dev/shm/test").Combined()
	assert.Equal(c, out, "test", "unexpected content for /dev/shm/test")

	// Check that the mq was created
	_, err = os.Stat("/dev/mqueue/toto")
	assert.NilError(c, err, "failed to confirm '/dev/mqueue/toto' presence on host")
}

func (s *DockerCLIRunSuite) TestContainerNetworkMode(c *testing.T) {
	// Not applicable on Windows as uses Unix-specific capabilities
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	id := cli.DockerCmd(c, "run", "-d", "busybox", "top").Stdout()
	id = strings.TrimSpace(id)
	cli.WaitRun(c, id)
	pid1 := inspectField(c, id, "State.Pid")

	parentContainerNet, err := os.Readlink(path.Join("/proc", pid1, "ns/net"))
	if err != nil {
		c.Fatal(err)
	}

	out := cli.DockerCmd(c, "run", "--net=container:"+id, "busybox", "readlink", "/proc/self/ns/net").Combined()
	out = strings.Trim(out, "\n")
	if parentContainerNet != out {
		c.Fatalf("NET different with --net=container:%s %s != %s\n", id, parentContainerNet, out)
	}
}

func (s *DockerCLIRunSuite) TestRunModeUTSHost(c *testing.T) {
	// Not applicable on Windows as uses Unix-specific capabilities
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	hostUTS, err := os.Readlink("/proc/1/ns/uts")
	if err != nil {
		c.Fatal(err)
	}

	out := cli.DockerCmd(c, "run", "--uts=host", "busybox", "readlink", "/proc/self/ns/uts").Combined()
	out = strings.Trim(out, "\n")
	if hostUTS != out {
		c.Fatalf("UTS different with --uts=host %s != %s\n", hostUTS, out)
	}

	out = cli.DockerCmd(c, "run", "busybox", "readlink", "/proc/self/ns/uts").Combined()
	out = strings.Trim(out, "\n")
	if hostUTS == out {
		c.Fatalf("UTS should be different without --uts=host %s == %s\n", hostUTS, out)
	}

	out = dockerCmdWithFail(c, "run", "-h=name", "--uts=host", "busybox", "ps")
	assert.Assert(c, is.Contains(out, "conflicting options: hostname and the UTS mode"))
}

func (s *DockerCLIRunSuite) TestRunTLSVerify(c *testing.T) {
	// Remote daemons use TLS and this test is not applicable when TLS is required.
	testRequires(c, testEnv.IsLocalDaemon)
	if out, code, err := dockerCmdWithError("version"); err != nil || code != 0 {
		c.Fatalf("Should have worked: %v:\n%v", err, out)
	}

	var notFoundErr string
	if runtime.GOOS == "windows" {
		notFoundErr = "ca.pem: The system cannot find the file specified"
	} else {
		notFoundErr = "ca.pem: no such file or directory"
	}

	// Regardless of whether we specify true or false we need to
	// test to make sure tls is turned on if --tlsverify is specified at all
	result := cli.Docker(cli.Args("--tlsverify=false", "version"))
	result.Assert(c, icmd.Expected{ExitCode: 1, Err: notFoundErr})

	result = cli.Docker(cli.Args("--tlsverify=true", "version"))
	result.Assert(c, icmd.Expected{ExitCode: 1, Err: notFoundErr})
}

func (s *DockerCLIRunSuite) TestRunPortFromDockerRangeInUse(c *testing.T) {
	// TODO Windows. Once moved to libnetwork/CNM, this may be able to be
	// re-instated.
	testRequires(c, DaemonIsLinux)
	// first find allocator current position
	id := cli.DockerCmd(c, "run", "-d", "-p", ":80", "busybox", "top").Stdout()
	id = strings.TrimSpace(id)

	out := cli.DockerCmd(c, "inspect", "--format", `{{index .NetworkSettings.Ports "80/tcp" 0 "HostPort" }}`, id).Stdout()
	out = strings.TrimSpace(out)
	if out == "" {
		c.Fatal("docker port command output is empty")
	}
	lastPort, err := strconv.Atoi(out)
	if err != nil {
		c.Fatal(err)
	}
	port := lastPort + 1
	l, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		c.Fatal(err)
	}
	defer l.Close()

	id = cli.DockerCmd(c, "run", "-d", "-p", ":80", "busybox", "top").Stdout()
	id = strings.TrimSpace(id)
	cli.DockerCmd(c, "port", id)
}

// FIXME(thaJeztah) migrate to docker/cli tests
func (s *DockerCLIRunSuite) TestRunTTYWithPipe(c *testing.T) {
	errChan := make(chan error, 1)
	go func() {
		defer close(errChan)

		cmd := exec.Command(dockerBinary, "run", "-ti", "busybox", "true")
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
			errChan <- errors.New("run should have failed")
			return
		} else if !strings.Contains(out, expected) {
			errChan <- fmt.Errorf("run failed with error %q: expected %q", out, expected)
			return
		}
	}()

	select {
	case err := <-errChan:
		assert.NilError(c, err)
	case <-time.After(30 * time.Second):
		c.Fatal("container is running but should have failed")
	}
}

func (s *DockerCLIRunSuite) TestRunNonLocalMacAddress(c *testing.T) {
	addr := "00:16:3E:08:00:50"
	args := []string{"run", "--mac-address", addr}
	expected := addr

	if testEnv.DaemonInfo.OSType != "windows" {
		args = append(args, "busybox", "ifconfig")
	} else {
		args = append(args, testEnv.PlatformDefaults.BaseImage, "ipconfig", "/all")
		expected = strings.ReplaceAll(strings.ToUpper(addr), ":", "-")
	}

	if out := cli.DockerCmd(c, args...).Combined(); !strings.Contains(out, expected) {
		c.Fatalf("Output should have contained %q: %s", expected, out)
	}
}

func (s *DockerCLIRunSuite) TestRunNetHost(c *testing.T) {
	// Not applicable on Windows as uses Unix-specific capabilities
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux, NotUserNamespace)

	hostNet, err := os.Readlink("/proc/1/ns/net")
	if err != nil {
		c.Fatal(err)
	}

	out := cli.DockerCmd(c, "run", "--net=host", "busybox", "readlink", "/proc/self/ns/net").Combined()
	out = strings.Trim(out, "\n")
	if hostNet != out {
		c.Fatalf("Net namespace different with --net=host %s != %s\n", hostNet, out)
	}

	out = cli.DockerCmd(c, "run", "busybox", "readlink", "/proc/self/ns/net").Combined()
	out = strings.Trim(out, "\n")
	if hostNet == out {
		c.Fatalf("Net namespace should be different without --net=host %s == %s\n", hostNet, out)
	}
}

func (s *DockerCLIRunSuite) TestRunNetHostTwiceSameName(c *testing.T) {
	// TODO Windows. As Windows networking evolves and converges towards
	// CNM, this test may be possible to enable on Windows.
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux, NotUserNamespace)

	cli.DockerCmd(c, "run", "--rm", "--name=thost", "--net=host", "busybox", "true")
	cli.DockerCmd(c, "run", "--rm", "--name=thost", "--net=host", "busybox", "true")
}

func (s *DockerCLIRunSuite) TestRunNetContainerWhichHost(c *testing.T) {
	// Not applicable on Windows as uses Unix-specific capabilities
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux, NotUserNamespace)

	hostNet, err := os.Readlink("/proc/1/ns/net")
	if err != nil {
		c.Fatal(err)
	}

	cli.DockerCmd(c, "run", "-d", "--net=host", "--name=test", "busybox", "top")

	out := cli.DockerCmd(c, "run", "--net=container:test", "busybox", "readlink", "/proc/self/ns/net").Combined()
	out = strings.Trim(out, "\n")
	if hostNet != out {
		c.Fatalf("Container should have host network namespace")
	}
}

func (s *DockerCLIRunSuite) TestRunAllowPortRangeThroughPublish(c *testing.T) {
	// TODO Windows. This may be possible to enable in the future. However,
	// Windows does not currently support --expose, or populate the network
	// settings seen through inspect.
	testRequires(c, DaemonIsLinux)
	id := cli.DockerCmd(c, "run", "-d", "--expose", "3000-3003", "-p", "3000-3003", "busybox", "top").Stdout()
	id = strings.TrimSpace(id)
	portStr := inspectFieldJSON(c, id, "NetworkSettings.Ports")

	var ports network.PortMap
	err := json.Unmarshal([]byte(portStr), &ports)
	assert.NilError(c, err, "failed to unmarshal: %v", portStr)
	for port, binding := range ports {
		if port.Num() < 3000 || port.Num() > 3003 {
			c.Fatalf("Port %d is out of range", port.Num())
		}
		if len(binding) == 0 || binding[0].HostPort == "" {
			c.Fatalf("Port is not mapped for the port %s", port)
		}
	}
}

func (s *DockerCLIRunSuite) TestRunSetDefaultRestartPolicy(c *testing.T) {
	runSleepingContainer(c, "--name=testrunsetdefaultrestartpolicy")
	out := inspectField(c, "testrunsetdefaultrestartpolicy", "HostConfig.RestartPolicy.Name")
	if out != "no" {
		c.Fatalf("Set default restart policy failed")
	}
}

func (s *DockerCLIRunSuite) TestRunRestartMaxRetries(c *testing.T) {
	id := cli.DockerCmd(c, "run", "-d", "--restart=on-failure:3", "busybox", "false").Stdout()
	id = strings.TrimSpace(id)
	timeout := 10 * time.Second
	if testEnv.DaemonInfo.OSType == "windows" {
		timeout = 120 * time.Second
	}

	if err := waitInspect(id, "{{ .State.Restarting }} {{ .State.Running }}", "false false", timeout); err != nil {
		c.Fatal(err)
	}

	count := inspectField(c, id, "RestartCount")
	if count != "3" {
		c.Fatalf("Container was restarted %s times, expected %d", count, 3)
	}

	MaximumRetryCount := inspectField(c, id, "HostConfig.RestartPolicy.MaximumRetryCount")
	if MaximumRetryCount != "3" {
		c.Fatalf("Container Maximum Retry Count is %s, expected %s", MaximumRetryCount, "3")
	}
}

func (s *DockerCLIRunSuite) TestRunContainerWithWritableRootfs(c *testing.T) {
	cli.DockerCmd(c, "run", "--rm", "busybox", "touch", "/file")
}

func (s *DockerCLIRunSuite) TestRunContainerWithReadonlyRootfs(c *testing.T) {
	// Not applicable on Windows which does not support --read-only
	testRequires(c, DaemonIsLinux, UserNamespaceROMount)

	testPriv := true
	// don't test privileged mode subtest if user namespaces enabled
	if root := os.Getenv("DOCKER_REMAP_ROOT"); root != "" {
		testPriv = false
	}
	testReadOnlyFile(c, testPriv, "/file", "/etc/hosts", "/etc/resolv.conf", "/etc/hostname")
}

func (s *DockerCLIRunSuite) TestPermissionsPtsReadonlyRootfs(c *testing.T) {
	// Not applicable on Windows due to use of Unix specific functionality, plus
	// the use of --read-only which is not supported.
	testRequires(c, DaemonIsLinux, UserNamespaceROMount)

	// Ensure we have not broken writing /dev/pts
	result := cli.DockerCmd(c, "run", "--read-only", "--rm", "busybox", "mount")
	if result.ExitCode != 0 {
		c.Fatal("Could not obtain mounts when checking /dev/pts mntpnt.")
	}
	out := result.Combined()
	expected := "type devpts (rw,"
	if !strings.Contains(out, expected) {
		c.Fatalf("expected output to contain %s but contains %s", expected, out)
	}
}

func testReadOnlyFile(t *testing.T, testPriv bool, filenames ...string) {
	touch := "touch " + strings.Join(filenames, " ")
	out, _, err := dockerCmdWithError("run", "--read-only", "--rm", "busybox", "sh", "-c", touch)
	assert.ErrorContains(t, err, "")

	for _, f := range filenames {
		expected := "touch: " + f + ": Read-only file system"
		assert.Assert(t, is.Contains(out, expected))
	}

	if !testPriv {
		return
	}

	out, _, err = dockerCmdWithError("run", "--read-only", "--privileged", "--rm", "busybox", "sh", "-c", touch)
	assert.ErrorContains(t, err, "")

	for _, f := range filenames {
		expected := "touch: " + f + ": Read-only file system"
		assert.Assert(t, is.Contains(out, expected))
	}
}

func (s *DockerCLIRunSuite) TestRunContainerWithReadonlyEtcHostsAndLinkedContainer(c *testing.T) {
	// Not applicable on Windows which does not support --link
	testRequires(c, DaemonIsLinux, UserNamespaceROMount)

	cli.DockerCmd(c, "run", "-d", "--name", "test-etc-hosts-ro-linked", "busybox", "top")

	out := cli.DockerCmd(c, "run", "--read-only", "--link", "test-etc-hosts-ro-linked:testlinked", "busybox", "cat", "/etc/hosts").Stdout()
	if !strings.Contains(out, "testlinked") {
		c.Fatal("Expected /etc/hosts to be updated even if --read-only enabled")
	}
}

func (s *DockerCLIRunSuite) TestRunContainerWithReadonlyRootfsWithDNSFlag(c *testing.T) {
	// Not applicable on Windows which does not support either --read-only or --dns.
	testRequires(c, DaemonIsLinux, UserNamespaceROMount)

	out := cli.DockerCmd(c, "run", "--read-only", "--dns", "1.1.1.1", "busybox", "/bin/cat", "/etc/resolv.conf").Stdout()
	if !strings.Contains(out, "1.1.1.1") {
		c.Fatal("Expected /etc/resolv.conf to be updated even if --read-only enabled and --dns flag used")
	}
}

func (s *DockerCLIRunSuite) TestRunContainerWithReadonlyRootfsWithAddHostFlag(c *testing.T) {
	// Not applicable on Windows which does not support --read-only
	testRequires(c, DaemonIsLinux, UserNamespaceROMount)

	out := cli.DockerCmd(c, "run", "--read-only", "--add-host", "testreadonly:127.0.0.1", "busybox", "/bin/cat", "/etc/hosts").Stdout()
	if !strings.Contains(out, "testreadonly") {
		c.Fatal("Expected /etc/hosts to be updated even if --read-only enabled and --add-host flag used")
	}
}

func (s *DockerCLIRunSuite) TestRunVolumesFromRestartAfterRemoved(c *testing.T) {
	runSleepingContainer(c, "--name=voltest", "-v", dPath("/foo"))
	runSleepingContainer(c, "--name=restarter", "--volumes-from", "voltest")

	// Remove the main volume container and restart the consuming container
	cli.DockerCmd(c, "rm", "-f", "voltest")

	// This should not fail since the volumes-from were already applied
	cli.DockerCmd(c, "restart", "restarter")
}

// run container with --rm should remove container if exit code != 0
func (s *DockerCLIRunSuite) TestRunContainerWithRmFlagExitCodeNotEqualToZero(c *testing.T) {
	name := "flowers"
	cli.Docker(cli.Args("run", "--name", name, "--rm", "busybox", "ls", "/notexists")).Assert(c, icmd.Expected{
		ExitCode: 1,
	})

	removeTimeout := 10 * time.Second
	if DaemonIsWindows() {
		removeTimeout = 60 * time.Second
	}
	poll.WaitOn(c, containerRemoved(name), poll.WithTimeout(removeTimeout))
}

func (s *DockerCLIRunSuite) TestRunContainerWithRmFlagCannotStartContainer(c *testing.T) {
	name := "sparkles"
	cli.Docker(cli.Args("run", "--name", name, "--rm", "busybox", "commandNotFound")).Assert(c, icmd.Expected{
		ExitCode: 127,
	})

	removeTimeout := 10 * time.Second
	if DaemonIsWindows() {
		removeTimeout = 60 * time.Second
	}
	poll.WaitOn(c, containerRemoved(name), poll.WithTimeout(removeTimeout))
}

func containerRemoved(name string) poll.Check {
	return func(l poll.LogT) poll.Result {
		err := cli.Docker(cli.Args("container", "inspect", "--format='{{.ID}}'", name)).Compare(icmd.Expected{
			ExitCode: 1,
			Out:      "",
			Err:      "o such container", // (N|n)o such container
		})
		if err != nil {
			return poll.Continue("waiting for container '%s' to be removed", name)
		}
		return poll.Success()
	}
}

func (s *DockerCLIRunSuite) TestRunPIDHostWithChildIsKillable(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	name := "ibuildthecloud"
	cli.DockerCmd(c, "run", "-d", "--pid=host", "--name", name, "busybox", "sh", "-c", "sleep 30; echo hi")
	cli.WaitRun(c, name)

	errchan := make(chan error, 1)
	go func() {
		if out, _, err := dockerCmdWithError("kill", name); err != nil {
			errchan <- fmt.Errorf("%v:\n%s", err, out)
		}
		close(errchan)
	}()
	select {
	case err := <-errchan:
		assert.NilError(c, err)
	case <-time.After(5 * time.Second):
		c.Fatal("Kill container timed out")
	}
}

func (s *DockerCLIRunSuite) TestRunWithTooSmallMemoryLimit(c *testing.T) {
	// TODO Windows. This may be possible to enable once Windows supports memory limits on containers
	testRequires(c, DaemonIsLinux)
	// this memory limit is 1 byte less than the min (daemon.linuxMinMemory), which is 6MB (6291456 bytes)
	out, _, err := dockerCmdWithError("create", "-m", "6291455", "busybox")
	if err == nil || !strings.Contains(out, "Minimum memory limit allowed is 6MB") {
		c.Fatalf("expected run to fail when using too low a memory limit: %q", out)
	}
}

func (s *DockerCLIRunSuite) TestRunWriteToProcAsound(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, DaemonIsLinux)
	_, code, err := dockerCmdWithError("run", "busybox", "sh", "-c", "echo 111 >> /proc/asound/version")
	if err == nil || code == 0 {
		c.Fatal("standard container should not be able to write to /proc/asound")
	}
}

func (s *DockerCLIRunSuite) TestRunReadProcTimer(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, DaemonIsLinux)
	out, code, err := dockerCmdWithError("run", "busybox", "cat", "/proc/timer_stats")
	if code != 0 {
		return
	}
	if err != nil {
		c.Fatal(err)
	}
	if strings.Trim(out, "\n ") != "" {
		c.Fatalf("expected to receive no output from /proc/timer_stats but received %q", out)
	}
}

func (s *DockerCLIRunSuite) TestRunReadProcLatency(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, DaemonIsLinux)
	// some kernels don't have this configured so skip the test if this file is not found
	// on the host running the tests.
	if _, err := os.Stat("/proc/latency_stats"); err != nil {
		c.Skip("kernel doesn't have latency_stats configured")
		return
	}
	out, code, err := dockerCmdWithError("run", "busybox", "cat", "/proc/latency_stats")
	if code != 0 {
		return
	}
	if err != nil {
		c.Fatal(err)
	}
	if strings.Trim(out, "\n ") != "" {
		c.Fatalf("expected to receive no output from /proc/latency_stats but received %q", out)
	}
}

func (s *DockerCLIRunSuite) TestRunReadFilteredProc(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, Apparmor, DaemonIsLinux, NotUserNamespace)

	testReadPaths := []string{
		"/proc/latency_stats",
		"/proc/timer_stats",
		"/proc/kcore",
	}
	for i, filePath := range testReadPaths {
		name := fmt.Sprintf("procsieve-%d", i)
		shellCmd := "exec 3<" + filePath

		out, exitCode, err := dockerCmdWithError("run", "--privileged", "--security-opt", "apparmor=docker-default", "--name", name, "busybox", "sh", "-c", shellCmd)
		if exitCode != 0 {
			return
		}
		if err != nil {
			c.Fatalf("Open FD for read should have failed with permission denied, got: %s, %v", out, err)
		}
	}
}

func (s *DockerCLIRunSuite) TestMountIntoProc(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, DaemonIsLinux)
	_, code, err := dockerCmdWithError("run", "-v", "/proc//sys", "busybox", "true")
	if err == nil || code == 0 {
		c.Fatal("container should not be able to mount into /proc")
	}
}

func (s *DockerCLIRunSuite) TestMountIntoSys(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, DaemonIsLinux)
	testRequires(c, NotUserNamespace)
	cli.DockerCmd(c, "run", "-v", "/sys/fs/cgroup", "busybox", "true")
}

func (s *DockerCLIRunSuite) TestRunUnshareProc(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, Apparmor, DaemonIsLinux, NotUserNamespace)

	// In this test goroutines are used to run test cases in parallel to prevent the test from taking a long time to run.
	errChan := make(chan error)

	go func() {
		name := "acidburn"
		out, _, err := dockerCmdWithError("run", "--name", name, "--security-opt", "seccomp=unconfined", "debian:trixie-slim", "unshare", "-p", "-m", "-f", "-r", "--mount-proc=/proc", "mount")
		if err == nil ||
			(!strings.Contains(strings.ToLower(out), "permission denied") && !strings.Contains(strings.ToLower(out), "operation not permitted")) {
			errChan <- fmt.Errorf("unshare with --mount-proc should have failed with 'permission denied' or 'operation not permitted', got: %s, %v", out, err)
		} else {
			errChan <- nil
		}
	}()

	go func() {
		name := "cereal"
		out, _, err := dockerCmdWithError("run", "--name", name, "--security-opt", "seccomp=unconfined", "debian:trixie-slim", "unshare", "-p", "-m", "-f", "-r", "mount", "-t", "proc", "none", "/proc")
		if err == nil ||
			(!strings.Contains(strings.ToLower(out), "mount: cannot mount none") && !strings.Contains(strings.ToLower(out), "permission denied") && !strings.Contains(strings.ToLower(out), "operation not permitted")) {
			errChan <- fmt.Errorf("unshare and mount of /proc should have failed with 'mount: cannot mount none' or 'permission denied', got: %s, %v", out, err)
		} else {
			errChan <- nil
		}
	}()

	/* Ensure still fails if running privileged with the default policy */
	go func() {
		name := "crashoverride"
		out, _, err := dockerCmdWithError("run", "--privileged", "--security-opt", "seccomp=unconfined", "--security-opt", "apparmor=docker-default", "--name", name, "debian:trixie-slim", "unshare", "-p", "-m", "-f", "-r", "mount", "-t", "proc", "none", "/proc")
		if err == nil ||
			(!strings.Contains(strings.ToLower(out), "mount: cannot mount none") && !strings.Contains(strings.ToLower(out), "permission denied") && !strings.Contains(strings.ToLower(out), "operation not permitted")) {
			errChan <- fmt.Errorf("privileged unshare with apparmor should have failed with 'mount: cannot mount none' or 'permission denied', got: %s, %v", out, err)
		} else {
			errChan <- nil
		}
	}()

	var retErr error
	for range 3 {
		err := <-errChan
		if retErr == nil && err != nil {
			retErr = err
		}
	}
	if retErr != nil {
		c.Fatal(retErr)
	}
}

func (s *DockerCLIRunSuite) TestRunPublishPort(c *testing.T) {
	// TODO Windows: This may be possible once Windows moves to libnetwork and CNM
	testRequires(c, DaemonIsLinux)
	cli.DockerCmd(c, "run", "-d", "--name", "test", "--expose", "8080", "busybox", "top")
	out := cli.DockerCmd(c, "port", "test").Stdout()
	out = strings.Trim(out, "\r\n")
	if out != "" {
		c.Fatalf("run without --publish-all should not publish port, out should be nil, but got: %s", out)
	}
}

// Issue #10184.
func (s *DockerCLIRunSuite) TestDevicePermissions(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, DaemonIsLinux)
	const permissions = "crw-rw-rw-"
	result := cli.DockerCmd(c, "run", "--device", "/dev/fuse:/dev/fuse:mrw", "busybox:latest", "ls", "-l", "/dev/fuse")
	if result.ExitCode != 0 {
		c.Fatalf("expected status 0, got %d", result.ExitCode)
	}
	out := result.Combined()
	if !strings.HasPrefix(out, permissions) {
		c.Fatalf("output should begin with %q, got %q", permissions, out)
	}
}

func (s *DockerCLIRunSuite) TestRunCapAddCHOWN(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, DaemonIsLinux)
	out := cli.DockerCmd(c, "run", "--cap-drop=ALL", "--cap-add=CHOWN", "busybox", "sh", "-c", "adduser -D -H newuser && chown newuser /home && echo ok").Combined()

	if actual := strings.Trim(out, "\r\n"); actual != "ok" {
		c.Fatalf("expected output ok received %s", actual)
	}
}

// https://github.com/moby/moby/pull/14498
func (s *DockerCLIRunSuite) TestVolumeFromMixedRWOptions(c *testing.T) {
	cli.DockerCmd(c, "run", "--name", "parent", "-v", dPath("/test"), "busybox", "true")

	cli.DockerCmd(c, "run", "--volumes-from", "parent:ro", "--name", "test-volumes-1", "busybox", "true")
	cli.DockerCmd(c, "run", "--volumes-from", "parent:rw", "--name", "test-volumes-2", "busybox", "true")

	if testEnv.DaemonInfo.OSType != "windows" {
		mRO, err := inspectMountPoint("test-volumes-1", dPath("/test"))
		assert.NilError(c, err, "failed to inspect mount point")
		if mRO.RW {
			c.Fatalf("Expected RO volume was RW")
		}
	}

	mRW, err := inspectMountPoint("test-volumes-2", dPath("/test"))
	assert.NilError(c, err, "failed to inspect mount point")
	if !mRW.RW {
		c.Fatalf("Expected RW volume was RO")
	}
}

func (s *DockerCLIRunSuite) TestRunWriteFilteredProc(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, Apparmor, DaemonIsLinux, NotUserNamespace)

	testWritePaths := []string{
		/* modprobe and core_pattern should both be denied by generic
		 * policy of denials for /proc/sys/kernel. These files have been
		 * picked to be checked as they are particularly sensitive to writes */
		"/proc/sys/kernel/modprobe",
		"/proc/sys/kernel/core_pattern",
		"/proc/sysrq-trigger",
		"/proc/kcore",
	}
	for i, filePath := range testWritePaths {
		name := fmt.Sprintf("writeprocsieve-%d", i)

		shellCmd := "exec 3>" + filePath
		out, code, err := dockerCmdWithError("run", "--privileged", "--security-opt", "apparmor=docker-default", "--name", name, "busybox", "sh", "-c", shellCmd)
		if code != 0 {
			return
		}
		if err != nil {
			c.Fatalf("Open FD for write should have failed with permission denied, got: %s, %v", out, err)
		}
	}
}

func (s *DockerCLIRunSuite) TestRunNetworkFilesBindMount(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	tmpDir := c.TempDir()
	filename := filepath.Join(tmpDir, "testfile")

	const expected = "test123"
	assert.NilError(c, os.WriteFile(filename, []byte(expected), 0o644))

	// #nosec G302 -- for user namespaced test runs, the temp file must be accessible to unprivileged root
	if err := os.Chmod(filename, 0o646); err != nil {
		c.Fatalf("error modifying permissions of %s: %v", filename, err)
	}

	nwfiles := []string{"/etc/resolv.conf", "/etc/hosts", "/etc/hostname"}

	for i := range nwfiles {
		actual := cli.DockerCmd(c, "run", "-v", filename+":"+nwfiles[i], "busybox", "cat", nwfiles[i]).Combined()
		if actual != expected {
			c.Fatalf("expected %s be: %q, but was: %q", nwfiles[i], expected, actual)
		}
	}
}

func (s *DockerCLIRunSuite) TestRunNetworkFilesBindMountRO(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	tmpDir := c.TempDir()
	filename := filepath.Join(tmpDir, "testfile")
	assert.NilError(c, os.WriteFile(filename, []byte("test123"), 0o644))

	// #nosec G302 -- for user namespaced test runs, the temp file must be accessible to unprivileged root
	if err := os.Chmod(filename, 0o646); err != nil {
		c.Fatalf("error modifying permissions of %s: %v", filename, err)
	}

	nwfiles := []string{"/etc/resolv.conf", "/etc/hosts", "/etc/hostname"}

	for i := range nwfiles {
		_, exitCode, err := dockerCmdWithError("run", "-v", filename+":"+nwfiles[i]+":ro", "busybox", "touch", nwfiles[i])
		if err == nil || exitCode == 0 {
			c.Fatalf("run should fail because bind mount of %s is ro: exit code %d", nwfiles[i], exitCode)
		}
	}
}

func (s *DockerCLIRunSuite) TestRunNetworkFilesBindMountROFilesystem(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux, UserNamespaceROMount)

	tmpDir := c.TempDir()
	filename := filepath.Join(tmpDir, "testfile")
	assert.NilError(c, os.WriteFile(filename, []byte("test123"), 0o644))

	// #nosec G302 -- for user namespaced test runs, the temp file must be accessible to unprivileged root
	if err := os.Chmod(filename, 0o646); err != nil {
		c.Fatalf("error modifying permissions of %s: %v", filename, err)
	}

	nwfiles := []string{"/etc/resolv.conf", "/etc/hosts", "/etc/hostname"}

	for i := range nwfiles {
		exitCode := cli.DockerCmd(c, "run", "-v", filename+":"+nwfiles[i], "--read-only", "busybox", "touch", nwfiles[i]).ExitCode
		if exitCode != 0 {
			c.Fatalf("run should not fail because %s is mounted writable on read-only root filesystem: exit code %d", nwfiles[i], exitCode)
		}
	}

	for i := range nwfiles {
		_, exitCode, err := dockerCmdWithError("run", "-v", filename+":"+nwfiles[i]+":ro", "--read-only", "busybox", "touch", nwfiles[i])
		if err == nil || exitCode == 0 {
			c.Fatalf("run should fail because %s is mounted read-only on read-only root filesystem: exit code %d", nwfiles[i], exitCode)
		}
	}
}

func (s *DockerCLIRunSuite) TestPtraceContainerProcsFromHost(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon)

	id := cli.DockerCmd(c, "run", "-d", "busybox", "top").Stdout()
	id = strings.TrimSpace(id)
	cli.WaitRun(c, id)
	pid1 := inspectField(c, id, "State.Pid")

	_, err := os.Readlink(path.Join("/proc", pid1, "ns/net"))
	if err != nil {
		c.Fatal(err)
	}
}

func (s *DockerCLIRunSuite) TestAppArmorDeniesPtrace(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, testEnv.IsLocalDaemon, Apparmor, DaemonIsLinux)

	// Run through 'sh' so we are NOT pid 1. Pid 1 may be able to trace
	// itself, but pid>1 should not be able to trace pid1.
	_, exitCode, _ := dockerCmdWithError("run", "busybox", "sh", "-c", "sh -c readlink /proc/1/ns/net")
	if exitCode == 0 {
		c.Fatal("ptrace was not successfully restricted by AppArmor")
	}
}

func (s *DockerCLIRunSuite) TestAppArmorTraceSelf(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon, Apparmor)

	_, exitCode, _ := dockerCmdWithError("run", "busybox", "readlink", "/proc/1/ns/net")
	if exitCode != 0 {
		c.Fatal("ptrace of self failed.")
	}
}

func (s *DockerCLIRunSuite) TestAppArmorDeniesChmodProc(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, testEnv.IsLocalDaemon, Apparmor, DaemonIsLinux, NotUserNamespace)
	_, exitCode, _ := dockerCmdWithError("run", "busybox", "chmod", "744", "/proc/cpuinfo")
	if exitCode == 0 {
		// If our test failed, attempt to repair the host system...
		_, exitCode, _ := dockerCmdWithError("run", "busybox", "chmod", "444", "/proc/cpuinfo")
		if exitCode == 0 {
			c.Fatal("AppArmor was unsuccessful in prohibiting chmod of /proc/* files.")
		}
	}
}

func (s *DockerCLIRunSuite) TestRunCapAddSYSTIME(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, DaemonIsLinux)

	cli.DockerCmd(c, "run", "--cap-drop=ALL", "--cap-add=SYS_TIME", "busybox", "sh", "-c", "grep ^CapEff /proc/self/status | sed 's/^CapEff:\t//' | grep ^0000000002000000$")
}

// run create container failed should clean up the container
func (s *DockerCLIRunSuite) TestRunCreateContainerFailedCleanUp(c *testing.T) {
	// TODO Windows. This may be possible to enable once link is supported
	testRequires(c, DaemonIsLinux)
	name := "unique_name"
	_, _, err := dockerCmdWithError("run", "--name", name, "--link", "nothing:nothing", "busybox")
	assert.Assert(c, err != nil, "Expected docker run to fail!")

	containerID, err := inspectFilter(name, ".Id")
	assert.Assert(c, err != nil, "Expected not to have this container: %s!", containerID)
	assert.Equal(c, containerID, "", "Expected not to have this container: %s!", containerID)
}

func (s *DockerCLIRunSuite) TestRunNamedVolume(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	cli.DockerCmd(c, "run", "--name=test", "-v", "testing:"+dPath("/foo"), "busybox", "sh", "-c", "echo hello > "+dPath("/foo/bar"))

	out := cli.DockerCmd(c, "run", "--volumes-from", "test", "busybox", "sh", "-c", "cat "+dPath("/foo/bar")).Combined()
	assert.Equal(c, strings.TrimSpace(out), "hello")

	out = cli.DockerCmd(c, "run", "-v", "testing:"+dPath("/foo"), "busybox", "sh", "-c", "cat "+dPath("/foo/bar")).Combined()
	assert.Equal(c, strings.TrimSpace(out), "hello")
}

func (s *DockerCLIRunSuite) TestRunWithUlimits(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, DaemonIsLinux)

	out := cli.DockerCmd(c, "run", "--name=testulimits", "--ulimit", "nofile=50", "busybox", "/bin/sh", "-c", "ulimit -n").Combined()
	ul := strings.TrimSpace(out)
	if ul != "50" {
		c.Fatalf("expected `ulimit -n` to be 50, got %s", ul)
	}
}

func (s *DockerCLIRunSuite) TestRunContainerWithCgroupParent(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, DaemonIsLinux)
	skip.If(c, onlyCgroupsv2(), "FIXME: cgroupsV2 not supported yet")

	// cgroup-parent relative path
	testRunContainerWithCgroupParent(c, "test", "cgroup-test")

	// cgroup-parent absolute path
	testRunContainerWithCgroupParent(c, "/cgroup-parent/test", "cgroup-test-absolute")
}

func testRunContainerWithCgroupParent(t *testing.T, cgroupParent, name string) {
	out, _, err := dockerCmdWithError("run", "--cgroup-parent", cgroupParent, "--name", name, "busybox", "cat", "/proc/self/cgroup")
	if err != nil {
		t.Fatalf("unexpected failure when running container with --cgroup-parent option - %s\n%v", out, err)
	}
	cgroupPaths := ParseCgroupPaths(out)
	if len(cgroupPaths) == 0 {
		t.Fatalf("unexpected output - %q", out)
	}
	id := getIDByName(t, name)
	expectedCgroup := path.Join(cgroupParent, id)
	found := false
	for _, p := range cgroupPaths {
		if strings.HasSuffix(p, expectedCgroup) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("unexpected cgroup paths. Expected at least one cgroup path to have suffix %q. Cgroup Paths: %v", expectedCgroup, cgroupPaths)
	}
}

// TestRunInvalidCgroupParent checks that a specially-crafted cgroup parent doesn't cause Docker to crash or start modifying /.
func (s *DockerCLIRunSuite) TestRunInvalidCgroupParent(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	testRequires(c, DaemonIsLinux)
	skip.If(c, onlyCgroupsv2(), "FIXME: cgroupsV2 not supported yet")

	testRunInvalidCgroupParent(c, "../../../../../../../../SHOULD_NOT_EXIST", "SHOULD_NOT_EXIST", "cgroup-invalid-test")

	testRunInvalidCgroupParent(c, "/../../../../../../../../SHOULD_NOT_EXIST", "/SHOULD_NOT_EXIST", "cgroup-absolute-invalid-test")
}

func testRunInvalidCgroupParent(t *testing.T, cgroupParent, cleanCgroupParent, name string) {
	out, _, err := dockerCmdWithError("run", "--cgroup-parent", cgroupParent, "--name", name, "busybox", "cat", "/proc/self/cgroup")
	if err != nil {
		// XXX: This may include a daemon crash.
		t.Fatalf("unexpected failure when running container with --cgroup-parent option - %s\n%v", out, err)
	}

	// We expect "/SHOULD_NOT_EXIST" to not exist. If not, we have a security issue.
	if _, err := os.Stat("/SHOULD_NOT_EXIST"); err == nil || !os.IsNotExist(err) {
		t.Fatalf("SECURITY: --cgroup-parent with ../../ relative paths cause files to be created in the host (this is bad) !!")
	}

	cgroupPaths := ParseCgroupPaths(out)
	if len(cgroupPaths) == 0 {
		t.Fatalf("unexpected output - %q", out)
	}
	id := getIDByName(t, name)
	expectedCgroup := path.Join(cleanCgroupParent, id)
	found := false
	for _, p := range cgroupPaths {
		if strings.HasSuffix(p, expectedCgroup) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("unexpected cgroup paths. Expected at least one cgroup path to have suffix %q. Cgroup Paths: %v", expectedCgroup, cgroupPaths)
	}
}

func (s *DockerCLIRunSuite) TestRunContainerWithCgroupMountRO(c *testing.T) {
	// Not applicable on Windows as uses Unix specific functionality
	// --read-only + userns has remount issues
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	skip.If(c, onlyCgroupsv2(), "FIXME: cgroupsV2 not supported yet")

	filename := "/sys/fs/cgroup/devices/test123"
	out, _, err := dockerCmdWithError("run", "busybox", "touch", filename)
	if err == nil {
		c.Fatal("expected cgroup mount point to be read-only, touch file should fail")
	}
	expected := "Read-only file system"
	if !strings.Contains(out, expected) {
		c.Fatalf("expected output from failure to contain %s but contains %s", expected, out)
	}
}

func (s *DockerCLIRunSuite) TestRunContainerNetworkModeToSelf(c *testing.T) {
	// Not applicable on Windows which does not support --net=container
	testRequires(c, DaemonIsLinux)
	out, _, err := dockerCmdWithError("run", "--name=me", "--net=container:me", "busybox", "true")
	if err == nil || !strings.Contains(out, "cannot join own network") {
		c.Fatalf("using container net mode to self should result in an error\nerr: %q\nout: %s", err, out)
	}
}

func (s *DockerCLIRunSuite) TestRunContainerNetModeWithDNSMacHosts(c *testing.T) {
	// Not applicable on Windows which does not support --net=container
	testRequires(c, DaemonIsLinux)
	out, _, err := dockerCmdWithError("run", "-d", "--name", "parent", "busybox", "top")
	if err != nil {
		c.Fatalf("failed to run container: %v, output: %q", err, out)
	}

	out, _, err = dockerCmdWithError("run", "--dns", "1.2.3.4", "--net=container:parent", "busybox")
	if err == nil || !strings.Contains(out, "conflicting options: dns and the network mode") {
		c.Fatalf("run --net=container with --dns should error out")
	}

	out, _, err = dockerCmdWithError("run", "--add-host", "test:192.168.2.109", "--net=container:parent", "busybox")
	if err == nil || !strings.Contains(out, "conflicting options: custom host-to-IP mapping and the network mode") {
		c.Fatalf("run --net=container with --add-host should error out")
	}
}

func (s *DockerCLIRunSuite) TestRunContainerNetModeWithExposePort(c *testing.T) {
	// Not applicable on Windows which does not support --net=container
	testRequires(c, DaemonIsLinux)
	cli.DockerCmd(c, "run", "-d", "--name", "parent", "busybox", "top")

	out, _, err := dockerCmdWithError("run", "-p", "5000:5000", "--net=container:parent", "busybox")
	if err == nil || !strings.Contains(out, "conflicting options: port publishing and the container type network mode") {
		c.Fatalf("run --net=container with -p should error out")
	}

	out, _, err = dockerCmdWithError("run", "-P", "--net=container:parent", "busybox")
	if err == nil || !strings.Contains(out, "conflicting options: port publishing and the container type network mode") {
		c.Fatalf("run --net=container with -P should error out")
	}

	out, _, err = dockerCmdWithError("run", "--expose", "5000", "--net=container:parent", "busybox")
	if err == nil || !strings.Contains(out, "conflicting options: port exposing and the container type network mode") {
		c.Fatalf("run --net=container with --expose should error out")
	}
}

func (s *DockerCLIRunSuite) TestRunLinkToContainerNetMode(c *testing.T) {
	// Not applicable on Windows which does not support --net=container or --link
	testRequires(c, DaemonIsLinux)
	cli.DockerCmd(c, "run", "--name", "test", "-d", "busybox", "top")
	cli.DockerCmd(c, "run", "--name", "parent", "-d", "--net=container:test", "busybox", "top")
	cli.DockerCmd(c, "run", "-d", "--link=parent:parent", "busybox", "top")
	cli.DockerCmd(c, "run", "--name", "child", "-d", "--net=container:parent", "busybox", "top")
	cli.DockerCmd(c, "run", "-d", "--link=child:child", "busybox", "top")
}

func (s *DockerCLIRunSuite) TestRunLoopbackOnlyExistsWhenNetworkingDisabled(c *testing.T) {
	// TODO Windows: This may be possible to convert.
	testRequires(c, DaemonIsLinux)
	out := cli.DockerCmd(c, "run", "--net=none", "busybox", "ip", "-o", "-4", "a", "show", "up").Combined()

	var (
		count = 0
		parts = strings.Split(out, "\n")
	)

	for _, l := range parts {
		if l != "" {
			count++
		}
	}

	if count != 1 {
		c.Fatalf("Wrong interface count in container %d", count)
	}

	if !strings.HasPrefix(out, "1: lo") {
		c.Fatalf("Wrong interface in test container: expected [1: lo], got %s", out)
	}
}

// Issue #4681
func (s *DockerCLIRunSuite) TestRunLoopbackWhenNetworkDisabled(c *testing.T) {
	if testEnv.DaemonInfo.OSType == "windows" {
		cli.DockerCmd(c, "run", "--net=none", testEnv.PlatformDefaults.BaseImage, "ping", "-n", "1", "127.0.0.1")
	} else {
		cli.DockerCmd(c, "run", "--net=none", "busybox", "ping", "-c", "1", "127.0.0.1")
	}
}

func (s *DockerCLIRunSuite) TestRunModeNetContainerHostname(c *testing.T) {
	// Windows does not support --net=container
	testRequires(c, DaemonIsLinux)

	cli.DockerCmd(c, "run", "-i", "-d", "--name", "parent", "busybox", "top")
	out := cli.DockerCmd(c, "exec", "parent", "cat", "/etc/hostname").Combined()
	out1 := cli.DockerCmd(c, "run", "--net=container:parent", "busybox", "cat", "/etc/hostname").Combined()

	if out1 != out {
		c.Fatal("containers with shared net namespace should have same hostname")
	}
}

func (s *DockerCLIRunSuite) TestRunNetworkNotInitializedNoneMode(c *testing.T) {
	// TODO Windows: Network settings are not currently propagated. This may
	// be resolved in the future with the move to libnetwork and CNM.
	testRequires(c, DaemonIsLinux)
	id := cli.DockerCmd(c, "run", "-d", "--net=none", "busybox", "top").Stdout()
	id = strings.TrimSpace(id)

	// Inspecting in JSON format, to prevent the "invalid IP" output from netip.Addr for empty IP-addresses.
	res := cli.DockerCmd(c, "container", "inspect", "--format", "{{json .NetworkSettings.Networks.none.IPAddress}}", id).Combined()
	if actual := strings.Trim(strings.TrimSpace(res), `"`); actual != "" {
		c.Fatalf("For 'none' mode network must not be initialized, but container got IP: %q", actual)
	}
}

func (s *DockerCLIRunSuite) TestTwoContainersInNetHost(c *testing.T) {
	// Not applicable as Windows does not support --net=host
	testRequires(c, DaemonIsLinux, NotUserNamespace, NotUserNamespace)
	cli.DockerCmd(c, "run", "-d", "--net=host", "--name=first", "busybox", "top")
	cli.DockerCmd(c, "run", "-d", "--net=host", "--name=second", "busybox", "top")
	cli.DockerCmd(c, "stop", "first")
	cli.DockerCmd(c, "stop", "second")
}

func (s *DockerCLIRunSuite) TestContainersInUserDefinedNetwork(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	cli.DockerCmd(c, "network", "create", "-d", "bridge", "testnetwork")
	cli.DockerCmd(c, "run", "-d", "--net=testnetwork", "--name=first", "busybox", "top")
	cli.WaitRun(c, "first")
	cli.DockerCmd(c, "run", "-t", "--net=testnetwork", "--name=second", "busybox", "ping", "-c", "1", "first")
}

func (s *DockerCLIRunSuite) TestContainersInMultipleNetworks(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	// Create 2 networks using bridge driver
	cli.DockerCmd(c, "network", "create", "-d", "bridge", "testnetwork1")
	cli.DockerCmd(c, "network", "create", "-d", "bridge", "testnetwork2")
	// Run and connect containers to testnetwork1
	cli.DockerCmd(c, "run", "-d", "--net=testnetwork1", "--name=first", "busybox", "top")
	cli.WaitRun(c, "first")
	cli.DockerCmd(c, "run", "-d", "--net=testnetwork1", "--name=second", "busybox", "top")
	cli.WaitRun(c, "second")
	// Check connectivity between containers in testnetwork2
	cli.DockerCmd(c, "exec", "first", "ping", "-c", "1", "second.testnetwork1")
	// Connect containers to testnetwork2
	cli.DockerCmd(c, "network", "connect", "testnetwork2", "first")
	cli.DockerCmd(c, "network", "connect", "testnetwork2", "second")
	// Check connectivity between containers
	cli.DockerCmd(c, "exec", "second", "ping", "-c", "1", "first.testnetwork2")
}

func (s *DockerCLIRunSuite) TestContainersNetworkIsolation(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	// Create 2 networks using bridge driver
	cli.DockerCmd(c, "network", "create", "-d", "bridge", "testnetwork1")
	cli.DockerCmd(c, "network", "create", "-d", "bridge", "testnetwork2")
	// Run 1 container in testnetwork1 and another in testnetwork2
	cli.DockerCmd(c, "run", "-d", "--net=testnetwork1", "--name=first", "busybox", "top")
	cli.WaitRun(c, "first")
	cli.DockerCmd(c, "run", "-d", "--net=testnetwork2", "--name=second", "busybox", "top")
	cli.WaitRun(c, "second")

	// Check Isolation between containers : ping must fail
	_, _, err := dockerCmdWithError("exec", "first", "ping", "-c", "1", "second")
	assert.ErrorContains(c, err, "")
	// Connect first container to testnetwork2
	cli.DockerCmd(c, "network", "connect", "testnetwork2", "first")
	// ping must succeed now
	_, _, err = dockerCmdWithError("exec", "first", "ping", "-c", "1", "second")
	assert.NilError(c, err)

	// Disconnect first container from testnetwork2
	cli.DockerCmd(c, "network", "disconnect", "testnetwork2", "first")
	// ping must fail again
	_, _, err = dockerCmdWithError("exec", "first", "ping", "-c", "1", "second")
	assert.ErrorContains(c, err, "")
}

func (s *DockerCLIRunSuite) TestNetworkRmWithActiveContainers(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	// Create 2 networks using bridge driver
	cli.DockerCmd(c, "network", "create", "-d", "bridge", "testnetwork1")
	// Run and connect containers to testnetwork1
	cli.DockerCmd(c, "run", "-d", "--net=testnetwork1", "--name=first", "busybox", "top")
	cli.WaitRun(c, "first")
	cli.DockerCmd(c, "run", "-d", "--net=testnetwork1", "--name=second", "busybox", "top")
	cli.WaitRun(c, "second")
	// Network delete with active containers must fail
	_, _, err := dockerCmdWithError("network", "rm", "testnetwork1")
	assert.ErrorContains(c, err, "")

	cli.DockerCmd(c, "stop", "first")
	_, _, err = dockerCmdWithError("network", "rm", "testnetwork1")
	assert.ErrorContains(c, err, "")
}

func (s *DockerCLIRunSuite) TestContainerRestartInMultipleNetworks(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	// Create 2 networks using bridge driver
	cli.DockerCmd(c, "network", "create", "-d", "bridge", "testnetwork1")
	cli.DockerCmd(c, "network", "create", "-d", "bridge", "testnetwork2")

	// Run and connect containers to testnetwork1
	cli.DockerCmd(c, "run", "-d", "--net=testnetwork1", "--name=first", "busybox", "top")
	cli.WaitRun(c, "first")
	cli.DockerCmd(c, "run", "-d", "--net=testnetwork1", "--name=second", "busybox", "top")
	cli.WaitRun(c, "second")
	// Check connectivity between containers in testnetwork2
	cli.DockerCmd(c, "exec", "first", "ping", "-c", "1", "second.testnetwork1")
	// Connect containers to testnetwork2
	cli.DockerCmd(c, "network", "connect", "testnetwork2", "first")
	cli.DockerCmd(c, "network", "connect", "testnetwork2", "second")
	// Check connectivity between containers
	cli.DockerCmd(c, "exec", "second", "ping", "-c", "1", "first.testnetwork2")

	// Stop second container and test ping failures on both networks
	cli.DockerCmd(c, "stop", "second")
	_, _, err := dockerCmdWithError("exec", "first", "ping", "-c", "1", "second.testnetwork1")
	assert.ErrorContains(c, err, "")
	_, _, err = dockerCmdWithError("exec", "first", "ping", "-c", "1", "second.testnetwork2")
	assert.ErrorContains(c, err, "")

	// Start second container and connectivity must be restored on both networks
	cli.DockerCmd(c, "start", "second")
	cli.DockerCmd(c, "exec", "first", "ping", "-c", "1", "second.testnetwork1")
	cli.DockerCmd(c, "exec", "second", "ping", "-c", "1", "first.testnetwork2")
}

func (s *DockerCLIRunSuite) TestContainerWithConflictingHostNetworks(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	// Run a container with --net=host
	cli.DockerCmd(c, "run", "-d", "--net=host", "--name=first", "busybox", "top")
	cli.WaitRun(c, "first")

	// Create a network using bridge driver
	cli.DockerCmd(c, "network", "create", "-d", "bridge", "testnetwork1")

	// Connecting to the user-defined network must fail
	_, _, err := dockerCmdWithError("network", "connect", "testnetwork1", "first")
	assert.ErrorContains(c, err, "")
}

func (s *DockerCLIRunSuite) TestContainerWithConflictingSharedNetwork(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	cli.DockerCmd(c, "run", "-d", "--name=first", "busybox", "top")
	cli.WaitRun(c, "first")
	// Run second container in first container's network namespace
	cli.DockerCmd(c, "run", "-d", "--net=container:first", "--name=second", "busybox", "top")
	cli.WaitRun(c, "second")

	// Create a network using bridge driver
	cli.DockerCmd(c, "network", "create", "-d", "bridge", "testnetwork1")

	// Connecting to the user-defined network must fail
	out, _, err := dockerCmdWithError("network", "connect", "testnetwork1", "second")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, is.Contains(out, "container sharing network namespace with another container or host cannot be connected to any other network"))
}

func (s *DockerCLIRunSuite) TestContainerWithConflictingNoneNetwork(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	cli.DockerCmd(c, "run", "-d", "--net=none", "--name=first", "busybox", "top")
	cli.WaitRun(c, "first")

	// Create a network using bridge driver
	cli.DockerCmd(c, "network", "create", "-d", "bridge", "testnetwork1")

	// Connecting to the user-defined network must fail
	out, _, err := dockerCmdWithError("network", "connect", "testnetwork1", "first")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, is.Contains(out, "container cannot be connected to multiple networks with one of the networks in private (none) mode"))
	// create a container connected to testnetwork1
	cli.DockerCmd(c, "run", "-d", "--net=testnetwork1", "--name=second", "busybox", "top")
	cli.WaitRun(c, "second")

	// Connect second container to none network. it must fail as well
	_, _, err = dockerCmdWithError("network", "connect", "none", "second")
	assert.ErrorContains(c, err, "")
}

// #11957 - stdin with no tty does not exit if stdin is not closed even though container exited
func (s *DockerCLIRunSuite) TestRunStdinBlockedAfterContainerExit(c *testing.T) {
	name := "test-stdin-blocked-" + stringid.GenerateRandomID()

	cmd := exec.Command(dockerBinary, "run", "-i", "--name", name, "busybox", "true")
	in, err := cmd.StdinPipe()
	assert.NilError(c, err)
	defer in.Close()
	stdout := bytes.NewBuffer(nil)
	cmd.Stdout = stdout
	cmd.Stderr = stdout
	assert.NilError(c, cmd.Start())
	c.Cleanup(func() {
		dockerCmdWithError("rm", "-f", name)
	})

	exitTimeout := 10 * time.Second
	if DaemonIsWindows() {
		exitTimeout = 60 * time.Second
	}
	cli.WaitExited(c, name, exitTimeout)

	cmdExited := make(chan error, 1)
	go func() {
		cmdExited <- cmd.Wait()
	}()

	select {
	case err := <-cmdExited:
		assert.Assert(c, err == nil, stdout.String())
	case <-time.After(30 * time.Second):
		c.Fatal("timeout waiting for command to exit")
	}
}

func (s *DockerCLIRunSuite) TestRunWrongCpusetCpusFlagValue(c *testing.T) {
	// TODO Windows: This needs validation (error out) in the daemon.
	testRequires(c, DaemonIsLinux)
	out, exitCode, err := dockerCmdWithError("run", "--cpuset-cpus", "1-10,11--", "busybox", "true")
	assert.ErrorContains(c, err, "")
	expected := "Error response from daemon: Invalid value 1-10,11-- for cpuset cpus.\n"
	if !strings.Contains(out, expected) && exitCode != 125 {
		c.Fatalf("Expected output to contain %q with exitCode 125, got out: %q exitCode: %v", expected, out, exitCode)
	}
}

func (s *DockerCLIRunSuite) TestRunWrongCpusetMemsFlagValue(c *testing.T) {
	// TODO Windows: This needs validation (error out) in the daemon.
	testRequires(c, DaemonIsLinux)
	out, exitCode, err := dockerCmdWithError("run", "--cpuset-mems", "1-42--", "busybox", "true")
	assert.ErrorContains(c, err, "")
	expected := "Error response from daemon: Invalid value 1-42-- for cpuset mems.\n"
	if !strings.Contains(out, expected) && exitCode != 125 {
		c.Fatalf("Expected output to contain %q with exitCode 125, got out: %q exitCode: %v", expected, out, exitCode)
	}
}

// TestRunNonExecutableCmd checks that 'docker run busybox foo' exits with error code 127'
func (s *DockerCLIRunSuite) TestRunNonExecutableCmd(c *testing.T) {
	name := "testNonExecutableCmd"
	icmd.RunCommand(dockerBinary, "run", "--name", name, "busybox", "foo").Assert(c, icmd.Expected{
		ExitCode: 127,
		Error:    "exit status 127",
	})
}

// TestRunNonExistingCmd checks that 'docker run busybox /bin/foo' exits with code 127.
func (s *DockerCLIRunSuite) TestRunNonExistingCmd(c *testing.T) {
	name := "testNonExistingCmd"
	icmd.RunCommand(dockerBinary, "run", "--name", name, "busybox", "/bin/foo").Assert(c, icmd.Expected{
		ExitCode: 127,
		Error:    "exit status 127",
	})
}

// TestCmdCannotBeInvoked checks that 'docker run busybox /etc' exits with 126, or
// 127 on Windows. The difference is that in Windows, the container must be started
// as that's when the check is made (and yes, by its design...)
func (s *DockerCLIRunSuite) TestCmdCannotBeInvoked(c *testing.T) {
	expected := 126
	if testEnv.DaemonInfo.OSType == "windows" {
		expected = 127
	}
	name := "testCmdCannotBeInvoked"
	icmd.RunCommand(dockerBinary, "run", "--name", name, "busybox", "/etc").Assert(c, icmd.Expected{
		ExitCode: expected,
		Error:    fmt.Sprintf("exit status %d", expected),
	})
}

// TestRunNonExistingImage checks that 'docker run foo' exits with error msg 125 and contains  'Unable to find image'
// FIXME(vdemeester) should be a unit test
func (s *DockerCLIRunSuite) TestRunNonExistingImage(c *testing.T) {
	icmd.RunCommand(dockerBinary, "run", "foo").Assert(c, icmd.Expected{
		ExitCode: 125,
		Err:      "Unable to find image",
	})
}

// TestDockerFails checks that 'docker run -foo busybox' exits with 125 to signal docker run failed
// FIXME(vdemeester) should be a unit test
func (s *DockerCLIRunSuite) TestDockerFails(c *testing.T) {
	icmd.RunCommand(dockerBinary, "run", "-foo", "busybox").Assert(c, icmd.Expected{
		ExitCode: 125,
		Error:    "exit status 125",
	})
}

// TestRunInvalidReference invokes docker run with a bad reference.
func (s *DockerCLIRunSuite) TestRunInvalidReference(c *testing.T) {
	out, exit, _ := dockerCmdWithError("run", "busybox@foo")
	if exit == 0 {
		c.Fatalf("expected non-zero exist code; received %d", exit)
	}

	if !strings.Contains(out, "invalid reference format") {
		c.Fatalf(`Expected "invalid reference format" in output; got: %s`, out)
	}
}

// Test fix for issue #17854
func (s *DockerCLIRunSuite) TestRunInitLayerPathOwnership(c *testing.T) {
	// Not applicable on Windows as it does not support Linux uid/gid ownership
	testRequires(c, DaemonIsLinux)
	name := "testetcfileownership"
	cli.BuildCmd(c, name, build.WithDockerfile(`FROM busybox
		RUN echo 'dockerio:x:1001:1001::/bin:/bin/false' >> /etc/passwd
		RUN echo 'dockerio:x:1001:' >> /etc/group
		RUN chown dockerio:dockerio /etc`))

	// Test that dockerio ownership of /etc is retained at runtime
	out := cli.DockerCmd(c, "run", "--rm", name, "stat", "-c", "%U:%G", "/etc").Combined()
	out = strings.TrimSpace(out)
	if out != "dockerio:dockerio" {
		c.Fatalf("Wrong /etc ownership: expected dockerio:dockerio, got %q", out)
	}
}

func (s *DockerCLIRunSuite) TestRunWithOomScoreAdj(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	const expected = "642"
	out := cli.DockerCmd(c, "run", "--oom-score-adj", expected, "busybox", "cat", "/proc/self/oom_score_adj").Combined()
	oomScoreAdj := strings.TrimSpace(out)
	if oomScoreAdj != expected {
		c.Fatalf("Expected oom_score_adj set to %q, got %q instead", expected, oomScoreAdj)
	}
}

func (s *DockerCLIRunSuite) TestRunWithOomScoreAdjInvalidRange(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	out, _, err := dockerCmdWithError("run", "--oom-score-adj", "1001", "busybox", "true")
	assert.ErrorContains(c, err, "")
	out = strings.ToLower(out)
	expected := "invalid value 1001"
	if !strings.Contains(out, expected) {
		c.Fatalf("Expected output to contain %q, got %q instead", expected, out)
	}
	out, _, err = dockerCmdWithError("run", "--oom-score-adj", "-1001", "busybox", "true")
	assert.ErrorContains(c, err, "")
	out = strings.ToLower(out)
	expected = "invalid value -1001"
	if !strings.Contains(out, expected) {
		c.Fatalf("Expected output to contain %q, got %q instead", expected, out)
	}
}

func (s *DockerCLIRunSuite) TestRunNamedVolumesMountedAsShared(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace)
	out, exitCode, _ := dockerCmdWithError("run", "-v", "foo:/test:shared", "busybox", "touch", "/test/somefile")
	assert.Assert(c, exitCode != 0)
	assert.Assert(c, is.Contains(out, "invalid mount config"))
}

func (s *DockerCLIRunSuite) TestRunNamedVolumeCopyImageData(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	testImg := "testvolumecopy"
	cli.BuildCmd(c, testImg, build.WithDockerfile(`
	FROM busybox
	RUN mkdir -p /foo && echo hello > /foo/hello
	`))

	cli.DockerCmd(c, "run", "-v", "foo:/foo", testImg)
	out := cli.DockerCmd(c, "run", "-v", "foo:/foo", "busybox", "cat", "/foo/hello").Stdout()
	assert.Equal(c, strings.TrimSpace(out), "hello")
}

func (s *DockerCLIRunSuite) TestRunNamedVolumeNotRemoved(c *testing.T) {
	cli.DockerCmd(c, "volume", "create", "test")

	cli.DockerCmd(c, "run", "--rm", "-v", "test:"+dPath("/foo"), "-v", dPath("/bar"), "busybox", "true")
	cli.DockerCmd(c, "volume", "inspect", "test")
	out := cli.DockerCmd(c, "volume", "ls", "-q").Combined()
	assert.Assert(c, is.Contains(out, "test"))

	cli.DockerCmd(c, "run", "--name=test", "-v", "test:"+dPath("/foo"), "-v", dPath("/bar"), "busybox", "true")
	cli.DockerCmd(c, "rm", "-fv", "test")
	cli.DockerCmd(c, "volume", "inspect", "test")
	out = cli.DockerCmd(c, "volume", "ls", "-q").Combined()
	assert.Assert(c, is.Contains(out, "test"))
}

func (s *DockerCLIRunSuite) TestRunNamedVolumesFromNotRemoved(c *testing.T) {
	cli.DockerCmd(c, "volume", "create", "test")
	cid := cli.DockerCmd(c, "run", "-d", "--name=parent", "-v", "test:"+dPath("/foo"), "-v", dPath("/bar"), "busybox", "true").Stdout()
	cli.DockerCmd(c, "run", "--name=child", "--volumes-from=parent", "busybox", "true")

	apiClient, err := client.New(client.FromEnv)
	assert.NilError(c, err)
	defer apiClient.Close()

	inspect, err := apiClient.ContainerInspect(testutil.GetContext(c), strings.TrimSpace(cid), client.ContainerInspectOptions{})
	assert.NilError(c, err)
	var vname string
	for _, v := range inspect.Container.Mounts {
		if v.Name != "test" {
			vname = v.Name
		}
	}
	assert.Assert(c, vname != "")

	// Remove the parent so there are not other references to the volumes
	cli.DockerCmd(c, "rm", "-f", "parent")
	// now remove the child and ensure the named volume (and only the named volume) still exists
	cli.DockerCmd(c, "rm", "-fv", "child")
	cli.DockerCmd(c, "volume", "inspect", "test")
	out := cli.DockerCmd(c, "volume", "ls", "-q").Combined()
	assert.Assert(c, is.Contains(out, "test"))
	assert.Assert(c, !strings.Contains(strings.TrimSpace(out), vname))
}

func (s *DockerCLIRunSuite) TestRunAttachFailedNoLeak(c *testing.T) {
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon)
	ctx := testutil.GetContext(c)
	d := daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvVars("OTEL_SDK_DISABLED=1"))
	defer func() {
		if c.Failed() {
			d.Daemon.DumpStackAndQuit()
		} else {
			d.Stop(c)
		}
		d.Cleanup(c)
	}()
	d.StartWithBusybox(ctx, c)

	// Run a dummy container to ensure all goroutines are up and running before we get a count
	_, err := d.Cmd("run", "--rm", "busybox", "true")
	assert.NilError(c, err)

	apiClient := d.NewClientT(c)

	nroutines := waitForStableGoroutineCount(ctx, c, apiClient)

	out, err := d.Cmd(append([]string{"run", "-d", "--name=test", "-p", "8000:8000", "busybox"}, sleepCommandForDaemonPlatform()...)...)
	assert.NilError(c, err, out)

	// Wait until container is fully up and running
	assert.NilError(c, d.WaitRun("test"))

	out, err = d.Cmd("run", "--name=fail", "-p", "8000:8000", "busybox", "true")

	// We will need the following `inspect` to diagnose the issue if test fails (#21247)
	out1, err1 := d.Cmd("inspect", "--format", "{{json .State}}", "test")
	out2, err2 := d.Cmd("inspect", "--format", "{{json .State}}", "fail")
	assert.Assert(c, err != nil, "Command should have failed but succeeded with: %s\nContainer 'test' [%+v]: %s\nContainer 'fail' [%+v]: %s", out, err1, out1, err2, out2)

	// check for windows error as well
	// TODO Windows Post TP5. Fix the error message string
	outLowerCase := strings.ToLower(out)
	assert.Assert(c, strings.Contains(outLowerCase, "port is already allocated") ||
		strings.Contains(outLowerCase, "were not connected because a duplicate name exists") ||
		strings.Contains(outLowerCase, "the specified port already exists") ||
		strings.Contains(outLowerCase, "hns failed with error : failed to create endpoint") ||
		strings.Contains(outLowerCase, "hns failed with error : the object already exists"), "Output: "+out)

	out, err = d.Cmd("rm", "-f", "test")
	assert.NilError(c, err, out)

	// NGoroutines is not updated right away, so we need to wait before failing
	waitForGoroutines(ctx, c, apiClient, nroutines)
}

// Test for one character directory name case (#20122)
func (s *DockerCLIRunSuite) TestRunVolumeWithOneCharacter(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	out := cli.DockerCmd(c, "run", "-v", "/tmp/q:/foo", "busybox", "sh", "-c", "find /foo").Combined()
	assert.Equal(c, strings.TrimSpace(out), "/foo")
}

func (s *DockerCLIRunSuite) TestRunVolumeCopyFlag(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Windows does not support copying data from image to the volume
	cli.BuildCmd(c, "volumecopy", build.WithDockerfile(`FROM busybox
		RUN mkdir /foo && echo hello > /foo/bar
		CMD cat /foo/bar`))
	cli.DockerCmd(c, "volume", "create", "test")

	// test with the nocopy flag
	out, _, err := dockerCmdWithError("run", "-v", "test:/foo:nocopy", "volumecopy")
	assert.ErrorContains(c, err, "", out)
	// test default behavior which is to copy for non-binds
	out = cli.DockerCmd(c, "run", "-v", "test:/foo", "volumecopy").Combined()
	assert.Equal(c, strings.TrimSpace(out), "hello")
	// error out when the volume is already populated
	out, _, err = dockerCmdWithError("run", "-v", "test:/foo:copy", "volumecopy")
	assert.ErrorContains(c, err, "", out)
	// do not error out when copy isn't explicitly set even though it's already populated
	out = cli.DockerCmd(c, "run", "-v", "test:/foo", "volumecopy").Combined()
	assert.Equal(c, strings.TrimSpace(out), "hello")

	// do not allow copy modes on volumes-from
	cli.DockerCmd(c, "run", "--name=test", "-v", "/foo", "busybox", "true")
	out, _, err = dockerCmdWithError("run", "--volumes-from=test:copy", "busybox", "true")
	assert.ErrorContains(c, err, "", out)
	out, _, err = dockerCmdWithError("run", "--volumes-from=test:nocopy", "busybox", "true")
	assert.ErrorContains(c, err, "", out)

	// do not allow copy modes on binds
	out, _, err = dockerCmdWithError("run", "-v", "/foo:/bar:copy", "busybox", "true")
	assert.ErrorContains(c, err, "", out)
	out, _, err = dockerCmdWithError("run", "-v", "/foo:/bar:nocopy", "busybox", "true")
	assert.ErrorContains(c, err, "", out)
}

// Test case for #21976
func (s *DockerCLIRunSuite) TestRunDNSInHostMode(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace)

	// FIXME(thaJeztah): localhost DNS no longer produces a warning: remove this test?
	// expectedOutput := "nameserver 127.0.0.1"
	// expectedWarning := "Localhost DNS setting"
	// cli.DockerCmd(c, "run", "--dns=127.0.0.1", "--net=host", "busybox", "cat", "/etc/resolv.conf").Assert(c, icmd.Expected{
	// 	Out: expectedOutput,
	// 	Err: expectedWarning,
	// })

	expectedOutput := "nameserver 1.2.3.4"
	cli.DockerCmd(c, "run", "--dns=1.2.3.4", "--net=host", "busybox", "cat", "/etc/resolv.conf").Assert(c, icmd.Expected{
		Out: expectedOutput,
	})

	expectedOutput = "search example.com"
	cli.DockerCmd(c, "run", "--dns-search=example.com", "--net=host", "busybox", "cat", "/etc/resolv.conf").Assert(c, icmd.Expected{
		Out: expectedOutput,
	})

	expectedOutput = "options timeout:3"
	cli.DockerCmd(c, "run", "--dns-opt=timeout:3", "--net=host", "busybox", "cat", "/etc/resolv.conf").Assert(c, icmd.Expected{
		Out: expectedOutput,
	})

	expectedOutput1 := "nameserver 1.2.3.4"
	expectedOutput2 := "search example.com"
	expectedOutput3 := "options timeout:3"
	out := cli.DockerCmd(c, "run", "--dns=1.2.3.4", "--dns-search=example.com", "--dns-opt=timeout:3", "--net=host", "busybox", "cat", "/etc/resolv.conf").Combined()
	assert.Assert(c, strings.Contains(out, expectedOutput1), "Expected '%s', but got %q", expectedOutput1, out)
	assert.Assert(c, strings.Contains(out, expectedOutput2), "Expected '%s', but got %q", expectedOutput2, out)
	assert.Assert(c, strings.Contains(out, expectedOutput3), "Expected '%s', but got %q", expectedOutput3, out)
}

// Test case for #21976
func (s *DockerCLIRunSuite) TestRunAddHostInHostMode(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace)

	expectedOutput := "1.2.3.4\textra"
	out := cli.DockerCmd(c, "run", "--add-host=extra:1.2.3.4", "--net=host", "busybox", "cat", "/etc/hosts").Combined()
	assert.Assert(c, strings.Contains(out, expectedOutput), "Expected '%s', but got %q", expectedOutput, out)
}

func (s *DockerCLIRunSuite) TestRunRmAndWait(c *testing.T) {
	cli.DockerCmd(c, "run", "--name=test", "--rm", "-d", "busybox", "sh", "-c", "sleep 3;exit 2")

	out, code, err := dockerCmdWithError("wait", "test")
	assert.Assert(c, err == nil, "out: %s; exit code: %d", out, code)
	assert.Equal(c, out, "2\n", "exit code: %d", code)
	assert.Equal(c, code, 0)
}

// Test that auto-remove is performed by the daemon (API 1.25 and above)
func (s *DockerCLIRunSuite) TestRunRm(c *testing.T) {
	name := "miss-me-when-im-gone"
	cli.DockerCmd(c, "run", "--name="+name, "--rm", "busybox")

	cli.Docker(cli.Args("inspect", name), cli.Format(".name")).Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "such object: " + name, // [Nn]o such object
	})
}

// Test that auto-remove is performed by the client on API versions that do not support daemon-side api-remove (API < 1.25)
func (s *DockerCLIRunSuite) TestRunRmPre125Api(c *testing.T) {
	name := "miss-me-when-im-gone"
	envs := appendBaseEnv(os.Getenv("DOCKER_TLS_VERIFY") != "", "DOCKER_API_VERSION=1.24")
	cli.Docker(cli.Args("run", "--name="+name, "--rm", "busybox"), cli.WithEnvironmentVariables(envs...)).Assert(c, icmd.Success)
	time.Sleep(5 * time.Second) // daemon may be in process of removing
	result := cli.Docker(cli.Args("inspect", name), cli.Format(".State.Status"))
	result.Assert(c, icmd.Expected{
		ExitCode: 1,
	})
	assert.Check(c, result.Error != nil)
	out := strings.ToLower(result.Stderr())
	if !strings.Contains(out, "no such object: "+name) && !strings.Contains(out, "no such container: "+name) {
		c.Error(fmt.Errorf("unexpected error: %s", result.Error))
	}
}

// Test case for #23498
func (s *DockerCLIRunSuite) TestRunUnsetEntrypoint(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	name := "test-entrypoint"
	dockerfile := `FROM busybox
ADD entrypoint.sh /entrypoint.sh
RUN chmod 755 /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
CMD echo foobar`

	ctx := fakecontext.New(c, "",
		fakecontext.WithDockerfile(dockerfile),
		fakecontext.WithFiles(map[string]string{
			"entrypoint.sh": `#!/bin/sh
echo "I am an entrypoint"
exec "$@"`,
		}))
	defer ctx.Close()

	cli.BuildCmd(c, name, build.WithExternalBuildContext(ctx))

	out := cli.DockerCmd(c, "run", "--entrypoint=", "-t", name, "echo", "foo").Combined()
	assert.Equal(c, strings.TrimSpace(out), "foo")

	// CMD will be reset as well (the same as setting a custom entrypoint)
	cli.Docker(cli.Args("run", "--entrypoint=", "-t", name)).Assert(c, icmd.Expected{
		ExitCode: 125,
		Err:      "no command specified",
	})
}

func (s *DockerDaemonSuite) TestRunWithUlimitAndDaemonDefault(c *testing.T) {
	ctx := testutil.GetContext(c)
	d := daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvVars("OTEL_SDK_DISABLED=1"))
	defer func() {
		d.Stop(c)
		d.Cleanup(c)
	}()
	d.StartWithBusybox(ctx, c, "--debug", "--default-ulimit=nofile=65535")

	name := "test-A"
	_, err := d.Cmd("run", "--name", name, "-d", "busybox", "top")
	assert.NilError(c, err)
	assert.NilError(c, d.WaitRun(name))

	out, err := d.Cmd("inspect", "--format", "{{.HostConfig.Ulimits}}", name)
	assert.NilError(c, err)
	assert.Assert(c, is.Contains(out, "[nofile=65535:65535]"))
	name = "test-B"
	_, err = d.Cmd("run", "--name", name, "--ulimit=nofile=50", "-d", "busybox", "top")
	assert.NilError(c, err)
	assert.NilError(c, d.WaitRun(name))

	out, err = d.Cmd("inspect", "--format", "{{.HostConfig.Ulimits}}", name)
	assert.NilError(c, err)
	assert.Assert(c, is.Contains(out, "[nofile=50:50]"))
}

func (s *DockerCLIRunSuite) TestRunStoppedLoggingDriverNoLeak(c *testing.T) {
	client := testEnv.APIClient()
	ctx := testutil.GetContext(c)
	nroutines, err := getGoroutineNumber(ctx, client)
	assert.NilError(c, err)

	out, _, err := dockerCmdWithError("run", "--name=fail", "--log-driver=splunk", "busybox", "true")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, strings.Contains(out, "failed to initialize logging driver"), "error should be about logging driver, got output %s", out)

	// NGoroutines is not updated right away, so we need to wait before failing
	waitForGoroutines(ctx, c, client, nroutines)
}

// Handles error conditions for --credentialspec. Validating E2E success cases
// requires additional infrastructure (AD for example) on CI servers.
func (s *DockerCLIRunSuite) TestRunCredentialSpecFailures(c *testing.T) {
	testRequires(c, DaemonIsWindows)

	attempts := []struct{ value, expectedError string }{
		{"rubbish", "invalid credential spec security option - value must be prefixed by 'file://', 'registry://', or 'raw://' followed by a non-empty value"},
		{"rubbish://", "invalid credential spec security option - value must be prefixed by 'file://', 'registry://', or 'raw://' followed by a non-empty value"},
		{"file://", "invalid credential spec security option - value must be prefixed by 'file://', 'registry://', or 'raw://' followed by a non-empty value"},
		{"registry://", "invalid credential spec security option - value must be prefixed by 'file://', 'registry://', or 'raw://' followed by a non-empty value"},
		{`file://c:\blah.txt`, "path cannot be absolute"},
		{`file://doesnotexist.txt`, "The system cannot find the file specified"},
	}
	for _, attempt := range attempts {
		_, _, err := dockerCmdWithError("run", "--security-opt=credentialspec="+attempt.value, "busybox", "true")
		assert.Assert(c, err != nil, "%s expected non-nil err", attempt.value)
		assert.Assert(c, strings.Contains(err.Error(), attempt.expectedError), "%s expected %s got %s", attempt.value, attempt.expectedError, err)
	}
}

// Windows specific test to validate credential specs with a well-formed spec.
func (s *DockerCLIRunSuite) TestRunCredentialSpecWellFormed(c *testing.T) {
	testRequires(c, DaemonIsWindows, testEnv.IsLocalDaemon)

	validCredSpecs := readFile(`fixtures\credentialspecs\valid.json`, c)
	writeFile(filepath.Join(testEnv.DaemonInfo.DockerRootDir, `credentialspecs\valid.json`), validCredSpecs, c)

	for _, value := range []string{"file://valid.json", "raw://" + validCredSpecs} {
		// `nltest /PARENTDOMAIN` simply reads the local config, and does not require having an AD
		// controller handy
		out := cli.DockerCmd(c, "run", "--rm", "--security-opt=credentialspec="+value, minimalBaseImage(), "nltest", "/PARENTDOMAIN").Combined()

		assert.Assert(c, is.Contains(out, "hyperv.local."))
		assert.Assert(c, is.Contains(out, "The command completed successfully"))
	}
}

func (s *DockerCLIRunSuite) TestRunDuplicateMount(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux, NotUserNamespace)

	const data = "touch-me-foo-bar\n"
	tmpFile := filepath.Join(c.TempDir(), "touch-me")
	err := os.WriteFile(tmpFile, []byte(data), 0o600)
	assert.NilError(c, err)

	const name = "test-run-duplicate-mount"
	out := cli.DockerCmd(c, "run", "--name", name, "-v", "/tmp:/tmp", "-v", "/tmp:/tmp", "busybox", "sh", "-c", "cat "+tmpFile+" && ls /").Combined()
	assert.Assert(c, !strings.Contains(out, "tmp:"))
	assert.Assert(c, is.Contains(out, data))
	out = inspectFieldJSON(c, name, "Config.Volumes")
	assert.Assert(c, is.Contains(out, "null"))
}

func (s *DockerCLIRunSuite) TestRunWindowsWithCPUCount(c *testing.T) {
	testRequires(c, DaemonIsWindows)

	out := cli.DockerCmd(c, "run", "--cpu-count=1", "--name", "test", "busybox", "echo", "testing").Combined()
	assert.Equal(c, strings.TrimSpace(out), "testing")

	out = inspectField(c, "test", "HostConfig.CPUCount")
	assert.Equal(c, out, "1")
}

func (s *DockerCLIRunSuite) TestRunWindowsWithCPUShares(c *testing.T) {
	testRequires(c, DaemonIsWindows)

	out := cli.DockerCmd(c, "run", "--cpu-shares=1000", "--name", "test", "busybox", "echo", "testing").Combined()
	assert.Equal(c, strings.TrimSpace(out), "testing")

	out = inspectField(c, "test", "HostConfig.CPUShares")
	assert.Equal(c, out, "1000")
}

func (s *DockerCLIRunSuite) TestRunWindowsWithCPUPercent(c *testing.T) {
	testRequires(c, DaemonIsWindows)

	out := cli.DockerCmd(c, "run", "--cpu-percent=80", "--name", "test", "busybox", "echo", "testing").Combined()
	assert.Equal(c, strings.TrimSpace(out), "testing")

	out = inspectField(c, "test", "HostConfig.CPUPercent")
	assert.Equal(c, out, "80")
}

func (s *DockerCLIRunSuite) TestRunProcessIsolationWithCPUCountCPUSharesAndCPUPercent(c *testing.T) {
	testRequires(c, DaemonIsWindows, testEnv.DaemonInfo.Isolation.IsProcess)

	out := cli.DockerCmd(c, "run", "--cpu-count=1", "--cpu-shares=1000", "--cpu-percent=80", "--name", "test", "busybox", "echo", "testing").Combined()
	assert.Assert(c, is.Contains(strings.TrimSpace(out), "WARNING: Conflicting options: CPU count takes priority over CPU shares on Windows Server Containers. CPU shares discarded"))
	assert.Assert(c, is.Contains(strings.TrimSpace(out), "WARNING: Conflicting options: CPU count takes priority over CPU percent on Windows Server Containers. CPU percent discarded"))
	assert.Assert(c, is.Contains(strings.TrimSpace(out), "testing"))
	out = inspectField(c, "test", "HostConfig.CPUCount")
	assert.Equal(c, out, "1")

	out = inspectField(c, "test", "HostConfig.CPUShares")
	assert.Equal(c, out, "0")

	out = inspectField(c, "test", "HostConfig.CPUPercent")
	assert.Equal(c, out, "0")
}

func (s *DockerCLIRunSuite) TestRunHypervIsolationWithCPUCountCPUSharesAndCPUPercent(c *testing.T) {
	testRequires(c, DaemonIsWindows, testEnv.DaemonInfo.Isolation.IsHyperV)

	out := cli.DockerCmd(c, "run", "--cpu-count=1", "--cpu-shares=1000", "--cpu-percent=80", "--name", "test", "busybox", "echo", "testing").Combined()
	assert.Assert(c, is.Contains(strings.TrimSpace(out), "testing"))
	out = inspectField(c, "test", "HostConfig.CPUCount")
	assert.Equal(c, out, "1")

	out = inspectField(c, "test", "HostConfig.CPUShares")
	assert.Equal(c, out, "1000")

	out = inspectField(c, "test", "HostConfig.CPUPercent")
	assert.Equal(c, out, "80")
}

// Test for #25099
func (s *DockerCLIRunSuite) TestRunEmptyEnv(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	expectedOutput := "invalid environment variable:"

	out, _, err := dockerCmdWithError("run", "-e", "", "busybox", "true")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, is.Contains(out, expectedOutput))

	out, _, err = dockerCmdWithError("run", "-e", "=", "busybox", "true")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, is.Contains(out, expectedOutput))

	out, _, err = dockerCmdWithError("run", "-e", "=foo", "busybox", "true")
	assert.ErrorContains(c, err, "")
	assert.Assert(c, is.Contains(out, expectedOutput))
}

// #28658
func (s *DockerCLIRunSuite) TestSlowStdinClosing(c *testing.T) {
	if DaemonIsWindows() {
		skip.If(c, testEnv.GitHubActions())
	}
	const repeat = 3 // regression happened 50% of the time
	for i := range repeat {
		c.Run(strconv.Itoa(i), func(c *testing.T) {
			cmd := icmd.Cmd{
				Command: []string{dockerBinary, "run", "--rm", "-i", "busybox", "cat"},
				Stdin:   &delayedReader{},
			}
			done := make(chan error, 1)
			go func() {
				result := icmd.RunCmd(cmd)
				if out := result.Combined(); out != "" {
					c.Log(out)
				}
				done <- result.Error
			}()

			select {
			case <-time.After(30 * time.Second):
				c.Fatal("running container timed out") // cleanup in teardown
			case err := <-done:
				assert.NilError(c, err)
			}
		})
	}
}

type delayedReader struct{}

func (s *delayedReader) Read([]byte) (int, error) {
	time.Sleep(500 * time.Millisecond)
	return 0, io.EOF
}

// #28823 (originally #28639)
func (s *DockerCLIRunSuite) TestRunMountReadOnlyDevShm(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux, NotUserNamespace)
	emptyDir, err := os.MkdirTemp("", "test-read-only-dev-shm")
	assert.NilError(c, err)
	defer os.RemoveAll(emptyDir)
	out, _, err := dockerCmdWithError("run", "--rm", "--read-only",
		"-v", emptyDir+":/dev/shm:ro",
		"busybox", "touch", "/dev/shm/foo")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, is.Contains(out, "Read-only file system"))
}

func (s *DockerCLIRunSuite) TestRunMount(c *testing.T) {
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon, NotUserNamespace)

	// mnt1, mnt2, and testCatFooBar are commonly used in multiple test cases
	tmpDir, err := os.MkdirTemp("", "mount")
	if err != nil {
		c.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)
	mnt1, mnt2 := path.Join(tmpDir, "mnt1"), path.Join(tmpDir, "mnt2")
	if err := os.Mkdir(mnt1, 0o755); err != nil {
		c.Fatal(err)
	}
	if err := os.Mkdir(mnt2, 0o755); err != nil {
		c.Fatal(err)
	}
	if err := os.WriteFile(path.Join(mnt1, "test1"), []byte("test1"), 0o644); err != nil {
		c.Fatal(err)
	}
	if err := os.WriteFile(path.Join(mnt2, "test2"), []byte("test2"), 0o644); err != nil {
		c.Fatal(err)
	}
	testCatFooBar := func(cName string) error {
		out := cli.DockerCmd(c, "exec", cName, "cat", "/foo/test1").Stdout()
		if out != "test1" {
			return fmt.Errorf("%s not mounted on /foo", mnt1)
		}
		out = cli.DockerCmd(c, "exec", cName, "cat", "/bar/test2").Stdout()
		if out != "test2" {
			return fmt.Errorf("%s not mounted on /bar", mnt2)
		}
		return nil
	}

	type testCase struct {
		equivalents [][]string
		valid       bool
		// fn should be nil if valid==false
		fn func(cName string) error
	}
	cases := []testCase{
		{
			equivalents: [][]string{
				{
					"--mount", fmt.Sprintf("type=bind,src=%s,dst=/foo", mnt1),
					"--mount", fmt.Sprintf("type=bind,src=%s,dst=/bar", mnt2),
				},
				{
					"--mount", fmt.Sprintf("type=bind,src=%s,dst=/foo", mnt1),
					"--mount", fmt.Sprintf("type=bind,src=%s,target=/bar", mnt2),
				},
				{
					"--volume", mnt1 + ":/foo",
					"--mount", fmt.Sprintf("type=bind,src=%s,target=/bar", mnt2),
				},
			},
			valid: true,
			fn:    testCatFooBar,
		},
		{
			equivalents: [][]string{
				{
					"--mount", fmt.Sprintf("type=volume,src=%s,dst=/foo", mnt1),
					"--mount", fmt.Sprintf("type=volume,src=%s,dst=/bar", mnt2),
				},
				{
					"--mount", fmt.Sprintf("type=volume,src=%s,dst=/foo", mnt1),
					"--mount", fmt.Sprintf("type=volume,src=%s,target=/bar", mnt2),
				},
			},
			valid: false,
		},
		{
			equivalents: [][]string{
				{
					"--mount", fmt.Sprintf("type=bind,src=%s,dst=/foo", mnt1),
					"--mount", fmt.Sprintf("type=volume,src=%s,dst=/bar", mnt2),
				},
				{
					"--volume", mnt1 + ":/foo",
					"--mount", fmt.Sprintf("type=volume,src=%s,target=/bar", mnt2),
				},
			},
			valid: false,
			fn:    testCatFooBar,
		},
		{
			equivalents: [][]string{
				{
					"--read-only",
					"--mount", "type=volume,dst=/bar",
				},
			},
			valid: true,
			fn: func(cName string) error {
				_, _, err := dockerCmdWithError("exec", cName, "touch", "/bar/icanwritehere")
				return err
			},
		},
		{
			equivalents: [][]string{
				{
					"--read-only",
					"--mount", fmt.Sprintf("type=bind,src=%s,dst=/foo", mnt1),
					"--mount", "type=volume,dst=/bar",
				},
				{
					"--read-only",
					"--volume", mnt1 + ":/foo",
					"--mount", "type=volume,dst=/bar",
				},
			},
			valid: true,
			fn: func(cName string) error {
				out := cli.DockerCmd(c, "exec", cName, "cat", "/foo/test1").Combined()
				if out != "test1" {
					return fmt.Errorf("%s not mounted on /foo", mnt1)
				}
				_, _, err := dockerCmdWithError("exec", cName, "touch", "/bar/icanwritehere")
				return err
			},
		},
		{
			equivalents: [][]string{
				{
					"--mount", fmt.Sprintf("type=bind,src=%s,dst=/foo", mnt1),
					"--mount", fmt.Sprintf("type=bind,src=%s,dst=/foo", mnt2),
				},
				{
					"--mount", fmt.Sprintf("type=bind,src=%s,dst=/foo", mnt1),
					"--mount", fmt.Sprintf("type=bind,src=%s,target=/foo", mnt2),
				},
				{
					"--volume", mnt1 + ":/foo",
					"--mount", fmt.Sprintf("type=bind,src=%s,target=/foo", mnt2),
				},
			},
			valid: false,
		},
		{
			equivalents: [][]string{
				{
					"--volume", mnt1 + ":/foo",
					"--mount", fmt.Sprintf("type=volume,src=%s,target=/foo", mnt2),
				},
			},
			valid: false,
		},
		{
			equivalents: [][]string{
				{
					"--mount", "type=volume,target=/foo",
					"--mount", "type=volume,target=/foo",
				},
			},
			valid: false,
		},
	}

	for i, testCase := range cases {
		for j, opts := range testCase.equivalents {
			cName := fmt.Sprintf("mount-%d-%d", i, j)
			_, _, err := dockerCmdWithError(append([]string{"run", "-i", "-d", "--name", cName},
				append(opts, []string{"busybox", "top"}...)...)...)
			if testCase.valid {
				assert.Assert(c, err == nil, "got error while creating a container with %v (%s)", opts, cName)
				assert.Assert(c, testCase.fn(cName) == nil, "got error while executing test for %v (%s)", opts, cName)
				cli.DockerCmd(c, "rm", "-f", cName)
			} else {
				assert.Assert(c, err != nil, "got nil while creating a container with %v (%s)", opts, cName)
			}
		}
	}
}

// Test that passing a FQDN as hostname properly sets hostname, and
// /etc/hostname. Test case for 29100
func (s *DockerCLIRunSuite) TestRunHostnameFQDN(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	expectedOutput := "foobar.example.com\nfoobar.example.com\nfoobar\nexample.com\nfoobar.example.com" //nolint:dupword
	out := cli.DockerCmd(c, "run", "--hostname=foobar.example.com", "busybox", "sh", "-c", `cat /etc/hostname && hostname && hostname -s && hostname -d && hostname -f`).Combined()
	assert.Equal(c, strings.TrimSpace(out), expectedOutput)

	out = cli.DockerCmd(c, "run", "--hostname=foobar.example.com", "busybox", "sh", "-c", `cat /etc/hosts`).Combined()
	expectedOutput = "foobar.example.com foobar"
	assert.Assert(c, is.Contains(strings.TrimSpace(out), expectedOutput))
}

// Test case for 29129
func (s *DockerCLIRunSuite) TestRunHostnameInHostMode(c *testing.T) {
	testRequires(c, DaemonIsLinux, NotUserNamespace)

	const expectedOutput = "foobar\nfoobar" //nolint:dupword
	out := cli.DockerCmd(c, "run", "--net=host", "--hostname=foobar", "busybox", "sh", "-c", `echo $HOSTNAME && hostname`).Combined()
	assert.Equal(c, strings.TrimSpace(out), expectedOutput)
}

func (s *DockerCLIRunSuite) TestRunAddDeviceCgroupRule(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	skip.If(c, onlyCgroupsv2(), "FIXME: cgroupsV2 not supported yet")

	const deviceRule = "c 7:128 rwm"

	out := cli.DockerCmd(c, "run", "--rm", "busybox", "cat", "/sys/fs/cgroup/devices/devices.list").Combined()
	if strings.Contains(out, deviceRule) {
		c.Fatalf("%s shouldn't been in the device.list", deviceRule)
	}

	out = cli.DockerCmd(c, "run", "--rm", "--device-cgroup-rule="+deviceRule, "busybox", "grep", deviceRule, "/sys/fs/cgroup/devices/devices.list").Combined()
	assert.Equal(c, strings.TrimSpace(out), deviceRule)
}

// Verifies that running as local system is operating correctly on Windows
func (s *DockerCLIRunSuite) TestWindowsRunAsSystem(c *testing.T) {
	testRequires(c, DaemonIsWindows)
	out := cli.DockerCmd(c, "run", "--net=none", `--user=nt authority\system`, "--hostname=XYZZY", minimalBaseImage(), "cmd", "/c", `@echo %USERNAME%`).Combined()
	assert.Equal(c, strings.TrimSpace(out), "XYZZY$")
}
