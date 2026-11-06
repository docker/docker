//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/cfssl/helpers"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/creack/pty"
	archive "github.com/moby/go-archive"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/daemon/pkg/opts"
	"github.com/moby/moby/v2/integration-cli/checker"
	"github.com/moby/moby/v2/integration-cli/cli"
	"github.com/moby/moby/v2/integration-cli/cli/build"
	"github.com/moby/moby/v2/integration-cli/daemon"
	"github.com/moby/moby/v2/internal/testutil"
	testdaemon "github.com/moby/moby/v2/internal/testutil/daemon"
	"github.com/moby/moby/v2/internal/testutil/registry"
	"github.com/moby/moby/v2/internal/testutil/specialimage"
	"github.com/moby/sys/mount"
	"golang.org/x/sys/unix"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/icmd"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

const containerdSocket = "/var/run/docker/containerd/containerd.sock"

// TestLegacyDaemonCommand test starting docker daemon using "deprecated" docker daemon
// command. Remove this test when we remove this.
func (s *DockerDaemonSuite) TestLegacyDaemonCommand(c *testing.T) {
	cmd := exec.Command(dockerBinary, "daemon", "--storage-driver=vfs", "--debug")
	err := cmd.Start()
	go cmd.Wait()
	assert.NilError(c, err, "could not start daemon using 'docker daemon'")
	assert.NilError(c, cmd.Process.Kill())
}

func (s *DockerDaemonSuite) TestDaemonRestartWithRunningContainersPorts(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c)

	cli.Docker(
		cli.Args("run", "-d", "--name", "top1", "-p", "1234:80", "--restart", "always", "busybox:latest", "top"),
		cli.Daemon(s.d),
	).Assert(c, icmd.Success)

	cli.Docker(
		cli.Args("run", "-d", "--name", "top2", "-p", "80", "busybox:latest", "top"),
		cli.Daemon(s.d),
	).Assert(c, icmd.Success)

	testRun := func(m map[string]bool, prefix string) {
		var format string
		for cont, shouldRun := range m {
			out := cli.Docker(cli.Args("ps"), cli.Daemon(s.d)).Assert(c, icmd.Success).Combined()
			if shouldRun {
				format = "%scontainer %q is not running"
			} else {
				format = "%scontainer %q is running"
			}
			if shouldRun != strings.Contains(out, cont) {
				c.Fatalf(format, prefix, cont)
			}
		}
	}

	testRun(map[string]bool{"top1": true, "top2": true}, "")

	s.d.Restart(c)
	testRun(map[string]bool{"top1": true, "top2": false}, "After daemon restart: ")
}

func (s *DockerDaemonSuite) TestDaemonRestartWithVolumesRefs(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c)

	if out, err := s.d.Cmd("run", "--name", "volrestarttest1", "-v", "/foo", "busybox"); err != nil {
		c.Fatal(err, out)
	}

	s.d.Restart(c)

	if out, err := s.d.Cmd("run", "-d", "--volumes-from", "volrestarttest1", "--name", "volrestarttest2", "busybox", "top"); err != nil {
		c.Fatal(err, out)
	}

	if out, err := s.d.Cmd("rm", "-fv", "volrestarttest2"); err != nil {
		c.Fatal(err, out)
	}

	out, err := s.d.Cmd("inspect", "-f", `{{range .Mounts}}{{.Destination}}{{"\n"}}{{end}}`, "volrestarttest1")
	assert.Check(c, err)
	assert.Check(c, is.Contains(strings.Split(out, "\n"), "/foo"))
}

// #11008
func (s *DockerDaemonSuite) TestDaemonRestartUnlessStopped(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c)

	out, err := s.d.Cmd("run", "-d", "--name", "top1", "--restart", "always", "busybox:latest", "top")
	assert.NilError(c, err, "run top1: %v", out)

	out, err = s.d.Cmd("run", "-d", "--name", "top2", "--restart", "unless-stopped", "busybox:latest", "top")
	assert.NilError(c, err, "run top2: %v", out)

	out, err = s.d.Cmd("run", "-d", "--name", "exit", "--restart", "unless-stopped", "busybox:latest", "false")
	assert.NilError(c, err, "run exit: %v", out)

	testRun := func(m map[string]bool, prefix string) {
		var format string
		for name, shouldRun := range m {
			out, err := s.d.Cmd("ps")
			assert.Assert(c, err == nil, "run ps: %v", out)
			if shouldRun {
				format = "%scontainer %q is not running"
			} else {
				format = "%scontainer %q is running"
			}
			assert.Equal(c, strings.Contains(out, name), shouldRun, fmt.Sprintf(format, prefix, name))
		}
	}

	// both running
	testRun(map[string]bool{"top1": true, "top2": true, "exit": true}, "")

	out, err = s.d.Cmd("stop", "exit")
	assert.NilError(c, err, out)

	out, err = s.d.Cmd("stop", "top1")
	assert.NilError(c, err, out)

	out, err = s.d.Cmd("stop", "top2")
	assert.NilError(c, err, out)

	// both stopped
	testRun(map[string]bool{"top1": false, "top2": false, "exit": false}, "")

	s.d.Restart(c)

	// restart=always running
	testRun(map[string]bool{"top1": true, "top2": false, "exit": false}, "After daemon restart: ")

	out, err = s.d.Cmd("start", "top2")
	assert.NilError(c, err, "start top2: %v", out)

	out, err = s.d.Cmd("start", "exit")
	assert.NilError(c, err, "start exit: %v", out)

	s.d.Restart(c)

	// both running
	testRun(map[string]bool{"top1": true, "top2": true, "exit": true}, "After second daemon restart: ")
}

func (s *DockerDaemonSuite) TestDaemonRestartOnFailure(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c)

	out, err := s.d.Cmd("run", "-d", "--name", "test1", "--restart", "on-failure:3", "busybox:latest", "false")
	assert.NilError(c, err, "run top1: %v", out)

	// wait test1 to stop
	hostArgs := []string{"--host", s.d.Sock()}
	err = daemon.WaitInspectWithArgs(dockerBinary, "test1", "{{.State.Running}} {{.State.Restarting}}", "false false", 10*time.Second, hostArgs...) //nolint:staticcheck // TODO WaitInspectWithArgs is deprecated.
	assert.NilError(c, err, "test1 should exit but not")

	// record last start time
	out, err = s.d.Cmd("inspect", "-f={{.State.StartedAt}}", "test1")
	assert.NilError(c, err, "out: %v", out)
	lastStartTime := out

	s.d.Restart(c)

	// test1 shouldn't restart at all
	err = daemon.WaitInspectWithArgs(dockerBinary, "test1", "{{.State.Running}} {{.State.Restarting}}", "false false", 0, hostArgs...) //nolint:staticcheck // TODO WaitInspectWithArgs is deprecated.
	assert.NilError(c, err, "test1 should exit but not")

	// make sure test1 isn't restarted when daemon restart
	// if "StartAt" time updates, means test1 was once restarted.
	out, err = s.d.Cmd("inspect", "-f={{.State.StartedAt}}", "test1")
	assert.NilError(c, err, "out: %v", out)
	assert.Equal(c, out, lastStartTime, "test1 shouldn't start after daemon restarts")
}

func (s *DockerDaemonSuite) TestDaemonStartIptablesFalse(c *testing.T) {
	s.d.Start(c, "--iptables=false")
}

// Issue #8444: If docker0 bridge is modified (intentionally or unintentionally) and
// no longer has an IP associated, we should gracefully handle that case and associate
// an IP with it rather than fail daemon start
func (s *DockerDaemonSuite) TestDaemonStartBridgeWithoutIPAssociation(c *testing.T) {
	// rather than depending on brctl commands to verify docker0 is created and up
	// let's start the daemon and stop it, and then make a modification to run the
	// actual test
	s.d.Start(c)
	s.d.Stop(c)

	// now we will remove the ip from docker0 and then try starting the daemon
	icmd.RunCommand("ip", "addr", "flush", "dev", "docker0").Assert(c, icmd.Success)

	if err := s.d.StartWithError(); err != nil {
		warning := "**WARNING: Docker bridge network in bad state--delete docker0 bridge interface to fix"
		c.Fatalf("Could not start daemon when docker0 has no IP address: %v\n%s", err, warning)
	}
}

// TestDaemonIPv6FixedCIDR checks that when the daemon is started with --ipv6=true and a fixed CIDR
// that running containers are given a link-local and global IPv6 address
func (s *DockerDaemonSuite) TestDaemonIPv6FixedCIDR(c *testing.T) {
	// IPv6 setup is messing with local bridge address.
	testRequires(c, testEnv.IsLocalDaemon)
	// Delete the docker0 bridge if its left around from previous daemon. It has to be recreated with
	// ipv6 enabled
	deleteInterface(c, "docker0")

	s.d.StartWithBusybox(testutil.GetContext(c), c, "--ipv6", "--fixed-cidr-v6=2001:db8:2::/64", "--default-gateway-v6=2001:db8:2::100")

	out, err := s.d.Cmd("run", "-d", "--name=ipv6test", "busybox:latest", "top")
	assert.NilError(c, err, "Could not run container: %s, %v", out, err)

	out, err = s.d.Cmd("inspect", "--format", "{{.NetworkSettings.Networks.bridge.GlobalIPv6Address}}", "ipv6test")
	assert.NilError(c, err, out)
	out = strings.Trim(out, " \r\n'")

	ip := net.ParseIP(out)
	assert.Assert(c, ip != nil, "Container should have a global IPv6 address")

	out, err = s.d.Cmd("inspect", "--format", "{{.NetworkSettings.Networks.bridge.IPv6Gateway}}", "ipv6test")
	assert.NilError(c, err, out)

	assert.Equal(c, strings.Trim(out, " \r\n'"), "2001:db8:2::100", "Container should have a global IPv6 gateway")
}

// TestDaemonIPv6HostMode checks that when the running a container with
// network=host the host ipv6 addresses are not removed
func (s *DockerDaemonSuite) TestDaemonIPv6HostMode(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)
	deleteInterface(c, "docker0")

	s.d.StartWithBusybox(testutil.GetContext(c), c, "--ipv6", "--fixed-cidr-v6=2001:db8:2::/64")
	out, err := s.d.Cmd("run", "-d", "--name=hostcnt", "--network=host", "busybox:latest", "top")
	assert.NilError(c, err, "Could not run container: %s, %v", out, err)

	out, err = s.d.Cmd("exec", "hostcnt", "ip", "-6", "addr", "show", "docker0")
	assert.NilError(c, err, out)
	assert.Assert(c, is.Contains(strings.Trim(out, " \r\n'"), "2001:db8:2::1"))
}

func (s *DockerDaemonSuite) TestDaemonLogLevelWrong(c *testing.T) {
	assert.Assert(c, s.d.StartWithError("--log-level=bogus") != nil, "Daemon shouldn't start with wrong log level")
}

func (s *DockerDaemonSuite) TestDaemonLogLevelDebug(c *testing.T) {
	s.d.Start(c, "--log-level=debug")
	content, err := s.d.ReadLogFile()
	assert.NilError(c, err)
	if !strings.Contains(string(content), `level=debug`) {
		c.Fatalf(`Missing level="debug" in log file:\n%s`, string(content))
	}
}

