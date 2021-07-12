// +build !windows

package main

import (
	"bytes"
	"io/ioutil"
	"os/exec"
	"strings"

	"github.com/docker/docker/pkg/sysinfo"
)

var (
	// SysInfo stores information about which features a kernel supports.
	SysInfo *sysinfo.SysInfo
)

func cpuCfsPeriod() bool {
	return testEnv.DaemonInfo.CPUCfsPeriod
}

func cpuCfsQuota() bool {
	return testEnv.DaemonInfo.CPUCfsQuota
}

func cpuShare() bool {
	return testEnv.DaemonInfo.CPUShares
}

func oomControl() bool {
	return testEnv.DaemonInfo.OomKillDisable
}

func pidsLimit() bool {
	return SysInfo.PidsLimit
}

func memoryLimitSupport() bool {
	return testEnv.DaemonInfo.MemoryLimit
}

func memoryReservationSupport() bool {
	return SysInfo.MemoryReservation
}

func swapMemorySupport() bool {
	return testEnv.DaemonInfo.SwapLimit
}

func memorySwappinessSupport() bool {
	return testEnv.IsLocalDaemon() && SysInfo.MemorySwappiness
}

func blkioWeight() bool {
	return testEnv.IsLocalDaemon() && SysInfo.BlkioWeight
}

func cgroupCpuset() bool {
	return testEnv.DaemonInfo.CPUSet
}

func seccompEnabled() bool {
	return SysInfo.Seccomp
}

func bridgeNfIptables() bool {
	return !SysInfo.BridgeNFCallIPTablesDisabled
}

func unprivilegedUsernsClone() bool {
	content, err := ioutil.ReadFile("/proc/sys/kernel/unprivileged_userns_clone")
	return err != nil || !strings.Contains(string(content), "0")
}

func overlayFSSupported() bool {
	cmd := exec.Command(dockerBinary, "run", "--rm", "busybox", "/bin/sh", "-c", "cat /proc/filesystems")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return bytes.Contains(out, []byte("overlay\n"))
}

func init() {
	if testEnv.IsLocalDaemon() {
		SysInfo = sysinfo.New(true)
	}
}
