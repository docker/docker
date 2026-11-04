package oci

import (
	"fmt"
	"os"
	"runtime"
	"sync"

	"github.com/moby/moby/v2/daemon/internal/platform"
	"github.com/moby/moby/v2/daemon/pkg/oci/caps"
	"github.com/opencontainers/runtime-spec/specs-go"
)

func iPtr(i int64) *int64 { return &i }

const defaultUnixPathEnv = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// DefaultPathEnv is unix style list of directories to search for
// executables. Each directory is separated from the next by a colon
// ':' character .
// For Windows containers, an empty string is returned as the default
// path will be set by the container, and Docker has no context of what the
// default path should be.
//
// TODO(thaJeztah) align Windows default with BuildKit; see https://github.com/moby/buildkit/pull/1747
// TODO(thaJeztah) use defaults from containerd (but align it with BuildKit; see https://github.com/moby/buildkit/pull/1747)
func DefaultPathEnv(os string) string {
	if os == "windows" {
		return ""
	}
	return defaultUnixPathEnv
}

// DefaultSpec returns the default spec used by docker for the current Platform
func DefaultSpec() specs.Spec {
	if runtime.GOOS == "windows" {
		return DefaultWindowsSpec()
	}
	return DefaultLinuxSpec()
}

// DefaultWindowsSpec create a default spec for running Windows containers
func DefaultWindowsSpec() specs.Spec {
	return specs.Spec{
		Version: specs.Version,
		Windows: &specs.Windows{},
		Process: &specs.Process{},
		Root:    &specs.Root{},
	}
}

// DefaultLinuxSpec create a default spec for running Linux containers
func DefaultLinuxSpec() specs.Spec {
	return specs.Spec{
		Version: specs.Version,
		Process: &specs.Process{
			Capabilities: &specs.LinuxCapabilities{
				Bounding:  caps.DefaultCapabilities(),
				Permitted: caps.DefaultCapabilities(),
				Effective: caps.DefaultCapabilities(),
			},
		},
		Root: &specs.Root{},
		Mounts: []specs.Mount{
			{
				Destination: "/proc",
				Type:        "proc",
				Source:      "proc",
				Options:     []string{"nosuid", "noexec", "nodev"},
			},
			{
				Destination: "/dev",
				Type:        "tmpfs",
				Source:      "tmpfs",
				Options:     []string{"nosuid", "strictatime", "mode=755", "size=65536k"},
			},
			{
				Destination: "/dev/pts",
				Type:        "devpts",
				Source:      "devpts",
				Options:     []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"},
			},
			{
				Destination: "/sys",
				Type:        "sysfs",
				Source:      "sysfs",
				Options:     []string{"nosuid", "noexec", "nodev", "ro"},
			},
			{
				Destination: "/sys/fs/cgroup",
				Type:        "cgroup",
				Source:      "cgroup",
				Options:     []string{"ro", "nosuid", "noexec", "nodev"},
			},
			{
				Destination: "/dev/mqueue",
				Type:        "mqueue",
				Source:      "mqueue",
				Options:     []string{"nosuid", "noexec", "nodev"},
			},
			{
				Destination: "/dev/shm",
				Type:        "tmpfs",
				Source:      "shm",
				Options:     []string{"nosuid", "noexec", "nodev", "mode=1777"},
			},
			{
				Destination: "/run",
				Type:        "tmpfs",
				Source:      "tmpfs",
				Options:     []string{"nosuid", "strictatime", "mode=755", "size=65536k"},
			},
		},
		Linux: &specs.Linux{
			MaskedPaths: defaultLinuxMaskedPaths(),
			ReadonlyPaths: []string{
				"/proc/bus",
				"/proc/fs",
				"/proc/irq",
				"/proc/sys",
				"/proc/sysrq-trigger",
			},
			Namespaces: []specs.LinuxNamespace{
				{Type: specs.MountNamespace},
				{Type: specs.NetworkNamespace},
				{Type: specs.TimeNamespace},
				{Type: specs.UTSNamespace},
				{Type: specs.PIDNamespace},
				{Type: specs.IPCNamespace},
			},
			// Devices implicitly contains the following devices:
			// null, zero, full, random, urandom, tty, console, and ptmx.
			// ptmx is a bind mount or symlink of the container's ptmx.
			// See also: https://github.com/opencontainers/runtime-spec/blob/master/config-linux.md#default-devices
			Devices: []specs.LinuxDevice{},
			Resources: &specs.LinuxResources{
				Devices: []specs.LinuxDeviceCgroup{
					{
						Allow:  false,
						Access: "rwm",
					},
					{
						Allow:  true,
						Type:   "c",
						Major:  iPtr(1),
						Minor:  iPtr(5),
						Access: "rwm",
					},
					{
						Allow:  true,
						Type:   "c",
						Major:  iPtr(1),
						Minor:  iPtr(3),
						Access: "rwm",
					},
					{
						Allow:  true,
						Type:   "c",
						Major:  iPtr(1),
						Minor:  iPtr(9),
						Access: "rwm",
					},
					{
						Allow:  true,
						Type:   "c",
						Major:  iPtr(1),
						Minor:  iPtr(8),
						Access: "rwm",
					},
					{
						Allow:  true,
						Type:   "c",
						Major:  iPtr(5),
						Minor:  iPtr(0),
						Access: "rwm",
					},
					{
						Allow:  true,
						Type:   "c",
						Major:  iPtr(5),
						Minor:  iPtr(1),
						Access: "rwm",
					},
					{
						Allow:  false,
						Type:   "c",
						Major:  iPtr(10),
						Minor:  iPtr(229),
						Access: "rwm",
					},
				},
			},
		},
	}
}

// defaultLinuxMaskedPaths returns the default list of paths to mask in a Linux
// container. The paths won't change while the docker daemon is running, so just
// compute them once.
var defaultLinuxMaskedPaths = sync.OnceValue(func() []string {
	maskedPaths := []string{
		"/proc/acpi",
		"/proc/asound",
		"/proc/interrupts", // https://github.com/moby/moby/security/advisories/GHSA-6fw5-f8r9-fgfm
		"/proc/kcore",
		"/proc/keys",
		"/proc/latency_stats",
		"/proc/sched_debug",
		"/proc/scsi",
		"/proc/timer_list",
		"/proc/timer_stats",
		"/sys/devices/virtual/powercap", // https://github.com/moby/moby/security/advisories/GHSA-jq35-85cj-fj4p
		"/sys/firmware",
	}

	// https://github.com/moby/moby/security/advisories/GHSA-6fw5-f8r9-fgfm
	cpus := platform.PossibleCPU()
	for _, cpu := range cpus {
		path := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/thermal_throttle", cpu)
		if _, err := os.Stat(path); err == nil {
			maskedPaths = append(maskedPaths, path)
		}
	}
	return maskedPaths
})