func (s *DockerDaemonSuite) TestDaemonLogLevelFatal(c *testing.T) {
	// we creating new daemons to create new logFile
	s.d.Start(c, "--log-level=fatal")
	content, err := s.d.ReadLogFile()
	assert.NilError(c, err)
	if strings.Contains(string(content), `level=debug`) {
		c.Fatalf(`Should not have level="debug" in log file:\n%s`, string(content))
	}
}

func (s *DockerDaemonSuite) TestDaemonFlagD(c *testing.T) {
	s.d.Start(c, "-D")
	content, err := s.d.ReadLogFile()
	assert.NilError(c, err)
	if !strings.Contains(string(content), `level=debug`) {
		c.Fatalf(`Should have level="debug" in log file using -D:\n%s`, string(content))
	}
}

func (s *DockerDaemonSuite) TestDaemonFlagDebug(c *testing.T) {
	s.d.Start(c, "--debug")
	content, err := s.d.ReadLogFile()
	assert.NilError(c, err)
	if !strings.Contains(string(content), `level=debug`) {
		c.Fatalf(`Should have level="debug" in log file using --debug:\n%s`, string(content))
	}
}

func (s *DockerDaemonSuite) TestDaemonFlagDebugLogLevelFatal(c *testing.T) {
	s.d.Start(c, "--debug", "--log-level=fatal")
	content, err := s.d.ReadLogFile()
	assert.NilError(c, err)
	if !strings.Contains(string(content), `level=debug`) {
		c.Fatalf(`Should have level="debug" in log file when using both --debug and --log-level=fatal:\n%s`, string(content))
	}
}

func (s *DockerDaemonSuite) TestDaemonAllocatesListeningPort(c *testing.T) {
	type listener struct {
		daemon string
		client string
		port   string
	}
	listeningPorts := []listener{
		{"0.0.0.0", "0.0.0.0", "5678"},
		{"127.0.0.1", "127.0.0.1", "1234"},
		{"localhost", "127.0.0.1", "1235"},
	}

	cmdArgs := make([]string, 0, len(listeningPorts)*2)
	for _, l := range listeningPorts {
		cmdArgs = append(cmdArgs, "--tls=false", "--host", "tcp://"+net.JoinHostPort(l.daemon, l.port))
	}

	s.d.StartWithBusybox(testutil.GetContext(c), c, cmdArgs...)

	for _, l := range listeningPorts {
		output, err := s.d.Cmd("run", "-p", fmt.Sprintf("%s:%s:80", l.client, l.port), "busybox", "true")
		if err == nil {
			c.Fatalf("Container should not start, expected port already allocated error: %q", output)
		} else if !strings.Contains(output, "port is already allocated") {
			c.Fatalf("Expected port is already allocated error: %q", output)
		}
	}
}

// GH#11320 - verify that the daemon exits on failure properly
// Note that this explicitly tests the conflict of {-b,--bridge} and {--bip} options as the means
// to get a daemon init failure; no other tests for -b/--bip conflict are therefore required
func (s *DockerDaemonSuite) TestDaemonExitOnFailure(c *testing.T) {
	// attempt to start daemon with incorrect flags (we know -b and --bip conflict)
	if err := s.d.StartWithError("--bridge", "nosuchbridge", "--bip", "1.1.1.1"); err != nil {
		// verify we got the right error
		if !strings.Contains(err.Error(), "daemon exited") {
			c.Fatalf("Expected daemon not to start, got %v", err)
		}
		// look in the log and make sure we got the message that daemon is shutting down
		icmd.RunCommand("grep", "failed to start daemon", s.d.LogFileName()).Assert(c, icmd.Success)
	} else {
		// if we didn't get an error and the daemon is running, this is a failure
		c.Fatal("Conflicting options should cause the daemon to error out with a failure")
	}
}

func (s *DockerDaemonSuite) TestDaemonBridgeNone(c *testing.T) {
	ctx := testutil.GetContext(c)
	// start with bridge none
	d := s.d
	d.StartWithBusybox(ctx, c, "--bridge", "none")
	defer d.Restart(c)

	// verify docker0 iface is not there
	icmd.RunCommand("ifconfig", "docker0").Assert(c, icmd.Expected{
		ExitCode: 1,
		Error:    "exit status 1",
		Err:      "Device not found",
	})

	// verify default "bridge" network is not there
	apiClient := d.NewClientT(c)
	_, err := apiClient.NetworkInspect(ctx, "bridge", client.NetworkInspectOptions{})
	assert.ErrorType(c, err, cerrdefs.IsNotFound, `"bridge" network should not be present if daemon started with --bridge=none`)
}

func createInterface(t *testing.T, ifType string, ifName string, ipNet string) {
	icmd.RunCommand("ip", "link", "add", "name", ifName, "type", ifType).Assert(t, icmd.Success)
	icmd.RunCommand("ifconfig", ifName, ipNet, "up").Assert(t, icmd.Success)
}

func deleteInterface(t *testing.T, ifName string) {
	icmd.RunCommand("ip", "link", "delete", ifName).Assert(t, icmd.Success)
	icmd.RunCommand("iptables", "-t", "nat", "--flush").Assert(t, icmd.Success)
	icmd.RunCommand("iptables", "--flush").Assert(t, icmd.Success)
}

func (s *DockerDaemonSuite) TestDaemonUlimitDefaults(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c, "--default-ulimit", "nofile=50:50", "--default-ulimit", "nproc=1024:1024")

	out, err := s.d.Cmd("run", "--ulimit", "nproc=2048", "--name=test", "busybox", "/bin/sh", "-c", "echo $(ulimit -n); echo $(ulimit -u)")
	if err != nil {
		c.Fatal(err, out)
	}

	outArr := strings.Split(out, "\n")
	if len(outArr) < 2 {
		c.Fatalf("got unexpected output: %s", out)
	}
	nofile := strings.TrimSpace(outArr[0])
	nproc := strings.TrimSpace(outArr[1])

	if nofile != "50" {
		c.Fatalf("expected `ulimit -n` to be `50`, got: %s", nofile)
	}
	if nproc != "2048" {
		c.Fatalf("expected `ulimit -u` to be 2048, got: %s", nproc)
	}

	// Now restart daemon with a new default
	s.d.Restart(c, "--default-ulimit", "nofile=50")

	out, err = s.d.Cmd("start", "-a", "test")
	if err != nil {
		c.Fatal(err, out)
	}

	outArr = strings.Split(out, "\n")
	if len(outArr) < 2 {
		c.Fatalf("got unexpected output: %s", out)
	}
	nofile = strings.TrimSpace(outArr[0])
	nproc = strings.TrimSpace(outArr[1])

	if nofile != "50" {
		c.Fatalf("expected `ulimit -n` to be `50`, got: %s", nofile)
	}
	if nproc != "2048" {
		c.Fatalf("expected `ulimit -u` to be 2048, got: %s", nproc)
	}
}

// #11315
func (s *DockerDaemonSuite) TestDaemonRestartRenameContainer(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c)

	if out, err := s.d.Cmd("run", "--name=test", "busybox"); err != nil {
		c.Fatal(err, out)
	}

	if out, err := s.d.Cmd("rename", "test", "test2"); err != nil {
		c.Fatal(err, out)
	}

	s.d.Restart(c)

	if out, err := s.d.Cmd("start", "test2"); err != nil {
		c.Fatal(err, out)
	}
}

func (s *DockerDaemonSuite) TestDaemonLoggingDriverDefault(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c)

	out, err := s.d.Cmd("run", "--name=test", "busybox", "echo", "testline")
	assert.NilError(c, err, out)
	id, err := s.d.GetIDByName("test")
	assert.NilError(c, err)

	logPath := filepath.Join(s.d.Root, "containers", id, id+"-json.log")

	if _, err := os.Stat(logPath); err != nil {
		c.Fatal(err)
	}
	f, err := os.Open(logPath)
	if err != nil {
		c.Fatal(err)
	}
	defer f.Close()

	var res struct {
		Log    string    `json:"log"`
		Stream string    `json:"stream"`
		Time   time.Time `json:"time"`
	}
	if err := json.NewDecoder(f).Decode(&res); err != nil {
		c.Fatal(err)
	}
	if res.Log != "testline\n" {
		c.Fatalf("Unexpected log line: %q, expected: %q", res.Log, "testline\n")
	}
	if res.Stream != "stdout" {
		c.Fatalf("Unexpected stream: %q, expected: %q", res.Stream, "stdout")
	}
	if !time.Now().After(res.Time) {
		c.Fatalf("Log time %v in future", res.Time)
	}
}

func (s *DockerDaemonSuite) TestDaemonLoggingDriverDefaultOverride(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c)

	out, err := s.d.Cmd("run", "--name=test", "--log-driver=none", "busybox", "echo", "testline")
	if err != nil {
		c.Fatal(out, err)
	}
	id, err := s.d.GetIDByName("test")
	assert.NilError(c, err)

	logPath := filepath.Join(s.d.Root, "containers", id, id+"-json.log")

	if _, err := os.Stat(logPath); err == nil || !os.IsNotExist(err) {
		c.Fatalf("%s shouldn't exits, error on Stat: %s", logPath, err)
	}
}

func (s *DockerDaemonSuite) TestDaemonLoggingDriverNone(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c, "--log-driver=none")

	out, err := s.d.Cmd("run", "--name=test", "busybox", "echo", "testline")
	if err != nil {
		c.Fatal(out, err)
	}
	id, err := s.d.GetIDByName("test")
	assert.NilError(c, err)

	logPath := filepath.Join(s.d.Root, "containers", id, id+"-json.log")

	if _, err := os.Stat(logPath); err == nil || !os.IsNotExist(err) {
		c.Fatalf("%s shouldn't exits, error on Stat: %s", logPath, err)
	}
}

func (s *DockerDaemonSuite) TestDaemonLoggingDriverNoneOverride(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c, "--log-driver=none")

	out, err := s.d.Cmd("run", "--name=test", "--log-driver=json-file", "busybox", "echo", "testline")
	if err != nil {
		c.Fatal(out, err)
	}
	id, err := s.d.GetIDByName("test")
	assert.NilError(c, err)

	logPath := filepath.Join(s.d.Root, "containers", id, id+"-json.log")

	if _, err := os.Stat(logPath); err != nil {
		c.Fatal(err)
	}
	f, err := os.Open(logPath)
	if err != nil {
		c.Fatal(err)
	}
	defer f.Close()

	var res struct {
		Log    string    `json:"log"`
		Stream string    `json:"stream"`
		Time   time.Time `json:"time"`
	}
	if err := json.NewDecoder(f).Decode(&res); err != nil {
		c.Fatal(err)
	}
	if res.Log != "testline\n" {
		c.Fatalf("Unexpected log line: %q, expected: %q", res.Log, "testline\n")
	}
	if res.Stream != "stdout" {
		c.Fatalf("Unexpected stream: %q, expected: %q", res.Stream, "stdout")
	}
	if !time.Now().After(res.Time) {
		c.Fatalf("Log time %v in future", res.Time)
	}
}

func (s *DockerDaemonSuite) TestDaemonLoggingDriverNoneLogsError(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c, "--log-driver=none")

	out, err := s.d.Cmd("run", "--name=test", "busybox", "echo", "testline")
	assert.NilError(c, err, out)

	out, err = s.d.Cmd("logs", "test")
	assert.Assert(c, err != nil, "Logs should fail with 'none' driver")
	expected := `configured logging driver does not support reading`
	assert.Assert(c, is.Contains(out, expected))
}

