//go:build !windows

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	runcoptions "github.com/containerd/containerd/api/types/runc/options"
	"github.com/containerd/log"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/internal/rootless"
	"github.com/moby/moby/v2/pkg/sysinfo"
	"github.com/pkg/errors"
	rkclient "github.com/rootless-containers/rootlesskit/v3/pkg/api/client"
)

// fillPlatformInfo fills the platform related info.
func (daemon *Daemon) fillPlatformInfo(ctx context.Context, v *system.Info, sysInfo *sysinfo.SysInfo, cfg *configStore) error {
	v.CgroupDriver = cgroupDriver(&cfg.Config)
	v.CgroupVersion = "1"
	if sysInfo.CgroupUnified {
		v.CgroupVersion = "2"
	}

	if v.CgroupDriver != cgroupNoneDriver {
		v.MemoryLimit = sysInfo.MemoryLimit
		v.SwapLimit = sysInfo.SwapLimit
		v.OomKillDisable = sysInfo.OomKillDisable
		v.CPUCfsPeriod = sysInfo.CPUCfs
		v.CPUCfsQuota = sysInfo.CPUCfs
		v.CPUShares = sysInfo.CPUShares
		v.CPUSet = sysInfo.Cpuset
		v.PidsLimit = sysInfo.PidsLimit
	}
	v.Runtimes = make(map[string]system.RuntimeWithStatus)
	for n, p := range stockRuntimes() {
		v.Runtimes[n] = system.RuntimeWithStatus{
			Runtime: system.Runtime{
				Path: p,
			},
			Status: daemon.runtimeStatus(ctx, cfg, n),
		}
	}
	for n, r := range cfg.Config.Runtimes {
		v.Runtimes[n] = system.RuntimeWithStatus{
			Runtime: system.Runtime{
				Path: r.Path,
				Args: append([]string(nil), r.Args...),
			},
			Status: daemon.runtimeStatus(ctx, cfg, n),
		}
	}
	v.DefaultRuntime = cfg.Runtimes.Default
	v.RuncCommit.ID = "N/A"
	v.ContainerdCommit.ID = "N/A"
	v.InitCommit.ID = "N/A"

	if err := populateRuncCommit(&v.RuncCommit, cfg); err != nil {
		log.G(ctx).WithError(err).Warn("Failed to retrieve default runtime version")
	}

	if err := daemon.populateContainerdCommit(ctx, &v.ContainerdCommit); err != nil {
		return err
	}

	if err := daemon.populateInitCommit(ctx, v, cfg); err != nil {
		return err
	}

	// Set expected and actual commits to the same value to prevent the client
	// showing that the version does not match the "expected" version/commit.

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
	if v.CgroupVersion == "1" {
		v.Warnings = append(v.Warnings, "WARNING: Support for cgroup v1 is deprecated and planned to be removed by no later than May 2029 (https://github.com/moby/moby/issues/51111)")
	}
	if !v.IPv4Forwarding {
		v.Warnings = append(v.Warnings, "WARNING: IPv4 forwarding is disabled")
	}
	// Env-var belonging to the bridge driver, disables use of the iptables "raw" table.
	if os.Getenv("DOCKER_INSECURE_NO_IPTABLES_RAW") == "1" {
		v.Warnings = append(v.Warnings, "WARNING: DOCKER_INSECURE_NO_IPTABLES_RAW is set")
	}
	return nil
}

func (daemon *Daemon) fillPlatformVersion(ctx context.Context, v *system.VersionResponse, cfg *configStore) error {
	if err := daemon.populateContainerdVersion(ctx, v); err != nil {
		return err
	}

	if err := populateRuncVersion(cfg, v); err != nil {
		log.G(ctx).WithError(err).Warn("Failed to retrieve default runtime version")
	}

	if err := populateInitVersion(ctx, cfg, v); err != nil {
		return err
	}

	if err := daemon.fillRootlessVersion(ctx, v); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.G(ctx).WithError(err).Warn("Failed to fill rootless version")
	}
	return nil
}

func populateRuncCommit(v *system.Commit, cfg *configStore) error {
	_, _, commit, err := parseDefaultRuntimeVersion(&cfg.Runtimes)
	if err != nil {
		return err
	}
	v.ID = commit
	return nil
}

func (daemon *Daemon) populateInitCommit(ctx context.Context, v *system.Info, cfg *configStore) error {
	v.InitBinary = cfg.GetInitPath()
	initBinary, err := cfg.LookupInitPath()
	if err != nil {
		log.G(ctx).WithError(err).Warnf("Failed to find docker-init")
		return nil
	}

	rv, err := exec.CommandContext(ctx, initBinary, "--version").Output()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.G(ctx).WithError(err).Warnf("Failed to retrieve %s version", initBinary)
		return nil
	}

	_, commit, err := parseInitVersion(string(rv))
	if err != nil {
		log.G(ctx).WithError(err).Warnf("failed to parse %s version", initBinary)
		return nil
	}
	v.InitCommit.ID = commit
	return nil
}

