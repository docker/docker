// +build !windows

package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/dockerversion"
	"github.com/docker/docker/pkg/sysinfo"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// fillPlatformInfo fills the platform related info.
func (daemon *Daemon) fillPlatformInfo(v *types.Info, sysInfo *sysinfo.SysInfo) {
	v.CgroupDriver = daemon.getCgroupDriver()
	v.CgroupVersion = "1"
	if sysInfo.CgroupUnified {
		v.CgroupVersion = "2"
	}

	if v.CgroupDriver != cgroupNoneDriver {
		v.MemoryLimit = sysInfo.MemoryLimit
		v.SwapLimit = sysInfo.SwapLimit
		v.KernelMemory = sysInfo.KernelMemory
		v.KernelMemoryTCP = sysInfo.KernelMemoryTCP
		v.OomKillDisable = sysInfo.OomKillDisable
		v.CPUCfsPeriod = sysInfo.CPUCfs
		v.CPUCfsQuota = sysInfo.CPUCfs
		v.CPUShares = sysInfo.CPUShares
		v.CPUSet = sysInfo.Cpuset
		v.PidsLimit = sysInfo.PidsLimit
	}
	v.Runtimes = daemon.configStore.GetAllRuntimes()
	v.DefaultRuntime = daemon.configStore.GetDefaultRuntimeName()
	v.InitBinary = daemon.configStore.GetInitPath()

	defaultRuntimeBinary := daemon.configStore.GetRuntime(v.DefaultRuntime).Path
	if rv, err := exec.Command(defaultRuntimeBinary, "--version").Output(); err == nil {
		if _, _, commit, err := parseRuntimeVersion(string(rv)); err != nil {
			logrus.Warnf("failed to parse %s version: %v", defaultRuntimeBinary, err)
			v.RuncCommit.ID = "N/A"
		} else {
			v.RuncCommit.ID = commit
		}
	} else {
		logrus.Warnf("failed to retrieve %s version: %v", defaultRuntimeBinary, err)
		v.RuncCommit.ID = "N/A"
	}

	// runc is now shipped as a separate package. Set "expected" to same value
	// as "ID" to prevent clients from reporting a version-mismatch
	v.RuncCommit.Expected = v.RuncCommit.ID

	if rv, err := daemon.containerd.Version(context.Background()); err == nil {
		v.ContainerdCommit.ID = rv.Revision
	} else {
		logrus.Warnf("failed to retrieve containerd version: %v", err)
		v.ContainerdCommit.ID = "N/A"
	}

	// containerd is now shipped as a separate package. Set "expected" to same
	// value as "ID" to prevent clients from reporting a version-mismatch
	v.ContainerdCommit.Expected = v.ContainerdCommit.ID

	// TODO is there still a need to check the expected version for tini?
	// if not, we can change this, and just set "Expected" to v.InitCommit.ID
	v.InitCommit.Expected = dockerversion.InitCommitID

	defaultInitBinary := daemon.configStore.GetInitPath()
	if rv, err := exec.Command(defaultInitBinary, "--version").Output(); err == nil {
		if _, commit, err := parseInitVersion(string(rv)); err != nil {
			logrus.Warnf("failed to parse %s version: %s", defaultInitBinary, err)
			v.InitCommit.ID = "N/A"
		} else {
			v.InitCommit.ID = commit
			if len(dockerversion.InitCommitID) > len(commit) {
				v.InitCommit.Expected = dockerversion.InitCommitID[0:len(commit)]
			}
		}
	} else {
		logrus.Warnf("failed to retrieve %s version: %s", defaultInitBinary, err)
		v.InitCommit.ID = "N/A"
	}

	if v.CgroupDriver == cgroupNoneDriver {
		if v.CgroupVersion == "2" {
			v.Warnings = append(v.Warnings, "WARNING: Running in rootless-mode without cgroups. Systemd is required to enable cgroups in rootless-mode.")
		} else {
			v.Warnings = append(v.Warnings, "WARNING: Running in rootless-mode without cgroups. To enable cgroups in rootless-mode, you need to boot the system in cgroup v2 mode.")
		}
	} else {
		if !v.MemoryLimit {
			v.Warnings = append(v.Warnings, "WARNING: No memory limit support")
		}
		if !v.SwapLimit {
			v.Warnings = append(v.Warnings, "WARNING: No swap limit support")
		}
		if !v.KernelMemoryTCP && v.CgroupVersion == "1" {
			// kernel memory is not available for cgroup v2.
			// Warning is not printed on cgroup v2, because there is no action user can take.
			v.Warnings = append(v.Warnings, "WARNING: No kernel memory TCP limit support")
		}
		if !v.OomKillDisable && v.CgroupVersion == "1" {
			// oom kill disable is not available for cgroup v2.
			// Warning is not printed on cgroup v2, because there is no action user can take.
			v.Warnings = append(v.Warnings, "WARNING: No oom kill disable support")
		}
		if !v.CPUCfsQuota {
			v.Warnings = append(v.Warnings, "WARNING: No cpu cfs quota support")
		}
		if !v.CPUCfsPeriod {
			v.Warnings = append(v.Warnings, "WARNING: No cpu cfs period support")
		}
		if !v.CPUShares {
			v.Warnings = append(v.Warnings, "WARNING: No cpu shares support")
		}
		if !v.CPUSet {
			v.Warnings = append(v.Warnings, "WARNING: No cpuset support")
		}
		// TODO add fields for these options in types.Info
		if !sysInfo.BlkioWeight && v.CgroupVersion == "2" {
			// blkio weight is not available on cgroup v1 since kernel 5.0.
			// Warning is not printed on cgroup v1, because there is no action user can take.
			// On cgroup v2, blkio weight is implemented using io.weight
			v.Warnings = append(v.Warnings, "WARNING: No io.weight support")
		}
		if !sysInfo.BlkioWeightDevice && v.CgroupVersion == "2" {
			v.Warnings = append(v.Warnings, "WARNING: No io.weight (per device) support")
		}
		if !sysInfo.BlkioReadBpsDevice {
			if v.CgroupVersion == "2" {
				v.Warnings = append(v.Warnings, "WARNING: No io.max (rbps) support")
			} else {
				v.Warnings = append(v.Warnings, "WARNING: No blkio throttle.read_bps_device support")
			}
		}
		if !sysInfo.BlkioWriteBpsDevice {
			if v.CgroupVersion == "2" {
				v.Warnings = append(v.Warnings, "WARNING: No io.max (wbps) support")
			} else {
				v.Warnings = append(v.Warnings, "WARNING: No blkio throttle.write_bps_device support")
			}
		}
		if !sysInfo.BlkioReadIOpsDevice {
			if v.CgroupVersion == "2" {
				v.Warnings = append(v.Warnings, "WARNING: No io.max (riops) support")
			} else {
				v.Warnings = append(v.Warnings, "WARNING: No blkio throttle.read_iops_device support")
			}
		}
		if !sysInfo.BlkioWriteIOpsDevice {
			if v.CgroupVersion == "2" {
				v.Warnings = append(v.Warnings, "WARNING: No io.max (wiops) support")
			} else {
				v.Warnings = append(v.Warnings, "WARNING: No blkio throttle.write_iops_device support")
			}
		}
	}
	if !v.IPv4Forwarding {
		v.Warnings = append(v.Warnings, "WARNING: IPv4 forwarding is disabled")
	}
	if !v.BridgeNfIptables {
		v.Warnings = append(v.Warnings, "WARNING: bridge-nf-call-iptables is disabled")
	}
	if !v.BridgeNfIP6tables {
		v.Warnings = append(v.Warnings, "WARNING: bridge-nf-call-ip6tables is disabled")
	}
}

