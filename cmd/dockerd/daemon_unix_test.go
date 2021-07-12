// +build !windows

package main

import (
	"testing"

	"github.com/docker/docker/daemon/config"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/fs"
)

func TestLoadDaemonCliConfigWithDaemonFlags(t *testing.T) {
	content := `{"log-opts": {"max-size": "1k"}}`
	tempFile := fs.NewFile(t, "config", fs.WithContent(content))
	defer tempFile.Remove()

	opts := defaultOptions(t, tempFile.Path())
	opts.Debug = true
	opts.LogLevel = "info"
	assert.Check(t, opts.flags.Set("selinux-enabled", "true"))

	loadedConfig, err := loadDaemonCliConfig(opts)
	assert.NilError(t, err)
	assert.Assert(t, loadedConfig != nil)

	assert.Check(t, loadedConfig.Debug)
	assert.Check(t, is.Equal("info", loadedConfig.LogLevel))
	assert.Check(t, loadedConfig.EnableSelinuxSupport)
	assert.Check(t, is.Equal("json-file", loadedConfig.LogConfig.Type))
	assert.Check(t, is.Equal("1k", loadedConfig.LogConfig.Config["max-size"]))
}

func TestLoadDaemonConfigWithNetwork(t *testing.T) {
	content := `{"bip": "127.0.0.2", "ip": "127.0.0.1"}`
	tempFile := fs.NewFile(t, "config", fs.WithContent(content))
	defer tempFile.Remove()

	opts := defaultOptions(t, tempFile.Path())
	loadedConfig, err := loadDaemonCliConfig(opts)
	assert.NilError(t, err)
	assert.Assert(t, loadedConfig != nil)

	assert.Check(t, is.Equal("127.0.0.2", loadedConfig.IP))
	assert.Check(t, is.Equal("127.0.0.1", loadedConfig.DefaultIP.String()))
}

func TestLoadDaemonConfigWithMapOptions(t *testing.T) {
	content := `{
		"log-opts": {"tag": "test"}
}`
	tempFile := fs.NewFile(t, "config", fs.WithContent(content))
	defer tempFile.Remove()

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
	defer tempFile.Remove()

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
	defer tempFile.Remove()

	opts := defaultOptions(t, tempFile.Path())
	loadedConfig, err := loadDaemonCliConfig(opts)
	assert.NilError(t, err)
	assert.Assert(t, loadedConfig != nil)

	assert.Check(t, loadedConfig.EnableUserlandProxy)
}