func (daemon *Daemon) fillRootlessVersion(ctx context.Context, v *system.VersionResponse) error {
	if !rootless.RunningWithRootlessKit() {
		return nil
	}
	rlc, err := getRootlessKitClient()
	if err != nil {
		return errors.Wrap(err, "failed to create RootlessKit client")
	}
	defer func() { _ = rlc.Close() }()
	rlInfo, err := rlc.Info(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to retrieve RootlessKit version")
	}
	rlV := system.ComponentVersion{
		Name:    "rootlesskit",
		Version: rlInfo.Version,
		Details: map[string]string{
			"ApiVersion": rlInfo.APIVersion,
			"StateDir":   rlInfo.StateDir,
		},
	}
	if netDriver := rlInfo.NetworkDriver; netDriver != nil {
		// netDriver is nil for the "host" network driver
		// (not used for Rootless Docker)
		rlV.Details["NetworkDriver"] = netDriver.Driver
	}
	if portDriver := rlInfo.PortDriver; portDriver != nil {
		// portDriver is nil for the "implicit" port driver
		// (used with "pasta" network driver)
		//
		// Because the ports are not managed via RootlessKit API in this case.
		rlV.Details["PortDriver"] = portDriver.Driver
	}
	v.Components = append(v.Components, rlV)

	switch rlInfo.NetworkDriver.Driver {
	case "slirp4netns":
		err = func() error {
			rv, err := exec.CommandContext(ctx, "slirp4netns", "--version").Output()
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				log.G(ctx).WithError(err).Warn("Failed to retrieve slirp4netns version")
				return nil
			}

			_, ver, commit, err := parseRuntimeVersion(string(rv))
			if err != nil {
				log.G(ctx).WithError(err).Warn("Failed to parse slirp4netns version")
				return nil
			}
			v.Components = append(v.Components, system.ComponentVersion{
				Name:    "slirp4netns",
				Version: ver,
				Details: map[string]string{
					"GitCommit": commit,
				},
			})
			return nil
		}()
		if err != nil {
			return err
		}
	case "vpnkit":
		err = func() error {
			out, err := exec.CommandContext(ctx, "vpnkit", "--version").Output()
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				log.G(ctx).WithError(err).Warn("Failed to retrieve vpnkit version")
				return nil
			}
			v.Components = append(v.Components, system.ComponentVersion{
				Name:    "vpnkit",
				Version: strings.TrimSpace(strings.TrimSpace(string(out))),
			})
			return nil
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

// getRootlessKitClient returns RootlessKit client
func getRootlessKitClient() (rkclient.Client, error) {
	stateDir := os.Getenv("ROOTLESSKIT_STATE_DIR")
	if stateDir == "" {
		return nil, errors.New("environment variable `ROOTLESSKIT_STATE_DIR` is not set")
	}
	apiSock := filepath.Join(stateDir, "api.sock")
	return rkclient.New(apiSock)
}

func fillDriverWarnings(v *system.Info) {
	for _, pair := range v.DriverStatus {
		if pair[0] == "Extended file attributes" && pair[1] == "best-effort" {
			msg := fmt.Sprintf("WARNING: %s: extended file attributes from container images "+
				"will be silently discarded if the backing filesystem does not support them.\n"+
				"         CONTAINERS MAY MALFUNCTION IF EXTENDED ATTRIBUTES ARE MISSING.\n"+
				"         This is an UNSUPPORTABLE configuration for which no bug reports will be accepted.\n", v.Driver)

			v.Warnings = append(v.Warnings, msg)
			continue
		}
	}
}

// parseInitVersion parses a Tini version string, and extracts the "version"
// and "git commit" from the output.
//
// Output example from `docker-init --version`:
//
//	tini version 0.18.0 - git.fec3683
func parseInitVersion(v string) (version string, commit string, _ error) {
	parts := strings.Split(v, " - ")

	if len(parts) >= 2 {
		gitParts := strings.Split(strings.TrimSpace(parts[1]), ".")
		if len(gitParts) == 2 && gitParts[0] == "git" {
			commit = gitParts[1]
		}
	}
	parts[0] = strings.TrimSpace(parts[0])
	if after, ok := strings.CutPrefix(parts[0], "tini version "); ok {
		version = after
	}
	if version == "" && commit == "" {
		return "", "", errors.Errorf("unknown output format: %s", v)
	}
	return version, commit, nil
}

// parseRuntimeVersion parses the output of `[runtime] --version` and extracts the
// "name", "version" and "git commit" from the output.
//
// Output example from `runc --version`:
//
//	runc version 1.0.0-rc5+dev
//	commit: 69663f0bd4b60df09991c08812a60108003fa340
//	spec: 1.0.0
func parseRuntimeVersion(v string) (runtime, version, commit string, _ error) {
	lines := strings.SplitSeq(strings.TrimSpace(v), "\n")
	for line := range lines {
		if strings.Contains(line, "version") {
			s := strings.Split(line, "version")
			runtime = strings.TrimSpace(s[0])
			version = strings.TrimSpace(s[len(s)-1])
			continue
		}
		if after, ok := strings.CutPrefix(line, "commit:"); ok {
			commit = strings.TrimSpace(after)
			continue
		}
	}
	if version == "" && commit == "" {
		return runtime, "", "", errors.Errorf("unknown output format: %s", v)
	}
	return runtime, version, commit, nil
}

func parseDefaultRuntimeVersion(rts *runtimes) (runtime, version, commit string, _ error) {
	shim, opts, err := rts.Get(rts.Default)
	if err != nil {
		return "", "", "", err
	}
	shimopts, ok := opts.(*runcoptions.Options)
	if !ok {
		return "", "", "", fmt.Errorf("%s: retrieving version not supported", shim)
	}
	rt := shimopts.BinaryName
	if rt == "" {
		rt = defaultRuntimeName
	}
	rv, err := exec.Command(rt, "--version").Output()
	if err != nil {
		return "", "", "", fmt.Errorf("failed to retrieve %s version: %w", rt, err)
	}
	runtime, version, commit, err = parseRuntimeVersion(string(rv))
	if err != nil {
		return "", "", "", fmt.Errorf("failed to parse %s version: %w", rt, err)
	}
	return runtime, version, commit, nil
}

func cgroupNamespacesEnabled(sysInfo *sysinfo.SysInfo, cfg *config.Config) bool {
	return sysInfo.CgroupNamespaces && containertypes.CgroupnsMode(cfg.CgroupNamespaceMode).IsPrivate()
}

// Rootless returns true if daemon is running in rootless mode
func Rootless(cfg *config.Config) bool {
	return cfg.Rootless
}

func noNewPrivileges(cfg *config.Config) bool {
	return cfg.NoNewPrivileges
}

func (daemon *Daemon) populateContainerdCommit(ctx context.Context, v *system.Commit) error {
	rv, err := daemon.containerd.Version(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.G(ctx).WithError(err).Warnf("Failed to retrieve containerd version")
		return nil
	}
	v.ID = rv.Revision
	return nil
}

func (daemon *Daemon) populateContainerdVersion(ctx context.Context, v *system.VersionResponse) error {
	rv, err := daemon.containerd.Version(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.G(ctx).WithError(err).Warn("Failed to retrieve containerd version")
		return nil
	}

	component := system.ComponentVersion{
		Name:    "containerd",
		Version: rv.Version,
	}
	if rv.Revision != "" {
		component.Details = map[string]string{"GitCommit": rv.Revision}
	}

	v.Components = append(v.Components, component)
	return nil
}

func populateRuncVersion(cfg *configStore, v *system.VersionResponse) error {
	_, ver, commit, err := parseDefaultRuntimeVersion(&cfg.Runtimes)
	if err != nil {
		return err
	}
	v.Components = append(v.Components, system.ComponentVersion{
		Name:    cfg.Runtimes.Default,
		Version: ver,
		Details: map[string]string{
			"GitCommit": commit,
		},
	})
	return nil
}

func populateInitVersion(ctx context.Context, cfg *configStore, v *system.VersionResponse) error {
	initBinary, err := cfg.LookupInitPath()
	if err != nil {
		log.G(ctx).WithError(err).Warn("Failed to find docker-init")
		return nil
	}

	rv, err := exec.CommandContext(ctx, initBinary, "--version").Output()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.G(ctx).WithError(err).Warnf("Failed to retrieve %s version", initBinary)
		return nil
	}

	ver, commit, err := parseInitVersion(string(rv))
	if err != nil {
		log.G(ctx).WithError(err).Warnf("failed to parse %s version", initBinary)
		return nil
	}
	v.Components = append(v.Components, system.ComponentVersion{
		Name:    filepath.Base(initBinary),
		Version: ver,
		Details: map[string]string{
			"GitCommit": commit,
		},
	})
	return nil
}

// ociRuntimeFeaturesKey is the "well-known" key used for including the
// OCI runtime spec "features" struct.
//
// see https://github.com/opencontainers/runtime-spec/blob/main/features.md
const ociRuntimeFeaturesKey = "org.opencontainers.runtime-spec.features"

func (daemon *Daemon) runtimeStatus(ctx context.Context, cfg *configStore, runtimeName string) map[string]string {
	m := make(map[string]string)
	if runtimeName == "" {
		runtimeName = cfg.Runtimes.Default
	}
	if features := cfg.Runtimes.Features(runtimeName); features != nil {
		if j, err := json.Marshal(features); err == nil {
			m[ociRuntimeFeaturesKey] = string(j)
		} else {
			log.G(ctx).WithFields(log.Fields{"error": err, "runtime": runtimeName}).Warn("Failed to call json.Marshal for the OCI features struct of runtime")
		}
	}
	return m
}