func (s *DockerDaemonSuite) TestDaemonLoggingDriverShouldBeIgnoredForBuild(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c, "--log-driver=splunk")

	result := cli.BuildCmd(c, "busyboxs", cli.Daemon(s.d),
		build.WithDockerfile(`
        FROM busybox
        RUN echo foo`),
		build.WithoutCache,
	)
	comment := fmt.Sprintf("Failed to build image. output %s, exitCode %d, err %v", result.Combined(), result.ExitCode, result.Error)
	assert.Assert(c, result.Error == nil, comment)
	assert.Equal(c, result.ExitCode, 0, comment)
	assert.Assert(c, strings.Contains(result.Combined(), "foo"), comment)
}

func (s *DockerDaemonSuite) TestDaemonUnixSockCleanedUp(c *testing.T) {
	dir, err := os.MkdirTemp("", "socket-cleanup-test")
	if err != nil {
		c.Fatal(err)
	}
	defer os.RemoveAll(dir)

	sockPath := filepath.Join(dir, "docker.sock")
	s.d.Start(c, "--host", "unix://"+sockPath)

	if _, err := os.Stat(sockPath); err != nil {
		c.Fatal("socket does not exist")
	}

	s.d.Stop(c)

	if _, err := os.Stat(sockPath); err == nil || !os.IsNotExist(err) {
		c.Fatal("unix socket is not cleaned up")
	}
}

func (s *DockerDaemonSuite) TestDaemonRestartKillWait(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c)

	out, err := s.d.Cmd("run", "-id", "busybox", "/bin/cat")
	if err != nil {
		c.Fatalf("Could not run /bin/cat: err=%v\n%s", err, out)
	}
	containerID := strings.TrimSpace(out)

	if out, err := s.d.Cmd("kill", containerID); err != nil {
		c.Fatalf("Could not kill %s: err=%v\n%s", containerID, err, out)
	}

	s.d.Restart(c)

	errchan := make(chan error, 1)
	go func() {
		if out, err := s.d.Cmd("wait", containerID); err != nil {
			errchan <- fmt.Errorf("%v:\n%s", err, out)
		}
		close(errchan)
	}()

	select {
	case <-time.After(5 * time.Second):
		c.Fatal("Waiting on a stopped (killed) container timed out")
	case err := <-errchan:
		if err != nil {
			c.Fatal(err)
		}
	}
}

// TestHTTPSInfo connects via two-way authenticated HTTPS to the info endpoint
func (s *DockerDaemonSuite) TestHTTPSInfo(c *testing.T) {
	const (
		testDaemonHTTPSAddr = "tcp://localhost:4271"
	)

	s.d.Start(c,
		"--tlsverify",
		"--tlscacert", "fixtures/https/ca.pem",
		"--tlscert", "fixtures/https/server-cert.pem",
		"--tlskey", "fixtures/https/server-key.pem",
		"-H", testDaemonHTTPSAddr)

	args := []string{
		"--host", testDaemonHTTPSAddr,
		"--tlsverify",
		"--tlscacert", "fixtures/https/ca.pem",
		"--tlscert", "fixtures/https/client-cert.pem",
		"--tlskey", "fixtures/https/client-key.pem",
		"info",
	}
	out, err := s.d.Cmd(args...)
	if err != nil {
		c.Fatalf("Error Occurred: %s and output: %s", err, out)
	}
}

// TestHTTPSRun connects via two-way authenticated HTTPS to the create, attach, start, and wait endpoints.
// https://github.com/moby/moby/issues/19280
func (s *DockerDaemonSuite) TestHTTPSRun(c *testing.T) {
	const (
		testDaemonHTTPSAddr = "tcp://localhost:4271"
	)

	s.d.StartWithBusybox(testutil.GetContext(c), c, "--tlsverify", "--tlscacert", "fixtures/https/ca.pem", "--tlscert", "fixtures/https/server-cert.pem",
		"--tlskey", "fixtures/https/server-key.pem", "-H", testDaemonHTTPSAddr)

	args := []string{
		"--host", testDaemonHTTPSAddr,
		"--tlsverify", "--tlscacert", "fixtures/https/ca.pem",
		"--tlscert", "fixtures/https/client-cert.pem",
		"--tlskey", "fixtures/https/client-key.pem",
		"run", "busybox", "echo", "TLS response",
	}
	out, err := s.d.Cmd(args...)
	if err != nil {
		c.Fatalf("Error Occurred: %s and output: %s", err, out)
	}

	if !strings.Contains(out, "TLS response") {
		c.Fatalf("expected output to include `TLS response`, got %v", out)
	}
}

// TestTLSVerify verifies that --tlsverify=false turns on tls
func (s *DockerDaemonSuite) TestTLSVerify(c *testing.T) {
	out, err := exec.Command(dockerdBinary, "--tlsverify=false").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "could not load X509 key pair") {
		c.Fatalf("Daemon should not have started due to missing certs: %v\n%s", err, string(out))
	}
}

// TestHTTPSInfoRogueCert connects via two-way authenticated HTTPS to the info endpoint
// by using a rogue client certificate and checks that it fails with the expected error.
func (s *DockerDaemonSuite) TestHTTPSInfoRogueCert(c *testing.T) {
	const (
		// Go 1.25 /  TLS 1.3 may produce a generic "handshake failure"
		// whereas TLS 1.2 may produce a "bad certificate" TLS alert.
		// See https://github.com/golang/go/issues/56371
		//
		// > https://tip.golang.org/doc/go1.12#tls_1_3
		// >
		// > In TLS 1.3 the client is the last one to speak in the handshake, so if
		// > it causes an error to occur on the server, it will be returned on the
		// > client by the first Read, not by Handshake. For example, that will be
		// > the case if the server rejects the client certificate.
		//
		// https://github.com/golang/go/blob/go1.25.1/src/crypto/tls/alert.go#L71-L72
		alertBadCertificate      = "bad certificate"   // go1.24 / TLS 1.2
		alertHandshakeFailure    = "handshake failure" // go1.25 / TLS 1.3
		alertCertificateRequired = "certificate required"
		testDaemonHTTPSAddr      = "tcp://localhost:4271"
	)

	s.d.Start(c,
		"--tlsverify",
		"--tlscacert", "fixtures/https/ca.pem",
		"--tlscert", "fixtures/https/server-cert.pem",
		"--tlskey", "fixtures/https/server-key.pem",
		"-H", testDaemonHTTPSAddr)

	args := []string{
		"--host", testDaemonHTTPSAddr,
		"--tlsverify",
		"--tlscacert", "fixtures/https/ca.pem",
		"--tlscert", "fixtures/https/client-rogue-cert.pem",
		"--tlskey", "fixtures/https/client-rogue-key.pem",
		"info",
	}
	out, err := s.d.Cmd(args...)
	if err == nil {
		c.Errorf("Expected an error, but got none; output: %s", out)
	}
	if !strings.Contains(out, alertHandshakeFailure) && !strings.Contains(out, alertBadCertificate) && !strings.Contains(out, alertCertificateRequired) {
		c.Errorf("Expected %q, %q, or %q; output: %s", alertHandshakeFailure, alertBadCertificate, alertCertificateRequired, out)
	}
}

// TestHTTPSInfoRogueServerCert connects via two-way authenticated HTTPS to the info endpoint
// which provides a rogue server certificate and checks that it fails with the expected error
func (s *DockerDaemonSuite) TestHTTPSInfoRogueServerCert(c *testing.T) {
	const (
		errCaUnknown             = "x509: certificate signed by unknown authority"
		testDaemonRogueHTTPSAddr = "tcp://localhost:4272"
	)
	s.d.Start(c,
		"--tlsverify",
		"--tlscacert", "fixtures/https/ca.pem",
		"--tlscert", "fixtures/https/server-rogue-cert.pem",
		"--tlskey", "fixtures/https/server-rogue-key.pem",
		"-H", testDaemonRogueHTTPSAddr)

	args := []string{
		"--host", testDaemonRogueHTTPSAddr,
		"--tlsverify",
		"--tlscacert", "fixtures/https/ca.pem",
		"--tlscert", "fixtures/https/client-rogue-cert.pem",
		"--tlskey", "fixtures/https/client-rogue-key.pem",
		"info",
	}
	out, err := s.d.Cmd(args...)
	if err == nil || !strings.Contains(out, errCaUnknown) {
		c.Fatalf("Expected err: %s, got instead: %s and output: %s", errCaUnknown, err, out)
	}
}

func (s *DockerDaemonSuite) TestDaemonRestartWithSocketAsVolume(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c)

	socket := filepath.Join(s.d.Folder, "docker.sock")

	out, err := s.d.Cmd("run", "--restart=always", "-v", socket+":/sock", "busybox")
	assert.NilError(c, err, "Output: %s", out)
	s.d.Restart(c)
}

// c8dContainerID finds the current runtime ID by its Docker ID prefix.
func c8dContainerID(t *testing.T, d *daemon.Daemon, cid string) string {
	t.Helper()

	result := icmd.RunCommand(ctrBinary, "--address", containerdSocket,
		"--namespace", d.ContainersNamespace(), "containers", "list", "-q")
	result.Assert(t, icmd.Success)

	var matches []string
	for _, id := range strings.Fields(result.Stdout()) {
		if strings.HasPrefix(id, cid[:32]) {
			matches = append(matches, id)
		}
	}
	assert.Equal(t, len(matches), 1, "expected exactly one containerd container for %s, got %v", cid, matches)
	return matches[0]
}

// os.Kill should kill daemon ungracefully, leaving behind container mounts.
// A subsequent daemon restart should clean up said mounts.
func (s *DockerDaemonSuite) TestCleanupMountsAfterDaemonAndContainerKill(c *testing.T) {
	d := daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
	d.StartWithBusybox(testutil.GetContext(c), c)

	out, err := d.Cmd("run", "-d", "busybox", "top")
	assert.NilError(c, err, "Output: %s", out)

	id := strings.TrimSpace(out)

	// If there are no mounts with container id visible from the host
	// (as those are in container's own mount ns), there is nothing
	// to check here and the test should be skipped.
	mountOut, err := os.ReadFile("/proc/self/mountinfo")
	assert.NilError(c, err, "Output: %s", mountOut)
	if !strings.Contains(string(mountOut), id) {
		d.Stop(c)
		c.Skip("no container mounts visible in host ns")
	}

	c8dID := c8dContainerID(c, d, id)

	// kill the daemon
	assert.NilError(c, d.Kill())

	// kill the container
	icmd.RunCommand(ctrBinary, "--address", containerdSocket,
		"--namespace", d.ContainersNamespace(), "tasks", "kill", c8dID).Assert(c, icmd.Success)

	// restart daemon.
	d.Restart(c)

	// Now, container mounts should be gone.
	mountOut, err = os.ReadFile("/proc/self/mountinfo")
	assert.NilError(c, err, "Output: %s", mountOut)
	assert.Assert(c, !strings.Contains(string(mountOut), id), "%s is still mounted from older daemon start:\nDaemon root repository %s\n%s", id, d.Root, mountOut)

	d.Stop(c)
}

// os.Interrupt should perform a graceful daemon shutdown and hence cleanup mounts.
func (s *DockerDaemonSuite) TestCleanupMountsAfterGracefulShutdown(c *testing.T) {
	d := daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
	d.StartWithBusybox(testutil.GetContext(c), c)

	out, err := d.Cmd("run", "-d", "busybox", "top")
	assert.NilError(c, err, "Output: %s", out)
	id := strings.TrimSpace(out)

	// Send SIGINT and daemon should clean up
	assert.NilError(c, d.Signal(os.Interrupt))
	// Wait for the daemon to stop.
	assert.NilError(c, <-d.Wait)

	mountOut, err := os.ReadFile("/proc/self/mountinfo")
	assert.NilError(c, err, "Output: %s", mountOut)

	assert.Assert(c, !strings.Contains(string(mountOut), id), "%s is still mounted from older daemon start:\nDaemon root repository %s\n%s", id, d.Root, mountOut)
}

