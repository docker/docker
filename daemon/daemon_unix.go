//go:build linux || freebsd

package daemon

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/cgroups/v3"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/moby/moby/api/types/blkiodev"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/initlayer"
	"github.com/moby/moby/v2/daemon/internal/ocihook"
	"github.com/moby/moby/v2/daemon/internal/otelutil"
	"github.com/moby/moby/v2/daemon/internal/usergroup"
	"github.com/moby/moby/v2/daemon/libnetwork"
	nwconfig "github.com/moby/moby/v2/daemon/libnetwork/config"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge"
	"github.com/moby/moby/v2/daemon/libnetwork/netlabel"
	"github.com/moby/moby/v2/daemon/libnetwork/nlwrap"
	lntypes "github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/moby/moby/v2/daemon/pkg/opts"
	volumemounts "github.com/moby/moby/v2/daemon/volume/mounts"
	"github.com/moby/moby/v2/errdefs"
	"github.com/moby/moby/v2/pkg/sysinfo"
	"github.com/moby/sys/mount"
	"github.com/moby/sys/user"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/selinux/go-selinux"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
	"go.opentelemetry.io/otel/baggage"
	"golang.org/x/sys/unix"
)

const (
	// These values were used to adjust the CPU-shares for older API versions,
	// but were not used for validation.
	//
	// TODO(thaJeztah): validate min/max values for CPU-shares, similar to Windows: https://github.com/moby/moby/issues/47340
	// https://github.com/moby/moby/blob/27e85c7b6885c2d21ae90791136d9aba78b83d01/daemon/daemon_windows.go#L97-L99
	//
	// See https://git.kernel.org/cgit/linux/kernel/git/tip/tip.git/tree/kernel/sched/sched.h?id=8cd9234c64c584432f6992fe944ca9e46ca8ea76#n269
	// linuxMinCPUShares = 2
	// linuxMaxCPUShares = 262144

	// It's not kernel limit, we want this 6M limit to account for overhead during startup, and to supply a reasonable functional container
	linuxMinMemory = 6291456
	// constants for remapped root settings
	defaultIDSpecifier = "default"
	defaultRemappedID  = "dockremap"

	// constant for cgroup drivers
	cgroupFsDriver      = "cgroupfs"
	cgroupSystemdDriver = "systemd"
	cgroupNoneDriver    = "none"
)

type containerGetter interface {
	GetContainer(string) (*container.Container, error)
}

func getMemoryResources(config containertypes.Resources) *specs.LinuxMemory {
	memory := specs.LinuxMemory{}

	if config.Memory > 0 {
		memory.Limit = &config.Memory
	}

	if config.MemoryReservation > 0 {
		memory.Reservation = &config.MemoryReservation
	}

	if config.MemorySwap > 0 {
		memory.Swap = &config.MemorySwap
	}

	if config.MemorySwappiness != nil {
		swappiness := uint64(*config.MemorySwappiness)
		memory.Swappiness = &swappiness
	}

	if config.OomKillDisable != nil {
		memory.DisableOOMKiller = config.OomKillDisable
	}

	if memory != (specs.LinuxMemory{}) {
		return &memory
	}
	return nil
}

func getPidsLimit(config containertypes.Resources) *specs.LinuxPids {
	if config.PidsLimit == nil {
		return nil
	}
	if *config.PidsLimit <= 0 {
		// docker API allows 0 and negative values to unset this to be consistent
		// with default values. When updating values, runc requires -1 to unset
		// the previous limit.
		val := int64(-1)
		return &specs.LinuxPids{Limit: &val}
	}
	return &specs.LinuxPids{Limit: config.PidsLimit}
}

func getCPUResources(config containertypes.Resources) (*specs.LinuxCPU, error) {
	cpu := specs.LinuxCPU{}

	if config.CPUShares != 0 {
		if config.CPUShares < 0 {
			return nil, fmt.Errorf("invalid CPU shares (%d): value must be a positive integer", config.CPUShares)
		}
		shares := uint64(config.CPUShares)
		cpu.Shares = &shares
	}

	if config.CpusetCpus != "" {
		cpu.Cpus = config.CpusetCpus
	}

	if config.CpusetMems != "" {
		cpu.Mems = config.CpusetMems
	}

	if config.NanoCPUs > 0 {
		// https://www.kernel.org/doc/Documentation/scheduler/sched-bwc.txt
		period := uint64(100 * time.Millisecond / time.Microsecond)
		quota := config.NanoCPUs * int64(period) / 1e9
		cpu.Period = &period
		cpu.Quota = &quota
	}

	if config.CPUPeriod != 0 {
		period := uint64(config.CPUPeriod)
		cpu.Period = &period
	}

	if config.CPUQuota != 0 {
		q := config.CPUQuota
		cpu.Quota = &q
	}

	if config.CPURealtimePeriod != 0 {
		period := uint64(config.CPURealtimePeriod)
		cpu.RealtimePeriod = &period
	}

	if config.CPURealtimeRuntime != 0 {
		c := config.CPURealtimeRuntime
		cpu.RealtimeRuntime = &c
	}

	if cpu != (specs.LinuxCPU{}) {
		return &cpu, nil
	}
	return nil, nil
}

func getBlkioWeightDevices(config containertypes.Resources) ([]specs.LinuxWeightDevice, error) {
	var stat unix.Stat_t
	var blkioWeightDevices []specs.LinuxWeightDevice

	for _, weightDevice := range config.BlkioWeightDevice {
		if err := unix.Stat(weightDevice.Path, &stat); err != nil {
			return nil, errors.WithStack(&os.PathError{Op: "stat", Path: weightDevice.Path, Err: err})
		}
		weight := weightDevice.Weight
		d := specs.LinuxWeightDevice{Weight: &weight}
		// The type is 32bit on mips.
		d.Major = int64(unix.Major(uint64(stat.Rdev))) //nolint: unconvert
		d.Minor = int64(unix.Minor(uint64(stat.Rdev))) //nolint: unconvert
		blkioWeightDevices = append(blkioWeightDevices, d)
	}

	return blkioWeightDevices, nil
}

func (daemon *Daemon) parseSecurityOpt(cfg *config.Config, securityOptions *container.SecurityOptions, hostConfig *containertypes.HostConfig) error {
	securityOptions.NoNewPrivileges = cfg.NoNewPrivileges
	return parseSecurityOpt(securityOptions, hostConfig)
}

func parseSecurityOpt(securityOptions *container.SecurityOptions, config *containertypes.HostConfig) error {
	var (
		labelOpts []string
		err       error
	)

	for _, opt := range config.SecurityOpt {
		if opt == "no-new-privileges" {
			securityOptions.NoNewPrivileges = true
			continue
		}
		if opt == "writable-cgroups" {
			trueVal := true
			securityOptions.WritableCgroups = &trueVal
			continue
		}
		if opt == "disable" {
			labelOpts = append(labelOpts, "disable")
			continue
		}

		var k, v string
		var ok bool
		if strings.Contains(opt, "=") {
			k, v, ok = strings.Cut(opt, "=")
		} else if strings.Contains(opt, ":") {
			k, v, ok = strings.Cut(opt, ":")
			log.G(context.TODO()).Warn("Security options with `:` as a separator are deprecated and will be completely unsupported in 17.04, use `=` instead.")
		}
		if !ok {
			return fmt.Errorf("invalid --security-opt 1: %q", opt)
		}

		switch k {
		case "label":
			labelOpts = append(labelOpts, v)
		case "apparmor":
			securityOptions.AppArmorProfile = v
		case "seccomp":
			securityOptions.SeccompProfile = v
		case "no-new-privileges":
			nnp, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("invalid --security-opt 2: %q", opt)
			}
			securityOptions.NoNewPrivileges = nnp
		case "writable-cgroups":
			writableCgroups, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("invalid --security-opt 2: %q", opt)
			}
			securityOptions.WritableCgroups = &writableCgroups
		default:
			return fmt.Errorf("invalid --security-opt 2: %q", opt)
		}
	}

	securityOptions.ProcessLabel, securityOptions.MountLabel, err = label.InitLabels(labelOpts)
	return err
}

func getBlkioThrottleDevices(devs []*blkiodev.ThrottleDevice) ([]specs.LinuxThrottleDevice, error) {
	var throttleDevices []specs.LinuxThrottleDevice
	var stat unix.Stat_t

	for _, d := range devs {
		if err := unix.Stat(d.Path, &stat); err != nil {
			return nil, errors.WithStack(&os.PathError{Op: "stat", Path: d.Path, Err: err})
		}
		d := specs.LinuxThrottleDevice{Rate: d.Rate}
		// the type is 32bit on mips
		d.Major = int64(unix.Major(uint64(stat.Rdev))) //nolint: unconvert
		d.Minor = int64(unix.Minor(uint64(stat.Rdev))) //nolint: unconvert
		throttleDevices = append(throttleDevices, d)
	}

	return throttleDevices, nil
}

