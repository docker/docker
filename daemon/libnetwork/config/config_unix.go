//go:build linux || freebsd

package config

// OptionNetnsDir returns an option setter for the directory the network
// namespaces of sandboxes are kept in.
func OptionNetnsDir(dir string) Option {
	return func(c *Config) {
		c.NetnsDir = dir
	}
}
