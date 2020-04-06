package main

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/docker/docker/integration-cli/daemon"
	testdaemon "github.com/docker/docker/testutil/daemon"
	"gotest.tools/assert"
)

// ensure docker info succeeds
func (s *DockerSuite) TestInfoEnsureSucceeds(c *testing.T) {
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
		assert.Assert(c, strings.Contains(out, linePrefix), "couldn't find string %v in output", linePrefix)
	}
}

// TestInfoFormat tests `docker info --format`
func (s *DockerSuite) TestInfoFormat(c *testing.T) {
	out, status := dockerCmd(c, "info", "--format", "{{json .}}")
	assert.Equal(c, status, 0)
	var m map[string]interface{}
	err := json.Unmarshal([]byte(out), &m)
	assert.NilError(c, err)
	_, _, err = dockerCmdWithError("info", "--format", "{{.badString}}")
	assert.ErrorContains(c, err, "")
}

// TestInfoDiscoveryBackend verifies that a daemon run with `--cluster-advertise` and
// `--cluster-store` properly show the backend's endpoint in info output.
func (s *DockerSuite) TestInfoDiscoveryBackend(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	d := daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
	discoveryBackend := "consul://consuladdr:consulport/some/path"
	discoveryAdvertise := "1.1.1.1:2375"
	d.Start(c, fmt.Sprintf("--cluster-store=%s", discoveryBackend), fmt.Sprintf("--cluster-advertise=%s", discoveryAdvertise))
	defer d.Stop(c)

	out, err := d.Cmd("info")
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, fmt.Sprintf("Cluster Store: %s\n", discoveryBackend)))
	assert.Assert(c, strings.Contains(out, fmt.Sprintf("Cluster Advertise: %s\n", discoveryAdvertise)))
}

// TestInfoDiscoveryInvalidAdvertise verifies that a daemon run with
// an invalid `--cluster-advertise` configuration
func (s *DockerSuite) TestInfoDiscoveryInvalidAdvertise(c *testing.T) {
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
func (s *DockerSuite) TestInfoDiscoveryAdvertiseInterfaceName(c *testing.T) {
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
	assert.Assert(c, strings.Contains(out, fmt.Sprintf("Cluster Store: %s\n", discoveryBackend)))
	assert.Assert(c, strings.Contains(out, fmt.Sprintf("Cluster Advertise: %s:2375\n", ip.String())))
}

func (s *DockerSuite) TestInfoDisplaysRunningContainers(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	existing := existingContainerStates(c)

	dockerCmd(c, "run", "-d", "busybox", "top")
	out, _ := dockerCmd(c, "info")
	assert.Assert(c, strings.Contains(out, fmt.Sprintf("Containers: %d\n", existing["Containers"]+1)))
	assert.Assert(c, strings.Contains(out, fmt.Sprintf(" Running: %d\n", existing["ContainersRunning"]+1)))
	assert.Assert(c, strings.Contains(out, fmt.Sprintf(" Paused: %d\n", existing["ContainersPaused"])))
	assert.Assert(c, strings.Contains(out, fmt.Sprintf(" Stopped: %d\n", existing["ContainersStopped"])))
}

func (s *DockerSuite) TestInfoDisplaysPausedContainers(c *testing.T) {
	testRequires(c, IsPausable)

	existing := existingContainerStates(c)

	out := runSleepingContainer(c, "-d")
	cleanedContainerID := strings.TrimSpace(out)

	dockerCmd(c, "pause", cleanedContainerID)

	out, _ = dockerCmd(c, "info")
	assert.Assert(c, strings.Contains(out, fmt.Sprintf("Containers: %d\n", existing["Containers"]+1)))
	assert.Assert(c, strings.Contains(out, fmt.Sprintf(" Running: %d\n", existing["ContainersRunning"])))
	assert.Assert(c, strings.Contains(out, fmt.Sprintf(" Paused: %d\n", existing["ContainersPaused"]+1)))
	assert.Assert(c, strings.Contains(out, fmt.Sprintf(" Stopped: %d\n", existing["ContainersStopped"])))
}

func (s *DockerSuite) TestInfoDisplaysStoppedContainers(c *testing.T) {
	testRequires(c, DaemonIsLinux)

	existing := existingContainerStates(c)

	out, _ := dockerCmd(c, "run", "-d", "busybox", "top")
	cleanedContainerID := strings.TrimSpace(out)

	dockerCmd(c, "stop", cleanedContainerID)

	out, _ = dockerCmd(c, "info")
	assert.Assert(c, strings.Contains(out, fmt.Sprintf("Containers: %d\n", existing["Containers"]+1)))
	assert.Assert(c, strings.Contains(out, fmt.Sprintf(" Running: %d\n", existing["ContainersRunning"])))
	assert.Assert(c, strings.Contains(out, fmt.Sprintf(" Paused: %d\n", existing["ContainersPaused"])))
	assert.Assert(c, strings.Contains(out, fmt.Sprintf(" Stopped: %d\n", existing["ContainersStopped"]+1)))
}

func (s *DockerSuite) TestInfoDebug(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	d := daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
	d.Start(c, "--debug")
	defer d.Stop(c)

	out, err := d.Cmd("--debug", "info")
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, "Debug Mode (client): true\n"))
	assert.Assert(c, strings.Contains(out, "Debug Mode (server): true\n"))
	assert.Assert(c, strings.Contains(out, "File Descriptors"))
	assert.Assert(c, strings.Contains(out, "Goroutines"))
	assert.Assert(c, strings.Contains(out, "System Time"))
	assert.Assert(c, strings.Contains(out, "EventsListeners"))
	assert.Assert(c, strings.Contains(out, "Docker Root Dir"))
}

func (s *DockerSuite) TestInsecureRegistries(c *testing.T) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	registryCIDR := "192.168.1.0/24"
	registryHost := "insecurehost.com:5000"

	d := daemon.New(c, dockerBinary, dockerdBinary, testdaemon.WithEnvironment(testEnv.Execution))
	d.Start(c, "--insecure-registry="+registryCIDR, "--insecure-registry="+registryHost)
	defer d.Stop(c)

	out, err := d.Cmd("info")
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, "Insecure Registries:\n"))
	assert.Assert(c, strings.Contains(out, fmt.Sprintf(" %s\n", registryHost)))
	assert.Assert(c, strings.Contains(out, fmt.Sprintf(" %s\n", registryCIDR)))
}

func (s *DockerDaemonSuite) TestRegistryMirrors(c *testing.T) {

	registryMirror1 := "https://192.168.1.2"
	registryMirror2 := "http://registry.mirror.com:5000"

	s.d.Start(c, "--registry-mirror="+registryMirror1, "--registry-mirror="+registryMirror2)

	out, err := s.d.Cmd("info")
	assert.NilError(c, err)
	assert.Assert(c, strings.Contains(out, "Registry Mirrors:\n"))
	assert.Assert(c, strings.Contains(out, fmt.Sprintf(" %s", registryMirror1)))
	assert.Assert(c, strings.Contains(out, fmt.Sprintf(" %s", registryMirror2)))
}

func existingContainerStates(c *testing.T) map[string]int {
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