// adjustParallelLimit takes a number of objects and a proposed limit and
// figures out if it's reasonable (and adjusts it accordingly). This is only
// used for daemon startup, which does a lot of parallel loading of containers
// (and if we exceed RLIMIT_NOFILE then we're in trouble).
func adjustParallelLimit(n int, limit int) int {
	// Rule-of-thumb overhead factor (how many files will each goroutine open
	// simultaneously). Yes, this is ugly but to be frank this whole thing is
	// ugly.
	const overhead = 2

	// On Linux, we need to ensure that parallelStartupJobs doesn't cause us to
	// exceed RLIMIT_NOFILE. If parallelStartupJobs is too large, we reduce it
	// and give a warning (since in theory the user should increase their
	// ulimits to the largest possible value for dockerd).
	var rlim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &rlim); err != nil {
		log.G(context.TODO()).Warnf("Couldn't find dockerd's RLIMIT_NOFILE to double-check startup parallelism factor: %v", err)
		return limit
	}
	softRlimit := int(rlim.Cur)

	// Much fewer containers than RLIMIT_NOFILE. No need to adjust anything.
	if softRlimit > overhead*n {
		return limit
	}

	// RLIMIT_NOFILE big enough, no need to adjust anything.
	if softRlimit > overhead*limit {
		return limit
	}

	log.G(context.TODO()).Warnf("Found dockerd's open file ulimit (%v) is far too small -- consider increasing it significantly (at least %v)", softRlimit, overhead*limit)
	return softRlimit / overhead
}

// adaptContainerSettings is called during container creation to modify any
// settings necessary in the HostConfig structure.
func (daemon *Daemon) adaptContainerSettings(daemonCfg *config.Config, hostConfig *containertypes.HostConfig) error {
	if hostConfig.Memory > 0 && hostConfig.MemorySwap == 0 {
		// By default, MemorySwap is set to twice the size of Memory.
		hostConfig.MemorySwap = hostConfig.Memory * 2
	}
	if hostConfig.ShmSize == 0 {
		hostConfig.ShmSize = config.DefaultShmSize
		if daemonCfg != nil {
			hostConfig.ShmSize = int64(daemonCfg.ShmSize)
		}
	}
	// Set default IPC mode, if unset for container
	if hostConfig.IpcMode.IsEmpty() {
		m := config.DefaultIpcMode
		if daemonCfg != nil {
			m = containertypes.IpcMode(daemonCfg.IpcMode)
		}
		hostConfig.IpcMode = m
	}

	// Set default cgroup namespace mode, if unset for container
	if hostConfig.CgroupnsMode.IsEmpty() {
		// for cgroup v2: unshare cgroupns even for privileged containers
		// https://github.com/containers/libpod/pull/4374#issuecomment-549776387
		if hostConfig.Privileged && cgroups.Mode() != cgroups.Unified {
			hostConfig.CgroupnsMode = containertypes.CgroupnsModeHost
		} else {
			m := containertypes.CgroupnsModeHost
			if cgroups.Mode() == cgroups.Unified {
				m = containertypes.CgroupnsModePrivate
			}
			if daemonCfg != nil {
				m = containertypes.CgroupnsMode(daemonCfg.CgroupNamespaceMode)
			}
			hostConfig.CgroupnsMode = m
		}
	}

	adaptSharedNamespaceContainer(daemon, hostConfig)

	var err error
	secOpts, err := daemon.generateSecurityOpt(hostConfig)
	if err != nil {
		return err
	}
	hostConfig.SecurityOpt = append(hostConfig.SecurityOpt, secOpts...)
	if hostConfig.OomKillDisable == nil {
		defaultOomKillDisable := false
		hostConfig.OomKillDisable = &defaultOomKillDisable
	}

	return nil
}

// adaptSharedNamespaceContainer replaces container name with its ID in hostConfig.
// To be more precisely, it modifies `container:name` to `container:ID` of PidMode, IpcMode
// and NetworkMode.
//
// When a container shares its namespace with another container, use ID can keep the namespace
// sharing connection between the two containers even the another container is renamed.
func adaptSharedNamespaceContainer(daemon containerGetter, hostConfig *containertypes.HostConfig) {
	containerPrefix := "container:"
	if hostConfig.PidMode.IsContainer() {
		pidContainer := hostConfig.PidMode.Container()
		// if there is any error returned here, we just ignore it and leave it to be
		// handled in the following logic
		if c, err := daemon.GetContainer(pidContainer); err == nil {
			hostConfig.PidMode = containertypes.PidMode(containerPrefix + c.ID)
		}
	}
	if hostConfig.IpcMode.IsContainer() {
		ipcContainer := hostConfig.IpcMode.Container()
		if c, err := daemon.GetContainer(ipcContainer); err == nil {
			hostConfig.IpcMode = containertypes.IpcMode(containerPrefix + c.ID)
		}
	}
	if hostConfig.NetworkMode.IsContainer() {
		netContainer := hostConfig.NetworkMode.ConnectedContainer()
		if c, err := daemon.GetContainer(netContainer); err == nil {
			hostConfig.NetworkMode = containertypes.NetworkMode(containerPrefix + c.ID)
		}
	}
}

