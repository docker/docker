package config

import (
	"testing"

	"github.com/moby/moby/v2/daemon/pkg/opts"
	"github.com/spf13/pflag"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestDaemonConfigurationMerge(t *testing.T) {
	configFile := makeConfigFile(t, `
		{
			"debug": true
		}`)

	conf, err := New()
	assert.NilError(t, err)

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.BoolVarP(&conf.Debug, "debug", "D", false, "")
	flags.BoolVarP(&conf.AutoRestart, "restart", "r", true, "")
	flags.StringVar(&conf.LogConfig.Type, "log-driver", "local", "")
	flags.Var(opts.NewNamedMapOpts("log-opts", conf.LogConfig.Config, nil), "log-opt", "")
	assert.Check(t, flags.Set("restart", "true"))
	assert.Check(t, flags.Set("log-driver", "syslog"))
	assert.Check(t, flags.Set("log-opt", "tag=from_flag"))

	cc, err := MergeDaemonConfigurations(conf, flags, configFile)
	assert.NilError(t, err)

	assert.Check(t, cc.Debug)
	assert.Check(t, cc.AutoRestart)

	expectedLogConfig := LogConfig{
		Type:   "syslog",
		Config: map[string]string{"tag": "from_flag"},
	}

	assert.Check(t, is.DeepEqual(expectedLogConfig, cc.LogConfig))
}
