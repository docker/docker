package sysinfo

import (
	"context"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/containerd/cgroups/v3"
	"github.com/containerd/cgroups/v3/cgroup1"
	"github.com/containerd/containerd/v2/pkg/seccomp"
	"github.com/containerd/log"
	"github.com/moby/sys/mountinfo"
)

var (
	readMountinfoOnce sync.Once
	readMountinfoErr  error
	cgroupMountinfo   []*mountinfo.Info
)

// readCgroupMountinfo returns a list of cgroup v1 mounts (i.e. the ones
// with fstype of "cgroup") for the current running process.
//
// The results are cached (to avoid re-reading mountinfo which is relatively
// expensive), so it is assumed that cgroup mounts are not being changed.
func readCgroupMountinfo() ([]*mountinfo.Info, error) {
	readMountinfoOnce.Do(func() {
		cgroupMountinfo, readMountinfoErr = mountinfo.GetMounts(
			mountinfo.FSTypeFilter("cgroup"),
		)
	})

	return cgroupMountinfo, readMountinfoErr
}

func findCgroupV1Mountpoints() (map[string]string, error) {
	mounts, err := readCgroupMountinfo()
	if err != nil {
		return nil, err
	}

	allSubsystems, err := cgroup1.ParseCgroupFile("/proc/self/cgroup")
	if err != nil {
		return nil, fmt.Errorf("Failed to parse cgroup information: %v", err)
	}

	allMap := make(map[string]bool)
	for s := range allSubsystems {
		allMap[s] = false
	}

	mps := make(map[string]string)
	for _, mi := range mounts {
		for opt := range strings.SplitSeq(mi.VFSOptions, ",") {
			seen, known := allMap[opt]
			if known && !seen {
				allMap[opt] = true
				mps[strings.TrimPrefix(opt, "name=")] = mi.Mountpoint
			}
		}
		if len(mps) >= len(allMap) {
			break
		}
	}
	return mps, nil
}

type infoCollector func(info *SysInfo)

// WithCgroup2GroupPath specifies the cgroup v2 group path to inspect availability
// of the controllers.
//
// WithCgroup2GroupPath is expected to be used for rootless mode with systemd driver.
//
// e.g. g = "/user.slice/user-1000.slice/user@1000.service"
func WithCgroup2GroupPath(g string) Opt {
	return func(o *SysInfo) {
		if p := path.Clean(g); p != "" {
			o.cg2GroupPath = p
		}
	}
}

// WithCgroup2Controllers specifies both the cgroup v2 group path and the
// controllers known to be available to it. An empty controller list explicitly
// means that no controllers are available.
func WithCgroup2Controllers(g string, controllers []string) Opt {
	return func(o *SysInfo) {
		if p := path.Clean(g); p != "" {
			o.cg2GroupPath = p
		}
		o.cg2Controllers = make(map[string]struct{}, len(controllers))
		for _, controller := range controllers {
			o.cg2Controllers[controller] = struct{}{}
		}
	}
}

// New returns a new SysInfo, using the filesystem to detect which features
// the kernel supports.
func New(options ...Opt) *SysInfo {
	if cgroups.Mode() == cgroups.Unified {
		return newV2(options...)
	}
	return newV1()
}

func newV1() *SysInfo {
	var (
		err     error
		sysInfo = &SysInfo{}
	)

	ops := []infoCollector{
		applyNetworkingInfo,
		applyAppArmorInfo,
		applySeccompInfo,
		applyCgroupNsInfo,
		applyTimeNsInfo,
	}

	sysInfo.cgMounts, err = findCgroupV1Mountpoints()
	if err != nil {
		log.G(context.TODO()).Warn(err)
	} else {
		ops = append(ops,
			applyMemoryCgroupInfo,
			applyCPUCgroupInfo,
			applyBlkioCgroupInfo,
			applyCPUSetCgroupInfo,
			applyPIDSCgroupInfo,
			applyDevicesCgroupInfo,
		)
	}

	for _, o := range ops {
		o(sysInfo)
	}
	return sysInfo
}

// applyMemoryCgroupInfo adds the memory cgroup controller information to the info.
func applyMemoryCgroupInfo(info *SysInfo) {
	mountPoint, ok := info.cgMounts["memory"]
	if !ok {
		info.Warnings = append(info.Warnings, "Your kernel does not support cgroup memory limit")
		return
	}
	info.MemoryLimit = ok

	info.SwapLimit = cgroupEnabled(mountPoint, "memory.memsw.limit_in_bytes")
	if !info.SwapLimit {
		info.Warnings = append(info.Warnings, "Your kernel does not support swap memory limit")
	}
	info.MemoryReservation = cgroupEnabled(mountPoint, "memory.soft_limit_in_bytes")
	if !info.MemoryReservation {
		info.Warnings = append(info.Warnings, "Your kernel does not support memory reservation")
	}
	info.OomKillDisable = cgroupEnabled(mountPoint, "memory.oom_control")
	if !info.OomKillDisable {
		info.Warnings = append(info.Warnings, "Your kernel does not support oom control")
	}
	info.MemorySwappiness = cgroupEnabled(mountPoint, "memory.swappiness")
	if !info.MemorySwappiness {
		info.Warnings = append(info.Warnings, "Your kernel does not support memory swappiness")
	}
}