// verifyPlatformContainerResources performs platform-specific validation of the container's resource-configuration
func verifyPlatformContainerResources(resources *containertypes.Resources, sysInfo *sysinfo.SysInfo, update bool) (warnings []string, _ error) {
	fixMemorySwappiness(resources)

	// memory subsystem checks and adjustments
	if resources.Memory != 0 && resources.Memory < linuxMinMemory {
		return warnings, errors.New("Minimum memory limit allowed is 6MB")
	}
	if resources.Memory > 0 && !sysInfo.MemoryLimit {
		warnings = append(warnings, "Your kernel does not support memory limit capabilities or the cgroup is not mounted. Limitation discarded.")
		resources.Memory = 0
		resources.MemorySwap = -1
	}
	if resources.Memory > 0 && resources.MemorySwap != -1 && !sysInfo.SwapLimit {
		warnings = append(warnings, "Your kernel does not support swap limit capabilities or the cgroup is not mounted. Memory limited without swap.")
		resources.MemorySwap = -1
	}
	if resources.Memory > 0 && resources.MemorySwap > 0 && resources.MemorySwap < resources.Memory {
		return warnings, errors.New("Minimum memoryswap limit should be larger than memory limit, see usage")
	}
	if resources.Memory == 0 && resources.MemorySwap > 0 && !update {
		return warnings, errors.New("You should always set the Memory limit when using Memoryswap limit, see usage")
	}
	if resources.MemorySwappiness != nil && !sysInfo.MemorySwappiness {
		warnings = append(warnings, "Your kernel does not support memory swappiness capabilities or the cgroup is not mounted. Memory swappiness discarded.")
		resources.MemorySwappiness = nil
	}
	if resources.MemorySwappiness != nil {
		swappiness := *resources.MemorySwappiness
		if swappiness < 0 || swappiness > 100 {
			return warnings, fmt.Errorf("Invalid value: %v, valid memory swappiness range is 0-100", swappiness)
		}
	}
	if resources.MemoryReservation > 0 && !sysInfo.MemoryReservation {
		warnings = append(warnings, "Your kernel does not support memory soft limit capabilities or the cgroup is not mounted. Limitation discarded.")
		resources.MemoryReservation = 0
	}
	if resources.MemoryReservation > 0 && resources.MemoryReservation < linuxMinMemory {
		return warnings, errors.New("Minimum memory reservation allowed is 6MB")
	}
	if resources.Memory > 0 && resources.MemoryReservation > 0 && resources.Memory < resources.MemoryReservation {
		return warnings, errors.New("Minimum memory limit can not be less than memory reservation limit, see usage")
	}
	if resources.OomKillDisable != nil && !sysInfo.OomKillDisable {
		// only produce warnings if the setting wasn't to *disable* the OOM Kill; no point
		// warning the caller if they already wanted the feature to be off
		if *resources.OomKillDisable {
			warnings = append(warnings, "Your kernel does not support OomKillDisable. OomKillDisable discarded.")
		}
		resources.OomKillDisable = nil
	}
	if resources.OomKillDisable != nil && *resources.OomKillDisable && resources.Memory == 0 {
		warnings = append(warnings, "OOM killer is disabled for the container, but no memory limit is set, this can result in the system running out of resources.")
	}
	if resources.PidsLimit != nil && !sysInfo.PidsLimit {
		if *resources.PidsLimit > 0 {
			warnings = append(warnings, "Your kernel does not support PIDs limit capabilities or the cgroup is not mounted. PIDs limit discarded.")
		}
		resources.PidsLimit = nil
	}

	// cpu subsystem checks and adjustments
	if resources.NanoCPUs > 0 && resources.CPUPeriod > 0 {
		return warnings, errors.New("Conflicting options: Nano CPUs and CPU Period cannot both be set")
	}
	if resources.NanoCPUs > 0 && resources.CPUQuota > 0 {
		return warnings, errors.New("Conflicting options: Nano CPUs and CPU Quota cannot both be set")
	}
	if resources.NanoCPUs > 0 && !sysInfo.CPUCfs {
		return warnings, errors.New("NanoCPUs can not be set, as your kernel does not support CPU CFS scheduler or the cgroup is not mounted")
	}
	// The highest precision we could get on Linux is 0.001, by setting
	//   cpu.cfs_period_us=1000ms
	//   cpu.cfs_quota=1ms
	// See the following link for details:
	// https://www.kernel.org/doc/Documentation/scheduler/sched-bwc.txt
	// Here we don't set the lower limit and it is up to the underlying platform (e.g., Linux) to return an error.
	// The error message is 0.01 so that this is consistent with Windows
	if resources.NanoCPUs != 0 {
		nc := runtime.NumCPU()
		if resources.NanoCPUs < 0 || resources.NanoCPUs > int64(nc)*1e9 {
			return warnings, fmt.Errorf("range of CPUs is from 0.01 to %[1]d.00, as there are only %[1]d CPUs available", nc)
		}
	}

	if resources.CPUShares > 0 && !sysInfo.CPUShares {
		warnings = append(warnings, "Your kernel does not support CPU shares or the cgroup is not mounted. Shares discarded.")
		resources.CPUShares = 0
	}
	if (resources.CPUPeriod != 0 || resources.CPUQuota != 0) && !sysInfo.CPUCfs {
		warnings = append(warnings, "Your kernel does not support CPU CFS scheduler. CPU period/quota discarded.")
		resources.CPUPeriod = 0
		resources.CPUQuota = 0
	}
	if resources.CPUPeriod != 0 && (resources.CPUPeriod < 1000 || resources.CPUPeriod > 1000000) {
		return warnings, errors.New("CPU cfs period can not be less than 1ms (i.e. 1000) or larger than 1s (i.e. 1000000)")
	}
	if resources.CPUQuota > 0 && resources.CPUQuota < 1000 {
		return warnings, errors.New("CPU cfs quota can not be less than 1ms (i.e. 1000)")
	}
	if resources.CPUPercent > 0 {
		warnings = append(warnings, runtime.GOOS+" does not support CPU percent. Percent discarded.")
		resources.CPUPercent = 0
	}

	// cpuset subsystem checks and adjustments
	if (resources.CpusetCpus != "" || resources.CpusetMems != "") && !sysInfo.Cpuset {
		warnings = append(warnings, "Your kernel does not support cpuset or the cgroup is not mounted. Cpuset discarded.")
		resources.CpusetCpus = ""
		resources.CpusetMems = ""
	}
	cpusAvailable, err := sysInfo.IsCpusetCpusAvailable(resources.CpusetCpus)
	if err != nil {
		return warnings, errors.Wrapf(err, "Invalid value %s for cpuset cpus", resources.CpusetCpus)
	}
	if !cpusAvailable {
		return warnings, fmt.Errorf("Requested CPUs are not available - requested %s, available: %s", resources.CpusetCpus, sysInfo.Cpus)
	}
	memsAvailable, err := sysInfo.IsCpusetMemsAvailable(resources.CpusetMems)
	if err != nil {
		return warnings, errors.Wrapf(err, "Invalid value %s for cpuset mems", resources.CpusetMems)
	}
	if !memsAvailable {
		return warnings, fmt.Errorf("Requested memory nodes are not available - requested %s, available: %s", resources.CpusetMems, sysInfo.Mems)
	}

	// blkio subsystem checks and adjustments
	if resources.BlkioWeight > 0 && !sysInfo.BlkioWeight {
		warnings = append(warnings, "Your kernel does not support Block I/O weight or the cgroup is not mounted. Weight discarded.")
		resources.BlkioWeight = 0
	}
	if resources.BlkioWeight > 0 && (resources.BlkioWeight < 10 || resources.BlkioWeight > 1000) {
		return warnings, errors.New("Range of blkio weight is from 10 to 1000")
	}
	if resources.IOMaximumBandwidth != 0 || resources.IOMaximumIOps != 0 {
		return warnings, fmt.Errorf("Invalid QoS settings: %s does not support Maximum IO Bandwidth or Maximum IO IOps", runtime.GOOS)
	}
	if len(resources.BlkioWeightDevice) > 0 && !sysInfo.BlkioWeightDevice {
		warnings = append(warnings, "Your kernel does not support Block I/O weight_device or the cgroup is not mounted. Weight-device discarded.")
		resources.BlkioWeightDevice = []*blkiodev.WeightDevice{}
	}
	if len(resources.BlkioDeviceReadBps) > 0 && !sysInfo.BlkioReadBpsDevice {
		warnings = append(warnings, "Your kernel does not support BPS Block I/O read limit or the cgroup is not mounted. Block I/O BPS read limit discarded.")
		resources.BlkioDeviceReadBps = []*blkiodev.ThrottleDevice{}
	}
	if len(resources.BlkioDeviceWriteBps) > 0 && !sysInfo.BlkioWriteBpsDevice {
		warnings = append(warnings, "Your kernel does not support BPS Block I/O write limit or the cgroup is not mounted. Block I/O BPS write limit discarded.")
		resources.BlkioDeviceWriteBps = []*blkiodev.ThrottleDevice{}
	}
	if len(resources.BlkioDeviceReadIOps) > 0 && !sysInfo.BlkioReadIOpsDevice {
		warnings = append(warnings, "Your kernel does not support IOPS Block read limit or the cgroup is not mounted. Block I/O IOPS read limit discarded.")
		resources.BlkioDeviceReadIOps = []*blkiodev.ThrottleDevice{}
	}
	if len(resources.BlkioDeviceWriteIOps) > 0 && !sysInfo.BlkioWriteIOpsDevice {
		warnings = append(warnings, "Your kernel does not support IOPS Block write limit or the cgroup is not mounted. Block I/O IOPS write limit discarded.")
		resources.BlkioDeviceWriteIOps = []*blkiodev.ThrottleDevice{}
	}

	return warnings, nil
}

func cgroupDriver(cfg *config.Config) string {
	if UsingSystemd(cfg) {
		return cgroupSystemdDriver
	}
	if cfg.Rootless {
		return cgroupNoneDriver
	}
	return cgroupFsDriver
}

// verifyCgroupDriver validates native.cgroupdriver
func verifyCgroupDriver(config *config.Config) error {
	cd, _, err := config.GetExecOpt("native.cgroupdriver")
	if err != nil {
		return err
	}
	switch cd {
	case "", cgroupFsDriver, cgroupSystemdDriver:
		return nil
	case cgroupNoneDriver:
		return fmt.Errorf("native.cgroupdriver option %s is internally used and cannot be specified manually", cd)
	default:
		return fmt.Errorf("native.cgroupdriver option %s not supported", cd)
	}
}

// UsingSystemd returns true if cli option includes native.cgroupdriver=systemd
func UsingSystemd(config *config.Config) bool {
	cd, _, _ := config.GetExecOpt("native.cgroupdriver")

	if cd == cgroupSystemdDriver {
		return true
	}
	// On cgroup v2 hosts, default to systemd driver
	if cd == "" && cgroups.Mode() == cgroups.Unified && isRunningSystemd() {
		return true
	}
	return false
}

var (
	runningSystemd bool
	detectSystemd  sync.Once
)

// isRunningSystemd checks whether the host was booted with systemd as its init
// system. This functions similarly to systemd's `sd_booted(3)`: internally, it
// checks whether /run/systemd/system/ exists and is a directory.
// http://www.freedesktop.org/software/systemd/man/sd_booted.html
//
// NOTE: This function comes from package github.com/coreos/go-systemd/util
// It was borrowed here to avoid a dependency on cgo.
func isRunningSystemd() bool {
	detectSystemd.Do(func() {
		fi, err := os.Lstat("/run/systemd/system")
		if err != nil {
			return
		}
		runningSystemd = fi.IsDir()
	})
	return runningSystemd
}

