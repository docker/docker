package config

// PlatformConfig defines platform-specific configuration.
type PlatformConfig struct {
	// NetnsDir is the directory the network namespaces of sandboxes are
	// kept in.
	NetnsDir string
}
