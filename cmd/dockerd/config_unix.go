// +build linux freebsd

package main

import (
	"os/exec"

	"github.com/docker/docker/daemon/config"
	"github.com/docker/docker/opts"
	"github.com/docker/docker/rootless"
	"github.com/docker/go-units"
	"github.com/pkg/errors"
	"github.com/spf13/pflag"
)

// installConfigFlags adds flags to the pflag.FlagSet to configure the daemon
func installConfigFlags(conf *config.Config, flags *pflag.FlagSet) error {
	// First handle install flags which are consistent cross-platform
	if err := installCommonConfigFlags(conf, flags); err != nil {
		return err
	}

	// Then install flags common to unix platforms
	installUnixConfigFlags(conf, flags)

	conf.Ulimits = make(map[string]*units.Ulimit)
	conf.NetworkConfig.DefaultAddressPools = opts.PoolsOpt{}

	// Set default value for `--default-shm-size`
	conf.ShmSize = opts.MemBytes(config.DefaultShmSize)

	// Then platform-specific install flags
	flags.BoolVar(&conf.EnableSelinuxSupport, "selinux-enabled", false, "Enable selinux support")
	flags.Var(opts.NewNamedUlimitOpt("default-ulimits", &conf.Ulimits), "default-ulimit", "Default ulimits for containers")
	flags.BoolVar(&conf.BridgeConfig.EnableIPTables, "iptables", true, "Enable addition of iptables rules")
	flags.BoolVar(&conf.BridgeConfig.EnableIPForward, "ip-forward", true, "Enable net.ipv4.ip_forward")
	flags.BoolVar(&conf.BridgeConfig.EnableIPMasq, "ip-masq", true, "Enable IP masquerading")
	flags.BoolVar(&conf.BridgeConfig.EnableIPv6, "ipv6", false, "Enable IPv6 networking")
	flags.StringVar(&conf.BridgeConfig.FixedCIDRv6, "fixed-cidr-v6", "", "IPv6 subnet for fixed IPs")
	flags.BoolVar(&conf.BridgeConfig.EnableUserlandProxy, "userland-proxy", true, "Use userland proxy for loopback traffic")
	defaultUserlandProxyPath := ""
	if rootless.RunningWithRootlessKit() {
		var err error
		// use rootlesskit-docker-proxy for exposing the ports in RootlessKit netns to the initial namespace.
		defaultUserlandProxyPath, err = exec.LookPath(rootless.RootlessKitDockerProxyBinary)
		if err != nil {
			return errors.Wrapf(err, "running with RootlessKit, but %s not installed", rootless.RootlessKitDockerProxyBinary)
		}
	}
	flags.StringVar(&conf.BridgeConfig.UserlandProxyPath, "userland-proxy-path", defaultUserlandProxyPath, "Path to the userland proxy binary")
	flags.StringVar(&conf.CgroupParent, "cgroup-parent", "", "Set parent cgroup for all containers")
	flags.StringVar(&conf.RemappedRoot, "userns-remap", "", "User/Group setting for user namespaces")
	flags.BoolVar(&conf.LiveRestoreEnabled, "live-restore", false, "Enable live restore of docker when containers are still running")
	flags.IntVar(&conf.OOMScoreAdjust, "oom-score-adjust", -500, "Set the oom_score_adj for the daemon")
	flags.BoolVar(&conf.Init, "init", false, "Run an init in the container to forward signals and reap processes")
	flags.StringVar(&conf.InitPath, "init-path", "", "Path to the docker-init binary")
	flags.Int64Var(&conf.CPURealtimePeriod, "cpu-rt-period", 0, "Limit the CPU real-time period in microseconds")
	flags.Int64Var(&conf.CPURealtimeRuntime, "cpu-rt-runtime", 0, "Limit the CPU real-time runtime in microseconds")
	flags.StringVar(&conf.SeccompProfile, "seccomp-profile", "", "Path to seccomp profile")
	flags.Var(&conf.ShmSize, "default-shm-size", "Default shm size for containers")
	flags.BoolVar(&conf.NoNewPrivileges, "no-new-privileges", false, "Set no-new-privileges by default for new containers")
	flags.StringVar(&conf.IpcMode, "default-ipc-mode", config.DefaultIpcMode, `Default mode for containers ipc ("shareable" | "private")`)
	flags.Var(&conf.NetworkConfig.DefaultAddressPools, "default-address-pool", "Default address pools for node specific local networks")
	// rootless needs to be explicitly specified for running "rootful" dockerd in rootless dockerd (#38702)
	// Note that defaultUserlandProxyPath and honorXDG are configured according to the value of rootless.RunningWithRootlessKit, not the value of --rootless.
	flags.BoolVar(&conf.Rootless, "rootless", rootless.RunningWithRootlessKit(), "Enable rootless mode; typically used with RootlessKit (experimental)")
	flags.StringVar(&conf.CgroupNamespaceMode, "default-cgroupns-mode", config.DefaultCgroupNamespaceMode, `Default mode for containers cgroup namespace ("host" | "private")`)
	return nil
}