func (s *DockerDaemonSuite) TestDaemonRestartWithContainerRunning(t *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(t), t)
	if out, err := s.d.Cmd("run", "-d", "--name", "test", "busybox", "top"); err != nil {
		t.Fatal(out, err)
	}

	s.d.Restart(t)
	// Container 'test' should be removed without error
	if out, err := s.d.Cmd("rm", "test"); err != nil {
		t.Fatal(out, err)
	}
}

// tests regression detailed in #13964 where DOCKER_TLS_VERIFY env is ignored
func (s *DockerDaemonSuite) TestDaemonTLSVerifyIssue13964(c *testing.T) {
	host := "tcp://localhost:4271"
	s.d.Start(c, "-H", host)
	icmd.RunCmd(icmd.Cmd{
		Command: []string{dockerBinary, "-H", host, "info"},
		Env:     []string{"DOCKER_TLS_VERIFY=1", "DOCKER_CERT_PATH=fixtures/https"},
	}).Assert(c, icmd.Expected{
		ExitCode: 1,
		Err:      "error during connect",
	})
}

func (s *DockerDaemonSuite) TestDaemonRestartWithContainerWithRestartPolicyAlways(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c)

	out, err := s.d.Cmd("run", "-d", "--restart", "always", "busybox", "top")
	assert.NilError(c, err, out)
	id := strings.TrimSpace(out)

	out, err = s.d.Cmd("stop", id)
	assert.NilError(c, err, out)
	out, err = s.d.Cmd("wait", id)
	assert.NilError(c, err, out)

	out, err = s.d.Cmd("ps", "-q")
	assert.NilError(c, err, out)
	assert.Equal(c, out, "")

	s.d.Restart(c)

	out, err = s.d.Cmd("ps", "-q")
	assert.NilError(c, err, out)
	assert.Equal(c, strings.TrimSpace(out), id[:12])
}

func (s *DockerDaemonSuite) TestDaemonWideLogConfig(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c, "--log-opt=max-size=1k")
	name := "logtest"
	out, err := s.d.Cmd("run", "-d", "--log-opt=max-file=5", "--name", name, "busybox", "top")
	assert.NilError(c, err, "Output: %s, err: %v", out, err)

	out, err = s.d.Cmd("inspect", "-f", "{{ .HostConfig.LogConfig.Config }}", name)
	assert.NilError(c, err, "Output: %s", out)
	assert.Assert(c, is.Contains(out, "max-size:1k"))
	assert.Assert(c, is.Contains(out, "max-file:5"))

	out, err = s.d.Cmd("inspect", "-f", "{{ .HostConfig.LogConfig.Type }}", name)
	assert.NilError(c, err, "Output: %s", out)
	assert.Equal(c, strings.TrimSpace(out), "json-file")
}

func (s *DockerDaemonSuite) TestDaemonRestartWithPausedContainer(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c)
	if out, err := s.d.Cmd("run", "-i", "-d", "--name", "test", "busybox", "top"); err != nil {
		c.Fatal(err, out)
	}
	if out, err := s.d.Cmd("pause", "test"); err != nil {
		c.Fatal(err, out)
	}
	s.d.Restart(c)

	errchan := make(chan error, 1)
	go func() {
		out, err := s.d.Cmd("start", "test")
		if err != nil {
			errchan <- fmt.Errorf("%v:\n%s", err, out)
			return
		}
		name := strings.TrimSpace(out)
		if name != "test" {
			errchan <- fmt.Errorf("Paused container start error on docker daemon restart, expected 'test' but got '%s'", name)
			return
		}
		close(errchan)
	}()

	select {
	case <-time.After(5 * time.Second):
		c.Fatal("Waiting on start a container timed out")
	case err := <-errchan:
		if err != nil {
			c.Fatal(err)
		}
	}
}

func (s *DockerDaemonSuite) TestDaemonRestartRmVolumeInUse(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c)

	out, err := s.d.Cmd("create", "-v", "test:/foo", "busybox")
	assert.NilError(c, err, out)

	s.d.Restart(c)

	out, err = s.d.Cmd("volume", "rm", "test")
	assert.Assert(c, err != nil, "should not be able to remove in use volume after daemon restart")
	assert.Assert(c, is.Contains(out, "in use"))
}

func (s *DockerDaemonSuite) TestDaemonRestartLocalVolumes(c *testing.T) {
	s.d.Start(c)

	out, err := s.d.Cmd("volume", "create", "test")
	assert.NilError(c, err, out)
	s.d.Restart(c)

	out, err = s.d.Cmd("volume", "inspect", "test")
	assert.NilError(c, err, out)
}

// FIXME(vdemeester) Use a new daemon instance instead of the Suite one
func (s *DockerDaemonSuite) TestDaemonStartWithoutHost(c *testing.T) {
	s.d.UseDefaultHost = true
	defer func() {
		s.d.UseDefaultHost = false
	}()
	s.d.Start(c)
}

// FIXME(vdemeester) Use a new daemon instance instead of the Suite one
func (s *DockerDaemonSuite) TestDaemonStartWithDefaultTLSHost(c *testing.T) {
	s.d.UseDefaultTLSHost = true
	defer func() {
		s.d.UseDefaultTLSHost = false
	}()
	s.d.Start(c,
		"--tlsverify",
		"--tlscacert", "fixtures/https/ca.pem",
		"--tlscert", "fixtures/https/server-cert.pem",
		"--tlskey", "fixtures/https/server-key.pem")

	// The client with --tlsverify should also use default host localhost:2376
	c.Setenv("DOCKER_HOST", "")

	out := cli.DockerCmd(c,
		"--tlsverify",
		"--tlscacert", "fixtures/https/ca.pem",
		"--tlscert", "fixtures/https/client-cert.pem",
		"--tlskey", "fixtures/https/client-key.pem",
		"version",
	).Stdout()
	if !strings.Contains(out, "Server") {
		c.Fatalf("docker version should return information of server side")
	}

	// ensure when connecting to the server that only a single acceptable CA is requested
	contents, err := os.ReadFile("fixtures/https/ca.pem")
	assert.NilError(c, err)
	rootCert, err := helpers.ParseCertificatePEM(contents)
	assert.NilError(c, err)
	rootPool := x509.NewCertPool()
	rootPool.AddCert(rootCert)

	var certRequestInfo *tls.CertificateRequestInfo
	conn, err := tls.Dial("tcp", fmt.Sprintf("%s:%d", opts.DefaultHTTPHost, opts.DefaultTLSHTTPPort), &tls.Config{
		RootCAs:    rootPool,
		MinVersion: tls.VersionTLS12,
		GetClientCertificate: func(cri *tls.CertificateRequestInfo) (*tls.Certificate, error) {
			certRequestInfo = cri
			cert, err := tls.LoadX509KeyPair("fixtures/https/client-cert.pem", "fixtures/https/client-key.pem")
			if err != nil {
				return nil, err
			}
			return &cert, nil
		},
	})
	assert.NilError(c, err)
	conn.Close()

	assert.Assert(c, certRequestInfo != nil)
	assert.Equal(c, len(certRequestInfo.AcceptableCAs), 1)
	assert.DeepEqual(c, certRequestInfo.AcceptableCAs[0], rootCert.RawSubject)
}

func (s *DockerDaemonSuite) TestBridgeIPIsExcludedFromAllocatorPool(c *testing.T) {
	defaultNetworkBridge := "docker0"
	deleteInterface(c, defaultNetworkBridge)

	bridgeIP := "192.169.1.1"
	bridgeRange := bridgeIP + "/30"

	s.d.StartWithBusybox(testutil.GetContext(c), c, "--bip", bridgeRange)
	defer s.d.Restart(c)

	apiClient := s.d.NewClientT(c)

	var cont int
	for {
		contName := fmt.Sprintf("container%d", cont)
		_, err := s.d.Cmd("run", "--name", contName, "-d", "busybox", "/bin/sleep", "2")
		if err != nil {
			// pool exhausted
			break
		}

		res, err := apiClient.ContainerInspect(c.Context(), contName, client.ContainerInspectOptions{})
		assert.NilError(c, err)

		assert.Check(c, res.Container.NetworkSettings != nil)
		assert.Check(c, res.Container.NetworkSettings.Networks["bridge"] != nil)
		ip := res.Container.NetworkSettings.Networks["bridge"].IPAddress.String()
		assert.Assert(c, err == nil, ip)
		assert.Assert(c, ip != bridgeIP)
		cont++
	}
}

// Test daemon for no space left on device error
func (s *DockerDaemonSuite) TestDaemonNoSpaceLeftOnDeviceError(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux, RegistryHosting)

	testDir, err := os.MkdirTemp("", "no-space-left-on-device-test")
	assert.NilError(c, err)
	defer os.RemoveAll(testDir)
	assert.NilError(c, mount.MakeRShared(testDir))
	defer mount.Unmount(testDir)

	// Start a local registry to avoid pulling from Docker Hub (rate limits,
	// transient network errors). 127.0.0.0/8 is auto-insecure for both the
	// host and test daemons, so no extra flags are needed.
	reg := registry.NewV2(c)
	reg.WaitReady(c)
	defer reg.Close()

	// Create a synthetic 4 MiB image using the host daemon and push it to
	// the local registry. The test daemon's data-root is only 3 MiB, so
	// we use the host daemon for image setup. The image is removed from the
	// host immediately after pushing — it is not needed locally after that.
	ctx := testutil.GetContext(c)
	apiClient := testEnv.APIClient()

	const bigImageRef = registry.DefaultURL + "/testbig:latest"
	imageTempDir := c.TempDir()
	_, err = specialimage.MultiLayerCustom(imageTempDir, bigImageRef, []specialimage.SingleFileLayer{
		{Name: "bigfile", Content: make([]byte, 4<<20)},
	})
	assert.NilError(c, err)
	tar, err := archive.TarWithOptions(imageTempDir, &archive.TarOptions{})
	assert.NilError(c, err)
	loadResp, err := apiClient.ImageLoad(ctx, tar, client.ImageLoadWithQuiet(true))
	assert.NilError(c, err)
	_, err = io.Copy(io.Discard, loadResp)
	assert.NilError(c, err)
	_ = loadResp.Close()
	pushResp, err := apiClient.ImagePush(ctx, bigImageRef, client.ImagePushOptions{RegistryAuth: "{}"})
	assert.NilError(c, err)
	assert.NilError(c, pushResp.Wait(ctx))
	_, err = apiClient.ImageRemove(ctx, bigImageRef, client.ImageRemoveOptions{Force: true})
	assert.NilError(c, err)

	// create a 3MiB image (with a 2MiB ext4 fs) and mount it as storage root
	storageFS := filepath.Join(testDir, "testfs.img")
	icmd.RunCommand("dd", "of="+storageFS, "bs=1M", "seek=3", "count=0").Assert(c, icmd.Success)
	icmd.RunCommand("mkfs.ext4", "-F", storageFS).Assert(c, icmd.Success)

	testMount, err := os.MkdirTemp(testDir, "test-mount")
	assert.NilError(c, err)
	icmd.RunCommand("mount", "-n", "-t", "ext4", storageFS, testMount).Assert(c, icmd.Success)
	defer mount.Unmount(testMount)

	driver := "vfs"
	if testEnv.UsingSnapshotter() {
		driver = "native"
	}

	s.d.Start(c,
		"--data-root", testMount,
		"--storage-driver", driver,

		// Pass empty containerd socket to force daemon to create a new
		// supervised containerd daemon, otherwise the global containerd daemon
		// will be used and its data won't be stored in the specified data-root.
		"--containerd", "",
	)
	defer s.d.Stop(c)

	// Pull the large image into the tiny data-root to trigger ENOSPC.
	pullOut, err := s.d.Cmd("pull", bigImageRef)
	assert.Check(c, err != nil)
	assert.Check(c, is.Contains(pullOut, "no space left on device"))
}

