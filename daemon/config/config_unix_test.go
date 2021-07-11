// +build !windows

package config // import "github.com/docker/docker/daemon/config"

import (
	"testing"

	"github.com/docker/docker/opts"
	units "github.com/docker/go-units"
	"github.com/spf13/pflag"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/fs"
)

func TestGetConflictFreeConfiguration(t *testing.T) {
	configFileData := `
		{
			"debug": true,
			"default-ulimits": {
				"nofile": {
					"Name": "nofile",
					"Hard": 2048,
					"Soft": 1024
				}
			},
			"log-opts": {
				"tag": "test_tag"
			}
		}`

	file := fs.NewFile(t, "docker-config", fs.WithContent(configFileData))
	defer file.Remove()

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	var debug bool
	flags.BoolVarP(&debug, "debug", "D", false, "")
	flags.Var(opts.NewNamedUlimitOpt("default-ulimits", nil), "default-ulimit", "")
	flags.Var(opts.NewNamedMapOpts("log-opts", nil, nil), "log-opt", "")

	cc, err := getConflictFreeConfiguration(file.Path(), flags)
	assert.NilError(t, err)

	assert.Check(t, cc.Debug)

	expectedUlimits := map[string]*units.Ulimit{
		"nofile": {
			Name: "nofile",
			Hard: 2048,
			Soft: 1024,
		},
	}

	assert.Check(t, is.DeepEqual(expectedUlimits, cc.Ulimits))
}

func TestDaemonConfigurationMerge(t *testing.T) {
	configFileData := `
		{
			"debug": true,
			"default-ulimits": {
				"nofile": {
					"Name": "nofile",
					"Hard": 2048,
					"Soft": 1024
				}
			},
			"log-opts": {
				"tag": "test_tag"
			}
		}`

	file := fs.NewFile(t, "docker-config", fs.WithContent(configFileData))
	defer file.Remove()

	c := &Config{
		CommonConfig: CommonConfig{
			AutoRestart: true,
			LogConfig: LogConfig{
				Type:   "syslog",
				Config: map[string]string{"tag": "test"},
			},
		},
	}

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)

	var debug bool
	flags.BoolVarP(&debug, "debug", "D", false, "")
	flags.Var(opts.NewNamedUlimitOpt("default-ulimits", nil), "default-ulimit", "")
	flags.Var(opts.NewNamedMapOpts("log-opts", nil, nil), "log-opt", "")

	cc, err := MergeDaemonConfigurations(c, flags, file.Path())
	assert.NilError(t, err)

	assert.Check(t, cc.Debug)
	assert.Check(t, cc.AutoRestart)

	expectedLogConfig := LogConfig{
		Type:   "syslog",
		Config: map[string]string{"tag": "test_tag"},
	}

	assert.Check(t, is.DeepEqual(expectedLogConfig, cc.LogConfig))

	expectedUlimits := map[string]*units.Ulimit{
		"nofile": {
			Name: "nofile",
			Hard: 2048,
			Soft: 1024,
		},
	}

	assert.Check(t, is.DeepEqual(expectedUlimits, cc.Ulimits))
}

func TestDaemonConfigurationMergeShmSize(t *testing.T) {
	data := `{"default-shm-size": "1g"}`

	file := fs.NewFile(t, "docker-config", fs.WithContent(data))
	defer file.Remove()

	c := &Config{}

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	shmSize := opts.MemBytes(DefaultShmSize)
	flags.Var(&shmSize, "default-shm-size", "")

	cc, err := MergeDaemonConfigurations(c, flags, file.Path())
	assert.NilError(t, err)

	expectedValue := 1 * 1024 * 1024 * 1024
	assert.Check(t, is.Equal(int64(expectedValue), cc.ShmSize.Value()))
}

func TestVerifyUlimits(t *testing.T) {
	assert.NilError(
		t,
		VerifyUlimits(map[string]*units.Ulimit{
			"nofile": {
				Name: "nofile",
				Hard: 2048,
				Soft: 1024,
			},
		}),
	)

	assert.Error(
		t,
		VerifyUlimits(map[string]*units.Ulimit{
			"nofile": {
				Name: "nofile",
				Hard: 2048,
				Soft: 4096,
			},
		}),
		"ulimit soft limit must be less than or equal to hard limit: 4096 > 2048",
	)

	assert.Error(
		t,
		VerifyUlimits(map[string]*units.Ulimit{
			"nosuchulimit": {
				Name: "nosuchulimit",
				Hard: 2048,
				Soft: 1024,
			},
		}),
		"invalid ulimit name nosuchulimit",
	)
}