// maxHostnameLen is the maximum length (in bytes) of a container hostname on
// Linux, as defined by HOST_NAME_MAX (include/uapi/linux/limits.h).
const maxHostnameLen = 64

// verifyPlatformContainerSettings performs platform-specific validation of the
// hostconfig and config structures.
func verifyPlatformContainerSettings(daemon *Daemon, daemonCfg *configStore, hostConfig *containertypes.HostConfig, config *containertypes.Config, update bool) (warnings []string, _ error) {
	if runtime.GOOS == "linux" && config != nil && len(config.Hostname) > maxHostnameLen {
		return warnings, errors.Errorf("hostname %q is too long (maximum %d bytes)", config.Hostname, maxHostnameLen)
	}

	if hostConfig == nil {
		return nil, nil
	}
	sysInfo, err := daemon.RawSysInfo()
	if err != nil {
		return nil, err
	}

	w, err := verifyPlatformContainerResources(&hostConfig.Resources, sysInfo, update)

	// no matter err is nil or not, w could have data in itself.
	warnings = append(warnings, w...)

	if err != nil {
		return warnings, err
	}

	if !hostConfig.IpcMode.Valid() {
		return warnings, errors.Errorf("invalid IPC mode: %v", hostConfig.IpcMode)
	}
	if !hostConfig.PidMode.Valid() {
		return warnings, errors.Errorf("invalid PID mode: %v", hostConfig.PidMode)
	}
	if hostConfig.ShmSize < 0 {
		return warnings, errors.New("SHM size can not be less than 0")
	}
	if !hostConfig.UTSMode.Valid() {
		return warnings, errors.Errorf("invalid UTS mode: %v", hostConfig.UTSMode)
	}

	if hostConfig.OomScoreAdj < -1000 || hostConfig.OomScoreAdj > 1000 {
		return warnings, fmt.Errorf("Invalid value %d, range for oom score adj is [-1000, 1000]", hostConfig.OomScoreAdj)
	}

	// ip-forwarding does not affect container with '--net=host' (or '--net=none')
	if sysInfo.IPv4ForwardingDisabled && (!hostConfig.NetworkMode.IsHost() && !hostConfig.NetworkMode.IsNone()) {
		warnings = append(warnings, "IPv4 forwarding is disabled. Networking will not work.")
	}
	if hostConfig.NetworkMode.IsHost() && len(hostConfig.PortBindings) > 0 {
		warnings = append(warnings, "Published ports are discarded when using host network mode")
	}

	// check for various conflicting options with user namespaces
	if daemonCfg.RemappedRoot != "" && hostConfig.UsernsMode.IsPrivate() {
		if hostConfig.Privileged {
			return warnings, errors.New("privileged mode is incompatible with user namespaces.  You must run the container in the host namespace when running privileged mode")
		}
		if hostConfig.NetworkMode.IsHost() && !hostConfig.UsernsMode.IsHost() {
			return warnings, errors.New("cannot share the host's network namespace when user namespaces are enabled")
		}
		if hostConfig.PidMode.IsHost() && !hostConfig.UsernsMode.IsHost() {
			return warnings, errors.New("cannot share the host PID namespace when user namespaces are enabled")
		}
	}
	if hostConfig.CgroupParent != "" && UsingSystemd(&daemonCfg.Config) {
		// CgroupParent for systemd cgroup should be named as "xxx.slice"
		if len(hostConfig.CgroupParent) <= 6 || !strings.HasSuffix(hostConfig.CgroupParent, ".slice") {
			return warnings, errors.New(`cgroup-parent for systemd cgroup should be a valid slice named as "xxx.slice"`)
		}
	}
	if hostConfig.Runtime == "" {
		hostConfig.Runtime = daemonCfg.Runtimes.Default
	}

	if _, _, err := daemonCfg.Runtimes.Get(hostConfig.Runtime); err != nil {
		return warnings, err
	}

	parser := volumemounts.NewParser()
	for dest := range hostConfig.Tmpfs {
		if err := parser.ValidateTmpfsMountDestination(dest); err != nil {
			return warnings, err
		}
	}

	if !hostConfig.CgroupnsMode.Valid() {
		return warnings, fmt.Errorf("invalid cgroup namespace mode: %v", hostConfig.CgroupnsMode)
	}
	if hostConfig.CgroupnsMode.IsPrivate() {
		if !sysInfo.CgroupNamespaces {
			warnings = append(warnings, "Your kernel does not support cgroup namespaces.  Cgroup namespace setting discarded.")
		}
	}

	return warnings, nil
}

// verifyDaemonSettings performs validation of daemon config struct
func verifyDaemonSettings(conf *config.Config) error {
	if conf.ContainerdNamespace == conf.ContainerdPluginNamespace {
		return errors.New("containers namespace and plugins namespace cannot be the same")
	}
	// Check for mutually incompatible config options
	if conf.BridgeConfig.Iface != "" && conf.BridgeConfig.IP != "" {
		return errors.New("You specified -b & --bip, mutually exclusive options. Please specify only one")
	}
	if conf.BridgeConfig.Iface != "" && conf.BridgeConfig.IP6 != "" {
		return errors.New("You specified -b & --bip6, mutually exclusive options. Please specify only one")
	}
	if !conf.BridgeConfig.InterContainerCommunication {
		if !conf.BridgeConfig.EnableIPTables {
			return errors.New("You specified --iptables=false with --icc=false. ICC=false uses iptables to function. Please set --icc or --iptables to true")
		}
		if conf.BridgeConfig.EnableIPv6 && !conf.BridgeConfig.EnableIP6Tables {
			return errors.New("You specified --ip6tables=false with --icc=false. ICC=false uses ip6tables to function. Please set --icc or --ip6tables to true")
		}
	}
	if !conf.BridgeConfig.EnableIPTables && conf.BridgeConfig.EnableIPMasq {
		conf.BridgeConfig.EnableIPMasq = false
	}
	if err := verifyCgroupDriver(conf); err != nil {
		return err
	}
	if conf.CgroupParent != "" && UsingSystemd(conf) {
		if len(conf.CgroupParent) <= 6 || !strings.HasSuffix(conf.CgroupParent, ".slice") {
			return errors.New(`cgroup-parent for systemd cgroup should be a valid slice named as "xxx.slice"`)
		}
	}

	if conf.Rootless && UsingSystemd(conf) && cgroups.Mode() != cgroups.Unified {
		return errors.New("exec-opt native.cgroupdriver=systemd requires cgroup v2 for rootless mode")
	}
	return nil
}

// checkSystem validates platform-specific requirements
func checkSystem() error {
	return nil
}

// configureMaxThreads sets the Go runtime max threads threshold
// which is 90% of the kernel setting from /proc/sys/kernel/threads-max
func configureMaxThreads(ctx context.Context) error {
	mt, err := os.ReadFile("/proc/sys/kernel/threads-max")
	if err != nil {
		return err
	}
	mtint, err := strconv.Atoi(strings.TrimSpace(string(mt)))
	if err != nil {
		return err
	}
	maxThreads := (mtint / 100) * 90
	debug.SetMaxThreads(maxThreads)
	log.G(ctx).Debugf("Golang's threads limit set to %d", maxThreads)
	return nil
}

func overlaySupportsSelinux() (bool, error) {
	f, err := os.Open("/proc/kallsyms")
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	for s.Scan() {
		if strings.HasSuffix(s.Text(), " security_inode_copy_up") {
			return true, nil
		}
	}

	return false, s.Err()
}

// configureKernelSecuritySupport configures and validates security support for the kernel
func configureKernelSecuritySupport(config *config.Config, driverName string) error {
	if config.EnableSelinuxSupport {
		if !selinux.GetEnabled() {
			log.G(context.TODO()).Warn("Docker could not enable SELinux on the host system")
			return nil
		}

		if driverName == "overlay2" || driverName == "overlayfs" {
			// If driver is overlay2, make sure kernel
			// supports selinux with overlay.
			supported, err := overlaySupportsSelinux()
			if err != nil {
				return err
			}

			if !supported {
				log.G(context.TODO()).Warnf("SELinux is not supported with the %v graph driver on this kernel", driverName)
			}
		}
	} else {
		selinux.SetDisabled()
	}
	return nil
}

