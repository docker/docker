//go:build unix

package command

import (
	"testing"

	"github.com/moby/moby/v2/daemon/config"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/fs"
)

func TestLoadDaemonCliConfigWithDaemonFlags(t *testing.T) {
	content := `{"log-opts": {"max-size": "1k"}}`
	tempFile := fs.NewFile(t, "config", fs.WithContent(content))

	opts := defaultOptions(t, tempFile.Path())
	opts.Debug = true
	opts.daemonConfig.DaemonLogConfig.LogLevel = "info"
	assert.Check(t, opts.flags.Set("selinux-enabled", "true"))

	loadedConfig, err := loadDaemonCliConfig(opts)
	assert.NilError(t, err)
	assert.Assert(t, loadedConfig != nil)

	assert.Check(t, loadedConfig.Debug)
	assert.Check(t, is.Equal("info", loadedConfig.DaemonLogConfig.LogLevel))
	assert.Check(t, loadedConfig.EnableSelinuxSupport)
	assert.Check(t, is.Equal("local", loadedConfig.LogConfig.Type))
	assert.Check(t, is.Equal("1k", loadedConfig.LogConfig.Config["max-size"]))
}

func TestLoadDaemonConfigWithNetwork(t *testing.T) {
	content := `{"bip": "127.0.0.2/8", "bip6": "fd98:e5f2:e637::1/64", "ip": "127.0.0.1"}`
	tempFile := fs.NewFile(t, "config", fs.WithContent(content))

	opts := defaultOptions(t, tempFile.Path())
	loadedConfig, err := loadDaemonCliConfig(opts)
	assert.NilError(t, err)
	assert.Assert(t, loadedConfig != nil)

	assert.Check(t, is.Equal(loadedConfig.IP, "127.0.0.2/8"))
	assert.Check(t, is.Equal(loadedConfig.IP6, "fd98:e5f2:e637::1/64"))
	assert.Check(t, is.Equal(loadedConfig.DefaultIP.String(), "127.0.0.1"))
}

func TestLoadDaemonConfigWithMapOptions(t *testing.T) {
	content := `{"log-opts": {"tag": "test"}}`
	tempFile := fs.NewFile(t, "config", fs.WithContent(content))

	opts := defaultOptions(t, tempFile.Path())
	loadedConfig, err := loadDaemonCliConfig(opts)
	assert.NilError(t, err)
	assert.Assert(t, loadedConfig != nil)
	assert.Check(t, loadedConfig.LogConfig.Config != nil)
	assert.Check(t, is.Equal("test", loadedConfig.LogConfig.Config["tag"]))
}

func TestLoadDaemonConfigWithTrueDefaultValues(t *testing.T) {
	content := `{ "userland-proxy": false }`
	tempFile := fs.NewFile(t, "config", fs.WithContent(content))

	opts := defaultOptions(t, tempFile.Path())
	loadedConfig, err := loadDaemonCliConfig(opts)
	assert.NilError(t, err)
	assert.Assert(t, loadedConfig != nil)

	assert.Check(t, !loadedConfig.EnableUserlandProxy)

	// make sure reloading doesn't generate configuration
	// conflicts after normalizing boolean values.
	reload := func(reloadedConfig *config.Config) {
		assert.Check(t, !reloadedConfig.EnableUserlandProxy)
	}
	assert.Check(t, config.Reload(opts.configFile, opts.flags, reload))
}

func TestLoadDaemonConfigWithTrueDefaultValuesLeaveDefaults(t *testing.T) {
	tempFile := fs.NewFile(t, "config", fs.WithContent(`{}`))

	opts := defaultOptions(t, tempFile.Path())
	loadedConfig, err := loadDaemonCliConfig(opts)
	assert.NilError(t, err)
	assert.Assert(t, loadedConfig != nil)

	assert.Check(t, loadedConfig.EnableUserlandProxy)
}
