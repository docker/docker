package daemon

import (
	"testing"

	containertypes "github.com/moby/moby/api/types/container"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestMergeAndVerifyLogConfigNilConfig(t *testing.T) {
	d := &Daemon{defaultLogConfig: containertypes.LogConfig{Type: "local", Config: map[string]string{"max-file": "1"}}}
	cfg := containertypes.LogConfig{Type: d.defaultLogConfig.Type}
	assert.NilError(t, d.mergeAndVerifyLogConfig(&cfg))
}

func TestMergeAndVerifyLogConfigUsesDefaultDriver(t *testing.T) {
	d := &Daemon{defaultLogConfig: containertypes.LogConfig{Type: "local"}}
	cfg := containertypes.LogConfig{}

	assert.NilError(t, d.mergeAndVerifyLogConfig(&cfg))
	assert.Check(t, is.Equal(cfg.Type, "local"))
}