// initOCIHooks starts the server for OCI hooks the daemon installs in
// containers it does not start itself.
func (daemon *Daemon) initOCIHooks(cfg *config.Config) error {
	s, err := ocihook.Listen(filepath.Join(cfg.GetExecRoot(), "oci-hook.sock"))
	if err != nil {
		return fmt.Errorf("error starting OCI hook server: %w", err)
	}
	daemon.ociHooks = s
	return nil
}

// initNetworkController initializes the libnetwork controller and configures
// network settings. If there's active sandboxes, configuration changes will not
// take effect.
func (daemon *Daemon) initNetworkController(cfg *config.Config, activeSandboxes map[string]any) error {
	netOptions, err := daemon.networkOptions(cfg, daemon.PluginStore, daemon.id, activeSandboxes)
	if err != nil {
		return err
	}

	ctx := baggage.ContextWithBaggage(context.TODO(), otelutil.MustNewBaggage(
		otelutil.MustNewMemberRaw(otelutil.TriggerKey, "daemon.initNetworkController"),
	))
	if err := daemon.newNetworkController(ctx, cfg, netOptions); err != nil {
		return fmt.Errorf("error obtaining controller instance: %v", err)
	}

	if len(activeSandboxes) > 0 {
		log.G(ctx).Info("there are running containers, updated network configuration will not take affect")
	} else if err := daemon.runInNetNS(func() error {
		return configureNetworking(ctx, daemon.netController, cfg)
	}); err != nil {
		return err
	}

	// Set HostGatewayIP to the default bridge's IP if it is empty
	setHostGatewayIP(daemon.netController, cfg)
	return nil
}

func configureNetworking(ctx context.Context, controller *libnetwork.Controller, conf *config.Config) error {
	// Create predefined network "none"
	if n, _ := controller.NetworkByName(network.NetworkNone); n == nil {
		if _, err := controller.NewNetwork(ctx, "null", network.NetworkNone, "", libnetwork.NetworkOptionPersist(true)); err != nil {
			return errors.Wrapf(err, `error creating default %q network`, network.NetworkNone)
		}
	}

	// Create predefined network "host"
	if n, _ := controller.NetworkByName(network.NetworkHost); n == nil {
		if _, err := controller.NewNetwork(ctx, "host", network.NetworkHost, "", libnetwork.NetworkOptionPersist(true)); err != nil {
			return errors.Wrapf(err, `error creating default %q network`, network.NetworkHost)
		}
	}

	// Clear stale bridge network
	if n, err := controller.NetworkByName(network.NetworkBridge); err == nil {
		if err = n.Delete(); err != nil {
			return errors.Wrapf(err, `could not delete the default %q network`, network.NetworkBridge)
		}
		if len(conf.NetworkConfig.DefaultAddressPools.Value()) > 0 && !conf.LiveRestoreEnabled {
			removeDefaultBridgeInterface()
		}
	}

	if !conf.DisableBridge {
		// Initialize default driver "bridge"
		if err := initBridgeDriver(ctx, controller, conf.BridgeConfig); err != nil {
			return err
		}
	} else {
		removeDefaultBridgeInterface()
	}

	return nil
}

// setHostGatewayIP sets cfg.HostGatewayIP to the default bridge's IP if it is empty.
func setHostGatewayIP(controller *libnetwork.Controller, config *config.Config) {
	if len(config.HostGatewayIPs) > 0 {
		return
	}
	if n, err := controller.NetworkByName(network.NetworkBridge); err == nil {
		v4Info, v6Info := n.IpamInfo()
		if len(v4Info) > 0 {
			addr, _ := netip.AddrFromSlice(v4Info[0].Gateway.IP)
			config.HostGatewayIPs = append(config.HostGatewayIPs, addr.Unmap())
		}
		if len(v6Info) > 0 {
			addr, _ := netip.AddrFromSlice(v6Info[0].Gateway.IP)
			config.HostGatewayIPs = append(config.HostGatewayIPs, addr)
		}
	}
}

// networkPlatformOptions returns a slice of platform-specific libnetwork
// options.
func networkPlatformOptions(conf *config.Config) []nwconfig.Option {
	return []nwconfig.Option{
		nwconfig.OptionNetnsDir(filepath.Join(conf.GetExecRoot(), "netns")),
		nwconfig.OptionRootless(conf.Rootless),
		nwconfig.OptionUserlandProxy(conf.EnableUserlandProxy, conf.UserlandProxyPath),
		nwconfig.OptionBridgeConfig(bridge.Configuration{
			EnableIPForwarding:       conf.BridgeConfig.EnableIPForward,
			DisableFilterForwardDrop: conf.BridgeConfig.DisableFilterForwardDrop,
			EnableIPTables:           conf.BridgeConfig.EnableIPTables,
			EnableIP6Tables:          conf.BridgeConfig.EnableIP6Tables,
			EnableProxy:              conf.EnableUserlandProxy && conf.UserlandProxyPath != "",
			ProxyPath:                conf.UserlandProxyPath,
			AllowDirectRouting:       conf.BridgeConfig.AllowDirectRouting,
			AcceptFwMark:             conf.BridgeConfig.BridgeAcceptFwMark,
		}),
	}
}

type defBrOptsV4 struct {
	cfg config.BridgeConfig
}

func (o defBrOptsV4) nlFamily() int {
	return netlink.FAMILY_V4
}

func (o defBrOptsV4) fixedCIDR() (fCIDR, optName string) {
	return o.cfg.FixedCIDR, "fixed-cidr"
}

func (o defBrOptsV4) bip() (bip, optName string) {
	return o.cfg.IP, "bip"
}

func (o defBrOptsV4) defGw() (gw net.IP, optName, auxAddrLabel string) {
	return o.cfg.DefaultGatewayIPv4, "default-gateway", bridge.DefaultGatewayV4AuxKey
}

type defBrOptsV6 struct {
	cfg config.BridgeConfig
}

func (o defBrOptsV6) nlFamily() int {
	return netlink.FAMILY_V6
}

func (o defBrOptsV6) fixedCIDR() (fCIDR, optName string) {
	return o.cfg.FixedCIDRv6, "fixed-cidr-v6"
}

func (o defBrOptsV6) bip() (bip, optName string) {
	return o.cfg.IP6, "bip6"
}

func (o defBrOptsV6) defGw() (gw net.IP, optName, auxAddrLabel string) {
	return o.cfg.DefaultGatewayIPv6, "default-gateway-v6", bridge.DefaultGatewayV6AuxKey
}

type defBrOpts interface {
	nlFamily() int
	fixedCIDR() (fCIDR, optName string)
	bip() (bip, optName string)
	defGw() (gw net.IP, optName, auxAddrLabel string)
}

func initBridgeDriver(ctx context.Context, controller *libnetwork.Controller, cfg config.BridgeConfig) error {
	bridgeName, userManagedBridge := getDefaultBridgeName(cfg)
	netOption := map[string]string{
		bridge.BridgeName:         bridgeName,
		bridge.DefaultBridge:      strconv.FormatBool(true),
		netlabel.DriverMTU:        strconv.Itoa(cfg.MTU),
		bridge.EnableIPMasquerade: strconv.FormatBool(cfg.EnableIPMasq),
		bridge.EnableICC:          strconv.FormatBool(cfg.InterContainerCommunication),
	}
	// --ip processing
	if cfg.DefaultIP != nil {
		netOption[bridge.DefaultBindingIP] = cfg.DefaultIP.String()
	}

	ipamV4Conf, err := getDefaultBridgeIPAMConf(bridgeName, userManagedBridge, defBrOptsV4{cfg})
	if err != nil {
		return err
	}

	var ipamV6Conf []*libnetwork.IpamConf
	if cfg.EnableIPv6 {
		ipamV6Conf, err = getDefaultBridgeIPAMConf(bridgeName, userManagedBridge, defBrOptsV6{cfg})
		if err != nil {
			return err
		}
	}

	// Initialize default network on "bridge" with the same name
	_, err = controller.NewNetwork(ctx, "bridge", network.NetworkBridge, "",
		libnetwork.NetworkOptionEnableIPv4(true),
		libnetwork.NetworkOptionEnableIPv6(cfg.EnableIPv6),
		libnetwork.NetworkOptionDriverOpts(netOption),
		libnetwork.NetworkOptionIpam("default", "", ipamV4Conf, ipamV6Conf, nil),
	)
	if err != nil {
		return fmt.Errorf(`error creating default %q network: %v`, network.NetworkBridge, err)
	}
	return nil
}

