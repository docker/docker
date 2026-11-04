//go:build !linux && !windows

package config

const (
	// StockRuntimeName is used by the 'default-runtime' flag in dockerd as the
	// default value. On Windows keep this empty so the value is auto-detected
	// based on other options.
	StockRuntimeName = ""

	defaultStopTimeout = 30
)

// BridgeConfig is meant to store all the parameters for both the bridge driver and the default bridge network. On
// Windows: 1. "bridge" in this context reference the nat driver and the default nat network; 2. the nat driver has no
// specific parameters, so this struct effectively just stores parameters for the default nat network.
type BridgeConfig struct {
	DefaultBridgeConfig
}

type DefaultBridgeConfig struct {
	commonBridgeConfig
	MTU int `json:"mtu,omitempty"`
}

// Config defines the configuration of a docker daemon.
// It includes json tags to deserialize configuration from a file
// using the same names that the flags in the command line uses.
type Config struct {
	CommonConfig

	// Fields below here are platform specific. (There are none presently
	// for the Windows daemon.)
}

// GetExecRoot returns the user configured Exec-root
func (conf *Config) GetExecRoot() string {
	return ""
}

// GetInitPath returns the configured docker-init path
func (conf *Config) GetInitPath() string {
	return ""
}

// IsSwarmCompatible defines if swarm mode can be enabled in this config
func (conf *Config) IsSwarmCompatible() error {
	return nil
}

// IsRootless returns conf.Rootless on Linux but false on Windows
func (conf *Config) IsRootless() bool {
	return false
}

func setPlatformDefaults(*Config) error { return nil }

// validatePlatformConfig checks if any platform-specific configuration settings are invalid.
func validatePlatformConfig(*Config) error { return nil }

// validatePlatformExecOpt validates if the given exec-opt and value are valid
// for the current platform.
func validatePlatformExecOpt(string, string) error { return nil }