// Test daemon restart with container links + auto restart
func (s *DockerDaemonSuite) TestDaemonRestartContainerLinksRestart(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c)

	var parent1Args []string
	var parent2Args []string
	wg := sync.WaitGroup{}
	maxChildren := 10
	chErr := make(chan error, maxChildren)

	for i := range maxChildren {
		wg.Add(1)
		name := fmt.Sprintf("test%d", i)

		if i < maxChildren/2 {
			parent1Args = append(parent1Args, []string{"--link", name}...)
		} else {
			parent2Args = append(parent2Args, []string{"--link", name}...)
		}

		go func() {
			_, err := s.d.Cmd("run", "-d", "--name", name, "--restart=always", "busybox", "top")
			chErr <- err
			wg.Done()
		}()
	}

	wg.Wait()
	close(chErr)
	for err := range chErr {
		assert.NilError(c, err)
	}

	parent1Args = append([]string{"run", "-d"}, parent1Args...)
	parent1Args = append(parent1Args, []string{"--name=parent1", "--restart=always", "busybox", "top"}...)
	parent2Args = append([]string{"run", "-d"}, parent2Args...)
	parent2Args = append(parent2Args, []string{"--name=parent2", "--restart=always", "busybox", "top"}...)

	_, err := s.d.Cmd(parent1Args...)
	assert.NilError(c, err)
	_, err = s.d.Cmd(parent2Args...)
	assert.NilError(c, err)

	s.d.Stop(c)
	// clear the log file -- we don't need any of it but may for the next part
	// can ignore the error here, this is just a cleanup
	os.Truncate(s.d.LogFileName(), 0)
	s.d.Start(c)

	for _, num := range []string{"1", "2"} {
		out, err := s.d.Cmd("inspect", "-f", "{{ .State.Running }}", "parent"+num)
		assert.NilError(c, err)
		if strings.TrimSpace(out) != "true" {
			log, _ := os.ReadFile(s.d.LogFileName())
			c.Fatalf("parent container is not running\n%s", string(log))
		}
	}
}

func (s *DockerDaemonSuite) TestDaemonCgroupParent(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	skip.If(c, onlyCgroupsv2(), "FIXME: cgroupsV2 not supported yet")

	cgroupParent := "test"
	name := "cgroup-test"

	s.d.StartWithBusybox(testutil.GetContext(c), c, "--cgroup-parent", cgroupParent)
	defer s.d.Restart(c)

	out, err := s.d.Cmd("run", "--name", name, "busybox", "cat", "/proc/self/cgroup")
	assert.NilError(c, err)
	cgroupPaths := ParseCgroupPaths(out)
	assert.Assert(c, len(cgroupPaths) != 0, "unexpected output - %q", out)
	out, err = s.d.Cmd("inspect", "-f", "{{.Id}}", name)
	assert.NilError(c, err)
	id := strings.TrimSpace(out)
	expectedCgroup := path.Join(cgroupParent, id)
	found := false
	for _, p := range cgroupPaths {
		if strings.HasSuffix(p, expectedCgroup) {
			found = true
			break
		}
	}
	assert.Assert(c, found, "Cgroup path for container (%s) doesn't found in cgroups file: %s", expectedCgroup, cgroupPaths)
}

func (s *DockerDaemonSuite) TestDaemonRestartWithLinks(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Windows does not support links
	s.d.StartWithBusybox(testutil.GetContext(c), c)

	out, err := s.d.Cmd("run", "-d", "--name=test", "busybox", "top")
	assert.NilError(c, err, out)

	out, err = s.d.Cmd("run", "--name=test2", "--link", "test:abc", "busybox", "sh", "-c", "ping -c 1 -w 1 abc")
	assert.NilError(c, err, out)

	s.d.Restart(c)

	// should fail since test is not running yet
	out, err = s.d.Cmd("start", "test2")
	assert.ErrorContains(c, err, "", out)

	out, err = s.d.Cmd("start", "test")
	assert.NilError(c, err, out)
	out, err = s.d.Cmd("start", "-a", "test2")
	assert.NilError(c, err, out)
	assert.Equal(c, strings.Contains(out, "1 packets transmitted, 1 packets received"), true, out)
}

func (s *DockerDaemonSuite) TestDaemonRestartWithNames(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Windows does not support links
	s.d.StartWithBusybox(testutil.GetContext(c), c)

	out, err := s.d.Cmd("create", "--name=test", "busybox")
	assert.NilError(c, err, out)

	out, err = s.d.Cmd("run", "-d", "--name=test2", "busybox", "top")
	assert.NilError(c, err, out)
	test2ID := strings.TrimSpace(out)

	res := s.d.RunCmd("run", "-d", "--name=test3", "--link", "test2:abc", "busybox", "top")
	assert.NilError(c, res.Error)
	// Discard any warnings about legacy links, which are emitted on stderr.
	test3ID := strings.TrimSpace(res.Stdout())

	s.d.Restart(c)

	_, err = s.d.Cmd("create", "--name=test", "busybox")
	assert.ErrorContains(c, err, "", "expected error trying to create container with duplicate name")
	// this one is no longer needed, removing simplifies the remainder of the test
	out, err = s.d.Cmd("rm", "-f", "test")
	assert.NilError(c, err, out)

	out, err = s.d.Cmd("ps", "-a", "--no-trunc")
	assert.NilError(c, err, out)

	lines := strings.Split(strings.TrimSpace(out), "\n")[1:]

	test2validated := false
	test3validated := false
	for _, line := range lines {
		fields := strings.Fields(line)
		names := fields[len(fields)-1]
		switch fields[0] {
		case test2ID:
			assert.Equal(c, names, "test2,test3/abc")
			test2validated = true
		case test3ID:
			assert.Equal(c, names, "test3")
			test3validated = true
		}
	}

	assert.Assert(c, test2validated)
	assert.Assert(c, test3validated)
}

// TestDaemonRestartWithKilledRunningContainer requires live restore of running containers
func (s *DockerDaemonSuite) TestDaemonRestartWithKilledRunningContainer(t *testing.T) {
	testRequires(t, DaemonIsLinux)
	s.d.StartWithBusybox(testutil.GetContext(t), t)

	cid, err := s.d.Cmd("run", "-d", "--name", "test", "busybox", "top")
	defer s.d.Stop(t)
	if err != nil {
		t.Fatal(cid, err)
	}
	cid = strings.TrimSpace(cid)

	pid, err := s.d.Cmd("inspect", "-f", "{{.State.Pid}}", cid)
	assert.NilError(t, err)
	pid = strings.TrimSpace(pid)

	c8dID := c8dContainerID(t, s.d, cid)

	// Kill the daemon
	if err := s.d.Kill(); err != nil {
		t.Fatal(err)
	}

	// kill the container
	icmd.RunCommand(ctrBinary, "--address", containerdSocket,
		"--namespace", s.d.ContainersNamespace(), "tasks", "kill", c8dID).Assert(t, icmd.Success)

	// Give time to containerd to process the command if we don't
	// the exit event might be received after we do the inspect
	result := icmd.RunCommand("kill", "-0", pid)
	for result.ExitCode == 0 {
		time.Sleep(1 * time.Second)
		// FIXME(vdemeester) should we check it doesn't error out ?
		result = icmd.RunCommand("kill", "-0", pid)
	}

	// restart the daemon
	s.d.Start(t)

	// Check that we've got the correct exit code
	out, err := s.d.Cmd("inspect", "-f", "{{.State.ExitCode}}", cid)
	assert.NilError(t, err)

	out = strings.TrimSpace(out)
	if out != "143" {
		t.Fatalf("Expected exit code '%s' got '%s' for container '%s'\n", "143", out, cid)
	}
}

// os.Kill should kill daemon ungracefully, leaving behind live containers.
// The live containers should be known to the restarted daemon. Stopping
// them now, should remove the mounts.
func (s *DockerDaemonSuite) TestCleanupMountsAfterDaemonCrash(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	s.d.StartWithBusybox(testutil.GetContext(c), c, "--live-restore")

	out, err := s.d.Cmd("run", "-d", "busybox", "top")
	assert.NilError(c, err, "Output: %s", out)
	id := strings.TrimSpace(out)

	// kill the daemon
	assert.NilError(c, s.d.Kill())

	// Check if there are mounts with container id visible from the host.
	// If not, those mounts exist in container's own mount ns, and so
	// the following check for mounts being cleared is pointless.
	skipMountCheck := false
	mountOut, err := os.ReadFile("/proc/self/mountinfo")
	assert.Assert(c, err == nil, "Output: %s", mountOut)
	if !strings.Contains(string(mountOut), id) {
		skipMountCheck = true
	}

	// restart daemon.
	s.d.Start(c, "--live-restore")

	// container should be running.
	out, err = s.d.Cmd("inspect", "--format={{.State.Running}}", id)
	assert.NilError(c, err, "Output: %s", out)
	out = strings.TrimSpace(out)
	if out != "true" {
		c.Fatalf("Container %s expected to stay alive after daemon restart", id)
	}

	// 'docker stop' should work.
	out, err = s.d.Cmd("stop", id)
	assert.NilError(c, err, "Output: %s", out)

	if skipMountCheck {
		return
	}
	// Now, container mounts should be gone.
	mountOut, err = os.ReadFile("/proc/self/mountinfo")
	assert.Assert(c, err == nil, "Output: %s", mountOut)
	comment := fmt.Sprintf("%s is still mounted from older daemon start:\nDaemon root repository %s\n%s", id, s.d.Root, mountOut)
	assert.Equal(c, strings.Contains(string(mountOut), id), false, comment)
}

