package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/client"
	"github.com/docker/docker/integration-cli/requirement"
	"github.com/docker/docker/testutil/registry"
)

func ArchitectureIsNot(arch string) bool {
	return os.Getenv("DOCKER_ENGINE_GOARCH") != arch
}

func DaemonIsWindows() bool {
	return testEnv.OSType == "windows"
}

func DaemonIsWindowsAtLeastBuild(buildNumber int) func() bool {
	return func() bool {
		if testEnv.OSType != "windows" {
			return false
		}
		version := testEnv.DaemonInfo.KernelVersion
		numVersion, _ := strconv.Atoi(strings.Split(version, " ")[1])
		return numVersion >= buildNumber
	}
}

func DaemonIsLinux() bool {
	return testEnv.OSType == "linux"
}

func MinimumAPIVersion(version string) func() bool {
	return func() bool {
		return versions.GreaterThanOrEqualTo(testEnv.DaemonAPIVersion(), version)
	}
}

func OnlyDefaultNetworks() bool {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return false
	}
	networks, err := cli.NetworkList(context.TODO(), types.NetworkListOptions{})
	if err != nil || len(networks) > 0 {
		return false
	}
	return true
}

func IsAmd64() bool {
	return os.Getenv("DOCKER_ENGINE_GOARCH") == "amd64"
}

func NotArm() bool {
	return ArchitectureIsNot("arm")
}

func NotArm64() bool {
	return ArchitectureIsNot("arm64")
}

func NotPpc64le() bool {
	return ArchitectureIsNot("ppc64le")
}

func UnixCli() bool {
	return isUnixCli
}

func Network() bool {
	// Set a timeout on the GET at 15s
	const timeout = 15 * time.Second
	const url = "https://hub.docker.com"

	client := http.Client{
		Timeout: timeout,
	}

	resp, err := client.Get(url)
	if err != nil && strings.Contains(err.Error(), "use of closed network connection") {
		panic(fmt.Sprintf("Timeout for GET request on %s", url))
	}
	if resp != nil {
		resp.Body.Close()
	}
	return err == nil
}

func Apparmor() bool {
	if strings.HasPrefix(testEnv.DaemonInfo.OperatingSystem, "SUSE Linux Enterprise Server ") {
		return false
	}
	buf, err := ioutil.ReadFile("/sys/module/apparmor/parameters/enabled")
	return err == nil && len(buf) > 1 && buf[0] == 'Y'
}

func Devicemapper() bool {
	return strings.HasPrefix(testEnv.DaemonInfo.Driver, "devicemapper")
}

func IPv6() bool {
	cmd := exec.Command("test", "-f", "/proc/net/if_inet6")
	return cmd.Run() != nil
}

func UserNamespaceROMount() bool {
	// quick case--userns not enabled in this test run
	if os.Getenv("DOCKER_REMAP_ROOT") == "" {
		return true
	}
	if _, _, err := dockerCmdWithError("run", "--rm", "--read-only", "busybox", "date"); err != nil {
		return false
	}
	return true
}

func NotUserNamespace() bool {
	root := os.Getenv("DOCKER_REMAP_ROOT")
	return root == ""
}

func UserNamespaceInKernel() bool {
	if _, err := os.Stat("/proc/self/uid_map"); os.IsNotExist(err) {
		/*
		 * This kernel-provided file only exists if user namespaces are
		 * supported
		 */
		return false
	}

	// We need extra check on redhat based distributions
	if f, err := os.Open("/sys/module/user_namespace/parameters/enable"); err == nil {
		defer f.Close()
		b := make([]byte, 1)
		_, _ = f.Read(b)
		return string(b) != "N"
	}

	return true
}

func IsPausable() bool {
	if testEnv.OSType == "windows" {
		return testEnv.DaemonInfo.Isolation == "hyperv"
	}
	return true
}

func IsolationIs(expectedIsolation string) bool {
	return testEnv.OSType == "windows" && string(testEnv.DaemonInfo.Isolation) == expectedIsolation
}

func IsolationIsHyperv() bool {
	return IsolationIs("hyperv")
}

func IsolationIsProcess() bool {
	return IsolationIs("process")
}

// RegistryHosting returns whether the host can host a registry (v2) or not
func RegistryHosting() bool {
	// for now registry binary is built only if we're running inside
	// container through `make test`. Figure that out by testing if
	// registry binary is in PATH.
	_, err := exec.LookPath(registry.V2binary)
	return err == nil
}

func SwarmInactive() bool {
	return testEnv.DaemonInfo.Swarm.LocalNodeState == swarm.LocalNodeStateInactive
}

func TODOBuildkit() bool {
	return os.Getenv("DOCKER_BUILDKIT") == ""
}

// testRequires checks if the environment satisfies the requirements
// for the test to run or skips the tests.
func testRequires(c interface{}, requirements ...requirement.Test) {
	requirement.Is(c.(requirement.SkipT), requirements...)
}
