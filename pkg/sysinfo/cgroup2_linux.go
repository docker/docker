package sysinfo // import "github.com/docker/docker/pkg/sysinfo"

import (
	"io/ioutil"
	"os"
	"path"
	"strings"

	cgroupsV2 "github.com/containerd/cgroups/v2"
	"github.com/containerd/containerd/sys"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/sirupsen/logrus"
)

type infoCollectorV2 func(info *SysInfo, controllers map[string]struct{}, dirPath string) (warnings []string)

func newV2(quiet bool, opts *opts) *SysInfo {
	var warnings []string
	sysInfo := &SysInfo{
		CgroupUnified: true,
	}
	g := opts.cg2GroupPath
	if g == "" {
		g = "/"
	}
	m, err := cgroupsV2.LoadManager("/sys/fs/cgroup", g)
	if err != nil {
		logrus.Warn(err)
	} else {
		controllersM := make(map[string]struct{})
		controllers, err := m.Controllers()
		if err != nil {
			logrus.Warn(err)
		}
		for _, c := range controllers {
			controllersM[c] = struct{}{}
		}
		opsV2 := []infoCollectorV2{
			applyMemoryCgroupInfoV2,
			applyCPUCgroupInfoV2,
			applyIOCgroupInfoV2,
			applyCPUSetCgroupInfoV2,
			applyPIDSCgroupInfoV2,
			applyDevicesCgroupInfoV2,
		}
		dirPath := path.Join("/sys/fs/cgroup", path.Clean(g))
		for _, o := range opsV2 {
			w := o(sysInfo, controllersM, dirPath)
			warnings = append(warnings, w...)
		}
	}

	ops := []infoCollector{
		applyNetworkingInfo,
		applyAppArmorInfo,
		applySeccompInfo,
		applyCgroupNsInfo,
	}
	for _, o := range ops {
		w := o(sysInfo, nil)
		warnings = append(warnings, w...)
	}
	if !quiet {
		for _, w := range warnings {
			logrus.Warn(w)
		}
	}
	return sysInfo
}

func getSwapLimitV2() bool {
	groups, err := cgroups.ParseCgroupFile("/proc/self/cgroup")
	if err != nil {
		return false
	}

	g := groups[""]
	if g == "" {
		return false
	}

	cGroupPath := path.Join("/sys/fs/cgroup", g, "memory.swap.max")
	if _, err = os.Stat(cGroupPath); os.IsNotExist(err) {
		return false
	}
	return true
}

func applyMemoryCgroupInfoV2(info *SysInfo, controllers map[string]struct{}, _ string) []string {
	var warnings []string
	if _, ok := controllers["memory"]; !ok {
		warnings = append(warnings, "Unable to find memory controller")
		return warnings
	}

	info.MemoryLimit = true
	info.SwapLimit = getSwapLimitV2()
	info.MemoryReservation = true
	info.OomKillDisable = false
	info.MemorySwappiness = false
	info.KernelMemory = false
	info.KernelMemoryTCP = false
	return warnings
}

func applyCPUCgroupInfoV2(info *SysInfo, controllers map[string]struct{}, _ string) []string {
	var warnings []string
	if _, ok := controllers["cpu"]; !ok {
		warnings = append(warnings, "Unable to find cpu controller")
		return warnings
	}
	info.CPUShares = true
	info.CPUCfs = true
	info.CPURealtime = false
	return warnings
}

func applyIOCgroupInfoV2(info *SysInfo, controllers map[string]struct{}, _ string) []string {
	var warnings []string
	if _, ok := controllers["io"]; !ok {
		warnings = append(warnings, "Unable to find io controller")
		return warnings
	}

	info.BlkioWeight = true
	info.BlkioWeightDevice = true
	info.BlkioReadBpsDevice = true
	info.BlkioWriteBpsDevice = true
	info.BlkioReadIOpsDevice = true
	info.BlkioWriteIOpsDevice = true
	return warnings
}

func applyCPUSetCgroupInfoV2(info *SysInfo, controllers map[string]struct{}, dirPath string) []string {
	var warnings []string
	if _, ok := controllers["cpuset"]; !ok {
		warnings = append(warnings, "Unable to find cpuset controller")
		return warnings
	}
	info.Cpuset = true

	cpus, err := ioutil.ReadFile(path.Join(dirPath, "cpuset.cpus.effective"))
	if err != nil {
		return warnings
	}
	info.Cpus = strings.TrimSpace(string(cpus))

	mems, err := ioutil.ReadFile(path.Join(dirPath, "cpuset.mems.effective"))
	if err != nil {
		return warnings
	}
	info.Mems = strings.TrimSpace(string(mems))
	return warnings
}

func applyPIDSCgroupInfoV2(info *SysInfo, controllers map[string]struct{}, _ string) []string {
	var warnings []string
	if _, ok := controllers["pids"]; !ok {
		warnings = append(warnings, "Unable to find pids controller")
		return warnings
	}
	info.PidsLimit = true
	return warnings
}

func applyDevicesCgroupInfoV2(info *SysInfo, controllers map[string]struct{}, _ string) []string {
	info.CgroupDevicesEnabled = !sys.RunningInUserNS()
	return nil
}