// TestDaemonRestartWithUnpausedRunningContainer requires live restore of running containers.
func (s *DockerDaemonSuite) TestDaemonRestartWithUnpausedRunningContainer(t *testing.T) {
	testRequires(t, DaemonIsLinux)
	s.d.StartWithBusybox(testutil.GetContext(t), t, "--live-restore")

	cid, err := s.d.Cmd("run", "-d", "--name", "test", "busybox", "top")
	defer s.d.Stop(t)
	if err != nil {
		t.Fatal(cid, err)
	}
	cid = strings.TrimSpace(cid)

	pid, err := s.d.Cmd("inspect", "-f", "{{.State.Pid}}", cid)
	assert.NilError(t, err)

	c8dID := c8dContainerID(t, s.d, cid)

	// pause the container
	if _, err := s.d.Cmd("pause", cid); err != nil {
		t.Fatal(cid, err)
	}

	// Kill the daemon
	if err := s.d.Kill(); err != nil {
		t.Fatal(err)
	}

	// resume the container
	result := icmd.RunCommand(
		ctrBinary,
		"--address", containerdSocket,
		"--namespace", s.d.ContainersNamespace(),
		"tasks", "resume", c8dID)
	result.Assert(t, icmd.Success)

	// Give time to containerd to process the command if we don't
	// the resume event might be received after we do the inspect
	poll.WaitOn(t, pollCheck(t, func(*testing.T) (any, string) {
		result := icmd.RunCommand("kill", "-0", strings.TrimSpace(pid))
		return result.ExitCode, ""
	}, checker.Equals(0)), poll.WithTimeout(defaultReconciliationTimeout))

	// restart the daemon
	s.d.Start(t, "--live-restore")

	// Check that we've got the correct status
	out, err := s.d.Cmd("inspect", "-f", "{{.State.Status}}", cid)
	assert.NilError(t, err)

	out = strings.TrimSpace(out)
	if out != "running" {
		t.Fatalf("Expected exit code '%s' got '%s' for container '%s'\n", "running", out, cid)
	}
	if _, err := s.d.Cmd("kill", cid); err != nil {
		t.Fatal(err)
	}
}

// TestRunLinksChanged checks that creating a new container with the same name does not update links
// this ensures that the old, pre gh#16032 functionality continues on
func (s *DockerDaemonSuite) TestRunLinksChanged(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Windows does not support links
	s.d.StartWithBusybox(testutil.GetContext(c), c)

	out, err := s.d.Cmd("run", "-d", "--name=test", "busybox", "top")
	assert.NilError(c, err, out)

	out, err = s.d.Cmd("run", "--name=test2", "--link=test:abc", "busybox", "sh", "-c", "ping -c 1 abc")
	assert.NilError(c, err, out)
	assert.Assert(c, is.Contains(out, "1 packets transmitted, 1 packets received"))
	out, err = s.d.Cmd("rm", "-f", "test")
	assert.NilError(c, err, out)

	out, err = s.d.Cmd("run", "-d", "--name=test", "busybox", "top")
	assert.NilError(c, err, out)
	out, err = s.d.Cmd("start", "-a", "test2")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, !strings.Contains(out, "1 packets transmitted, 1 packets received"))
	s.d.Restart(c)
	out, err = s.d.Cmd("start", "-a", "test2")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, !strings.Contains(out, "1 packets transmitted, 1 packets received"))
}

func (s *DockerDaemonSuite) TestDaemonStartWithoutColors(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	infoLog := "\x1b[36mINFO\x1b"

	b := bytes.NewBuffer(nil)
	done := make(chan bool)

	p, tty, err := pty.Open()
	assert.NilError(c, err)
	defer func() {
		tty.Close()
		p.Close()
	}()

	go func() {
		io.Copy(b, p)
		done <- true
	}()

	// Enable coloring explicitly
	s.d.StartWithLogFile(tty, "--raw-logs=false")
	s.d.Stop(c)
	// Wait for io.Copy() before checking output
	<-done
	assert.Assert(c, is.Contains(b.String(), infoLog))
	b.Reset()

	// "tty" is already closed in prev s.d.Stop(),
	// we have to close the other side "p" and open another pair of
	// pty for the next test.
	p.Close()
	p, tty, err = pty.Open()
	assert.NilError(c, err)

	go func() {
		io.Copy(b, p)
		done <- true
	}()

	// Disable coloring explicitly
	s.d.StartWithLogFile(tty, "--raw-logs=true")
	s.d.Stop(c)
	// Wait for io.Copy() before checking output
	<-done
	assert.Assert(c, b.String() != "")
	assert.Assert(c, !strings.Contains(b.String(), infoLog))
}

func (s *DockerDaemonSuite) TestDaemonDebugLog(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	logFile, err := os.CreateTemp(c.TempDir(), "dockerd-debug-*.log")
	assert.NilError(c, err)
	c.Cleanup(func() { _ = logFile.Close() })

	err = s.d.StartWithLogFile(logFile, "--debug", "--log-format=json")
	assert.NilError(c, err)
	s.d.Stop(c)

	data, err := os.ReadFile(logFile.Name())
	assert.NilError(c, err)

	assert.Assert(c, is.Contains(string(data), `"level":"debug"`))
}

// Test for #21956
func (s *DockerDaemonSuite) TestDaemonLogOptions(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c, "--log-driver=syslog", "--log-opt=syslog-address=udp://127.0.0.1:514")

	out, err := s.d.Cmd("run", "-d", "--log-driver=json-file", "busybox", "top")
	assert.NilError(c, err, out)
	id := strings.TrimSpace(out)

	out, err = s.d.Cmd("inspect", "--format='{{.HostConfig.LogConfig}}'", id)
	assert.NilError(c, err, out)
	assert.Assert(c, is.Contains(out, "{json-file map[]}"))
}

// Test case for #20936, #22443
func (s *DockerDaemonSuite) TestDaemonMaxConcurrency(c *testing.T) {
	s.d.Start(c, "--max-concurrent-uploads=6", "--max-concurrent-downloads=8")

	expectedMaxConcurrentUploads := `level=debug msg="Max Concurrent Uploads: 6"`
	expectedMaxConcurrentDownloads := `level=debug msg="Max Concurrent Downloads: 8"`
	content, err := s.d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, is.Contains(string(content), expectedMaxConcurrentUploads))
	assert.Assert(c, is.Contains(string(content), expectedMaxConcurrentDownloads))
}

// Test case for #20936, #22443
func (s *DockerDaemonSuite) TestDaemonMaxConcurrencyWithConfigFile(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	// daemon config file
	const configFilePath = "test-daemon.json"
	err := os.WriteFile(configFilePath, []byte(`{ "max-concurrent-downloads" : 8 }`), 0o666)
	assert.NilError(c, err)
	defer os.Remove(configFilePath)
	s.d.Start(c, "--config-file="+configFilePath)

	expectedMaxConcurrentUploads := `level=debug msg="Max Concurrent Uploads: 5"`
	expectedMaxConcurrentDownloads := `level=debug msg="Max Concurrent Downloads: 8"`
	content, err := s.d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, is.Contains(string(content), expectedMaxConcurrentUploads))
	assert.Assert(c, is.Contains(string(content), expectedMaxConcurrentDownloads))
	err = os.WriteFile(configFilePath, []byte(`{ "max-concurrent-uploads" : 7, "max-concurrent-downloads" : 9 }`), 0o666)
	assert.NilError(c, err)
	assert.NilError(c, s.d.Signal(unix.SIGHUP))
	// unix.Kill(s.d.cmd.Process.Pid, unix.SIGHUP)

	time.Sleep(3 * time.Second)

	expectedMaxConcurrentUploads = `level=debug msg="Reset Max Concurrent Uploads: 7"`
	expectedMaxConcurrentDownloads = `level=debug msg="Reset Max Concurrent Downloads: 9"`
	content, err = s.d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, is.Contains(string(content), expectedMaxConcurrentUploads))
	assert.Assert(c, is.Contains(string(content), expectedMaxConcurrentDownloads))
}

// Test case for #20936, #22443
func (s *DockerDaemonSuite) TestDaemonMaxConcurrencyWithConfigFileReload(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	// daemon config file
	const configFilePath = "test-daemon.json"
	err := os.WriteFile(configFilePath, []byte(`{ "max-concurrent-uploads" : null }`), 0o666)
	assert.NilError(c, err)
	defer os.Remove(configFilePath)

	s.d.Start(c, "--config-file="+configFilePath)

	expectedMaxConcurrentUploads := `level=debug msg="Max Concurrent Uploads: 5"`
	expectedMaxConcurrentDownloads := `level=debug msg="Max Concurrent Downloads: 3"`
	content, err := s.d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, is.Contains(string(content), expectedMaxConcurrentUploads))
	assert.Assert(c, is.Contains(string(content), expectedMaxConcurrentDownloads))
	err = os.WriteFile(configFilePath, []byte(`{ "max-concurrent-uploads" : 1, "max-concurrent-downloads" : null }`), 0o666)
	assert.NilError(c, err)

	assert.NilError(c, s.d.Signal(unix.SIGHUP))
	// unix.Kill(s.d.cmd.Process.Pid, unix.SIGHUP)

	time.Sleep(3 * time.Second)

	expectedMaxConcurrentUploads = `level=debug msg="Reset Max Concurrent Uploads: 1"`
	expectedMaxConcurrentDownloads = `level=debug msg="Reset Max Concurrent Downloads: 3"`
	content, err = s.d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, is.Contains(string(content), expectedMaxConcurrentUploads))
	assert.Assert(c, is.Contains(string(content), expectedMaxConcurrentDownloads))
	err = os.WriteFile(configFilePath, []byte(`{ "labels":["foo=bar"] }`), 0o666)
	assert.NilError(c, err)

	assert.NilError(c, s.d.Signal(unix.SIGHUP))

	time.Sleep(3 * time.Second)

	expectedMaxConcurrentUploads = `level=debug msg="Reset Max Concurrent Uploads: 5"`
	expectedMaxConcurrentDownloads = `level=debug msg="Reset Max Concurrent Downloads: 3"`
	content, err = s.d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, is.Contains(string(content), expectedMaxConcurrentUploads))
	assert.Assert(c, is.Contains(string(content), expectedMaxConcurrentDownloads))
}

func (s *DockerDaemonSuite) TestBuildOnDisabledBridgeNetworkDaemon(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c, "-b=none", "--iptables=false")

	result := cli.BuildCmd(c, "busyboxs", cli.Daemon(s.d),
		build.WithDockerfile(`
        FROM busybox
        RUN cat /etc/hosts`),
		build.WithoutCache,
		build.WithBuildkit(false), // FIXME(thaJeztah): doesn't work with BuildKit? 'ERROR: process "/bin/sh -c cat /etc/hosts" did not complete successfully: network bridge not found'
	)
	comment := fmt.Sprintf("Failed to build image. output %s, exitCode %d, err %v", result.Combined(), result.ExitCode, result.Error)
	assert.Assert(c, result.Error == nil, comment)
	assert.Equal(c, result.ExitCode, 0, comment)
}

// Test case for #21976
func (s *DockerDaemonSuite) TestDaemonDNSFlagsInHostMode(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	s.d.StartWithBusybox(testutil.GetContext(c), c, "--dns", "1.2.3.4", "--dns-search", "example.com", "--dns-opt", "timeout:3")

	expectedOutput := "nameserver 1.2.3.4"
	out, _ := s.d.Cmd("run", "--net=host", "busybox", "cat", "/etc/resolv.conf")
	assert.Assert(c, strings.Contains(out, expectedOutput), "Expected '%s', but got %q", expectedOutput, out)
	expectedOutput = "search example.com"
	assert.Assert(c, strings.Contains(out, expectedOutput), "Expected '%s', but got %q", expectedOutput, out)
	expectedOutput = "options timeout:3"
	assert.Assert(c, strings.Contains(out, expectedOutput), "Expected '%s', but got %q", expectedOutput, out)
}