func getDefaultBridgeName(cfg config.BridgeConfig) (bridgeName string, userManagedBridge bool) {
	// cfg.Iface is --bridge, the option to supply a user-managed bridge.
	if cfg.Iface != "" {
		// The default network will use a user-managed bridge, the daemon will not
		// create it, and it is not possible to supply an address using --bip.
		return cfg.Iface, true
	}
	return bridge.DefaultBridgeName, false
}

// getDefaultBridgeIPAMConf works out IPAM configuration for the
// default bridge, for the given address family (netlink.FAMILY_V4 or
// netlink.FAMILY_V6).
//
// Inputs are:
// - bip
//   - CIDR, not a plain IP address.
//   - docker-managed bridge only (docker0), not allowed with a user-managed
//     bridge (--bridge) where the user is responsible for creating the bridge
//     and configuring its addresses.
//   - determines the network subnet (ipamConf.PreferredPool) and becomes the
//     bridge/gateway address (ipamConf.Gateway).
//
// - existing bridge addresses
//   - for a user-managed bridge
//   - an address is selected from the bridge to perform the same role as
//     bip for a daemon-managed bridge.
//   - for docker0
//   - if there's an address on the bridge that's compatible with the other
//     options, it's used as the gateway address and - because it's always
//     worked this way, to determine the subnet if it's bigger than the
//     sub-pool configured through fixed-cidr[-v6]. For example, if the
//     bridge has address 10.11.12.13/16 and fixed-cidr=10.11.22.0/24, the
//     default bridge network's subnet is 10.11.0.0/16, sub-pool for
//     automatic address allocation 10.11.22.0/24, gateway 10.11.12.13.
//
// - fixed-cidr/fixed-cidr-v6
//   - ipamConf.SubPool, the pool for automatic address allocation (somewhat
//     equivalent to --ip-range for a user-defined network), must be contained
//     within the subnet. Used as ipamConf.PreferredPool if it's not given a
//     value by other rules.
//
// So, for example, with this config (taken from docs):
//
//	"bip": "192.168.1.1/24",
//	"fixed-cidr": "192.168.1.0/25",
//
// - the bridge's address is "192.168.1.1/24"
// - the subnet is "192.168.1.0/24"
// - the bridge driver can allocate addresses from "192.168.1.0/25"
//
// The result is the same if "bip" is unset (including for a user-managed
// bridge), when the bridge already has address "192.168.1.1/24".
//
// Note that this function logs-then-ignores invalid configuration, because it
// has to tolerate existing configuration - raising an error prevents daemon
// startup. Earlier versions of the daemon didn't spot bad config, but generally
// did something unsurprising with it.
func getDefaultBridgeIPAMConf(
	bridgeName string,
	userManagedBridge bool,
	opts defBrOpts,
) ([]*libnetwork.IpamConf, error) {
	var (
		fCidrIP, bIP       net.IP
		fCidrIPNet, bIPNet *net.IPNet
		err                error
	)

	if fixedCIDR, fixedCIDROpt := opts.fixedCIDR(); fixedCIDR != "" {
		if fCidrIP, fCidrIPNet, err = net.ParseCIDR(fixedCIDR); err != nil {
			return nil, errors.Wrap(err, "parse "+fixedCIDROpt+" failed")
		}
	}

	if cfgBIP, cfgBIPOpt := opts.bip(); cfgBIP != "" {
		if bIP, bIPNet, err = net.ParseCIDR(cfgBIP); err != nil {
			return nil, errors.Wrap(err, "parse "+cfgBIPOpt+" failed")
		}
	} else {
		if bIP, bIPNet, err = selectBIP(userManagedBridge, bridgeName, opts.nlFamily(), fCidrIP, fCidrIPNet); err != nil {
			return nil, err
		}
	}

	ipamConf := &libnetwork.IpamConf{AuxAddresses: make(map[string]string)}
	if bIP != nil {
		ipamConf.PreferredPool = bIPNet.String()
		ipamConf.Gateway = bIP.String()
	} else if !userManagedBridge && ipamConf.PreferredPool != "" {
		_, bipOptName := opts.bip()
		log.G(context.TODO()).Infof("Default bridge (%s) is assigned with an IP address %s. Daemon option --"+bipOptName+" can be used to set a preferred IP address", bridgeName, ipamConf.PreferredPool)
	}

	if fCidrIP != nil && fCidrIPNet != nil {
		ipamConf.SubPool = fCidrIPNet.String()
		if ipamConf.PreferredPool == "" {
			ipamConf.PreferredPool = fCidrIPNet.String()
		} else if userManagedBridge && bIPNet != nil {
			fCidrOnes, _ := fCidrIPNet.Mask.Size()
			bIPOnes, _ := bIPNet.Mask.Size()
			if !bIPNet.Contains(fCidrIP) || (fCidrOnes < bIPOnes) {
				// Don't allow SubPool (the range of allocatable addresses) to be outside, or
				// bigger than, the network itself. This is a configuration error, either the
				// user-managed bridge is missing an address to match fixed-cidr, or fixed-cidr
				// is wrong.
				fixedCIDR, fixedCIDROpt := opts.fixedCIDR()
				if opts.nlFamily() == netlink.FAMILY_V6 {
					return nil, fmt.Errorf("%s=%s is outside any subnet implied by addresses on the user-managed default bridge",
						fixedCIDROpt, fixedCIDR)
				}
				// For IPv4, just log rather than raise an error that would cause daemon
				// startup to fail, because this has been allowed by earlier versions. Remove
				// the SubPool, so that addresses are allocated from the whole of PreferredPool.
				log.G(context.TODO()).WithFields(log.Fields{
					"bridge":         bridgeName,
					fixedCIDROpt:     fixedCIDR,
					"bridge-network": bIPNet.String(),
				}).Warn(fixedCIDROpt + " is outside any subnet implied by addresses on the user-managed default bridge, this may be treated as an error in a future release")
				ipamConf.SubPool = ""
			}
		}
	}

	if defGw, _, auxAddrLabel := opts.defGw(); defGw != nil {
		ipamConf.AuxAddresses[auxAddrLabel] = defGw.String()
	}

	return []*libnetwork.IpamConf{ipamConf}, nil
}

// selectBIP searches the addresses from family on bridge bridgeName for:
// - An address that encompasses fCidrNet if there is one.
// - Else, an address that is within fCidrNet if there is one.
// - Else, any address, if there is one.
//
// If an address is found, the bridge is docker managed (docker0), and the
// bridge address is not compatible with current fixed-cidr/bip configuration,
// the address is ignored or modified accordingly, so that the current config
// can take effect.
//
// If there is an address, it's returned as bIP with its subnet in canonical
// form in bIPNet.
func selectBIP(
	userManagedBridge bool,
	bridgeName string,
	family int,
	fCidrIP net.IP,
	fCidrNet *net.IPNet,
) (bIP net.IP, bIPNet *net.IPNet, err error) {
	bridgeNws, err := ifaceAddrs(bridgeName, family)
	if err != nil {
		return nil, nil, errors.Wrap(err, "list bridge addresses failed")
	}
	// For IPv6, ignore the kernel-assigned link-local address. Remove all
	// link-local addresses, unless fixed-cidr-v6 has the standard link-local
	// prefix. (If fixed-cidr-v6 is the standard LL prefix, the kernel-assigned
	// address is likely to be used instead of an IPAM assigned address.)
	if family == netlink.FAMILY_V6 && (fCidrIP == nil || !isStandardLL(fCidrIP)) {
		bridgeNws = slices.DeleteFunc(bridgeNws, func(nlAddr netlink.Addr) bool {
			return isStandardLL(nlAddr.IP)
		})
	}
	if len(bridgeNws) > 0 {
		// Pick any address from the bridge as a starting point.
		nw := bridgeNws[0].IPNet
		if len(bridgeNws) > 1 && fCidrNet != nil {
			// If there's an address with a subnet that contains fixed-cidr, use it.
			for _, entry := range bridgeNws {
				if entry.Contains(fCidrIP) {
					nw = entry.IPNet
					break
				}
				// For backwards compatibility - prefer the first bridge address within
				// fixed-cidr. If fixed-cidr has a bigger subnet than nw.IP, this doesn't really
				// make sense - the allocatable range (fixed-cidr) will be bigger than the subnet
				// (entry.IPNet).
				if fCidrNet.Contains(entry.IP) && !fCidrNet.Contains(nw.IP) {
					nw = entry.IPNet
				}
			}
		}

		bIP = nw.IP
		bIPNet = lntypes.GetIPNetCanonical(nw)
	}

	if !userManagedBridge && fCidrIP != nil && bIPNet != nil {
		if !bIPNet.Contains(fCidrIP) {
			// The bridge is docker-managed (docker0) and fixed-cidr is not
			// inside a subnet belonging to any existing bridge IP. (fixed-cidr
			// has changed.) So, ignore the existing bridge IP.
			bIP = nil
			bIPNet = nil
		} else {
			fCidrOnes, _ := fCidrNet.Mask.Size()
			bIPOnes, _ := bIPNet.Mask.Size()
			if fCidrOnes < bIPOnes {
				// The bridge is docker-managed (docker0) and fixed-cidr (the
				// allocatable address range) is bigger than the subnet implied
				// by the bridge's current address. (fixed-cidr has changed.)
				// The bridge's address is ok, but its subnet needs to be updated.
				bIPNet.IP = bIPNet.IP.Mask(fCidrNet.Mask)
				bIPNet.Mask = fCidrNet.Mask
			}
		}
	}

	return bIP, bIPNet, nil
}

