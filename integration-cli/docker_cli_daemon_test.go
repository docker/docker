// +build linux

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
	"io/ioutil"
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
	"github.com/creack/pty"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/integration-cli/checker"
	"github.com/docker/docker/integration-cli/cli"
	"github.com/docker/docker/integration-cli/cli/build"
	"github.com/docker/docker/integration-cli/daemon"
	"github.com/docker/docker/opts"
	"github.com/docker/docker/pkg/mount"
	testdaemon "github.com/docker/docker/testutil/daemon"
	"github.com/docker/go-units"
	"github.com/docker/libnetwork/iptables"
	"github.com/docker/libtrust"
	"golang.org/x/sys/unix"
	"gotest.tools/assert"
	"gotest.tools/icmd"
	"gotest.tools/poll"
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
	s.d.StartWithBusybox(c)

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
	s.d.StartWithBusybox(c)

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

	out, err := s.d.Cmd("inspect", "-f", "{{json .Mounts}}", "volrestarttest1")
	assert.NilError(c, err, out)

	if _, err := inspectMountPointJSON(out, "/foo"); err != nil {
		c.Fatalf("Expected volume to exist: /foo, error: %v\n", err)
	}
}

// #11008
func (s *DockerDaemonSuite) TestDaemonRestartUnlessStopped(c *testing.T) {
	s.d.StartWithBusybox(c)

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
	s.d.StartWithBusybox(c)

	out, err := s.d.Cmd("run", "-d", "--name", "test1", "--restart", "on-failure:3", "busybox:latest", "false")
	assert.NilError(c, err, "run top1: %v", out)

	// wait test1 to stop
	hostArgs := []string{"--host", s.d.Sock()}
	err = waitInspectWithArgs("test1", "{{.State.Running}} {{.State.Restarting}}", "false false", 10*time.Second, hostArgs...)
	assert.NilError(c, err, "test1 should exit but not")

	// record last start time
	out, err = s.d.Cmd("inspect", "-f={{.State.StartedAt}}", "test1")
	assert.NilError(c, err, "out: %v", out)
	lastStartTime := out

	s.d.Restart(c)

	// test1 shouldn't restart at all
	err = waitInspectWithArgs("test1", "{{.State.Running}} {{.State.Restarting}}", "false false", 0, hostArgs...)
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

// Make sure we cannot shrink base device at daemon restart.
func (s *DockerDaemonSuite) TestDaemonRestartWithInvalidBasesize(c *testing.T) {
	testRequires(c, Devicemapper)
	s.d.Start(c)

	oldBasesizeBytes := getBaseDeviceSize(c, s.d)
	var newBasesizeBytes int64 = 1073741824 //1GB in bytes

	if newBasesizeBytes < oldBasesizeBytes {
		err := s.d.RestartWithError("--storage-opt", fmt.Sprintf("dm.basesize=%d", newBasesizeBytes))
		assert.Assert(c, err != nil, "daemon should not have started as new base device size is less than existing base device size: %v", err)
		// 'err != nil' is expected behaviour, no new daemon started,
		// so no need to stop daemon.
		if err != nil {
			return
		}
	}
	s.d.Stop(c)
}

// Make sure we can grow base device at daemon restart.
func (s *DockerDaemonSuite) TestDaemonRestartWithIncreasedBasesize(c *testing.T) {
	testRequires(c, Devicemapper)
	s.d.Start(c)

	oldBasesizeBytes := getBaseDeviceSize(c, s.d)

	var newBasesizeBytes int64 = 53687091200 //50GB in bytes

	if newBasesizeBytes < oldBasesizeBytes {
		c.Skip(fmt.Sprintf("New base device size (%v) must be greater than (%s)", units.HumanSize(float64(newBasesizeBytes)), units.HumanSize(float64(oldBasesizeBytes))))
	}

	err := s.d.RestartWithError("--storage-opt", fmt.Sprintf("dm.basesize=%d", newBasesizeBytes))
	assert.Assert(c, err == nil, "we should have been able to start the daemon with increased base device size: %v", err)

	basesizeAfterRestart := getBaseDeviceSize(c, s.d)
	newBasesize, err := convertBasesize(newBasesizeBytes)
	assert.Assert(c, err == nil, "Error in converting base device size: %v", err)
	assert.Equal(c, newBasesize, basesizeAfterRestart, "Basesize passed is not equal to Basesize set")
	s.d.Stop(c)
}

func getBaseDeviceSize(c *testing.T, d *daemon.Daemon) int64 {
	info := d.Info(c)
	for _, statusLine := range info.DriverStatus {
		key, value := statusLine[0], statusLine[1]
		if key == "Base Device Size" {
			return parseDeviceSize(c, value)
		}
	}
	c.Fatal("failed to parse Base Device Size from info")
	return int64(0)
}

func parseDeviceSize(c *testing.T, raw string) int64 {
	size, err := units.RAMInBytes(strings.TrimSpace(raw))
	assert.NilError(c, err)
	return size
}

func convertBasesize(basesizeBytes int64) (int64, error) {
	basesize := units.HumanSize(float64(basesizeBytes))
	basesize = strings.Trim(basesize, " ")[:len(basesize)-3]
	basesizeFloat, err := strconv.ParseFloat(strings.Trim(basesize, " "), 64)
	if err != nil {
		return 0, err
	}
	return int64(basesizeFloat) * 1024 * 1024 * 1024, nil
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

func (s *DockerDaemonSuite) TestDaemonIptablesClean(c *testing.T) {
	s.d.StartWithBusybox(c)

	if out, err := s.d.Cmd("run", "-d", "--name", "top", "-p", "80", "busybox:latest", "top"); err != nil {
		c.Fatalf("Could not run top: %s, %v", out, err)
	}

	ipTablesSearchString := "tcp dpt:80"

	// get output from iptables with container running
	verifyIPTablesContains(c, ipTablesSearchString)

	s.d.Stop(c)

	// get output from iptables after restart
	verifyIPTablesDoesNotContains(c, ipTablesSearchString)
}

func (s *DockerDaemonSuite) TestDaemonIptablesCreate(c *testing.T) {
	s.d.StartWithBusybox(c)

	if out, err := s.d.Cmd("run", "-d", "--name", "top", "--restart=always", "-p", "80", "busybox:latest", "top"); err != nil {
		c.Fatalf("Could not run top: %s, %v", out, err)
	}

	// get output from iptables with container running
	ipTablesSearchString := "tcp dpt:80"
	verifyIPTablesContains(c, ipTablesSearchString)

	s.d.Restart(c)

	// make sure the container is not running
	runningOut, err := s.d.Cmd("inspect", "--format={{.State.Running}}", "top")
	if err != nil {
		c.Fatalf("Could not inspect on container: %s, %v", runningOut, err)
	}
	if strings.TrimSpace(runningOut) != "true" {
		c.Fatalf("Container should have been restarted after daemon restart. Status running should have been true but was: %q", strings.TrimSpace(runningOut))
	}

	// get output from iptables after restart
	verifyIPTablesContains(c, ipTablesSearchString)
}

func verifyIPTablesContains(c *testing.T, ipTablesSearchString string) {
	result := icmd.RunCommand("iptables", "-nvL")
	result.Assert(c, icmd.Success)
	if !strings.Contains(result.Combined(), ipTablesSearchString) {
		c.Fatalf("iptables output should have contained %q, but was %q", ipTablesSearchString, result.Combined())
	}
}

func verifyIPTablesDoesNotContains(c *testing.T, ipTablesSearchString string) {
	result := icmd.RunCommand("iptables", "-nvL")
	result.Assert(c, icmd.Success)
	if strings.Contains(result.Combined(), ipTablesSearchString) {
		c.Fatalf("iptables output should not have contained %q, but was %q", ipTablesSearchString, result.Combined())
	}
}

// TestDaemonIPv6Enabled checks that when the daemon is started with --ipv6=true that the docker0 bridge
// has the fe80::1 address and that a container is assigned a link-local address
func (s *DockerDaemonSuite) TestDaemonIPv6Enabled(c *testing.T) {
	testRequires(c, IPv6)

	setupV6(c)
	defer teardownV6(c)

	s.d.StartWithBusybox(c, "--ipv6")

	iface, err := net.InterfaceByName("docker0")
	if err != nil {
		c.Fatalf("Error getting docker0 interface: %v", err)
	}

	addrs, err := iface.Addrs()
	if err != nil {
		c.Fatalf("Error getting addresses for docker0 interface: %v", err)
	}

	var found bool
	expected := "fe80::1/64"

	for i := range addrs {
		if addrs[i].String() == expected {
			found = true
			break
		}
	}

	if !found {
		c.Fatalf("Bridge does not have an IPv6 Address")
	}

	if out, err := s.d.Cmd("run", "-itd", "--name=ipv6test", "busybox:latest"); err != nil {
		c.Fatalf("Could not run container: %s, %v", out, err)
	}

	out, err := s.d.Cmd("inspect", "--format", "'{{.NetworkSettings.Networks.bridge.LinkLocalIPv6Address}}'", "ipv6test")
	if err != nil {
		c.Fatalf("Error inspecting container: %s, %v", out, err)
	}
	out = strings.Trim(out, " \r\n'")

	if ip := net.ParseIP(out); ip == nil {
		c.Fatalf("Container should have a link-local IPv6 address")
	}

	out, err = s.d.Cmd("inspect", "--format", "'{{.NetworkSettings.Networks.bridge.GlobalIPv6Address}}'", "ipv6test")
	if err != nil {
		c.Fatalf("Error inspecting container: %s, %v", out, err)
	}
	out = strings.Trim(out, " \r\n'")

	if ip := net.ParseIP(out); ip != nil {
		c.Fatalf("Container should not have a global IPv6 address: %v", out)
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

	s.d.StartWithBusybox(c, "--ipv6", "--fixed-cidr-v6=2001:db8:2::/64", "--default-gateway-v6=2001:db8:2::100")

	out, err := s.d.Cmd("run", "-itd", "--name=ipv6test", "busybox:latest")
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

// TestDaemonIPv6FixedCIDRAndMac checks that when the daemon is started with ipv6 fixed CIDR
// the running containers are given an IPv6 address derived from the MAC address and the ipv6 fixed CIDR
func (s *DockerDaemonSuite) TestDaemonIPv6FixedCIDRAndMac(c *testing.T) {
	// IPv6 setup is messing with local bridge address.
	testRequires(c, testEnv.IsLocalDaemon)
	// Delete the docker0 bridge if its left around from previous daemon. It has to be recreated with
	// ipv6 enabled
	deleteInterface(c, "docker0")

	s.d.StartWithBusybox(c, "--ipv6", "--fixed-cidr-v6=2001:db8:1::/64")

	out, err := s.d.Cmd("run", "-itd", "--name=ipv6test", "--mac-address", "AA:BB:CC:DD:EE:FF", "busybox")
	assert.NilError(c, err, out)

	out, err = s.d.Cmd("inspect", "--format", "{{.NetworkSettings.Networks.bridge.GlobalIPv6Address}}", "ipv6test")
	assert.NilError(c, err, out)
	assert.Equal(c, strings.Trim(out, " \r\n'"), "2001:db8:1::aabb:ccdd:eeff")
}

// TestDaemonIPv6HostMode checks that when the running a container with
// network=host the host ipv6 addresses are not removed
func (s *DockerDaemonSuite) TestDaemonIPv6HostMode(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)
	deleteInterface(c, "docker0")

	s.d.StartWithBusybox(c, "--ipv6", "--fixed-cidr-v6=2001:db8:2::/64")
	out, err := s.d.Cmd("run", "-itd", "--name=hostcnt", "--network=host", "busybox:latest")
	assert.NilError(c, err, "Could not run container: %s, %v", out, err)

	out, err = s.d.Cmd("exec", "hostcnt", "ip", "-6", "addr", "show", "docker0")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(strings.Trim(out, " \r\n'"), "2001:db8:2::1"))
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
	listeningPorts := [][]string{
		{"0.0.0.0", "0.0.0.0", "5678"},
		{"127.0.0.1", "127.0.0.1", "1234"},
		{"localhost", "127.0.0.1", "1235"},
	}

	cmdArgs := make([]string, 0, len(listeningPorts)*2)
	for _, hostDirective := range listeningPorts {
		cmdArgs = append(cmdArgs, "--host", fmt.Sprintf("tcp://%s:%s", hostDirective[0], hostDirective[2]))
	}

	s.d.StartWithBusybox(c, cmdArgs...)

	for _, hostDirective := range listeningPorts {
		output, err := s.d.Cmd("run", "-p", fmt.Sprintf("%s:%s:80", hostDirective[1], hostDirective[2]), "busybox", "true")
		if err == nil {
			c.Fatalf("Container should not start, expected port already allocated error: %q", output)
		} else if !strings.Contains(output, "port is already allocated") {
			c.Fatalf("Expected port is already allocated error: %q", output)
		}
	}
}

func (s *DockerDaemonSuite) TestDaemonKeyGeneration(c *testing.T) {
	// TODO: skip or update for Windows daemon
	os.Remove("/etc/docker/key.json")
	s.d.Start(c)
	s.d.Stop(c)

	k, err := libtrust.LoadKeyFile("/etc/docker/key.json")
	if err != nil {
		c.Fatalf("Error opening key file")
	}
	kid := k.KeyID()
	// Test Key ID is a valid fingerprint (e.g. QQXN:JY5W:TBXI:MK3X:GX6P:PD5D:F56N:NHCS:LVRZ:JA46:R24J:XEFF)
	if len(kid) != 59 {
		c.Fatalf("Bad key ID: %s", kid)
	}
}

// GH#11320 - verify that the daemon exits on failure properly
// Note that this explicitly tests the conflict of {-b,--bridge} and {--bip} options as the means
// to get a daemon init failure; no other tests for -b/--bip conflict are therefore required
func (s *DockerDaemonSuite) TestDaemonExitOnFailure(c *testing.T) {
	//attempt to start daemon with incorrect flags (we know -b and --bip conflict)
	if err := s.d.StartWithError("--bridge", "nosuchbridge", "--bip", "1.1.1.1"); err != nil {
		//verify we got the right error
		if !strings.Contains(err.Error(), "Daemon exited") {
			c.Fatalf("Expected daemon not to start, got %v", err)
		}
		// look in the log and make sure we got the message that daemon is shutting down
		icmd.RunCommand("grep", "failed to start daemon", s.d.LogFileName()).Assert(c, icmd.Success)
	} else {
		//if we didn't get an error and the daemon is running, this is a failure
		c.Fatal("Conflicting options should cause the daemon to error out with a failure")
	}
}

func (s *DockerDaemonSuite) TestDaemonBridgeExternal(c *testing.T) {
	d := s.d
	err := d.StartWithError("--bridge", "nosuchbridge")
	assert.ErrorContains(c, err, "", `--bridge option with an invalid bridge should cause the daemon to fail`)
	defer d.Restart(c)

	bridgeName := "external-bridge"
	bridgeIP := "192.169.1.1/24"
	_, bridgeIPNet, _ := net.ParseCIDR(bridgeIP)

	createInterface(c, "bridge", bridgeName, bridgeIP)
	defer deleteInterface(c, bridgeName)

	d.StartWithBusybox(c, "--bridge", bridgeName)

	ipTablesSearchString := bridgeIPNet.String()
	icmd.RunCommand("iptables", "-t", "nat", "-nvL").Assert(c, icmd.Expected{
		Out: ipTablesSearchString,
	})

	out, err := d.Cmd("run", "-d", "--name", "ExtContainer", "busybox", "top")
	assert.NilError(c, err, out)

	containerIP := d.FindContainerIP(c, "ExtContainer")
	ip := net.ParseIP(containerIP)
	assert.Assert(c, bridgeIPNet.Contains(ip), "Container IP-Address must be in the same subnet range : %s", containerIP)
}

func (s *DockerDaemonSuite) TestDaemonBridgeNone(c *testing.T) {
	// start with bridge none
	d := s.d
	d.StartWithBusybox(c, "--bridge", "none")
	defer d.Restart(c)

	// verify docker0 iface is not there
	icmd.RunCommand("ifconfig", "docker0").Assert(c, icmd.Expected{
		ExitCode: 1,
		Error:    "exit status 1",
		Err:      "Device not found",
	})

	// verify default "bridge" network is not there
	out, err := d.Cmd("network", "inspect", "bridge")
	assert.ErrorContains(c, err, "", `"bridge" network should not be present if daemon started with --bridge=none`)
	assert.Assert(c, strings.Contains(out, "No such network"))
}

func createInterface(c *testing.T, ifType string, ifName string, ipNet string) {
	icmd.RunCommand("ip", "link", "add", "name", ifName, "type", ifType).Assert(c, icmd.Success)
	icmd.RunCommand("ifconfig", ifName, ipNet, "up").Assert(c, icmd.Success)
}

func deleteInterface(c *testing.T, ifName string) {
	if icmd.RunCommand("ip", "link", "show", ifName).ExitCode != 0 {
		return
	}
	icmd.RunCommand("ip", "link", "delete", ifName).Assert(c, icmd.Success)
	icmd.RunCommand("iptables", "-t", "nat", "--flush").Assert(c, icmd.Success)
	icmd.RunCommand("iptables", "--flush").Assert(c, icmd.Success)
}

func (s *DockerDaemonSuite) TestDaemonBridgeIP(c *testing.T) {
	// TestDaemonBridgeIP Steps
	// 1. Delete the existing docker0 Bridge
	// 2. Set --bip daemon configuration and start the new Docker Daemon
	// 3. Check if the bip config has taken effect using ifconfig and iptables commands
	// 4. Launch a Container and make sure the IP-Address is in the expected subnet
	// 5. Delete the docker0 Bridge
	// 6. Restart the Docker Daemon (via deferred action)
	//    This Restart takes care of bringing docker0 interface back to auto-assigned IP

	defaultNetworkBridge := "docker0"
	deleteInterface(c, defaultNetworkBridge)

	d := s.d

	bridgeIP := "192.169.1.1/24"
	ip, bridgeIPNet, _ := net.ParseCIDR(bridgeIP)

	d.StartWithBusybox(c, "--bip", bridgeIP)
	defer d.Restart(c)

	ifconfigSearchString := ip.String()
	icmd.RunCommand("ifconfig", defaultNetworkBridge).Assert(c, icmd.Expected{
		Out: ifconfigSearchString,
	})

	ipTablesSearchString := bridgeIPNet.String()
	icmd.RunCommand("iptables", "-t", "nat", "-nvL").Assert(c, icmd.Expected{
		Out: ipTablesSearchString,
	})

	out, err := d.Cmd("run", "-d", "--name", "test", "busybox", "top")
	assert.NilError(c, err, out)

	containerIP := d.FindContainerIP(c, "test")
	ip = net.ParseIP(containerIP)
	assert.Equal(c, bridgeIPNet.Contains(ip), true, fmt.Sprintf("Container IP-Address must be in the same subnet range : %s", containerIP))
	deleteInterface(c, defaultNetworkBridge)
}

func (s *DockerDaemonSuite) TestDaemonRestartWithBridgeIPChange(c *testing.T) {
	s.d.Start(c)
	defer s.d.Restart(c)
	s.d.Stop(c)

	// now we will change the docker0's IP and then try starting the daemon
	bridgeIP := "192.169.100.1/24"
	_, bridgeIPNet, _ := net.ParseCIDR(bridgeIP)

	icmd.RunCommand("ifconfig", "docker0", bridgeIP).Assert(c, icmd.Success)

	s.d.Start(c, "--bip", bridgeIP)

	//check if the iptables contains new bridgeIP MASQUERADE rule
	ipTablesSearchString := bridgeIPNet.String()
	icmd.RunCommand("iptables", "-t", "nat", "-nvL").Assert(c, icmd.Expected{
		Out: ipTablesSearchString,
	})
}

func (s *DockerDaemonSuite) TestDaemonBridgeFixedCidr(c *testing.T) {
	d := s.d

	bridgeName := "external-bridge"
	bridgeIP := "192.169.1.1/24"

	createInterface(c, "bridge", bridgeName, bridgeIP)
	defer deleteInterface(c, bridgeName)

	args := []string{"--bridge", bridgeName, "--fixed-cidr", "192.169.1.0/30"}
	d.StartWithBusybox(c, args...)
	defer d.Restart(c)

	for i := 0; i < 4; i++ {
		cName := "Container" + strconv.Itoa(i)
		out, err := d.Cmd("run", "-d", "--name", cName, "busybox", "top")
		if err != nil {
			assert.Assert(c, strings.Contains(out, "no available IPv4 addresses"), "Could not run a Container : %s %s", err.Error(), out)
		}
	}
}

func (s *DockerDaemonSuite) TestDaemonBridgeFixedCidr2(c *testing.T) {
	d := s.d

	bridgeName := "external-bridge"
	bridgeIP := "10.2.2.1/16"

	createInterface(c, "bridge", bridgeName, bridgeIP)
	defer deleteInterface(c, bridgeName)

	d.StartWithBusybox(c, "--bip", bridgeIP, "--fixed-cidr", "10.2.2.0/24")
	defer s.d.Restart(c)

	out, err := d.Cmd("run", "-d", "--name", "bb", "busybox", "top")
	assert.NilError(c, err, out)
	defer d.Cmd("stop", "bb")

	out, err = d.Cmd("exec", "bb", "/bin/sh", "-c", "ifconfig eth0 | awk '/inet addr/{print substr($2,6)}'")
	assert.NilError(c, err)
	assert.Equal(c, out, "10.2.2.0\n")

	out, err = d.Cmd("run", "--rm", "busybox", "/bin/sh", "-c", "ifconfig eth0 | awk '/inet addr/{print substr($2,6)}'")
	assert.NilError(c, err, out)
	assert.Equal(c, out, "10.2.2.2\n")
}

func (s *DockerDaemonSuite) TestDaemonBridgeFixedCIDREqualBridgeNetwork(c *testing.T) {
	d := s.d

	bridgeName := "external-bridge"
	bridgeIP := "172.27.42.1/16"

	createInterface(c, "bridge", bridgeName, bridgeIP)
	defer deleteInterface(c, bridgeName)

	d.StartWithBusybox(c, "--bridge", bridgeName, "--fixed-cidr", bridgeIP)
	defer s.d.Restart(c)

	out, err := d.Cmd("run", "-d", "busybox", "top")
	assert.NilError(c, err, out)
	cid1 := strings.TrimSpace(out)
	defer d.Cmd("stop", cid1)
}

func (s *DockerDaemonSuite) TestDaemonDefaultGatewayIPv4Implicit(c *testing.T) {
	defaultNetworkBridge := "docker0"
	deleteInterface(c, defaultNetworkBridge)

	d := s.d

	bridgeIP := "192.169.1.1"
	bridgeIPNet := fmt.Sprintf("%s/24", bridgeIP)

	d.StartWithBusybox(c, "--bip", bridgeIPNet)
	defer d.Restart(c)

	expectedMessage := fmt.Sprintf("default via %s dev", bridgeIP)
	out, err := d.Cmd("run", "busybox", "ip", "-4", "route", "list", "0/0")
	assert.NilError(c, err, out)
	assert.Equal(c, strings.Contains(out, expectedMessage), true, fmt.Sprintf("Implicit default gateway should be bridge IP %s, but default route was '%s'", bridgeIP, strings.TrimSpace(out)))
	deleteInterface(c, defaultNetworkBridge)
}

func (s *DockerDaemonSuite) TestDaemonDefaultGatewayIPv4Explicit(c *testing.T) {
	defaultNetworkBridge := "docker0"
	deleteInterface(c, defaultNetworkBridge)

	d := s.d

	bridgeIP := "192.169.1.1"
	bridgeIPNet := fmt.Sprintf("%s/24", bridgeIP)
	gatewayIP := "192.169.1.254"

	d.StartWithBusybox(c, "--bip", bridgeIPNet, "--default-gateway", gatewayIP)
	defer d.Restart(c)

	expectedMessage := fmt.Sprintf("default via %s dev", gatewayIP)
	out, err := d.Cmd("run", "busybox", "ip", "-4", "route", "list", "0/0")
	assert.NilError(c, err, out)
	assert.Equal(c, strings.Contains(out, expectedMessage), true, fmt.Sprintf("Explicit default gateway should be %s, but default route was '%s'", gatewayIP, strings.TrimSpace(out)))
	deleteInterface(c, defaultNetworkBridge)
}

func (s *DockerDaemonSuite) TestDaemonDefaultGatewayIPv4ExplicitOutsideContainerSubnet(c *testing.T) {
	defaultNetworkBridge := "docker0"
	deleteInterface(c, defaultNetworkBridge)

	// Program a custom default gateway outside of the container subnet, daemon should accept it and start
	s.d.StartWithBusybox(c, "--bip", "172.16.0.10/16", "--fixed-cidr", "172.16.1.0/24", "--default-gateway", "172.16.0.254")

	deleteInterface(c, defaultNetworkBridge)
	s.d.Restart(c)
}

func (s *DockerDaemonSuite) TestDaemonDefaultNetworkInvalidClusterConfig(c *testing.T) {

	// Start daemon without docker0 bridge
	defaultNetworkBridge := "docker0"
	deleteInterface(c, defaultNetworkBridge)

	discoveryBackend := "consul://consuladdr:consulport/some/path"
	s.d.Start(c, fmt.Sprintf("--cluster-store=%s", discoveryBackend))

	// Start daemon with docker0 bridge
	result := icmd.RunCommand("ifconfig", defaultNetworkBridge)
	result.Assert(c, icmd.Success)

	s.d.Restart(c, fmt.Sprintf("--cluster-store=%s", discoveryBackend))
}

func (s *DockerDaemonSuite) TestDaemonIP(c *testing.T) {
	d := s.d

	ipStr := "192.170.1.1/24"
	ip, _, _ := net.ParseCIDR(ipStr)
	args := []string{"--ip", ip.String()}
	d.StartWithBusybox(c, args...)
	defer d.Restart(c)

	out, err := d.Cmd("run", "-d", "-p", "8000:8000", "busybox", "top")
	assert.Assert(c, err != nil, "Running a container must fail with an invalid --ip option")
	assert.Equal(c, strings.Contains(out, "Error starting userland proxy"), true)

	ifName := "dummy"
	createInterface(c, "dummy", ifName, ipStr)
	defer deleteInterface(c, ifName)

	_, err = d.Cmd("run", "-d", "-p", "8000:8000", "busybox", "top")
	assert.NilError(c, err, out)

	result := icmd.RunCommand("iptables", "-t", "nat", "-nvL")
	result.Assert(c, icmd.Success)
	regex := fmt.Sprintf("DNAT.*%s.*dpt:8000", ip.String())
	matched, _ := regexp.MatchString(regex, result.Combined())
	assert.Equal(c, matched, true, fmt.Sprintf("iptables output should have contained %q, but was %q", regex, result.Combined()))
}

func (s *DockerDaemonSuite) TestDaemonICCPing(c *testing.T) {
	testRequires(c, bridgeNfIptables)
	d := s.d

	bridgeName := "external-bridge"
	bridgeIP := "192.169.1.1/24"

	createInterface(c, "bridge", bridgeName, bridgeIP)
	defer deleteInterface(c, bridgeName)

	d.StartWithBusybox(c, "--bridge", bridgeName, "--icc=false")
	defer d.Restart(c)

	result := icmd.RunCommand("iptables", "-nvL", "FORWARD")
	result.Assert(c, icmd.Success)
	regex := fmt.Sprintf("DROP.*all.*%s.*%s", bridgeName, bridgeName)
	matched, _ := regexp.MatchString(regex, result.Combined())
	assert.Equal(c, matched, true, fmt.Sprintf("iptables output should have contained %q, but was %q", regex, result.Combined()))
	// Pinging another container must fail with --icc=false
	pingContainers(c, d, true)

	ipStr := "192.171.1.1/24"
	ip, _, _ := net.ParseCIDR(ipStr)
	ifName := "icc-dummy"

	createInterface(c, "dummy", ifName, ipStr)

	// But, Pinging external or a Host interface must succeed
	pingCmd := fmt.Sprintf("ping -c 1 %s -W 1", ip.String())
	runArgs := []string{"run", "--rm", "busybox", "sh", "-c", pingCmd}
	out, err := d.Cmd(runArgs...)
	assert.NilError(c, err, out)
}

func (s *DockerDaemonSuite) TestDaemonICCLinkExpose(c *testing.T) {
	d := s.d

	bridgeName := "external-bridge"
	bridgeIP := "192.169.1.1/24"

	createInterface(c, "bridge", bridgeName, bridgeIP)
	defer deleteInterface(c, bridgeName)

	d.StartWithBusybox(c, "--bridge", bridgeName, "--icc=false")
	defer d.Restart(c)

	result := icmd.RunCommand("iptables", "-nvL", "FORWARD")
	result.Assert(c, icmd.Success)
	regex := fmt.Sprintf("DROP.*all.*%s.*%s", bridgeName, bridgeName)
	matched, _ := regexp.MatchString(regex, result.Combined())
	assert.Equal(c, matched, true, fmt.Sprintf("iptables output should have contained %q, but was %q", regex, result.Combined()))
	out, err := d.Cmd("run", "-d", "--expose", "4567", "--name", "icc1", "busybox", "nc", "-l", "-p", "4567")
	assert.NilError(c, err, out)

	out, err = d.Cmd("run", "--link", "icc1:icc1", "busybox", "nc", "icc1", "4567")
	assert.NilError(c, err, out)
}

func (s *DockerDaemonSuite) TestDaemonLinksIpTablesRulesWhenLinkAndUnlink(c *testing.T) {
	bridgeName := "external-bridge"
	bridgeIP := "192.169.1.1/24"

	createInterface(c, "bridge", bridgeName, bridgeIP)
	defer deleteInterface(c, bridgeName)

	s.d.StartWithBusybox(c, "--bridge", bridgeName, "--icc=false")
	defer s.d.Restart(c)

	out, err := s.d.Cmd("run", "-d", "--name", "child", "--publish", "8080:80", "busybox", "top")
	assert.NilError(c, err, out)
	out, err = s.d.Cmd("run", "-d", "--name", "parent", "--link", "child:http", "busybox", "top")
	assert.NilError(c, err, out)

	childIP := s.d.FindContainerIP(c, "child")
	parentIP := s.d.FindContainerIP(c, "parent")

	sourceRule := []string{"-i", bridgeName, "-o", bridgeName, "-p", "tcp", "-s", childIP, "--sport", "80", "-d", parentIP, "-j", "ACCEPT"}
	destinationRule := []string{"-i", bridgeName, "-o", bridgeName, "-p", "tcp", "-s", parentIP, "--dport", "80", "-d", childIP, "-j", "ACCEPT"}
	if !iptables.Exists("filter", "DOCKER", sourceRule...) || !iptables.Exists("filter", "DOCKER", destinationRule...) {
		c.Fatal("Iptables rules not found")
	}

	s.d.Cmd("rm", "--link", "parent/http")
	if iptables.Exists("filter", "DOCKER", sourceRule...) || iptables.Exists("filter", "DOCKER", destinationRule...) {
		c.Fatal("Iptables rules should be removed when unlink")
	}

	s.d.Cmd("kill", "child")
	s.d.Cmd("kill", "parent")
}

func (s *DockerDaemonSuite) TestDaemonUlimitDefaults(c *testing.T) {

	s.d.StartWithBusybox(c, "--default-ulimit", "nofile=42:42", "--default-ulimit", "nproc=1024:1024")

	out, err := s.d.Cmd("run", "--ulimit", "nproc=2048", "--name=test", "busybox", "/bin/sh", "-c", "echo $(ulimit -n); echo $(ulimit -p)")
	if err != nil {
		c.Fatal(err, out)
	}

	outArr := strings.Split(out, "\n")
	if len(outArr) < 2 {
		c.Fatalf("got unexpected output: %s", out)
	}
	nofile := strings.TrimSpace(outArr[0])
	nproc := strings.TrimSpace(outArr[1])

	if nofile != "42" {
		c.Fatalf("expected `ulimit -n` to be `42`, got: %s", nofile)
	}
	if nproc != "2048" {
		c.Fatalf("expected `ulimit -p` to be 2048, got: %s", nproc)
	}

	// Now restart daemon with a new default
	s.d.Restart(c, "--default-ulimit", "nofile=43")

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

	if nofile != "43" {
		c.Fatalf("expected `ulimit -n` to be `43`, got: %s", nofile)
	}
	if nproc != "2048" {
		c.Fatalf("expected `ulimit -p` to be 2048, got: %s", nproc)
	}
}

// #11315
func (s *DockerDaemonSuite) TestDaemonRestartRenameContainer(c *testing.T) {
	s.d.StartWithBusybox(c)

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
	s.d.StartWithBusybox(c)

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
	s.d.StartWithBusybox(c)

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
	s.d.StartWithBusybox(c, "--log-driver=none")

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
	s.d.StartWithBusybox(c, "--log-driver=none")

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
	s.d.StartWithBusybox(c, "--log-driver=none")

	out, err := s.d.Cmd("run", "--name=test", "busybox", "echo", "testline")
	assert.NilError(c, err, out)

	out, err = s.d.Cmd("logs", "test")
	assert.Assert(c, err != nil, "Logs should fail with 'none' driver")
	expected := `configured logging driver does not support reading`
	assert.Assert(c, strings.Contains(out, expected))
}

func (s *DockerDaemonSuite) TestDaemonLoggingDriverShouldBeIgnoredForBuild(c *testing.T) {
	s.d.StartWithBusybox(c, "--log-driver=splunk")

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
	dir, err := ioutil.TempDir("", "socket-cleanup-test")
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

func (s *DockerDaemonSuite) TestDaemonWithWrongkey(c *testing.T) {
	type Config struct {
		Crv string `json:"crv"`
		D   string `json:"d"`
		Kid string `json:"kid"`
		Kty string `json:"kty"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}

	os.Remove("/etc/docker/key.json")
	s.d.Start(c)
	s.d.Stop(c)

	config := &Config{}
	bytes, err := ioutil.ReadFile("/etc/docker/key.json")
	if err != nil {
		c.Fatalf("Error reading key.json file: %s", err)
	}

	// byte[] to Data-Struct
	if err := json.Unmarshal(bytes, &config); err != nil {
		c.Fatalf("Error Unmarshal: %s", err)
	}

	//replace config.Kid with the fake value
	config.Kid = "VSAJ:FUYR:X3H2:B2VZ:KZ6U:CJD5:K7BX:ZXHY:UZXT:P4FT:MJWG:HRJ4"

	// NEW Data-Struct to byte[]
	newBytes, err := json.Marshal(&config)
	if err != nil {
		c.Fatalf("Error Marshal: %s", err)
	}

	// write back
	if err := ioutil.WriteFile("/etc/docker/key.json", newBytes, 0400); err != nil {
		c.Fatalf("Error ioutil.WriteFile: %s", err)
	}

	defer os.Remove("/etc/docker/key.json")

	if err := s.d.StartWithError(); err == nil {
		c.Fatalf("It should not be successful to start daemon with wrong key: %v", err)
	}

	content, err := s.d.ReadLogFile()
	assert.Assert(c, err == nil)

	if !strings.Contains(string(content), "Public Key ID does not match") {
		c.Fatalf("Missing KeyID message from daemon logs: %s", string(content))
	}
}

func (s *DockerDaemonSuite) TestDaemonRestartKillWait(c *testing.T) {
	s.d.StartWithBusybox(c)

	out, err := s.d.Cmd("run", "-id", "busybox", "/bin/cat")
	if err != nil {
		c.Fatalf("Could not run /bin/cat: err=%v\n%s", err, out)
	}
	containerID := strings.TrimSpace(out)

	if out, err := s.d.Cmd("kill", containerID); err != nil {
		c.Fatalf("Could not kill %s: err=%v\n%s", containerID, err, out)
	}

	s.d.Restart(c)

	errchan := make(chan error)
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
// https://github.com/docker/docker/issues/19280
func (s *DockerDaemonSuite) TestHTTPSRun(c *testing.T) {
	const (
		testDaemonHTTPSAddr = "tcp://localhost:4271"
	)

	s.d.StartWithBusybox(c, "--tlsverify", "--tlscacert", "fixtures/https/ca.pem", "--tlscert", "fixtures/https/server-cert.pem",
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
	if err == nil || !strings.Contains(string(out), "Could not load X509 key pair") {
		c.Fatalf("Daemon should not have started due to missing certs: %v\n%s", err, string(out))
	}
}

// TestHTTPSInfoRogueCert connects via two-way authenticated HTTPS to the info endpoint
// by using a rogue client certificate and checks that it fails with the expected error.
func (s *DockerDaemonSuite) TestHTTPSInfoRogueCert(c *testing.T) {
	const (
		errBadCertificate   = "bad certificate"
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
		"--tlscert", "fixtures/https/client-rogue-cert.pem",
		"--tlskey", "fixtures/https/client-rogue-key.pem",
		"info",
	}
	out, err := s.d.Cmd(args...)
	if err == nil || !strings.Contains(out, errBadCertificate) {
		c.Fatalf("Expected err: %s, got instead: %s and output: %s", errBadCertificate, err, out)
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

func pingContainers(c *testing.T, d *daemon.Daemon, expectFailure bool) {
	var dargs []string
	if d != nil {
		dargs = []string{"--host", d.Sock()}
	}

	args := append(dargs, "run", "-d", "--name", "container1", "busybox", "top")
	dockerCmd(c, args...)

	args = append(dargs, "run", "--rm", "--link", "container1:alias1", "busybox", "sh", "-c")
	pingCmd := "ping -c 1 %s -W 1"
	args = append(args, fmt.Sprintf(pingCmd, "alias1"))
	_, _, err := dockerCmdWithError(args...)

	if expectFailure {
		assert.ErrorContains(c, err, "")
	} else {
		assert.NilError(c, err)
	}

	args = append(dargs, "rm", "-f", "container1")
	dockerCmd(c, args...)
}

func (s *DockerDaemonSuite) TestDaemonRestartWithSocketAsVolume(c *testing.T) {
	s.d.StartWithBusybox(c)

	socket := filepath.Join(s.d.Folder, "docker.sock")

	out, err := s.d.Cmd("run", "--restart=always", "-v", socket+":/sock", "busybox")
	assert.NilError(c, err, "Output: %s", out)
	s.d.Restart(c)
}

// os.Kill should kill daemon ungracefully, leaving behind container mounts.
// A subsequent daemon restart should clean up said mounts.
func (s *DockerDaemonSuite) TestCleanupMountsAfterDaemonAndContainerKill(c *testing.T) {
	d := daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
	d.StartWithBusybox(c)

	out, err := d.Cmd("run", "-d", "busybox", "top")
	assert.NilError(c, err, "Output: %s", out)

	id := strings.TrimSpace(out)

	// If there are no mounts with container id visible from the host
	// (as those are in container's own mount ns), there is nothing
	// to check here and the test should be skipped.
	mountOut, err := ioutil.ReadFile("/proc/self/mountinfo")
	assert.NilError(c, err, "Output: %s", mountOut)
	if !strings.Contains(string(mountOut), id) {
		d.Stop(c)
		c.Skip("no container mounts visible in host ns")
	}

	// kill the daemon
	assert.NilError(c, d.Kill())

	// kill the container
	icmd.RunCommand(ctrBinary, "--address", containerdSocket,
		"--namespace", d.ContainersNamespace(), "tasks", "kill", id).Assert(c, icmd.Success)

	// restart daemon.
	d.Restart(c)

	// Now, container mounts should be gone.
	mountOut, err = ioutil.ReadFile("/proc/self/mountinfo")
	assert.NilError(c, err, "Output: %s", mountOut)
	assert.Assert(c, !strings.Contains(string(mountOut), id), "%s is still mounted from older daemon start:\nDaemon root repository %s\n%s", id, d.Root, mountOut)

	d.Stop(c)
}

// os.Interrupt should perform a graceful daemon shutdown and hence cleanup mounts.
func (s *DockerDaemonSuite) TestCleanupMountsAfterGracefulShutdown(c *testing.T) {
	d := daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
	d.StartWithBusybox(c)

	out, err := d.Cmd("run", "-d", "busybox", "top")
	assert.NilError(c, err, "Output: %s", out)
	id := strings.TrimSpace(out)

	// Send SIGINT and daemon should clean up
	assert.NilError(c, d.Signal(os.Interrupt))
	// Wait for the daemon to stop.
	assert.NilError(c, <-d.Wait)

	mountOut, err := ioutil.ReadFile("/proc/self/mountinfo")
	assert.NilError(c, err, "Output: %s", mountOut)

	assert.Assert(c, !strings.Contains(string(mountOut), id), "%s is still mounted from older daemon start:\nDaemon root repository %s\n%s", id, d.Root, mountOut)
}

func (s *DockerDaemonSuite) TestDaemonRestartWithContainerRunning(t *testing.T) {
	s.d.StartWithBusybox(t)
	if out, err := s.d.Cmd("run", "-d", "--name", "test", "busybox", "top"); err != nil {
		t.Fatal(out, err)
	}

	s.d.Restart(t)
	// Container 'test' should be removed without error
	if out, err := s.d.Cmd("rm", "test"); err != nil {
		t.Fatal(out, err)
	}
}

func (s *DockerDaemonSuite) TestDaemonRestartCleanupNetns(c *testing.T) {
	s.d.StartWithBusybox(c)
	out, err := s.d.Cmd("run", "--name", "netns", "-d", "busybox", "top")
	if err != nil {
		c.Fatal(out, err)
	}

	// Get sandbox key via inspect
	out, err = s.d.Cmd("inspect", "--format", "'{{.NetworkSettings.SandboxKey}}'", "netns")
	if err != nil {
		c.Fatalf("Error inspecting container: %s, %v", out, err)
	}
	fileName := strings.Trim(out, " \r\n'")

	if out, err := s.d.Cmd("stop", "netns"); err != nil {
		c.Fatal(out, err)
	}

	// Test if the file still exists
	icmd.RunCommand("stat", "-c", "%n", fileName).Assert(c, icmd.Expected{
		Out: fileName,
	})

	// Remove the container and restart the daemon
	if out, err := s.d.Cmd("rm", "netns"); err != nil {
		c.Fatal(out, err)
	}

	s.d.Restart(c)

	// Test again and see now the netns file does not exist
	icmd.RunCommand("stat", "-c", "%n", fileName).Assert(c, icmd.Expected{
		Err:      "No such file or directory",
		ExitCode: 1,
	})
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

func setupV6(c *testing.T) {
	// Hack to get the right IPv6 address on docker0, which has already been created
	result := icmd.RunCommand("ip", "addr", "add", "fe80::1/64", "dev", "docker0")
	result.Assert(c, icmd.Success)
}

func teardownV6(c *testing.T) {
	result := icmd.RunCommand("ip", "addr", "del", "fe80::1/64", "dev", "docker0")
	result.Assert(c, icmd.Success)
}

func (s *DockerDaemonSuite) TestDaemonRestartWithContainerWithRestartPolicyAlways(c *testing.T) {
	s.d.StartWithBusybox(c)

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
	s.d.StartWithBusybox(c, "--log-opt=max-size=1k")
	name := "logtest"
	out, err := s.d.Cmd("run", "-d", "--log-opt=max-file=5", "--name", name, "busybox", "top")
	assert.NilError(c, err, "Output: %s, err: %v", out, err)

	out, err = s.d.Cmd("inspect", "-f", "{{ .HostConfig.LogConfig.Config }}", name)
	assert.NilError(c, err, "Output: %s", out)
	assert.Assert(c, strings.Contains(out, "max-size:1k"))
	assert.Assert(c, strings.Contains(out, "max-file:5"))

	out, err = s.d.Cmd("inspect", "-f", "{{ .HostConfig.LogConfig.Type }}", name)
	assert.NilError(c, err, "Output: %s", out)
	assert.Equal(c, strings.TrimSpace(out), "json-file")
}

func (s *DockerDaemonSuite) TestDaemonRestartWithPausedContainer(c *testing.T) {
	s.d.StartWithBusybox(c)
	if out, err := s.d.Cmd("run", "-i", "-d", "--name", "test", "busybox", "top"); err != nil {
		c.Fatal(err, out)
	}
	if out, err := s.d.Cmd("pause", "test"); err != nil {
		c.Fatal(err, out)
	}
	s.d.Restart(c)

	errchan := make(chan error)
	go func() {
		out, err := s.d.Cmd("start", "test")
		if err != nil {
			errchan <- fmt.Errorf("%v:\n%s", err, out)
		}
		name := strings.TrimSpace(out)
		if name != "test" {
			errchan <- fmt.Errorf("Paused container start error on docker daemon restart, expected 'test' but got '%s'", name)
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
	s.d.StartWithBusybox(c)

	out, err := s.d.Cmd("create", "-v", "test:/foo", "busybox")
	assert.NilError(c, err, out)

	s.d.Restart(c)

	out, err = s.d.Cmd("volume", "rm", "test")
	assert.Assert(c, err != nil, "should not be able to remove in use volume after daemon restart")
	assert.Assert(c, strings.Contains(out, "in use"))
}

func (s *DockerDaemonSuite) TestDaemonRestartLocalVolumes(c *testing.T) {
	s.d.Start(c)

	out, err := s.d.Cmd("volume", "create", "test")
	assert.NilError(c, err, out)
	s.d.Restart(c)

	out, err = s.d.Cmd("volume", "inspect", "test")
	assert.NilError(c, err, out)
}

// FIXME(vdemeester) should be a unit test
func (s *DockerDaemonSuite) TestDaemonCorruptedLogDriverAddress(c *testing.T) {
	d := daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
	assert.Assert(c, d.StartWithError("--log-driver=syslog", "--log-opt", "syslog-address=corrupted:42") != nil)
	expected := "syslog-address should be in form proto://address"
	icmd.RunCommand("grep", expected, d.LogFileName()).Assert(c, icmd.Success)
}

// FIXME(vdemeester) should be a unit test
func (s *DockerDaemonSuite) TestDaemonCorruptedFluentdAddress(c *testing.T) {
	d := daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
	assert.Assert(c, d.StartWithError("--log-driver=fluentd", "--log-opt", "fluentd-address=corrupted:c") != nil)
	expected := "invalid fluentd-address corrupted:c: "
	icmd.RunCommand("grep", expected, d.LogFileName()).Assert(c, icmd.Success)
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
	tmpHost := os.Getenv("DOCKER_HOST")
	defer func() {
		os.Setenv("DOCKER_HOST", tmpHost)
	}()

	os.Setenv("DOCKER_HOST", "")

	out, _ := dockerCmd(
		c,
		"--tlsverify",
		"--tlscacert", "fixtures/https/ca.pem",
		"--tlscert", "fixtures/https/client-cert.pem",
		"--tlskey", "fixtures/https/client-key.pem",
		"version",
	)
	if !strings.Contains(out, "Server") {
		c.Fatalf("docker version should return information of server side")
	}

	// ensure when connecting to the server that only a single acceptable CA is requested
	contents, err := ioutil.ReadFile("fixtures/https/ca.pem")
	assert.NilError(c, err)
	rootCert, err := helpers.ParseCertificatePEM(contents)
	assert.NilError(c, err)
	rootPool := x509.NewCertPool()
	rootPool.AddCert(rootCert)

	var certRequestInfo *tls.CertificateRequestInfo
	conn, err := tls.Dial("tcp", fmt.Sprintf("%s:%d", opts.DefaultHTTPHost, opts.DefaultTLSHTTPPort), &tls.Config{
		RootCAs: rootPool,
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

	s.d.StartWithBusybox(c, "--bip", bridgeRange)
	defer s.d.Restart(c)

	var cont int
	for {
		contName := fmt.Sprintf("container%d", cont)
		_, err := s.d.Cmd("run", "--name", contName, "-d", "busybox", "/bin/sleep", "2")
		if err != nil {
			// pool exhausted
			break
		}
		ip, err := s.d.Cmd("inspect", "--format", "'{{.NetworkSettings.IPAddress}}'", contName)
		assert.Assert(c, err == nil, "%s", ip)

		assert.Assert(c, ip != bridgeIP)
		cont++
	}
}

// Test daemon for no space left on device error
func (s *DockerDaemonSuite) TestDaemonNoSpaceLeftOnDeviceError(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux, Network)

	testDir, err := ioutil.TempDir("", "no-space-left-on-device-test")
	assert.NilError(c, err)
	defer os.RemoveAll(testDir)
	assert.Assert(c, mount.MakeRShared(testDir) == nil)
	defer mount.Unmount(testDir)

	// create a 3MiB image (with a 2MiB ext4 fs) and mount it as graph root
	// Why in a container? Because `mount` sometimes behaves weirdly and often fails outright on this test in debian:jessie (which is what the test suite runs under if run from the Makefile)
	dockerCmd(c, "run", "--rm", "-v", testDir+":/test", "busybox", "sh", "-c", "dd of=/test/testfs.img bs=1M seek=3 count=0")
	icmd.RunCommand("mkfs.ext4", "-F", filepath.Join(testDir, "testfs.img")).Assert(c, icmd.Success)

	dockerCmd(c, "run", "--privileged", "--rm", "-v", testDir+":/test:shared", "busybox", "sh", "-c", "mkdir -p /test/test-mount/vfs && mount -n -t ext4 /test/testfs.img /test/test-mount/vfs")
	defer mount.Unmount(filepath.Join(testDir, "test-mount"))

	s.d.Start(c, "--storage-driver", "vfs", "--data-root", filepath.Join(testDir, "test-mount"))
	defer s.d.Stop(c)

	// pull a repository large enough to overfill the mounted filesystem
	pullOut, err := s.d.Cmd("pull", "debian:stretch")
	assert.Assert(c, err != nil, "%s", pullOut)
	assert.Assert(c, strings.Contains(pullOut, "no space left on device"))
}

// Test daemon restart with container links + auto restart
func (s *DockerDaemonSuite) TestDaemonRestartContainerLinksRestart(c *testing.T) {
	s.d.StartWithBusybox(c)

	var parent1Args []string
	var parent2Args []string
	wg := sync.WaitGroup{}
	maxChildren := 10
	chErr := make(chan error, maxChildren)

	for i := 0; i < maxChildren; i++ {
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
			log, _ := ioutil.ReadFile(s.d.LogFileName())
			c.Fatalf("parent container is not running\n%s", string(log))
		}
	}
}

func (s *DockerDaemonSuite) TestDaemonCgroupParent(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	cgroupParent := "test"
	name := "cgroup-test"

	s.d.StartWithBusybox(c, "--cgroup-parent", cgroupParent)
	defer s.d.Restart(c)

	out, err := s.d.Cmd("run", "--name", name, "busybox", "cat", "/proc/self/cgroup")
	assert.NilError(c, err)
	cgroupPaths := ParseCgroupPaths(string(out))
	assert.Assert(c, len(cgroupPaths) != 0, "unexpected output - %q", string(out))
	out, err = s.d.Cmd("inspect", "-f", "{{.Id}}", name)
	assert.NilError(c, err)
	id := strings.TrimSpace(string(out))
	expectedCgroup := path.Join(cgroupParent, id)
	found := false
	for _, path := range cgroupPaths {
		if strings.HasSuffix(path, expectedCgroup) {
			found = true
			break
		}
	}
	assert.Assert(c, found, "Cgroup path for container (%s) doesn't found in cgroups file: %s", expectedCgroup, cgroupPaths)
}

func (s *DockerDaemonSuite) TestDaemonRestartWithLinks(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Windows does not support links
	s.d.StartWithBusybox(c)

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
	assert.Equal(c, strings.Contains(out, "1 packets transmitted, 1 packets received"), true, fmt.Sprintf("%s", out))
}

func (s *DockerDaemonSuite) TestDaemonRestartWithNames(c *testing.T) {
	testRequires(c, DaemonIsLinux) // Windows does not support links
	s.d.StartWithBusybox(c)

	out, err := s.d.Cmd("create", "--name=test", "busybox")
	assert.NilError(c, err, out)

	out, err = s.d.Cmd("run", "-d", "--name=test2", "busybox", "top")
	assert.NilError(c, err, out)
	test2ID := strings.TrimSpace(out)

	out, err = s.d.Cmd("run", "-d", "--name=test3", "--link", "test2:abc", "busybox", "top")
	assert.NilError(c, err)
	test3ID := strings.TrimSpace(out)

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
	s.d.StartWithBusybox(t)

	cid, err := s.d.Cmd("run", "-d", "--name", "test", "busybox", "top")
	defer s.d.Stop(t)
	if err != nil {
		t.Fatal(cid, err)
	}
	cid = strings.TrimSpace(cid)

	pid, err := s.d.Cmd("inspect", "-f", "{{.State.Pid}}", cid)
	assert.NilError(t, err)
	pid = strings.TrimSpace(pid)

	// Kill the daemon
	if err := s.d.Kill(); err != nil {
		t.Fatal(err)
	}

	// kill the container
	icmd.RunCommand(ctrBinary, "--address", containerdSocket,
		"--namespace", s.d.ContainersNamespace(), "tasks", "kill", cid).Assert(t, icmd.Success)

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
	s.d.StartWithBusybox(c, "--live-restore")

	out, err := s.d.Cmd("run", "-d", "busybox", "top")
	assert.NilError(c, err, "Output: %s", out)
	id := strings.TrimSpace(out)

	// kill the daemon
	assert.Assert(c, s.d.Kill() == nil)

	// Check if there are mounts with container id visible from the host.
	// If not, those mounts exist in container's own mount ns, and so
	// the following check for mounts being cleared is pointless.
	skipMountCheck := false
	mountOut, err := ioutil.ReadFile("/proc/self/mountinfo")
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
	mountOut, err = ioutil.ReadFile("/proc/self/mountinfo")
	assert.Assert(c, err == nil, "Output: %s", mountOut)
	comment := fmt.Sprintf("%s is still mounted from older daemon start:\nDaemon root repository %s\n%s", id, s.d.Root, mountOut)
	assert.Equal(c, strings.Contains(string(mountOut), id), false, comment)
}

// TestDaemonRestartWithUnpausedRunningContainer requires live restore of running containers.
func (s *DockerDaemonSuite) TestDaemonRestartWithUnpausedRunningContainer(t *testing.T) {
	testRequires(t, DaemonIsLinux)
	s.d.StartWithBusybox(t, "--live-restore")

	cid, err := s.d.Cmd("run", "-d", "--name", "test", "busybox", "top")
	defer s.d.Stop(t)
	if err != nil {
		t.Fatal(cid, err)
	}
	cid = strings.TrimSpace(cid)

	pid, err := s.d.Cmd("inspect", "-f", "{{.State.Pid}}", cid)
	assert.NilError(t, err)

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
		"tasks", "resume", cid)
	result.Assert(t, icmd.Success)

	// Give time to containerd to process the command if we don't
	// the resume event might be received after we do the inspect
	poll.WaitOn(t, pollCheck(t, func(*testing.T) (interface{}, string) {
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
	s.d.StartWithBusybox(c)

	out, err := s.d.Cmd("run", "-d", "--name=test", "busybox", "top")
	assert.NilError(c, err, out)

	out, err = s.d.Cmd("run", "--name=test2", "--link=test:abc", "busybox", "sh", "-c", "ping -c 1 abc")
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, "1 packets transmitted, 1 packets received"))
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
	assert.Assert(c, strings.Contains(b.String(), infoLog))
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

	debugLog := "\x1b[37mDEBU\x1b"

	p, tty, err := pty.Open()
	assert.NilError(c, err)
	defer func() {
		tty.Close()
		p.Close()
	}()

	b := bytes.NewBuffer(nil)
	go io.Copy(b, p)

	s.d.StartWithLogFile(tty, "--debug")
	s.d.Stop(c)
	assert.Assert(c, strings.Contains(b.String(), debugLog))
}

func (s *DockerDaemonSuite) TestDaemonDiscoveryBackendConfigReload(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	// daemon config file
	daemonConfig := `{ "debug" : false }`
	configFile, err := ioutil.TempFile("", "test-daemon-discovery-backend-config-reload-config")
	assert.Assert(c, err == nil, "could not create temp file for config reload")
	configFilePath := configFile.Name()
	defer func() {
		configFile.Close()
		os.RemoveAll(configFile.Name())
	}()

	_, err = configFile.Write([]byte(daemonConfig))
	assert.NilError(c, err)

	// --log-level needs to be set so that d.Start() doesn't add --debug causing
	// a conflict with the config
	s.d.Start(c, "--config-file", configFilePath, "--log-level=info")

	// daemon config file
	daemonConfig = `{
	      "cluster-store": "consul://consuladdr:consulport/some/path",
	      "cluster-advertise": "192.168.56.100:0",
	      "debug" : false
	}`

	err = configFile.Truncate(0)
	assert.NilError(c, err)
	_, err = configFile.Seek(0, os.SEEK_SET)
	assert.NilError(c, err)

	_, err = configFile.Write([]byte(daemonConfig))
	assert.NilError(c, err)

	err = s.d.ReloadConfig()
	assert.Assert(c, err == nil, "error reloading daemon config")

	out, err := s.d.Cmd("info")
	assert.NilError(c, err)

	assert.Assert(c, strings.Contains(out, "Cluster Store: consul://consuladdr:consulport/some/path"))
	assert.Assert(c, strings.Contains(out, "Cluster Advertise: 192.168.56.100:0"))
}

// Test for #21956
func (s *DockerDaemonSuite) TestDaemonLogOptions(c *testing.T) {
	s.d.StartWithBusybox(c, "--log-driver=syslog", "--log-opt=syslog-address=udp://127.0.0.1:514")

	out, err := s.d.Cmd("run", "-d", "--log-driver=json-file", "busybox", "top")
	assert.NilError(c, err, out)
	id := strings.TrimSpace(out)

	out, err = s.d.Cmd("inspect", "--format='{{.HostConfig.LogConfig}}'", id)
	assert.NilError(c, err, out)
	assert.Assert(c, strings.Contains(out, "{json-file map[]}"))
}

// Test case for #20936, #22443
func (s *DockerDaemonSuite) TestDaemonMaxConcurrency(c *testing.T) {
	s.d.Start(c, "--max-concurrent-uploads=6", "--max-concurrent-downloads=8")

	expectedMaxConcurrentUploads := `level=debug msg="Max Concurrent Uploads: 6"`
	expectedMaxConcurrentDownloads := `level=debug msg="Max Concurrent Downloads: 8"`
	content, err := s.d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(string(content), expectedMaxConcurrentUploads))
	assert.Assert(c, strings.Contains(string(content), expectedMaxConcurrentDownloads))
}

// Test case for #20936, #22443
func (s *DockerDaemonSuite) TestDaemonMaxConcurrencyWithConfigFile(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	// daemon config file
	configFilePath := "test.json"
	configFile, err := os.Create(configFilePath)
	assert.NilError(c, err)
	defer os.Remove(configFilePath)

	daemonConfig := `{ "max-concurrent-downloads" : 8 }`
	fmt.Fprintf(configFile, "%s", daemonConfig)
	configFile.Close()
	s.d.Start(c, fmt.Sprintf("--config-file=%s", configFilePath))

	expectedMaxConcurrentUploads := `level=debug msg="Max Concurrent Uploads: 5"`
	expectedMaxConcurrentDownloads := `level=debug msg="Max Concurrent Downloads: 8"`
	content, err := s.d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(string(content), expectedMaxConcurrentUploads))
	assert.Assert(c, strings.Contains(string(content), expectedMaxConcurrentDownloads))
	configFile, err = os.Create(configFilePath)
	assert.NilError(c, err)
	daemonConfig = `{ "max-concurrent-uploads" : 7, "max-concurrent-downloads" : 9 }`
	fmt.Fprintf(configFile, "%s", daemonConfig)
	configFile.Close()

	assert.Assert(c, s.d.Signal(unix.SIGHUP) == nil)
	// unix.Kill(s.d.cmd.Process.Pid, unix.SIGHUP)

	time.Sleep(3 * time.Second)

	expectedMaxConcurrentUploads = `level=debug msg="Reset Max Concurrent Uploads: 7"`
	expectedMaxConcurrentDownloads = `level=debug msg="Reset Max Concurrent Downloads: 9"`
	content, err = s.d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(string(content), expectedMaxConcurrentUploads))
	assert.Assert(c, strings.Contains(string(content), expectedMaxConcurrentDownloads))
}

// Test case for #20936, #22443
func (s *DockerDaemonSuite) TestDaemonMaxConcurrencyWithConfigFileReload(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	// daemon config file
	configFilePath := "test.json"
	configFile, err := os.Create(configFilePath)
	assert.NilError(c, err)
	defer os.Remove(configFilePath)

	daemonConfig := `{ "max-concurrent-uploads" : null }`
	fmt.Fprintf(configFile, "%s", daemonConfig)
	configFile.Close()
	s.d.Start(c, fmt.Sprintf("--config-file=%s", configFilePath))

	expectedMaxConcurrentUploads := `level=debug msg="Max Concurrent Uploads: 5"`
	expectedMaxConcurrentDownloads := `level=debug msg="Max Concurrent Downloads: 3"`
	content, err := s.d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(string(content), expectedMaxConcurrentUploads))
	assert.Assert(c, strings.Contains(string(content), expectedMaxConcurrentDownloads))
	configFile, err = os.Create(configFilePath)
	assert.NilError(c, err)
	daemonConfig = `{ "max-concurrent-uploads" : 1, "max-concurrent-downloads" : null }`
	fmt.Fprintf(configFile, "%s", daemonConfig)
	configFile.Close()

	assert.Assert(c, s.d.Signal(unix.SIGHUP) == nil)
	// unix.Kill(s.d.cmd.Process.Pid, unix.SIGHUP)

	time.Sleep(3 * time.Second)

	expectedMaxConcurrentUploads = `level=debug msg="Reset Max Concurrent Uploads: 1"`
	expectedMaxConcurrentDownloads = `level=debug msg="Reset Max Concurrent Downloads: 3"`
	content, err = s.d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(string(content), expectedMaxConcurrentUploads))
	assert.Assert(c, strings.Contains(string(content), expectedMaxConcurrentDownloads))
	configFile, err = os.Create(configFilePath)
	assert.NilError(c, err)
	daemonConfig = `{ "labels":["foo=bar"] }`
	fmt.Fprintf(configFile, "%s", daemonConfig)
	configFile.Close()

	assert.Assert(c, s.d.Signal(unix.SIGHUP) == nil)

	time.Sleep(3 * time.Second)

	expectedMaxConcurrentUploads = `level=debug msg="Reset Max Concurrent Uploads: 5"`
	expectedMaxConcurrentDownloads = `level=debug msg="Reset Max Concurrent Downloads: 3"`
	content, err = s.d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(string(content), expectedMaxConcurrentUploads))
	assert.Assert(c, strings.Contains(string(content), expectedMaxConcurrentDownloads))
}

func (s *DockerDaemonSuite) TestBuildOnDisabledBridgeNetworkDaemon(c *testing.T) {
	s.d.StartWithBusybox(c, "-b=none", "--iptables=false")

	result := cli.BuildCmd(c, "busyboxs", cli.Daemon(s.d),
		build.WithDockerfile(`
        FROM busybox
        RUN cat /etc/hosts`),
		build.WithoutCache,
	)
	comment := fmt.Sprintf("Failed to build image. output %s, exitCode %d, err %v", result.Combined(), result.ExitCode, result.Error)
	assert.Assert(c, result.Error == nil, comment)
	assert.Equal(c, result.ExitCode, 0, comment)
}

// Test case for #21976
func (s *DockerDaemonSuite) TestDaemonDNSFlagsInHostMode(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	s.d.StartWithBusybox(c, "--dns", "1.2.3.4", "--dns-search", "example.com", "--dns-opt", "timeout:3")

	expectedOutput := "nameserver 1.2.3.4"
	out, _ := s.d.Cmd("run", "--net=host", "busybox", "cat", "/etc/resolv.conf")
	assert.Assert(c, strings.Contains(out, expectedOutput), "Expected '%s', but got %q", expectedOutput, out)
	expectedOutput = "search example.com"
	assert.Assert(c, strings.Contains(out, expectedOutput), "Expected '%s', but got %q", expectedOutput, out)
	expectedOutput = "options timeout:3"
	assert.Assert(c, strings.Contains(out, expectedOutput), "Expected '%s', but got %q", expectedOutput, out)
}

func (s *DockerDaemonSuite) TestRunWithRuntimeFromConfigFile(c *testing.T) {
	conf, err := ioutil.TempFile("", "config-file-")
	assert.NilError(c, err)
	configName := conf.Name()
	conf.Close()
	defer os.Remove(configName)

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
	ioutil.WriteFile(configName, []byte(config), 0644)
	s.d.StartWithBusybox(c, "--config-file", configName)

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
	assert.Assert(c, strings.Contains(out, "/usr/local/bin/vm-manager: no such file or directory"))
	// Reset config to only have the default
	config = `
{
    "runtimes": {
    }
}
`
	ioutil.WriteFile(configName, []byte(config), 0644)
	assert.Assert(c, s.d.Signal(unix.SIGHUP) == nil)
	// Give daemon time to reload config
	<-time.After(1 * time.Second)

	// Run with default runtime
	out, err = s.d.Cmd("run", "--rm", "--runtime=runc", "busybox", "ls")
	assert.NilError(c, err, out)

	// Run with "oci"
	out, err = s.d.Cmd("run", "--rm", "--runtime=oci", "busybox", "ls")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, strings.Contains(out, "Unknown runtime specified oci"))
	// Start previously created container with oci
	out, err = s.d.Cmd("start", "oci-runtime-ls")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, strings.Contains(out, "Unknown runtime specified oci"))
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
	ioutil.WriteFile(configName, []byte(config), 0644)
	assert.Assert(c, s.d.Signal(unix.SIGHUP) == nil)
	// Give daemon time to reload config
	<-time.After(1 * time.Second)

	content, err := s.d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(string(content), `file configuration validation failed: runtime name 'runc' is reserved`))
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
	ioutil.WriteFile(configName, []byte(config), 0644)
	assert.Assert(c, s.d.Signal(unix.SIGHUP) == nil)
	// Give daemon time to reload config
	<-time.After(1 * time.Second)

	out, err = s.d.Cmd("run", "--rm", "busybox", "ls")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, strings.Contains(out, "/usr/local/bin/vm-manager: no such file or directory"))
	// Run with default runtime explicitly
	out, err = s.d.Cmd("run", "--rm", "--runtime=runc", "busybox", "ls")
	assert.NilError(c, err, out)
}

func (s *DockerDaemonSuite) TestRunWithRuntimeFromCommandLine(c *testing.T) {
	s.d.StartWithBusybox(c, "--add-runtime", "oci=runc", "--add-runtime", "vm=/usr/local/bin/vm-manager")

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
	assert.Assert(c, strings.Contains(out, "/usr/local/bin/vm-manager: no such file or directory"))
	// Start a daemon without any extra runtimes
	s.d.Stop(c)
	s.d.StartWithBusybox(c)

	// Run with default runtime
	out, err = s.d.Cmd("run", "--rm", "--runtime=runc", "busybox", "ls")
	assert.NilError(c, err, out)

	// Run with "oci"
	out, err = s.d.Cmd("run", "--rm", "--runtime=oci", "busybox", "ls")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, strings.Contains(out, "Unknown runtime specified oci"))
	// Start previously created container with oci
	out, err = s.d.Cmd("start", "oci-runtime-ls")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, strings.Contains(out, "Unknown runtime specified oci"))
	// Check that we can't override the default runtime
	s.d.Stop(c)
	assert.Assert(c, s.d.StartWithError("--add-runtime", "runc=my-runc") != nil)

	content, err := s.d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(string(content), `runtime name 'runc' is reserved`))
	// Check that we can select a default runtime
	s.d.Stop(c)
	s.d.StartWithBusybox(c, "--default-runtime=vm", "--add-runtime", "oci=runc", "--add-runtime", "vm=/usr/local/bin/vm-manager")

	out, err = s.d.Cmd("run", "--rm", "busybox", "ls")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, strings.Contains(out, "/usr/local/bin/vm-manager: no such file or directory"))
	// Run with default runtime explicitly
	out, err = s.d.Cmd("run", "--rm", "--runtime=runc", "busybox", "ls")
	assert.NilError(c, err, out)
}

func (s *DockerDaemonSuite) TestDaemonRestartWithAutoRemoveContainer(c *testing.T) {
	s.d.StartWithBusybox(c)

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
	s.d.StartWithBusybox(c)

	containerName := "error-values"
	// Make a container with both a non 0 exit code and an error message
	// We explicitly disable `--init` for this test, because `--init` is enabled by default
	// on "experimental". Enabling `--init` results in a different behavior; because the "init"
	// process itself is PID1, the container does not fail on _startup_ (i.e., `docker-init` starting),
	// but directly after. The exit code of the container is still 127, but the Error Message is not
	// captured, so `.State.Error` is empty.
	// See the discussion on https://github.com/docker/docker/pull/30227#issuecomment-274161426,
	// and https://github.com/docker/docker/pull/26061#r78054578 for more information.
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
	assert.Assert(c, strings.Contains(errMsg1, "executable file not found"))
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

	dockerProxyPath, err := exec.LookPath("docker-proxy")
	assert.NilError(c, err)
	tmpDir, err := ioutil.TempDir("", "test-docker-proxy")
	assert.NilError(c, err)

	newProxyPath := filepath.Join(tmpDir, "docker-proxy")
	cmd := exec.Command("cp", dockerProxyPath, newProxyPath)
	assert.NilError(c, cmd.Run())

	// custom one
	s.d.StartWithBusybox(c, "--userland-proxy-path", newProxyPath)
	out, err := s.d.Cmd("run", "-p", "5000:5000", "busybox:latest", "true")
	assert.NilError(c, err, out)

	// try with the original one
	s.d.Restart(c, "--userland-proxy-path", dockerProxyPath)
	out, err = s.d.Cmd("run", "-p", "5000:5000", "busybox:latest", "true")
	assert.NilError(c, err, out)

	// not exist
	s.d.Restart(c, "--userland-proxy-path", "/does/not/exist")
	out, err = s.d.Cmd("run", "-p", "5000:5000", "busybox:latest", "true")
	assert.ErrorContains(c, err, "", out)
	assert.Assert(c, strings.Contains(out, "driver failed programming external connectivity on endpoint"))
	assert.Assert(c, strings.Contains(out, "/does/not/exist: no such file or directory"))
}

// Test case for #22471
func (s *DockerDaemonSuite) TestDaemonShutdownTimeout(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)
	s.d.StartWithBusybox(c, "--shutdown-timeout=3")

	_, err := s.d.Cmd("run", "-d", "busybox", "top")
	assert.NilError(c, err)

	assert.Assert(c, s.d.Signal(unix.SIGINT) == nil)

	select {
	case <-s.d.Wait:
	case <-time.After(5 * time.Second):
	}

	expectedMessage := `level=debug msg="daemon configured with a 3 seconds minimum shutdown timeout"`
	content, err := s.d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(string(content), expectedMessage))
}

// Test case for #22471
func (s *DockerDaemonSuite) TestDaemonShutdownTimeoutWithConfigFile(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon)

	// daemon config file
	configFilePath := "test.json"
	configFile, err := os.Create(configFilePath)
	assert.NilError(c, err)
	defer os.Remove(configFilePath)

	daemonConfig := `{ "shutdown-timeout" : 8 }`
	fmt.Fprintf(configFile, "%s", daemonConfig)
	configFile.Close()
	s.d.Start(c, fmt.Sprintf("--config-file=%s", configFilePath))

	configFile, err = os.Create(configFilePath)
	assert.NilError(c, err)
	daemonConfig = `{ "shutdown-timeout" : 5 }`
	fmt.Fprintf(configFile, "%s", daemonConfig)
	configFile.Close()

	assert.Assert(c, s.d.Signal(unix.SIGHUP) == nil)

	select {
	case <-s.d.Wait:
	case <-time.After(3 * time.Second):
	}

	expectedMessage := `level=debug msg="Reset Shutdown Timeout: 5"`
	content, err := s.d.ReadLogFile()
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(string(content), expectedMessage))
}

// Test case for 29342
func (s *DockerDaemonSuite) TestExecWithUserAfterLiveRestore(c *testing.T) {
	testRequires(c, DaemonIsLinux)
	s.d.StartWithBusybox(c, "--live-restore")

	out, err := s.d.Cmd("run", "-d", "--name=top", "busybox", "sh", "-c", "addgroup -S test && adduser -S -G test test -D -s /bin/sh && touch /adduser_end && top")
	assert.NilError(c, err, "Output: %s", out)

	s.d.WaitRun("top")

	// Wait for shell command to be completed
	_, err = s.d.Cmd("exec", "top", "sh", "-c", `for i in $(seq 1 5); do if [ -e /adduser_end ]; then rm -f /adduser_end && break; else sleep 1 && false; fi; done`)
	assert.Assert(c, err == nil, "Timeout waiting for shell command to be completed")

	out1, err := s.d.Cmd("exec", "-u", "test", "top", "id")
	// uid=100(test) gid=101(test) groups=101(test)
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
	testRequires(c, DaemonIsLinux, overlayFSSupported, testEnv.IsLocalDaemon)
	s.d.StartWithBusybox(c, "--live-restore", "--storage-driver", "overlay")
	out, err := s.d.Cmd("run", "-d", "--name=top", "busybox", "top")
	assert.NilError(c, err, "Output: %s", out)

	s.d.WaitRun("top")

	// restart daemon.
	s.d.Restart(c, "--live-restore", "--storage-driver", "overlay")

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
	s.d.StartWithBusybox(c, "--live-restore")

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

	s.d.StartWithBusybox(c, "--default-shm-size", fmt.Sprintf("%v", size))

	name := "shm1"
	out, err := s.d.Cmd("run", "--name", name, "busybox", "mount")
	assert.NilError(c, err, "Output: %s", out)
	assert.Assert(c, pattern.MatchString(out))
	out, err = s.d.Cmd("inspect", "--format", "{{.HostConfig.ShmSize}}", name)
	assert.NilError(c, err, "Output: %s", out)
	assert.Equal(c, strings.TrimSpace(out), fmt.Sprintf("%v", size))
}

func (s *DockerDaemonSuite) TestShmSizeReload(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	configPath, err := ioutil.TempDir("", "test-daemon-shm-size-reload-config")
	assert.Assert(c, err == nil, "could not create temp file for config reload")
	defer os.RemoveAll(configPath) // clean up
	configFile := filepath.Join(configPath, "config.json")

	size := 67108864 * 2
	configData := []byte(fmt.Sprintf(`{"default-shm-size": "%dM"}`, size/1024/1024))
	assert.Assert(c, ioutil.WriteFile(configFile, configData, 0666) == nil, "could not write temp file for config reload")
	pattern := regexp.MustCompile(fmt.Sprintf("shm on /dev/shm type tmpfs(.*)size=%dk", size/1024))

	s.d.StartWithBusybox(c, "--config-file", configFile)

	name := "shm1"
	out, err := s.d.Cmd("run", "--name", name, "busybox", "mount")
	assert.NilError(c, err, "Output: %s", out)
	assert.Assert(c, pattern.MatchString(out))
	out, err = s.d.Cmd("inspect", "--format", "{{.HostConfig.ShmSize}}", name)
	assert.NilError(c, err, "Output: %s", out)
	assert.Equal(c, strings.TrimSpace(out), fmt.Sprintf("%v", size))

	size = 67108864 * 3
	configData = []byte(fmt.Sprintf(`{"default-shm-size": "%dM"}`, size/1024/1024))
	assert.Assert(c, ioutil.WriteFile(configFile, configData, 0666) == nil, "could not write temp file for config reload")
	pattern = regexp.MustCompile(fmt.Sprintf("shm on /dev/shm type tmpfs(.*)size=%dk", size/1024))

	err = s.d.ReloadConfig()
	assert.Assert(c, err == nil, "error reloading daemon config")

	name = "shm2"
	out, err = s.d.Cmd("run", "--name", name, "busybox", "mount")
	assert.NilError(c, err, "Output: %s", out)
	assert.Assert(c, pattern.MatchString(out))
	out, err = s.d.Cmd("inspect", "--format", "{{.HostConfig.ShmSize}}", name)
	assert.NilError(c, err, "Output: %s", out)
	assert.Equal(c, strings.TrimSpace(out), fmt.Sprintf("%v", size))
}

func testDaemonStartIpcMode(c *testing.T, from, mode string, valid bool) {
	d := daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
	c.Logf("Checking IpcMode %s set from %s\n", mode, from)
	var serr error
	switch from {
	case "config":
		f, err := ioutil.TempFile("", "test-daemon-ipc-config")
		assert.NilError(c, err)
		defer os.Remove(f.Name())
		config := `{"default-ipc-mode": "` + mode + `"}`
		_, err = f.WriteString(config)
		assert.NilError(c, f.Close())
		assert.NilError(c, err)

		serr = d.StartWithError("--config-file", f.Name())
	case "cli":
		serr = d.StartWithError("--default-ipc-mode", mode)
	default:
		c.Fatalf("testDaemonStartIpcMode: invalid 'from' argument")
	}
	if serr == nil {
		d.Stop(c)
	}

	if valid {
		assert.NilError(c, serr)
	} else {
		assert.ErrorContains(c, serr, "")
		icmd.RunCommand("grep", "-E", "IPC .* is (invalid|not supported)", d.LogFileName()).Assert(c, icmd.Success)
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
	cli := d.NewClientT(c)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	name := "test-plugin-rm-fail"
	out, err := cli.PluginInstall(ctx, name, types.PluginInstallOptions{
		Disabled:             true,
		AcceptAllPermissions: true,
		RemoteRef:            "cpuguy83/docker-logdriver-test",
	})
	assert.NilError(c, err)
	defer out.Close()
	io.Copy(ioutil.Discard, out)

	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, _, err := cli.PluginInspectWithRaw(ctx, name)
	assert.NilError(c, err)

	// simulate a bad/partial removal by removing the plugin config.
	configPath := filepath.Join(d.Root, "plugins", p.ID, "config.json")
	assert.NilError(c, os.Remove(configPath))

	d.Restart(c)
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = cli.Ping(ctx)
	assert.NilError(c, err)

	_, _, err = cli.PluginInspectWithRaw(ctx, name)
	// plugin should be gone since the config.json is gone
	assert.ErrorContains(c, err, "")
}