func (s *DockerDaemonSuite) TestRunWithRuntimeFromConfigFile(c *testing.T) {
	config := `
{
    "runtimes": {
        "oci": {
            "path": "runc"
        },
        "vm": {
            "path": "/usr/local/bin/vm-manager",
            "runtimeArgs": [
                "--debug"
            ]
        }
    }
}
`
	cfgFile := filepath.Join(c.TempDir(), "config.json")
	err := os.WriteFile(cfgFile, []byte(config), 0o644)
	assert.NilError(c, err)
	s.d.StartWithBusybox(testutil.GetContext(c), c, "--config-file", cfgFile)

	// Run with default runtime
	out, err := s.d.Cmd("run", "--rm", "busybox", "ls")
	assert.NilError(c, err, out)

	// Run with default runtime explicitly
	out, err = s.d.Cmd("run", "--rm", "--runtime=runc", "busybox", "ls")
	assert.NilError(c, err, out)

	// Run with oci (same path as default) but keep it around
	out, err = s.d.Cmd("run", "--name", "oci-runtime-ls", "--runtime=oci", "busybox", "ls")
	assert.NilError(c, err, out)

	// Run with "vm"
	out, err = s.d.Cmd("run", "--rm", "--runtime=vm", "busybox", "ls")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, is.Contains(out, "/usr/local/bin/vm-manager: no such file or directory"))
	// Reset config to only have the default
	config = `
{
    "runtimes": {
    }
}
`

	err = os.WriteFile(cfgFile, []byte(config), 0o644)
	assert.NilError(c, err)
	assert.NilError(c, s.d.Signal(unix.SIGHUP))
	// Give daemon time to reload config
	<-time.After(1 * time.Second)

	// Run with default runtime
	out, err = s.d.Cmd("run", "--rm", "--runtime=runc", "busybox", "ls")
	assert.NilError(c, err, out)

	// Run with "oci"
	out, err = s.d.Cmd("run", "--rm", "--runtime=oci", "busybox", "ls")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, is.Contains(out, "unknown or invalid runtime name: oci"))
	// Start previously created container with oci
	out, err = s.d.Cmd("start", "oci-runtime-ls")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, is.Contains(out, "unknown or invalid runtime name: oci"))
	// Check that we can't override the default runtime
	config = `
{
    "runtimes": {
        "runc": {
            "path": "my-runc"
        }
    }
}
`
	err = os.WriteFile(cfgFile, []byte(config), 0o644)
	assert.NilError(c, err)
	assert.NilError(c, s.d.Signal(unix.SIGHUP))
	// Give daemon time to reload config
	<-time.After(1 * time.Second)

	content, err := s.d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, is.Contains(string(content), `runtime name 'runc' is reserved`))
	// Check that we can select a default runtime
	config = `
{
    "default-runtime": "vm",
    "runtimes": {
        "oci": {
            "path": "runc"
        },
        "vm": {
            "path": "/usr/local/bin/vm-manager",
            "runtimeArgs": [
                "--debug"
            ]
        }
    }
}
`
	err = os.WriteFile(cfgFile, []byte(config), 0o644)
	assert.NilError(c, err)
	assert.NilError(c, s.d.Signal(unix.SIGHUP))
	// Give daemon time to reload config
	<-time.After(1 * time.Second)

	out, err = s.d.Cmd("run", "--rm", "busybox", "ls")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, is.Contains(out, "/usr/local/bin/vm-manager: no such file or directory"))
	// Run with default runtime explicitly
	out, err = s.d.Cmd("run", "--rm", "--runtime=runc", "busybox", "ls")
	assert.NilError(c, err, out)
}

func (s *DockerDaemonSuite) TestRunWithRuntimeFromCommandLine(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c, "--add-runtime", "oci=runc", "--add-runtime", "vm=/usr/local/bin/vm-manager")

	// Run with default runtime
	out, err := s.d.Cmd("run", "--rm", "busybox", "ls")
	assert.NilError(c, err, out)

	// Run with default runtime explicitly
	out, err = s.d.Cmd("run", "--rm", "--runtime=runc", "busybox", "ls")
	assert.NilError(c, err, out)

	// Run with oci (same path as default) but keep it around
	out, err = s.d.Cmd("run", "--name", "oci-runtime-ls", "--runtime=oci", "busybox", "ls")
	assert.NilError(c, err, out)

	// Run with "vm"
	out, err = s.d.Cmd("run", "--rm", "--runtime=vm", "busybox", "ls")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, is.Contains(out, "/usr/local/bin/vm-manager: no such file or directory"))
	// Start a daemon without any extra runtimes
	s.d.Stop(c)
	s.d.StartWithBusybox(testutil.GetContext(c), c)

	// Run with default runtime
	out, err = s.d.Cmd("run", "--rm", "--runtime=runc", "busybox", "ls")
	assert.NilError(c, err, out)

	// Run with "oci"
	out, err = s.d.Cmd("run", "--rm", "--runtime=oci", "busybox", "ls")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, is.Contains(out, "unknown or invalid runtime name: oci"))
	// Start previously created container with oci
	out, err = s.d.Cmd("start", "oci-runtime-ls")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, is.Contains(out, "unknown or invalid runtime name: oci"))
	// Check that we can't override the default runtime
	s.d.Stop(c)
	assert.Assert(c, s.d.StartWithError("--add-runtime", "runc=my-runc") != nil)

	content, err := s.d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, is.Contains(string(content), `runtime name 'runc' is reserved`))
	// Check that we can select a default runtime
	s.d.Stop(c)
	s.d.StartWithBusybox(testutil.GetContext(c), c, "--default-runtime=vm", "--add-runtime", "oci=runc", "--add-runtime", "vm=/usr/local/bin/vm-manager")

	out, err = s.d.Cmd("run", "--rm", "busybox", "ls")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, is.Contains(out, "/usr/local/bin/vm-manager: no such file or directory"))
	// Run with default runtime explicitly
	out, err = s.d.Cmd("run", "--rm", "--runtime=runc", "busybox", "ls")
	assert.NilError(c, err, out)
}

func (s *DockerDaemonSuite) TestDaemonRestartWithAutoRemoveContainer(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c)

	// top1 will exist after daemon restarts
	out, err := s.d.Cmd("run", "-d", "--name", "top1", "busybox:latest", "top")
	assert.Assert(c, err == nil, "run top1: %v", out)
	// top2 will be removed after daemon restarts
	out, err = s.d.Cmd("run", "-d", "--rm", "--name", "top2", "busybox:latest", "top")
	assert.Assert(c, err == nil, "run top2: %v", out)

	out, err = s.d.Cmd("ps")
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, "top1"), "top1 should be running")
	assert.Assert(c, strings.Contains(out, "top2"), "top2 should be running")
	// now restart daemon gracefully
	s.d.Restart(c)

	out, err = s.d.Cmd("ps", "-a")
	assert.NilError(c, err, "out: %v", out)
	assert.Assert(c, strings.Contains(out, "top1"), "top1 should exist after daemon restarts")
	assert.Assert(c, !strings.Contains(out, "top2"), "top2 should be removed after daemon restarts")
}

func (s *DockerDaemonSuite) TestDaemonRestartSaveContainerExitCode(c *testing.T) {
	s.d.StartWithBusybox(testutil.GetContext(c), c)

	containerName := "error-values"
	// Make a container with both a non 0 exit code and an error message
	// We explicitly disable `--init` for this test, because `--init` is enabled by default
	// on "experimental". Enabling `--init` results in a different behavior; because the "init"
	// process itself is PID1, the container does not fail on _startup_ (i.e., `docker-init` starting),
	// but directly after. The exit code of the container is still 127, but the Error Message is not
	// captured, so `.State.Error` is empty.
	// See the discussion on https://github.com/moby/moby/pull/30227#issuecomment-274161426,
	// and https://github.com/moby/moby/pull/26061#r78054578 for more information.
	_, err := s.d.Cmd("run", "--name", containerName, "--init=false", "busybox", "toto")
	assert.ErrorContains(c, err, "")

	// Check that those values were saved on disk
	out, err := s.d.Cmd("inspect", "-f", "{{.State.ExitCode}}", containerName)
	out = strings.TrimSpace(out)
	assert.NilError(c, err)
	assert.Equal(c, out, "127")

	errMsg1, err := s.d.Cmd("inspect", "-f", "{{.State.Error}}", containerName)
	errMsg1 = strings.TrimSpace(errMsg1)
	assert.NilError(c, err)
	assert.Assert(c, is.Contains(errMsg1, "executable file not found"))
	// now restart daemon
	s.d.Restart(c)

	// Check that those values are still around
	out, err = s.d.Cmd("inspect", "-f", "{{.State.ExitCode}}", containerName)
	out = strings.TrimSpace(out)
	assert.NilError(c, err)
	assert.Equal(c, out, "127")

	out, err = s.d.Cmd("inspect", "-f", "{{.State.Error}}", containerName)
	out = strings.TrimSpace(out)
	assert.NilError(c, err)
	assert.Equal(c, out, errMsg1)
}

func (s *DockerDaemonSuite) TestDaemonWithUserlandProxyPath(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)
	ctx := c.Context()

	dockerProxyPath, err := exec.LookPath("docker-proxy")
	assert.NilError(c, err)
	tmpDir, err := os.MkdirTemp("", "test-docker-proxy")
	assert.NilError(c, err)

	newProxyPath := filepath.Join(tmpDir, "docker-proxy")
	cmd := exec.Command("cp", dockerProxyPath, newProxyPath)
	assert.NilError(c, cmd.Run())

	// custom one
	s.d.StartWithBusybox(testutil.GetContext(c), c, "--userland-proxy-path", newProxyPath)
	out, err := s.d.Cmd("run", "-p", "5000:5000", "busybox:latest", "true")
	assert.NilError(c, err, out)

	// try with the original one
	s.d.Restart(c, "--userland-proxy-path", dockerProxyPath)
	out, err = s.d.Cmd("run", "-p", "5000:5000", "busybox:latest", "true")
	assert.NilError(c, err, out)

	// not exist
	s.d.Stop(c)
	err = s.d.StartWithError("--userland-proxy-path", "/does/not/exist")
	assert.ErrorContains(c, err, "", "daemon should fail to start")
	expected := "invalid userland-proxy-path"
	ok, _ := s.d.ScanLogsT(ctx, c, testdaemon.ScanLogsMatchString(expected))
	assert.Assert(c, ok, "logs did not contain: %s", expected)

	// not an absolute path
	s.d.Stop(c)
	err = s.d.StartWithError("--userland-proxy-path", "docker-proxy")
	assert.ErrorContains(c, err, "", "daemon should fail to start")
	expected = "invalid userland-proxy-path: must be an absolute path: docker-proxy"
	ok, _ = s.d.ScanLogsT(ctx, c, testdaemon.ScanLogsMatchString(expected))
	assert.Assert(c, ok, "logs did not contain: %s", expected)
}

// Test case for #22471
func (s *DockerDaemonSuite) TestDaemonShutdownTimeout(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)
	s.d.StartWithBusybox(testutil.GetContext(c), c, "--shutdown-timeout=3")

	_, err := s.d.Cmd("run", "-d", "busybox", "top")
	assert.NilError(c, err)

	assert.NilError(c, s.d.Signal(unix.SIGINT))

	select {
	case <-s.d.Wait:
	case <-time.After(5 * time.Second):
	}

	expectedMessage := `level=debug msg="daemon configured with a 3 seconds minimum shutdown timeout"`
	content, err := s.d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, is.Contains(string(content), expectedMessage))
}