// applyCPUCgroupInfo adds the cpu cgroup controller information to the info.
func applyCPUCgroupInfo(info *SysInfo) {
	mountPoint, ok := info.cgMounts["cpu"]
	if !ok {
		info.Warnings = append(info.Warnings, "Unable to find cpu cgroup in mounts")
		return
	}

	info.CPUShares = cgroupEnabled(mountPoint, "cpu.shares")
	if !info.CPUShares {
		info.Warnings = append(info.Warnings, "Your kernel does not support CPU shares")
	}

	info.CPUCfs = cgroupEnabled(mountPoint, "cpu.cfs_quota_us")
	if !info.CPUCfs {
		info.Warnings = append(info.Warnings, "Your kernel does not support CPU CFS scheduler")
	}

	info.CPURealtime = cgroupEnabled(mountPoint, "cpu.rt_period_us")
	if !info.CPURealtime {
		info.Warnings = append(info.Warnings, "Your kernel does not support CPU realtime scheduler")
	}
}

// applyBlkioCgroupInfo adds the blkio cgroup controller information to the info.
func applyBlkioCgroupInfo(info *SysInfo) {
	mountPoint, ok := info.cgMounts["blkio"]
	if !ok {
		info.Warnings = append(info.Warnings, "Unable to find blkio cgroup in mounts")
		return
	}

	info.BlkioWeight = cgroupEnabled(mountPoint, "blkio.weight")
	if !info.BlkioWeight {
		info.Warnings = append(info.Warnings, "Your kernel does not support cgroup blkio weight")
	}

	info.BlkioWeightDevice = cgroupEnabled(mountPoint, "blkio.weight_device")
	if !info.BlkioWeightDevice {
		info.Warnings = append(info.Warnings, "Your kernel does not support cgroup blkio weight_device")
	}

	info.BlkioReadBpsDevice = cgroupEnabled(mountPoint, "blkio.throttle.read_bps_device")
	if !info.BlkioReadBpsDevice {
		info.Warnings = append(info.Warnings, "Your kernel does not support cgroup blkio throttle.read_bps_device")
	}

	info.BlkioWriteBpsDevice = cgroupEnabled(mountPoint, "blkio.throttle.write_bps_device")
	if !info.BlkioWriteBpsDevice {
		info.Warnings = append(info.Warnings, "Your kernel does not support cgroup blkio throttle.write_bps_device")
	}
	info.BlkioReadIOpsDevice = cgroupEnabled(mountPoint, "blkio.throttle.read_iops_device")
	if !info.BlkioReadIOpsDevice {
		info.Warnings = append(info.Warnings, "Your kernel does not support cgroup blkio throttle.read_iops_device")
	}

	info.BlkioWriteIOpsDevice = cgroupEnabled(mountPoint, "blkio.throttle.write_iops_device")
	if !info.BlkioWriteIOpsDevice {
		info.Warnings = append(info.Warnings, "Your kernel does not support cgroup blkio throttle.write_iops_device")
	}
}

// applyCPUSetCgroupInfo adds the cpuset cgroup controller information to the info.
func applyCPUSetCgroupInfo(info *SysInfo) {
	mountPoint, ok := info.cgMounts["cpuset"]
	if !ok {
		info.Warnings = append(info.Warnings, "Unable to find cpuset cgroup in mounts")
		return
	}
	info.Cpuset = ok

	var err error

	cpus, err := os.ReadFile(path.Join(mountPoint, "cpuset.cpus"))
	if err != nil {
		return
	}
	info.Cpus = strings.TrimSpace(string(cpus))
	cpuSets, err := parseUintList(info.Cpus, 0)
	if err != nil {
		info.Warnings = append(info.Warnings, "Unable to parse cpuset cpus: "+err.Error())
		return
	}
	info.CPUSets = cpuSets

	mems, err := os.ReadFile(path.Join(mountPoint, "cpuset.mems"))
	if err != nil {
		return
	}
	info.Mems = strings.TrimSpace(string(mems))
	memSets, err := parseUintList(info.Cpus, 0)
	if err != nil {
		info.Warnings = append(info.Warnings, "Unable to parse cpuset mems: "+err.Error())
		return
	}
	info.MemSets = memSets
}

// applyPIDSCgroupInfo adds whether the pids cgroup controller is available to the info.
func applyPIDSCgroupInfo(info *SysInfo) {
	_, ok := info.cgMounts["pids"]
	if !ok {
		info.Warnings = append(info.Warnings, "Unable to find pids cgroup in mounts")
		return
	}
	info.PidsLimit = true
}

// applyDevicesCgroupInfo adds whether the devices cgroup controller is available to the info.
func applyDevicesCgroupInfo(info *SysInfo) {
	_, ok := info.cgMounts["devices"]
	info.CgroupDevicesEnabled = ok
}

