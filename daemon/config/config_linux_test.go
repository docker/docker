package config // import "github.com/docker/docker/daemon/config"

import (
	"testing"

	"github.com/docker/docker/api/types"
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

func TestUnixValidateConfigurationErrors(t *testing.T) {
	testCases := []struct {
		config *Config
	}{
		// Can't override the stock runtime
		{
			config: &Config{
				Runtimes: map[string]types.Runtime{
					StockRuntimeName: {},
				},
			},
		},
		// Default runtime should be present in runtimes
		{
			config: &Config{
				Runtimes: map[string]types.Runtime{
					"foo": {},
				},
				DefaultRuntime: "bar",
			},
		},
	}
	for _, tc := range testCases {
		err := Validate(tc.config)
		if err == nil {
			t.Fatalf("expected error, got nil for config %v", tc.config)
		}
	}
}

func TestUnixGetInitPath(t *testing.T) {
	testCases := []struct {
		config           *Config
		expectedInitPath string
	}{
		{
			config: &Config{
				InitPath: "some-init-path",
			},
			expectedInitPath: "some-init-path",
		},
		{
			config: &Config{
				DefaultInitBinary: "foo-init-bin",
			},
			expectedInitPath: "foo-init-bin",
		},
		{
			config: &Config{
				InitPath:          "init-path-A",
				DefaultInitBinary: "init-path-B",
			},
			expectedInitPath: "init-path-A",
		},
		{
			config:           &Config{},
			expectedInitPath: "docker-init",
		},
	}
	for _, tc := range testCases {
		initPath := tc.config.GetInitPath()
		if initPath != tc.expectedInitPath {
			t.Fatalf("expected initPath to be %v, got %v", tc.expectedInitPath, initPath)
		}
	}
}
