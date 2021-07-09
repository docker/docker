package oci // import "github.com/docker/docker/oci"

import (
	"os"
	"runtime"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func iPtr(i int64) *int64        { return &i }
func u32Ptr(i int64) *uint32     { u := uint32(i); return &u }
func fmPtr(i int64) *os.FileMode { fm := os.FileMode(i); return &fm }

// DefaultSpec returns the default spec used by docker for the current Platform
func DefaultSpec() specs.Spec {
	return DefaultOSSpec(runtime.GOOS)
}

// DefaultOSSpec returns the spec for a given OS
func DefaultOSSpec(osName string) specs.Spec {
	if osName == "windows" {
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
	s := specs.Spec{
		Version: specs.Version,
		Process: &specs.Process{},
		Root:    &specs.Root{},
	}
	s.Mounts = []specs.Mount{
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
	}

	s.Linux = &specs.Linux{
		MaskedPaths: []string{
			"/proc/asound",
			"/proc/acpi",
			"/proc/kcore",
			"/proc/keys",
			"/proc/latency_stats",
			"/proc/timer_list",
			"/proc/timer_stats",
			"/proc/sched_debug",
			"/proc/scsi",
			"/sys/firmware",
		},
		ReadonlyPaths: []string{
			"/proc/bus",
			"/proc/fs",
			"/proc/irq",
			"/proc/sys",
			"/proc/sysrq-trigger",
		},
		Namespaces: []specs.LinuxNamespace{
			{Type: "mount"},
			{Type: "network"},
			{Type: "uts"},
			{Type: "pid"},
			{Type: "ipc"},
		},
		// Devices implicitly contains the following devices:
		// null, zero, full, random, urandom, tty, console, and ptmx.
		// ptmx is a bind mount or symlink of the container's ptmx.
		// See also: https://github.com/opencontainers/runtime-spec/blob/master/config-linux.md#default-devices
		Devices: []specs.LinuxDevice{},
		Resources: &specs.LinuxResources{
			Devices: []specs.LinuxDeviceCgroup{},
		},
	}

	// For LCOW support, populate a blank Windows spec
	if runtime.GOOS == "windows" {
		s.Windows = &specs.Windows{}
	}

	return s
}
