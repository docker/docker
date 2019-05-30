package main

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/docker/docker/integration-cli/checker"
	"github.com/docker/docker/integration-cli/daemon"
	testdaemon "github.com/docker/docker/internal/test/daemon"
	"github.com/go-check/check"
	"gotest.tools/assert"
)

// ensure docker info succeeds
func (s *DockerSuite) TestInfoEnsureSucceeds(c *check.C) {
	out, _ := dockerCmd(c, "info")

	// always shown fields
	stringsToCheck := []string{
		"ID:",
		"Containers:",
		" Running:",
		" Paused:",
		" Stopped:",
		"Images:",
		"OSType:",
		"Architecture:",
		"Logging Driver:",
		"Operating System:",
		"CPUs:",
		"Total Memory:",
		"Kernel Version:",
		"Storage Driver:",
		"Volume:",
		"Network:",
		"Live Restore Enabled:",
	}

	if testEnv.OSType == "linux" {
		stringsToCheck = append(stringsToCheck, "Init Binary:", "Security Options:", "containerd version:", "runc version:", "init version:")
	}

	if DaemonIsLinux() {
		stringsToCheck = append(stringsToCheck, "Runtimes:", "Default Runtime: runc")
	}

	if testEnv.DaemonInfo.ExperimentalBuild {
		stringsToCheck = append(stringsToCheck, "Experimental: true")
	} else {
		stringsToCheck = append(stringsToCheck, "Experimental: false")
	}

	for _, linePrefix := range stringsToCheck {
		c.Assert(out, checker.Contains, linePrefix, check.Commentf("couldn't find string %v in output", linePrefix))
	}
}

// TestInfoFormat tests `docker info --format`
func (s *DockerSuite) TestInfoFormat(c *check.C) {
	out, status := dockerCmd(c, "info", "--format", "{{json .}}")
	c.Assert(status, checker.Equals, 0)
	var m map[string]interface{}
	err := json.Unmarshal([]byte(out), &m)
	assert.NilError(c, err)
	_, _, err = dockerCmdWithError("info", "--format", "{{.badString}}")
	assert.ErrorContains(c, err, "")
}

// TestInfoDiscoveryBackend verifies that a daemon run with `--cluster-advertise` and
// `--cluster-store` properly show the backend's endpoint in info output.
func (s *DockerSuite) TestInfoDiscoveryBackend(c *check.C) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	d := daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
	discoveryBackend := "consul://consuladdr:consulport/some/path"
	discoveryAdvertise := "1.1.1.1:2375"
	d.Start(c, fmt.Sprintf("--cluster-store=%s", discoveryBackend), fmt.Sprintf("--cluster-advertise=%s", discoveryAdvertise))
	defer d.Stop(c)

	out, err := d.Cmd("info")
	assert.NilError(c, err)
	c.Assert(out, checker.Contains, fmt.Sprintf("Cluster Store: %s\n", discoveryBackend))
	c.Assert(out, checker.Contains, fmt.Sprintf("Cluster Advertise: %s\n", discoveryAdvertise))
}

// TestInfoDiscoveryInvalidAdvertise verifies that a daemon run with
// an invalid `--cluster-advertise` configuration
func (s *DockerSuite) TestInfoDiscoveryInvalidAdvertise(c *check.C) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	d := daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
	discoveryBackend := "consul://consuladdr:consulport/some/path"

	// --cluster-advertise with an invalid string is an error
	err := d.StartWithError(fmt.Sprintf("--cluster-store=%s", discoveryBackend), "--cluster-advertise=invalid")
	assert.ErrorContains(c, err, "")

	// --cluster-advertise without --cluster-store is also an error
	err = d.StartWithError("--cluster-advertise=1.1.1.1:2375")
	assert.ErrorContains(c, err, "")
}

// TestInfoDiscoveryAdvertiseInterfaceName verifies that a daemon run with `--cluster-advertise`
// configured with interface name properly show the advertise ip-address in info output.
func (s *DockerSuite) TestInfoDiscoveryAdvertiseInterfaceName(c *check.C) {
	testRequires(c, testEnv.IsLocalDaemon, Network, DaemonIsLinux)

	d := daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
	discoveryBackend := "consul://consuladdr:consulport/some/path"
	discoveryAdvertise := "eth0"

	d.Start(c, fmt.Sprintf("--cluster-store=%s", discoveryBackend), fmt.Sprintf("--cluster-advertise=%s:2375", discoveryAdvertise))
	defer d.Stop(c)

	iface, err := net.InterfaceByName(discoveryAdvertise)
	assert.NilError(c, err)
	addrs, err := iface.Addrs()
	assert.NilError(c, err)
	assert.Assert(c, len(addrs) > 0)
	ip, _, err := net.ParseCIDR(addrs[0].String())
	assert.NilError(c, err)

	out, err := d.Cmd("info")
	assert.NilError(c, err)
	c.Assert(out, checker.Contains, fmt.Sprintf("Cluster Store: %s\n", discoveryBackend))
	c.Assert(out, checker.Contains, fmt.Sprintf("Cluster Advertise: %s:2375\n", ip.String()))
}

