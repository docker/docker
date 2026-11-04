//go:build !windows

package config

import (
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/v2/daemon/internal/rootless"
	"github.com/moby/moby/v2/daemon/pkg/opts"
	"github.com/moby/moby/v2/pkg/homedir"
	"github.com/pkg/errors"
)

const (
	// DefaultIpcMode is default for container's IpcMode, if not set otherwise
	DefaultIpcMode = container.IPCModePrivate

	// DefaultCgroupNamespaceMode is the default mode for containers cgroup
	// namespace when using cgroups v2.
	DefaultCgroupNamespaceMode = container.CgroupnsModePrivate

	// DefaultCgroupV1NamespaceMode is the default mode for containers cgroup
	// namespace when using cgroups v1.
	DefaultCgroupV1NamespaceMode = container.CgroupnsModeHost

	// StockRuntimeName is the reserved name/alias used to represent the
	// OCI runtime being shipped with the docker daemon package.
	StockRuntimeName = "runc"

	defaultStopTimeout = 10
)

// BridgeConfig stores all the parameters for both the bridge driver and the
// default bridge network.
type BridgeConfig struct {
	DefaultBridgeConfig

	EnableIPTables           bool   `json:"iptables,omitempty"`
	EnableIP6Tables          bool   `json:"ip6tables,omitempty"`
	EnableIPForward          bool   `json:"ip-forward,omitempty"`
	DisableFilterForwardDrop bool   `json:"ip-forward-no-drop,omitempty"`
	EnableIPMasq             bool   `json:"ip-masq,omitempty"`
	EnableUserlandProxy      bool   `json:"userland-proxy,omitempty"`
	UserlandProxyPath        string `json:"userland-proxy-path,omitempty"`
	AllowDirectRouting       bool   `json:"allow-direct-routing,omitempty"`
	BridgeAcceptFwMark       string `json:"bridge-accept-fwmark,omitempty"`
}

// DefaultBridgeConfig stores all the parameters for the default bridge network.
type DefaultBridgeConfig struct {
	commonBridgeConfig

	// Fields below here are platform specific.
	EnableIPv6                  bool   `json:"ipv6,omitempty"`
	FixedCIDRv6                 string `json:"fixed-cidr-v6,omitempty"`
	MTU                         int    `json:"mtu,omitempty"`
	DefaultIP                   net.IP `json:"ip,omitempty"`
	IP                          string `json:"bip,omitempty"`
	IP6                         string `json:"bip6,omitempty"`
	DefaultGatewayIPv4          net.IP `json:"default-gateway,omitempty"`
	DefaultGatewayIPv6          net.IP `json:"default-gateway-v6,omitempty"`
	InterContainerCommunication bool   `json:"icc,omitempty"`
}

// Config defines the configuration of a docker daemon.
// It includes json tags to deserialize configuration from a file
// using the same names that the flags in the command line uses.
type Config struct {
	CommonConfig

	// Fields below here are platform specific.
	Runtimes             map[string]system.Runtime    `json:"runtimes,omitempty"`
	DefaultInitBinary    string                       `json:"default-init,omitempty"`
	CgroupParent         string                       `json:"cgroup-parent,omitempty"`
	EnableSelinuxSupport bool                         `json:"selinux-enabled,omitempty"`
	RemappedRoot         string                       `json:"userns-remap,omitempty"`
	Ulimits              map[string]*container.Ulimit `json:"default-ulimits,omitempty"`
	CPURealtimePeriod    int64                        `json:"cpu-rt-period,omitempty"`
	CPURealtimeRuntime   int64                        `json:"cpu-rt-runtime,omitempty"`
	Init                 bool                         `json:"init,omitempty"`
	InitPath             string                       `json:"init-path,omitempty"`
	SeccompProfile       string                       `json:"seccomp-profile,omitempty"`
	AppArmorProfile      string                       `json:"apparmor-profile,omitempty"`
	ShmSize              opts.MemBytes                `json:"default-shm-size,omitempty"`
	NoNewPrivileges      bool                         `json:"no-new-privileges,omitempty"`
	IpcMode              string                       `json:"default-ipc-mode,omitempty"`
	CgroupNamespaceMode  string                       `json:"default-cgroupns-mode,omitempty"`
	// ResolvConf is the path to the configuration of the host resolver
	ResolvConf string `json:"resolv-conf,omitempty"`
	Rootless   bool   `json:"rootless,omitempty"`
}

// GetExecRoot returns the user configured Exec-root
func (conf *Config) GetExecRoot() string {
	return conf.ExecRoot
}

// GetInitPath returns the configured docker-init path
func (conf *Config) GetInitPath() string {
	if conf.InitPath != "" {
		return conf.InitPath
	}
	if conf.DefaultInitBinary != "" {
		return conf.DefaultInitBinary
	}
	return DefaultInitBinary
}

// LookupInitPath returns an absolute path to the "docker-init" binary by
// searching relevant "libexec" directories (per FHS 3.0 & 2.3) followed by
// PATH.
func (conf *Config) LookupInitPath() (string, error) {
	return lookupBinPath(conf.GetInitPath())
}

// GetResolvConf returns the appropriate resolv.conf
// Check setupResolvConf on how this is selected
func (conf *Config) GetResolvConf() string {
	return conf.ResolvConf
}

// IsSwarmCompatible defines if swarm mode can be enabled in this config.
func (conf *Config) IsSwarmCompatible() error {
	if conf.LiveRestoreEnabled {
		return errors.New("--live-restore daemon configuration is incompatible with swarm mode")
	}
	// Swarm has not yet been updated to use nftables. But, if "iptables" is disabled, it
	// doesn't add rules anyway.
	if conf.FirewallBackend == "nftables" && conf.EnableIPTables && !conf.Features["swarm-nftables"] {
		return errors.New("--firewall-backend=nftables is incompatible with swarm mode")
	}
	return nil
}