// Test case for #22471
func (s *DockerDaemonSuite) TestDaemonShutdownTimeoutWithConfigFile(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)

	// daemon config file
	const configFilePath = "test-daemon.json"
	err := os.WriteFile(configFilePath, []byte(`{ "shutdown-timeout" : 8 }`), 0o666)
	assert.NilError(c, err)
	defer os.Remove(configFilePath)

	s.d.Start(c, "--config-file="+configFilePath)

	err = os.WriteFile(configFilePath, []byte(`{ "shutdown-timeout" : 5 }`), 0o666)
	assert.NilError(c, err)

	assert.NilError(c, s.d.Signal(unix.SIGHUP))

	select {
	case <-s.d.Wait:
	case <-time.After(3 * time.Second):
	}

	expectedMessage := `level=debug msg="Reset Shutdown Timeout: 5"`
	content, err := s.d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, is.Contains(string(content), expectedMessage))
}

// Test case for 29342
func (s *DockerDaemonSuite) TestExecWithUserAfterLiveRestore(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	s.d.StartWithBusybox(testutil.GetContext(c), c, "--live-restore")

	out, err := s.d.Cmd("run", "--init", "-d", "--name=top", "busybox", "sh", "-c", "addgroup -S testgroup && adduser -S -G testgroup test -D -s /bin/sh && touch /adduser_end && exec top")
	assert.NilError(c, err, "Output: %s", out)

	s.d.WaitRun("top")

	// Wait for shell command to be completed
	_, err = s.d.Cmd("exec", "top", "sh", "-c", `for i in $(seq 1 5); do if [ -e /adduser_end ]; then rm -f /adduser_end && break; else sleep 1 && false; fi; done`)
	assert.Assert(c, err == nil, "Timeout waiting for shell command to be completed")

	out1, err := s.d.Cmd("exec", "-u", "test", "top", "id")
	// uid=100(test) gid=101(testgroup) groups=101(testgroup)
	assert.Assert(c, err == nil, "Output: %s", out1)

	// restart daemon.
	s.d.Restart(c, "--live-restore")

	out2, err := s.d.Cmd("exec", "-u", "test", "top", "id")
	assert.Assert(c, err == nil, "Output: %s", out2)
	assert.Equal(c, out2, out1, fmt.Sprintf("Output: before restart '%s', after restart '%s'", out1, out2))

	out, err = s.d.Cmd("stop", "top")
	assert.NilError(c, err, "Output: %s", out)
}

func (s *DockerDaemonSuite) TestRemoveContainerAfterLiveRestore(c *testing.T) {
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon)
	s.d.StartWithBusybox(testutil.GetContext(c), c, "--live-restore")
	out, err := s.d.Cmd("run", "-d", "--name=top", "busybox", "top")
	assert.NilError(c, err, "Output: %s", out)

	s.d.WaitRun("top")

	// restart daemon.
	s.d.Restart(c, "--live-restore")

	out, err = s.d.Cmd("stop", "top")
	assert.NilError(c, err, "Output: %s", out)

	// test if the rootfs mountpoint still exist
	mountpoint, err := s.d.InspectField("top", ".GraphDriver.Data.MergedDir")
	assert.NilError(c, err)
	f, err := os.Open("/proc/self/mountinfo")
	assert.NilError(c, err)
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, mountpoint) {
			c.Fatalf("mountinfo should not include the mountpoint of stop container")
		}
	}

	out, err = s.d.Cmd("rm", "top")
	assert.NilError(c, err, "Output: %s", out)
}

// #29598
func (s *DockerDaemonSuite) TestRestartPolicyWithLiveRestore(c *testing.T) {
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon)
	s.d.StartWithBusybox(testutil.GetContext(c), c, "--live-restore")

	out, err := s.d.Cmd("run", "-d", "--restart", "always", "busybox", "top")
	assert.NilError(c, err, "Output: %s", out)
	id := strings.TrimSpace(out)

	type state struct {
		Running   bool
		StartedAt time.Time
	}
	out, err = s.d.Cmd("inspect", "-f", "{{json .State}}", id)
	assert.Assert(c, err == nil, "output: %s", out)

	var origState state
	err = json.Unmarshal([]byte(strings.TrimSpace(out)), &origState)
	assert.NilError(c, err)

	s.d.Restart(c, "--live-restore")

	pid, err := s.d.Cmd("inspect", "-f", "{{.State.Pid}}", id)
	assert.NilError(c, err)
	pidint, err := strconv.Atoi(strings.TrimSpace(pid))
	assert.NilError(c, err)
	assert.Assert(c, pidint > 0)
	assert.NilError(c, unix.Kill(pidint, unix.SIGKILL))

	ticker := time.NewTicker(50 * time.Millisecond)
	timeout := time.After(10 * time.Second)

	for range ticker.C {
		select {
		case <-timeout:
			c.Fatal("timeout waiting for container restart")
		default:
		}

		out, err := s.d.Cmd("inspect", "-f", "{{json .State}}", id)
		assert.Assert(c, err == nil, "output: %s", out)

		var newState state
		err = json.Unmarshal([]byte(strings.TrimSpace(out)), &newState)
		assert.NilError(c, err)

		if !newState.Running {
			continue
		}
		if newState.StartedAt.After(origState.StartedAt) {
			break
		}
	}

	out, err = s.d.Cmd("stop", id)
	assert.NilError(c, err, "Output: %s", out)
}

func (s *DockerDaemonSuite) TestShmSize(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	size := 67108864 * 2
	pattern := regexp.MustCompile(fmt.Sprintf("shm on /dev/shm type tmpfs(.*)size=%dk", size/1024))

	s.d.StartWithBusybox(testutil.GetContext(c), c, "--default-shm-size", strconv.Itoa(size))

	name := "shm1"
	out, err := s.d.Cmd("run", "--name", name, "busybox", "mount")
	assert.NilError(c, err, "Output: %s", out)
	assert.Assert(c, pattern.MatchString(out))
	out, err = s.d.Cmd("inspect", "--format", "{{.HostConfig.ShmSize}}", name)
	assert.NilError(c, err, "Output: %s", out)
	assert.Equal(c, strings.TrimSpace(out), strconv.Itoa(size))
}

func (s *DockerDaemonSuite) TestShmSizeReload(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	configPath, err := os.MkdirTemp("", "test-daemon-shm-size-reload-config")
	assert.Assert(c, err == nil, "could not create temp file for config reload")
	defer os.RemoveAll(configPath) // clean up
	configFile := filepath.Join(configPath, "config.json")

	size := 67108864 * 2
	configData := fmt.Appendf(nil, `{"default-shm-size": "%dM"}`, size/1024/1024)
	assert.Assert(c, os.WriteFile(configFile, configData, 0o666) == nil, "could not write temp file for config reload")
	pattern := regexp.MustCompile(fmt.Sprintf("shm on /dev/shm type tmpfs(.*)size=%dk", size/1024))

	s.d.StartWithBusybox(testutil.GetContext(c), c, "--config-file", configFile)

	name := "shm1"
	out, err := s.d.Cmd("run", "--name", name, "busybox", "mount")
	assert.NilError(c, err, "Output: %s", out)
	assert.Assert(c, pattern.MatchString(out))
	out, err = s.d.Cmd("inspect", "--format", "{{.HostConfig.ShmSize}}", name)
	assert.NilError(c, err, "Output: %s", out)
	assert.Equal(c, strings.TrimSpace(out), strconv.Itoa(size))

	size = 67108864 * 3
	configData = fmt.Appendf(nil, `{"default-shm-size": "%dM"}`, size/1024/1024)
	assert.Assert(c, os.WriteFile(configFile, configData, 0o666) == nil, "could not write temp file for config reload")
	pattern = regexp.MustCompile(fmt.Sprintf("shm on /dev/shm type tmpfs(.*)size=%dk", size/1024))

	err = s.d.ReloadConfig()
	assert.Assert(c, err == nil, "error reloading daemon config")

	name = "shm2"
	out, err = s.d.Cmd("run", "--name", name, "busybox", "mount")
	assert.NilError(c, err, "Output: %s", out)
	assert.Assert(c, pattern.MatchString(out))
	out, err = s.d.Cmd("inspect", "--format", "{{.HostConfig.ShmSize}}", name)
	assert.NilError(c, err, "Output: %s", out)
	assert.Equal(c, strings.TrimSpace(out), strconv.Itoa(size))
}

func testDaemonStartIpcMode(t *testing.T, from, mode string, valid bool) {
	d := daemon.New(t, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
	t.Logf("Checking IpcMode %s set from %s\n", mode, from)
	var serr error
	switch from {
	case "config":
		cfgFile := filepath.Join(t.TempDir(), "config.json")
		config := `{"default-ipc-mode": "` + mode + `"}`
		err := os.WriteFile(cfgFile, []byte(config), 0o600)
		assert.NilError(t, err)
		serr = d.StartWithError("--config-file", cfgFile)
	case "cli":
		serr = d.StartWithError("--default-ipc-mode", mode)
	default:
		t.Fatalf("testDaemonStartIpcMode: invalid 'from' argument")
	}
	if serr == nil {
		d.Stop(t)
	}

	if valid {
		assert.NilError(t, serr)
	} else {
		assert.ErrorContains(t, serr, "")
		icmd.RunCommand("grep", "-E", "IPC .* is (invalid|not supported)", d.LogFileName()).Assert(t, icmd.Success)
	}
}

// TestDaemonStartWithIpcModes checks that daemon starts fine given correct
// arguments for default IPC mode, and bails out with incorrect ones.
// Both CLI option (--default-ipc-mode) and config parameter are tested.
func (s *DockerDaemonSuite) TestDaemonStartWithIpcModes(c *testing.T) {
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon)

	ipcModes := []struct {
		mode  string
		valid bool
	}{
		{"private", true},
		{"shareable", true},

		{"host", false},
		{"container:123", false},
		{"nosuchmode", false},
	}

	for _, from := range []string{"config", "cli"} {
		for _, m := range ipcModes {
			testDaemonStartIpcMode(c, from, m.mode, m.valid)
		}
	}
}

// TestFailedPluginRemove makes sure that a failed plugin remove does not block
// the daemon from starting
func (s *DockerDaemonSuite) TestFailedPluginRemove(c *testing.T) {
	testRequires(c, DaemonIsLinux, IsAmd64, testEnv.IsLocalDaemon)
	d := daemon.New(c, dockerBinary, dockerdBinary)
	d.Start(c)
	apiClient := d.NewClientT(c)

	ctx, cancel := context.WithTimeout(testutil.GetContext(c), 300*time.Second)
	defer cancel()

	name := "test-plugin-rm-fail"
	out, err := apiClient.PluginInstall(ctx, name, client.PluginInstallOptions{
		Disabled:             true,
		AcceptAllPermissions: true,
		RemoteRef:            "cpuguy83/docker-logdriver-test",
	})
	assert.NilError(c, err)
	defer out.Close()
	io.Copy(io.Discard, out)

	ctx, cancel = context.WithTimeout(testutil.GetContext(c), 30*time.Second)
	defer cancel()
	res, err := apiClient.PluginInspect(ctx, name, client.PluginInspectOptions{})
	assert.NilError(c, err)

	// simulate a bad/partial removal by removing the plugin config.
	configPath := filepath.Join(d.Root, "plugins", res.Plugin.ID, "config.json")
	assert.NilError(c, os.Remove(configPath))

	d.Restart(c)
	ctx, cancel = context.WithTimeout(testutil.GetContext(c), 30*time.Second)
	defer cancel()
	_, err = apiClient.Ping(ctx, client.PingOptions{})
	assert.NilError(c, err)

	_, err = apiClient.PluginInspect(ctx, name, client.PluginInspectOptions{})
	// plugin should be gone since the config.json is gone
	assert.ErrorContains(c, err, "")
}