func (daemon *Daemon) fillPlatformVersion(v *types.Version) {
	if rv, err := daemon.containerd.Version(context.Background()); err == nil {
		v.Components = append(v.Components, types.ComponentVersion{
			Name:    "containerd",
			Version: rv.Version,
			Details: map[string]string{
				"GitCommit": rv.Revision,
			},
		})
	}

	defaultRuntime := daemon.configStore.GetDefaultRuntimeName()
	defaultRuntimeBinary := daemon.configStore.GetRuntime(defaultRuntime).Path
	if rv, err := exec.Command(defaultRuntimeBinary, "--version").Output(); err == nil {
		if _, ver, commit, err := parseRuntimeVersion(string(rv)); err != nil {
			logrus.Warnf("failed to parse %s version: %v", defaultRuntimeBinary, err)
		} else {
			v.Components = append(v.Components, types.ComponentVersion{
				Name:    defaultRuntime,
				Version: ver,
				Details: map[string]string{
					"GitCommit": commit,
				},
			})
		}
	} else {
		logrus.Warnf("failed to retrieve %s version: %v", defaultRuntimeBinary, err)
	}

	defaultInitBinary := daemon.configStore.GetInitPath()
	if rv, err := exec.Command(defaultInitBinary, "--version").Output(); err == nil {
		if ver, commit, err := parseInitVersion(string(rv)); err != nil {
			logrus.Warnf("failed to parse %s version: %s", defaultInitBinary, err)
		} else {
			v.Components = append(v.Components, types.ComponentVersion{
				Name:    filepath.Base(defaultInitBinary),
				Version: ver,
				Details: map[string]string{
					"GitCommit": commit,
				},
			})
		}
	} else {
		logrus.Warnf("failed to retrieve %s version: %s", defaultInitBinary, err)
	}
}

