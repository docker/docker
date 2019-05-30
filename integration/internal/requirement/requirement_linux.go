package requirement // import "github.com/docker/docker/integration/internal/requirement"

import (
	"os"
	"strings"

	"github.com/docker/docker/pkg/parsers/kernel"
	"gotest.tools/icmd"
)

// CgroupNamespacesEnabled checks if cgroup namespaces are enabled on this host
func CgroupNamespacesEnabled() bool {
	if _, err := os.Stat("/proc/self/ns/cgroup"); os.IsNotExist(err) {
		return false
	}

	return true
}

func overlayFSSupported() bool {
	result := icmd.RunCommand("/bin/sh", "-c", "cat /proc/filesystems")
	if result.Error != nil {
		return false
	}
	return strings.Contains(result.Combined(), "overlay\n")
}

// Overlay2Supported returns true if the current system supports overlay2 as graphdriver
func Overlay2Supported(kernelVersion string) bool {
	if !overlayFSSupported() {
		return false
	}

	daemonV, err := kernel.ParseRelease(kernelVersion)
	if err != nil {
		return false
	}
	requiredV := kernel.VersionInfo{Kernel: 4}
	return kernel.CompareKernelVersion(*daemonV, requiredV) > -1

}