// IsRootless returns conf.Rootless on Unix platforms.
func (conf *Config) IsRootless() bool {
	return conf.Rootless
}

func setPlatformDefaults(cfg *Config) error {
	cfg.Ulimits = make(map[string]*container.Ulimit)
	cfg.ShmSize = opts.MemBytes(DefaultShmSize)
	cfg.SeccompProfile = SeccompProfileDefault
	cfg.IpcMode = string(DefaultIpcMode)
	cfg.Runtimes = make(map[string]system.Runtime)
	cfg.CgroupNamespaceMode = string(defaultCgroupNamespaceMode())
	cfg.BridgeConfig.EnableUserlandProxy = true
	cfg.BridgeConfig.UserlandProxyPath = defaultUserlandProxyPath()

	if rootless.RunningWithRootlessKit() {
		cfg.Rootless = true

		dataHome, err := homedir.GetDataHome()
		if err != nil {
			return err
		}
		runtimeDir, err := homedir.GetRuntimeDir()
		if err != nil {
			return err
		}

		cfg.Root = filepath.Join(dataHome, "docker")
		cfg.ExecRoot = filepath.Join(runtimeDir, "docker")
		cfg.Pidfile = filepath.Join(runtimeDir, "docker.pid")
	} else {
		cfg.Root = "/var/lib/docker"
		cfg.ExecRoot = "/var/run/docker"
		cfg.Pidfile = "/var/run/docker.pid"
	}

	return nil
}

// lookupBinPath returns an absolute path to the provided binary by searching
// relevant "libexec" locations (per FHS 3.0 & 2.3) followed by PATH.
func lookupBinPath(binary string) (string, error) {
	if filepath.IsAbs(binary) {
		return binary, nil
	}

	lookupPaths := []string{
		// FHS 3.0: "/usr/libexec includes internal binaries that are not intended to be executed directly by users or shell scripts. Applications may use a single subdirectory under /usr/libexec."
		// https://refspecs.linuxfoundation.org/FHS_3.0/fhs/ch04s07.html
		"/usr/local/libexec/docker",
		"/usr/libexec/docker",

		// FHS 2.3: "/usr/lib includes object files, libraries, and internal binaries that are not intended to be executed directly by users or shell scripts."
		// https://refspecs.linuxfoundation.org/FHS_2.3/fhs-2.3.html#USRLIBLIBRARIESFORPROGRAMMINGANDPA
		"/usr/local/lib/docker",
		"/usr/lib/docker",
	}

	// According to FHS 3.0, it is not necessary to have a subdir here (see note and reference above).
	// If the binary has a `docker-` prefix, let's look it up without the dir prefix.
	if strings.HasPrefix(binary, "docker-") {
		lookupPaths = append(lookupPaths, "/usr/local/libexec")
		lookupPaths = append(lookupPaths, "/usr/libexec")
	}

	for _, dir := range lookupPaths {
		// exec.LookPath has a fast-path short-circuit for paths that contain "/" (skipping the PATH lookup) that then verifies whether the given path is likely to be an actual executable binary (so we invoke that instead of reimplementing the same checks)
		if file, err := exec.LookPath(filepath.Join(dir, binary)); err == nil {
			return file, nil
		}
	}

	// if we checked all the "libexec" directories and found no matches, fall back to PATH
	return exec.LookPath(binary)
}

// validatePlatformConfig checks if any platform-specific configuration settings are invalid.
func validatePlatformConfig(conf *Config) error {
	if err := verifyUserlandProxyConfig(conf); err != nil {
		return err
	}
	if err := verifyDefaultIpcMode(conf.IpcMode); err != nil {
		return err
	}
	if err := validateFixedCIDRV6(conf.FixedCIDRv6); err != nil {
		return errors.Wrap(err, "invalid fixed-cidr-v6")
	}
	if err := validateFirewallBackend(conf.FirewallBackend); err != nil {
		return errors.Wrap(err, "invalid firewall-backend")
	}
	if err := validateFwMarkMask(conf.BridgeAcceptFwMark); err != nil {
		return errors.Wrap(err, "invalid bridge-accept-fwmark")
	}
	return verifyDefaultCgroupNsMode(conf.CgroupNamespaceMode)
}

func verifyDefaultIpcMode(mode string) error {
	const hint = `use "shareable" or "private"`

	dm := container.IpcMode(mode)
	if !dm.Valid() {
		return fmt.Errorf("default IPC mode setting (%v) is invalid; "+hint, dm)
	}
	if dm != "" && !dm.IsPrivate() && !dm.IsShareable() {
		return fmt.Errorf(`IPC mode "%v" is not supported as default value; `+hint, dm)
	}
	return nil
}

func validateFirewallBackend(val string) error {
	switch val {
	case "", "iptables", "nftables":
		return nil
	}
	return errors.New(`allowed values are "iptables" and "nftables"`)
}

func validateFwMarkMask(val string) error {
	if val == "" {
		return nil
	}
	mark, mask, haveMask := strings.Cut(val, "/")
	if _, err := strconv.ParseUint(mark, 0, 32); err != nil {
		return fmt.Errorf("invalid firewall mark %q: %w", val, err)
	}
	if haveMask {
		if _, err := strconv.ParseUint(mask, 0, 32); err != nil {
			return fmt.Errorf("invalid firewall mask %q: %w", val, err)
		}
	}
	return nil
}

func verifyDefaultCgroupNsMode(mode string) error {
	cm := container.CgroupnsMode(mode)
	if !cm.Valid() {
		return fmt.Errorf(`invalid default cgroup namespace (%v): use "host" or "private"`, cm)
	}

	return nil
}
