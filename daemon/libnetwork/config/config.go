package config

import (
	"context"
	"strings"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/libnetwork/cluster"
	"github.com/moby/moby/v2/daemon/libnetwork/ipamutils"
	"github.com/moby/moby/v2/daemon/libnetwork/kvstore"
	"github.com/moby/moby/v2/pkg/plugingetter"
)

const (
	warningThNetworkControlPlaneMTU = 1500
	minimumNetworkControlPlaneMTU   = 500
)

// Config encapsulates configurations of various Libnetwork components
type Config struct {
	PlatformConfig

	// KVStore is where the controller keeps its state. If it is nil, the
	// controller keeps its state in memory.
	KVStore                kvstore.Store
	DefaultNetwork         string
	DefaultDriver          string
	Labels                 []string
	ClusterProvider        cluster.Provider
	NetworkControlPlaneMTU int
	DefaultAddressPool     []*ipamutils.NetworkToSplit
	ActiveSandboxes        map[string]any
	PluginGetter           plugingetter.PluginGetter
	FirewallBackend        string
	Rootless               bool
	EnableUserlandProxy    bool
	UserlandProxyPath      string
}

// New creates a new Config and initializes it with the given Options.
func New(opts ...Option) *Config {
	cfg := &Config{}

	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}

	return cfg
}

// Option is an option setter function type used to pass various configurations
// to the controller
type Option func(c *Config)

// OptionDefaultNetwork function returns an option setter for a default network
func OptionDefaultNetwork(dn string) Option {
	return func(c *Config) {
		log.G(context.TODO()).Debugf("Option DefaultNetwork: %s", dn)
		c.DefaultNetwork = strings.TrimSpace(dn)
	}
}

// OptionDefaultDriver function returns an option setter for default driver
func OptionDefaultDriver(dd string) Option {
	return func(c *Config) {
		log.G(context.TODO()).Debugf("Option DefaultDriver: %s", dd)
		c.DefaultDriver = strings.TrimSpace(dd)
	}
}

// OptionDefaultAddressPoolConfig function returns an option setter for default address pool
func OptionDefaultAddressPoolConfig(addressPool []*ipamutils.NetworkToSplit) Option {
	return func(c *Config) {
		c.DefaultAddressPool = addressPool
	}
}

// WithKVStore returns an option setter for the store the controller keeps its
// state in.
func WithKVStore(kv kvstore.Store) Option {
	return func(c *Config) {
		c.KVStore = kv
	}
}

// OptionPluginGetter returns a plugingetter for remote drivers.
func OptionPluginGetter(pg plugingetter.PluginGetter) Option {
	return func(c *Config) {
		c.PluginGetter = pg
	}
}

// OptionNetworkControlPlaneMTU function returns an option setter for control plane MTU
func OptionNetworkControlPlaneMTU(exp int) Option {
	return func(c *Config) {
		log.G(context.TODO()).Debugf("Network Control Plane MTU: %d", exp)
		if exp < warningThNetworkControlPlaneMTU {
			log.G(context.TODO()).Warnf("Received a MTU of %d, this value is very low, the network control plane can misbehave,"+
				" defaulting to minimum value (%d)", exp, minimumNetworkControlPlaneMTU)
			if exp < minimumNetworkControlPlaneMTU {
				exp = minimumNetworkControlPlaneMTU
			}
		}
		c.NetworkControlPlaneMTU = exp
	}
}

// OptionActiveSandboxes function returns an option setter for passing the sandboxes
// which were active during previous daemon life
func OptionActiveSandboxes(sandboxes map[string]any) Option {
	return func(c *Config) {
		c.ActiveSandboxes = sandboxes
	}
}

// OptionFirewallBackend returns an option setter for selection of the firewall backend.
func OptionFirewallBackend(val string) Option {
	return func(c *Config) {
		c.FirewallBackend = val
	}
}

// OptionRootless returns an option setter that indicates whether the daemon is
// running in rootless mode.
func OptionRootless(rootless bool) Option {
	return func(c *Config) {
		c.Rootless = rootless
	}
}

// OptionUserlandProxy returns an option setter that indicates whether the
// userland proxy is enabled, and sets the path to the proxy binary.
func OptionUserlandProxy(enabled bool, proxyPath string) Option {
	return func(c *Config) {
		c.EnableUserlandProxy = enabled
		c.UserlandProxyPath = proxyPath
	}
}