// isStandardLL returns true if ip is in fe80::/64 (the link local prefix is fe80::/10,
// but only fe80::/64 is normally used - however, it's possible to ask IPAM for a
// link-local subnet that's outside that range).
func isStandardLL(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return ip.Mask(net.CIDRMask(64, 128)).Equal(net.ParseIP("fe80::"))
}

// Remove default bridge interface if present (--bridge=none use case)
func removeDefaultBridgeInterface() {
	if lnk, err := nlwrap.LinkByName(bridge.DefaultBridgeName); err == nil {
		if err := netlink.LinkDel(lnk); err != nil {
			log.G(context.TODO()).Warnf("Failed to remove bridge interface (%s): %v", bridge.DefaultBridgeName, err)
		}
	}
}

func setupInitLayer(uid int, gid int) func(string) error {
	return func(initPath string) error {
		return initlayer.Setup(initPath, uid, gid)
	}
}

// Parse the remapped root (user namespace) option, which can be one of:
//
// - username            - valid username from /etc/passwd
// - username:groupname  - valid username; valid groupname from /etc/group
// - uid                 - 32-bit unsigned int valid Linux UID value
// - uid:gid             - uid value; 32-bit unsigned int Linux GID value
//
// If no groupname is specified, and a username is specified, an attempt
// will be made to lookup a gid for that username as a groupname
//
// If names are used, they are verified to exist in passwd/group
func parseRemappedRoot(usergrp string) (string, string, error) {
	var (
		userID, groupID     int
		username, groupname string
	)

	idparts := strings.Split(usergrp, ":")
	if len(idparts) > 2 {
		return "", "", fmt.Errorf("Invalid user/group specification in --userns-remap: %q", usergrp)
	}

	if uid, err := strconv.ParseInt(idparts[0], 10, 32); err == nil {
		// must be a uid; take it as valid
		userID = int(uid)
		luser, err := usergroup.LookupUID(userID)
		if err != nil {
			return "", "", fmt.Errorf("Uid %d has no entry in /etc/passwd: %v", userID, err)
		}
		username = luser.Name
		if len(idparts) == 1 {
			// if the uid was numeric and no gid was specified, take the uid as the gid
			groupID = userID
			lgrp, err := usergroup.LookupGID(groupID)
			if err != nil {
				return "", "", fmt.Errorf("Gid %d has no entry in /etc/group: %v", groupID, err)
			}
			groupname = lgrp.Name
		}
	} else {
		lookupName := idparts[0]
		// special case: if the user specified "default", they want Docker to create or
		// use (after creation) the "dockremap" user/group for root remapping
		if lookupName == defaultIDSpecifier {
			lookupName = defaultRemappedID
		}
		luser, err := usergroup.LookupUser(lookupName)
		if err != nil && idparts[0] != defaultIDSpecifier {
			// error if the name requested isn't the special "dockremap" ID
			return "", "", fmt.Errorf("Error during uid lookup for %q: %v", lookupName, err)
		} else if err != nil {
			// special case-- if the username == "default", then we have been asked
			// to create a new entry pair in /etc/{passwd,group} for which the /etc/sub{uid,gid}
			// ranges will be used for the user and group mappings in user namespaced containers
			_, _, err := usergroup.AddNamespaceRangesUser(defaultRemappedID)
			if err == nil {
				return defaultRemappedID, defaultRemappedID, nil
			}
			return "", "", fmt.Errorf("Error during %q user creation: %v", defaultRemappedID, err)
		}
		username = luser.Name
		if len(idparts) == 1 {
			// we only have a string username, and no group specified; look up gid from username as group
			group, err := usergroup.LookupGroup(lookupName)
			if err != nil {
				return "", "", fmt.Errorf("Error during gid lookup for %q: %v", lookupName, err)
			}
			groupname = group.Name
		}
	}

	if len(idparts) == 2 {
		// groupname or gid is separately specified and must be resolved
		// to an unsigned 32-bit gid
		if gid, err := strconv.ParseInt(idparts[1], 10, 32); err == nil {
			// must be a gid, take it as valid
			groupID = int(gid)
			lgrp, err := usergroup.LookupGID(groupID)
			if err != nil {
				return "", "", fmt.Errorf("Gid %d has no entry in /etc/passwd: %v", groupID, err)
			}
			groupname = lgrp.Name
		} else {
			// not a number; attempt a lookup
			if _, err := usergroup.LookupGroup(idparts[1]); err != nil {
				return "", "", fmt.Errorf("Error during groupname lookup for %q: %v", idparts[1], err)
			}
			groupname = idparts[1]
		}
	}
	return username, groupname, nil
}

func setupRemappedRoot(cfg *config.Config) (user.IdentityMapping, error) {
	if cfg.RemappedRoot == "" {
		return user.IdentityMapping{}, nil
	}

	// parse the config option to the int uid,gid values
	username, groupname, err := parseRemappedRoot(cfg.RemappedRoot)
	if err != nil {
		return user.IdentityMapping{}, err
	}
	if username == "root" {
		// Cannot set up user namespaces with a 1-to-1 mapping; "--root=0:0" is a no-op effectively
		log.G(context.TODO()).Warn("User namespaces: root cannot be remapped with itself; user namespaces are OFF")
		return user.IdentityMapping{}, nil
	}
	log.G(context.TODO()).Infof("User namespaces: ID ranges will be mapped to subuid/subgid ranges of: %s", username)
	// update remapped root setting now that we have resolved them to actual names
	cfg.RemappedRoot = fmt.Sprintf("%s:%s", username, groupname)

	mappings, err := usergroup.LoadIdentityMapping(username)
	if err != nil {
		return user.IdentityMapping{}, fmt.Errorf("error creating ID mappings: %w", err)
	}
	return mappings, nil
}

func setupDaemonRoot(config *config.Config, rootDir string, uid, gid int) error {
	config.Root = rootDir
	// the docker root metadata directory needs to have execute permissions for all users (g+x,o+x)
	// so that syscalls executing as non-root, operating on subdirectories of the graph root
	// (e.g. mounted layers of a container) can traverse this path.
	// The user namespace support will create subdirectories for the remapped root host uid:gid
	// pair owned by that same uid:gid pair for proper write access to those needed metadata and
	// layer content subtrees.
	if _, err := os.Stat(rootDir); err == nil {
		// root current exists; verify the access bits are correct by setting them
		if err = os.Chmod(rootDir, 0o711); err != nil {
			return err
		}
	} else if os.IsNotExist(err) {
		// no root exists yet, create it 0711 with root:root ownership
		if err := os.MkdirAll(rootDir, 0o711); err != nil {
			return err
		}
	}

	curuid := os.Getuid()
	// First make sure the current root dir has the correct perms.
	if err := user.MkdirAllAndChown(config.Root, 0o710, curuid, gid); err != nil {
		return errors.Wrapf(err, "could not create or set daemon root permissions: %s", config.Root)
	}

	// if user namespaces are enabled we will create a subtree underneath the specified root
	// with any/all specified remapped root uid/gid options on the daemon creating
	// a new subdirectory with ownership set to the remapped uid/gid (so as to allow
	// `chdir()` to work for containers namespaced to that uid/gid)
	if config.RemappedRoot != "" {
		config.Root = filepath.Join(rootDir, fmt.Sprintf("%d.%d", uid, gid))
		log.G(context.TODO()).Debugf("Creating user namespaced daemon root: %s", config.Root)
		// Create the root directory if it doesn't exist
		if err := user.MkdirAllAndChown(config.Root, 0o710, curuid, gid); err != nil {
			return fmt.Errorf("Cannot create daemon root: %s: %v", config.Root, err)
		}
		// we also need to verify that any pre-existing directories in the path to
		// the graphroot won't block access to remapped root--if any pre-existing directory
		// has strict permissions that don't allow "x", container start will fail, so
		// better to warn and fail now
		dirPath := config.Root
		for {
			dirPath = filepath.Dir(dirPath)
			if dirPath == "/" {
				break
			}
			if !canAccess(dirPath, uid, gid) {
				return fmt.Errorf("a subdirectory in your graphroot path (%s) restricts access to the remapped root uid/gid; please fix by allowing 'o+x' permissions on existing directories", config.Root)
			}
		}
	}

	if err := setupDaemonRootPropagation(config); err != nil {
		log.G(context.TODO()).WithError(err).WithField("dir", config.Root).Warn("Error while setting daemon root propagation, this is not generally critical but may cause some functionality to not work or fallback to less desirable behavior")
	}
	return nil
}