// applyNetworkingInfo adds networking information to the info.
func applyNetworkingInfo(info *SysInfo) {
	info.IPv4ForwardingDisabled = !readProcBool("/proc/sys/net/ipv4/ip_forward")
}

// applyAppArmorInfo adds whether AppArmor is enabled to the info.
func applyAppArmorInfo(info *SysInfo) {
	info.AppArmor = apparmorSupported()
}

// applyCgroupNsInfo adds whether cgroupns is enabled to the info.
func applyCgroupNsInfo(info *SysInfo) {
	info.CgroupNamespaces = cgroupnsSupported()
}

// applyTimeNsInfo adds whether time namespaces are supported to the info.
func applyTimeNsInfo(info *SysInfo) {
	info.TimeNamespaces = timeNsSupported()
}

// applySeccompInfo checks if Seccomp is supported, via CONFIG_SECCOMP.
func applySeccompInfo(info *SysInfo) {
	info.Seccomp = seccomp.IsEnabled()
}

// apparmorSupported adds whether AppArmor is enabled.
func apparmorSupported() bool {
	if _, err := os.Stat("/sys/kernel/security/apparmor"); !os.IsNotExist(err) {
		if _, err := os.ReadFile("/sys/kernel/security/apparmor/profiles"); err == nil {
			return true
		}
	}
	return false
}

// cgroupnsSupported adds whether cgroup namespaces are supported.
func cgroupnsSupported() bool {
	if _, err := os.Stat("/proc/self/ns/cgroup"); !os.IsNotExist(err) {
		return true
	}
	return false
}

// timeNsSupported checks whether time namespaces are supported.
func timeNsSupported() bool {
	if _, err := os.Stat("/proc/self/ns/time"); !os.IsNotExist(err) {
		return true
	}
	return false
}

func cgroupEnabled(mountPoint, name string) bool {
	_, err := os.Stat(path.Join(mountPoint, name))
	return err == nil
}

func readProcBool(path string) bool {
	val, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(val)) == "1"
}

// defaultMaxCPUs is the normal maximum number of CPUs on Linux.
const defaultMaxCPUs = 8192

func isCpusetListAvailable(requested string, available map[int]struct{}) (bool, error) {
	// Start with the normal maximum number of CPUs on Linux, but accept
	// more if we actually have more CPUs available.
	//
	// This limit was added in f8e876d7616469d07b8b049ecb48967eeb8fa7a5
	// to address CVE-2018-20699:
	//
	// Using a value such as `--cpuset-mems=1-9223372036854775807` would cause
	// dockerd to run out of memory allocating a map of the values in the
	// validation code. Set limits to the normal limit of the number of CPUs.
	//
	// More details in https://github.com/docker-archive/engine/pull/70#issuecomment-458458288
	maxCPUs := defaultMaxCPUs
	for m := range available {
		if m > maxCPUs {
			maxCPUs = m
		}
	}
	parsedRequested, err := parseUintList(requested, maxCPUs)
	if err != nil {
		return false, err
	}
	for k := range parsedRequested {
		if _, ok := available[k]; !ok {
			return false, nil
		}
	}
	return true, nil
}

// parseUintList parses and validates the specified string as the value
// found in some cgroup file (e.g. `cpuset.cpus`, `cpuset.mems`), which could be
// one of the formats below. Note that duplicates are actually allowed in the
// input string. It returns a `map[int]bool` with available elements from `val`
// set to `true`. Values larger than `maximum` cause an error if max is non-zero,
// in order to stop the map becoming excessively large.
// Supported formats:
//
//	7
//	1-6
//	0,3-4,7,8-10
//	0-0,0,1-7
//	03,1-3      <- this is gonna get parsed as [1,2,3]
//	3,2,1
//	0-2,3,1
func parseUintList(val string, maximum int) (map[int]struct{}, error) {
	if val == "" {
		return map[int]struct{}{}, nil
	}

	availableInts := make(map[int]struct{})
	errInvalidFormat := fmt.Errorf("invalid format: %s", val)

	for r := range strings.SplitSeq(val, ",") {
		if !strings.Contains(r, "-") {
			v, err := strconv.Atoi(r)
			if err != nil {
				return nil, errInvalidFormat
			}
			if maximum != 0 && v > maximum {
				return nil, fmt.Errorf("value of out range, maximum is %d", maximum)
			}
			availableInts[v] = struct{}{}
		} else {
			minS, maxS, _ := strings.Cut(r, "-")
			minAvailable, err := strconv.Atoi(minS)
			if err != nil {
				return nil, errInvalidFormat
			}
			maxAvailable, err := strconv.Atoi(maxS)
			if err != nil {
				return nil, errInvalidFormat
			}
			if maxAvailable < minAvailable {
				return nil, errInvalidFormat
			}
			if maximum != 0 && maxAvailable > maximum {
				return nil, fmt.Errorf("value of out range, maximum is %d", maximum)
			}
			for i := minAvailable; i <= maxAvailable; i++ {
				availableInts[i] = struct{}{}
			}
		}
	}
	return availableInts, nil
}
