package config

import (
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge"
)

// PlatformConfig defines platform-specific configuration.
type PlatformConfig struct {
	BridgeConfig bridge.Configuration
	// NetnsDir is the directory the network namespaces of sandboxes are
	// kept in.
	NetnsDir string
}

// OptionBridgeConfig returns an option setter for bridge driver config.
func OptionBridgeConfig(config bridge.Configuration) Option {
	return func(c *Config) {
		c.BridgeConfig = config
	}
}