// canAccess takes a valid (existing) directory and a uid, gid pair and determines
// if that uid, gid pair has access (execute bit) to the directory.
//
// Note: this is a very rudimentary check, and may not produce accurate results,
// so should not be used for anything other than the current use, see:
// https://github.com/moby/moby/issues/43724
func canAccess(path string, uid, gid int) bool {
	statInfo, err := os.Stat(path)
	if err != nil {
		return false
	}
	perms := statInfo.Mode().Perm()
	if perms&0o001 == 0o001 {
		// world access
		return true
	}
	ssi := statInfo.Sys().(*syscall.Stat_t)
	if ssi.Uid == uint32(uid) && (perms&0o100 == 0o100) {
		// owner access.
		return true
	}
	if ssi.Gid == uint32(gid) && (perms&0o010 == 0o010) {
		// group access.
		return true
	}
	return false
}

func setupDaemonRootPropagation(cfg *config.Config) error {
	rootParentMount, mountOptions, err := getSourceMount(cfg.Root)
	if err != nil {
		return errors.Wrap(err, "error getting daemon root's parent mount")
	}

	var cleanupOldFile bool
	cleanupFile := getUnmountOnShutdownPath(cfg)
	defer func() {
		if !cleanupOldFile {
			return
		}
		if err := os.Remove(cleanupFile); err != nil && !os.IsNotExist(err) {
			log.G(context.TODO()).WithError(err).WithField("file", cleanupFile).Warn("could not clean up old root propagation unmount file")
		}
	}()

	if hasMountInfoOption(mountOptions, sharedPropagationOption, slavePropagationOption) {
		cleanupOldFile = true
		return nil
	}

	if err := mount.MakeShared(cfg.Root); err != nil {
		return errors.Wrap(err, "could not setup daemon root propagation to shared")
	}

	// check the case where this may have already been a mount to itself.
	// If so then the daemon only performed a remount and should not try to unmount this later.
	if rootParentMount == cfg.Root {
		cleanupOldFile = true
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(cleanupFile), 0o700); err != nil {
		return errors.Wrap(err, "error creating dir to store mount cleanup file")
	}

	if err := os.WriteFile(cleanupFile, nil, 0o600); err != nil {
		return errors.Wrap(err, "error writing file to signal mount cleanup on shutdown")
	}
	return nil
}

// getUnmountOnShutdownPath generates the path to used when writing the file that signals to the daemon that on shutdown
// the daemon root should be unmounted.
func getUnmountOnShutdownPath(config *config.Config) string {
	return filepath.Join(config.ExecRoot, "unmount-on-shutdown")
}

// registerLinks registers network links between container and other containers
// with the daemon using the specification in hostConfig.
func (daemon *Daemon) registerLinks(ctr *container.Container) error {
	if ctr.HostConfig == nil || ctr.HostConfig.NetworkMode.IsUserDefined() {
		return nil
	}

	for _, l := range ctr.HostConfig.Links {
		name, alias, err := opts.ParseLink(l)
		if err != nil {
			return err
		}
		child, err := daemon.GetContainer(name)
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				// Trying to link to a non-existing container is not valid, and
				// should return an "invalid parameter" error. Returning a "not
				// found" error here would make the client report the container's
				// image could not be found (see moby/moby#39823)
				err = errdefs.InvalidParameter(err)
			}
			return errors.Wrapf(err, "could not get container for %s", name)
		}
		for child.HostConfig.NetworkMode.IsContainer() {
			cid := child.HostConfig.NetworkMode.ConnectedContainer()
			child, err = daemon.GetContainer(cid)
			if err != nil {
				if cerrdefs.IsNotFound(err) {
					// Trying to link to a non-existing container is not valid, and
					// should return an "invalid parameter" error. Returning a "not
					// found" error here would make the client report the container's
					// image could not be found (see moby/moby#39823)
					err = errdefs.InvalidParameter(err)
				}
				return errors.Wrapf(err, "could not get container for %s", cid)
			}
		}
		if child.HostConfig.NetworkMode.IsHost() {
			return cerrdefs.ErrInvalidArgument.WithMessage("conflicting options: host type networking can't be used with links. This would result in undefined behavior")
		}
		if err := daemon.registerLink(ctr, child, alias); err != nil {
			return err
		}
	}

	return nil
}

// conditionalMountOnStart is a platform specific helper function during the
// container start to call mount.
func (daemon *Daemon) conditionalMountOnStart(container *container.Container) error {
	return daemon.Mount(container)
}

// conditionalUnmountOnCleanup is a platform specific helper function called
// during the cleanup of a container to unmount.
func (daemon *Daemon) conditionalUnmountOnCleanup(container *container.Container) error {
	return daemon.Unmount(container)
}

// setDefaultIsolation determines the default isolation mode for the
// daemon to run in. This is only applicable on Windows
func (daemon *Daemon) setDefaultIsolation(*config.Config) error {
	return nil
}

func (daemon *Daemon) initCPURtController(cfg *config.Config, mnt, path string) error {
	if path == "/" || path == "." {
		return nil
	}

	// Recursively create cgroup to ensure that the system and all parent cgroups have values set
	// for the period and runtime as this limits what the children can be set to.
	if err := daemon.initCPURtController(cfg, mnt, filepath.Dir(path)); err != nil {
		return err
	}

	path = filepath.Join(mnt, path)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	if err := maybeCreateCPURealTimeFile(cfg.CPURealtimePeriod, "cpu.rt_period_us", path); err != nil {
		return err
	}
	return maybeCreateCPURealTimeFile(cfg.CPURealtimeRuntime, "cpu.rt_runtime_us", path)
}

func maybeCreateCPURealTimeFile(configValue int64, file string, path string) error {
	if configValue == 0 {
		return nil
	}
	return os.WriteFile(filepath.Join(path, file), []byte(strconv.FormatInt(configValue, 10)), 0o700)
}

func (daemon *Daemon) setupSeccompProfile(cfg *config.Config) error {
	switch profile := cfg.SeccompProfile; profile {
	case "", config.SeccompProfileDefault:
		daemon.seccompProfilePath = config.SeccompProfileDefault
	case config.SeccompProfileUnconfined:
		daemon.seccompProfilePath = config.SeccompProfileUnconfined
	default:
		daemon.seccompProfilePath = profile
		b, err := os.ReadFile(profile)
		if err != nil {
			return fmt.Errorf("opening seccomp profile (%s) failed: %v", profile, err)
		}
		daemon.seccompProfile = b
	}
	return nil
}

func getSysInfo(cfg *config.Config) *sysinfo.SysInfo {
	var siOpts []sysinfo.Opt
	if cgroupDriver(cfg) == cgroupSystemdDriver {
		if euid := os.Getenv("ROOTLESSKIT_PARENT_EUID"); euid != "" {
			siOpts = append(siOpts, sysinfo.WithCgroup2GroupPath("/user.slice/user-"+euid+".slice"))
		}
	}
	si := sysinfo.New(siOpts...)

	// The "time-namespaces" feature-flag is a (temporary) escape-hatch to disable
	// the use of time-namespaces for containers. This allows users to return to the
	// previous default, which unconditionally used the host's time-namespace.
	//
	// We can consider removing this flag if there's no regressions; see https://github.com/moby/moby/pull/52326#issuecomment-4401495854
	if enabled, ok := cfg.Features["time-namespaces"]; ok && !enabled {
		if si.TimeNamespaces {
			si.TimeNamespaces = false
			log.G(context.TODO()).Info("time-namespaces disabled through feature-override")
		}
	}
	return si
}

func recursiveUnmount(target string) error {
	return mount.RecursiveUnmount(target)
}