func (s *DockerSuite) TestInfoDisplaysRunningContainers(c *check.C) {
	testRequires(c, DaemonIsLinux)

	existing := existingContainerStates(c)

	dockerCmd(c, "run", "-d", "busybox", "top")
	out, _ := dockerCmd(c, "info")
	c.Assert(out, checker.Contains, fmt.Sprintf("Containers: %d\n", existing["Containers"]+1))
	c.Assert(out, checker.Contains, fmt.Sprintf(" Running: %d\n", existing["ContainersRunning"]+1))
	c.Assert(out, checker.Contains, fmt.Sprintf(" Paused: %d\n", existing["ContainersPaused"]))
	c.Assert(out, checker.Contains, fmt.Sprintf(" Stopped: %d\n", existing["ContainersStopped"]))
}

func (s *DockerSuite) TestInfoDisplaysPausedContainers(c *check.C) {
	testRequires(c, IsPausable)

	existing := existingContainerStates(c)

	out := runSleepingContainer(c, "-d")
	cleanedContainerID := strings.TrimSpace(out)

	dockerCmd(c, "pause", cleanedContainerID)

	out, _ = dockerCmd(c, "info")
	c.Assert(out, checker.Contains, fmt.Sprintf("Containers: %d\n", existing["Containers"]+1))
	c.Assert(out, checker.Contains, fmt.Sprintf(" Running: %d\n", existing["ContainersRunning"]))
	c.Assert(out, checker.Contains, fmt.Sprintf(" Paused: %d\n", existing["ContainersPaused"]+1))
	c.Assert(out, checker.Contains, fmt.Sprintf(" Stopped: %d\n", existing["ContainersStopped"]))
}

func (s *DockerSuite) TestInfoDisplaysStoppedContainers(c *check.C) {
	testRequires(c, DaemonIsLinux)

	existing := existingContainerStates(c)

	out, _ := dockerCmd(c, "run", "-d", "busybox", "top")
	cleanedContainerID := strings.TrimSpace(out)

	dockerCmd(c, "stop", cleanedContainerID)

	out, _ = dockerCmd(c, "info")
	c.Assert(out, checker.Contains, fmt.Sprintf("Containers: %d\n", existing["Containers"]+1))
	c.Assert(out, checker.Contains, fmt.Sprintf(" Running: %d\n", existing["ContainersRunning"]))
	c.Assert(out, checker.Contains, fmt.Sprintf(" Paused: %d\n", existing["ContainersPaused"]))
	c.Assert(out, checker.Contains, fmt.Sprintf(" Stopped: %d\n", existing["ContainersStopped"]+1))
}

func (s *DockerSuite) TestInfoDebug(c *check.C) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	d := daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
	d.Start(c, "--debug")
	defer d.Stop(c)

	out, err := d.Cmd("--debug", "info")
	assert.NilError(c, err)
	c.Assert(out, checker.Contains, "Debug Mode (client): true\n")
	c.Assert(out, checker.Contains, "Debug Mode (server): true\n")
	c.Assert(out, checker.Contains, "File Descriptors")
	c.Assert(out, checker.Contains, "Goroutines")
	c.Assert(out, checker.Contains, "System Time")
	c.Assert(out, checker.Contains, "EventsListeners")
	c.Assert(out, checker.Contains, "Docker Root Dir")
}

func (s *DockerSuite) TestInsecureRegistries(c *check.C) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	registryCIDR := "192.168.1.0/24"
	registryHost := "insecurehost.com:5000"

	d := daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
	d.Start(c, "--insecure-registry="+registryCIDR, "--insecure-registry="+registryHost)
	defer d.Stop(c)

	out, err := d.Cmd("info")
	assert.NilError(c, err)
	c.Assert(out, checker.Contains, "Insecure Registries:\n")
	c.Assert(out, checker.Contains, fmt.Sprintf(" %s\n", registryHost))
	c.Assert(out, checker.Contains, fmt.Sprintf(" %s\n", registryCIDR))
}

func (s *DockerDaemonSuite) TestRegistryMirrors(c *check.C) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	registryMirror1 := "https://192.168.1.2"
	registryMirror2 := "http://registry.mirror.com:5000"

	s.d.Start(c, "--registry-mirror="+registryMirror1, "--registry-mirror="+registryMirror2)

	out, err := s.d.Cmd("info")
	assert.NilError(c, err)
	c.Assert(out, checker.Contains, "Registry Mirrors:\n")
	c.Assert(out, checker.Contains, fmt.Sprintf(" %s", registryMirror1))
	c.Assert(out, checker.Contains, fmt.Sprintf(" %s", registryMirror2))
}

func existingContainerStates(c *check.C) map[string]int {
	out, _ := dockerCmd(c, "info", "--format", "{{json .}}")
	var m map[string]interface{}
	err := json.Unmarshal([]byte(out), &m)
	assert.NilError(c, err)
	res := map[string]int{}
	res["Containers"] = int(m["Containers"].(float64))
	res["ContainersRunning"] = int(m["ContainersRunning"].(float64))
	res["ContainersPaused"] = int(m["ContainersPaused"].(float64))
	res["ContainersStopped"] = int(m["ContainersStopped"].(float64))
	return res
}