func fillDriverWarnings(v *types.Info) {
	for _, pair := range v.DriverStatus {
		if pair[0] == "Data loop file" {
			msg := fmt.Sprintf("WARNING: %s: usage of loopback devices is "+
				"strongly discouraged for production use.\n         "+
				"Use `--storage-opt dm.thinpooldev` to specify a custom block storage device.", v.Driver)

			v.Warnings = append(v.Warnings, msg)
			continue
		}
		if pair[0] == "Supports d_type" && pair[1] == "false" {
			backingFs := getBackingFs(v)

			msg := fmt.Sprintf("WARNING: %s: the backing %s filesystem is formatted without d_type support, which leads to incorrect behavior.\n", v.Driver, backingFs)
			if backingFs == "xfs" {
				msg += "         Reformat the filesystem with ftype=1 to enable d_type support.\n"
			}
			msg += "         Running without d_type support will not be supported in future releases."

			v.Warnings = append(v.Warnings, msg)
			continue
		}
	}
}

func getBackingFs(v *types.Info) string {
	for _, pair := range v.DriverStatus {
		if pair[0] == "Backing Filesystem" {
			return pair[1]
		}
	}
	return ""
}

// parseInitVersion parses a Tini version string, and extracts the "version"
// and "git commit" from the output.
//
// Output example from `docker-init --version`:
//
//     tini version 0.18.0 - git.fec3683
func parseInitVersion(v string) (version string, commit string, err error) {
	parts := strings.Split(v, " - ")

	if len(parts) >= 2 {
		gitParts := strings.Split(strings.TrimSpace(parts[1]), ".")
		if len(gitParts) == 2 && gitParts[0] == "git" {
			commit = gitParts[1]
		}
	}
	parts[0] = strings.TrimSpace(parts[0])
	if strings.HasPrefix(parts[0], "tini version ") {
		version = strings.TrimPrefix(parts[0], "tini version ")
	}
	if version == "" && commit == "" {
		err = errors.Errorf("unknown output format: %s", v)
	}
	return version, commit, err
}

// parseRuntimeVersion parses the output of `[runtime] --version` and extracts the
// "name", "version" and "git commit" from the output.
//
// Output example from `runc --version`:
//
//   runc version 1.0.0-rc5+dev
//   commit: 69663f0bd4b60df09991c08812a60108003fa340
//   spec: 1.0.0
func parseRuntimeVersion(v string) (runtime string, version string, commit string, err error) {
	lines := strings.Split(strings.TrimSpace(v), "\n")
	for _, line := range lines {
		if strings.Contains(line, "version") {
			s := strings.Split(line, "version")
			runtime = strings.TrimSpace(s[0])
			version = strings.TrimSpace(s[len(s)-1])
			continue
		}
		if strings.HasPrefix(line, "commit:") {
			commit = strings.TrimSpace(strings.TrimPrefix(line, "commit:"))
			continue
		}
	}
	if version == "" && commit == "" {
		err = errors.Errorf("unknown output format: %s", v)
	}
	return runtime, version, commit, err
}

func (daemon *Daemon) cgroupNamespacesEnabled(sysInfo *sysinfo.SysInfo) bool {
	return sysInfo.CgroupNamespaces && containertypes.CgroupnsMode(daemon.configStore.CgroupNamespaceMode).IsPrivate()
}

// Rootless returns true if daemon is running in rootless mode
func (daemon *Daemon) Rootless() bool {
	return daemon.configStore.Rootless
}
