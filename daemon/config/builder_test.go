package config

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/moby/moby/v2/daemon/internal/filters"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/fs"
)

// TestBuilderProxyNetwork verifies that the proxy-network builder option is
// correctly parsed from daemon configuration.
func TestBuilderProxyNetwork(t *testing.T) {
	tests := []struct {
		doc      string
		config   string
		expected bool
	}{
		{doc: "omitted", config: `{}`, expected: false},
		{doc: "empty builder", config: `{"builder": {}}`, expected: false},
		{doc: "explicit false", config: `{"builder": {"proxy-network": false}}`, expected: false},
		{doc: "explicit true", config: `{"builder": {"proxy-network": true}}`, expected: true},
		{doc: "with other fields", config: `{"builder": {"gc": {"enabled": true}, "proxy-network": true}}`, expected: true},
	}
	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			tempFile := fs.NewFile(t, "config", fs.WithContent(tc.config))
			defer tempFile.Remove()

			cfg, err := MergeDaemonConfigurations(&Config{}, nil, tempFile.Path())
			assert.NilError(t, err)
			assert.Equal(t, cfg.Builder.ProxyNetwork, tc.expected)
		})
	}
}

func TestBuilderGC(t *testing.T) {
	tempFile := fs.NewFile(t, "config", fs.WithContent(`{
  "builder": {
    "gc": {
      "enabled": true,
      "policy": [
        {"reservedSpace": "10GB", "filter": ["unused-for=2200h"]},
        {"reservedSpace": "50GB", "filter": {"unused-for": {"3300h": true}}},
        {"reservedSpace": "100GB", "minFreeSpace": "10GB", "maxUsedSpace": "200GB", "all": true}
      ]
    }
  }
}`))
	defer tempFile.Remove()
	configFile := tempFile.Path()

	cfg, err := MergeDaemonConfigurations(&Config{}, nil, configFile)
	assert.NilError(t, err)
	assert.Assert(t, cfg.Builder.GC.IsEnabled())
	f1 := filters.NewArgs()
	f1.Add("unused-for", "2200h")
	f2 := filters.NewArgs()
	f2.Add("unused-for", "3300h")
	expectedPolicy := []BuilderGCRule{
		{ReservedSpace: "10GB", Filter: BuilderGCFilter(f1)},
		{ReservedSpace: "50GB", Filter: BuilderGCFilter(f2)}, /* parsed from deprecated form */
		{ReservedSpace: "100GB", MinFreeSpace: "10GB", MaxUsedSpace: "200GB", All: true},
	}
	assert.DeepEqual(t, cfg.Builder.GC.Policy, expectedPolicy, cmp.AllowUnexported(BuilderGCFilter{}))
	// double check to please the skeptics
	assert.Assert(t, filters.Args(cfg.Builder.GC.Policy[0].Filter).UniqueExactMatch("unused-for", "2200h"))
	assert.Assert(t, filters.Args(cfg.Builder.GC.Policy[1].Filter).UniqueExactMatch("unused-for", "3300h"))
}

func TestBuilderGC_DeprecatedKeepStorage(t *testing.T) {
	tempFile := fs.NewFile(t, "config", fs.WithContent(`{
  "builder": {
    "gc": {
      "enabled": true,
      "policy": [
        {"keepStorage": "10GB", "filter": ["unused-for=2200h"]},
        {"keepStorage": "50GB", "filter": {"unused-for": {"3300h": true}}},
        {"keepStorage": "100GB", "all": true}
      ]
    }
  }
}`))
	configFile := tempFile.Path()

	cfg, err := MergeDaemonConfigurations(&Config{}, nil, configFile)
	assert.NilError(t, err)
	assert.Assert(t, cfg.Builder.GC.IsEnabled())
	f1 := filters.NewArgs()
	f1.Add("unused-for", "2200h")
	f2 := filters.NewArgs()
	f2.Add("unused-for", "3300h")
	expectedPolicy := []BuilderGCRule{
		{ReservedSpace: "10GB", Filter: BuilderGCFilter(f1)},
		{ReservedSpace: "50GB", Filter: BuilderGCFilter(f2)}, /* parsed from deprecated form */
		{ReservedSpace: "100GB", All: true},
	}
	assert.DeepEqual(t, cfg.Builder.GC.Policy, expectedPolicy, cmp.AllowUnexported(BuilderGCFilter{}))
	// double check to please the skeptics
	assert.Assert(t, filters.Args(cfg.Builder.GC.Policy[0].Filter).UniqueExactMatch("unused-for", "2200h"))
	assert.Assert(t, filters.Args(cfg.Builder.GC.Policy[1].Filter).UniqueExactMatch("unused-for", "3300h"))
}

// TestBuilderGCFilterUnmarshal is a regression test for https://github.com/moby/moby/issues/44361,
// where and incorrectly formatted gc filter option ("unused-for2200h",
// missing a "=" separator). resulted in a panic during unmarshal.
func TestBuilderGCFilterUnmarshal(t *testing.T) {
	var cfg BuilderGCConfig
	err := json.Unmarshal([]byte(`{"poliCy": [{"reservedSpace": "10GB", "filter": ["unused-for2200h"]}]}`), &cfg)
	assert.Check(t, err)
	expectedPolicy := []BuilderGCRule{{
		ReservedSpace: "10GB", Filter: BuilderGCFilter(filters.NewArgs(filters.Arg("unused-for2200h", ""))),
	}}
	assert.DeepEqual(t, cfg.Policy, expectedPolicy, cmp.AllowUnexported(BuilderGCFilter{}))
}

func TestBuilderGC_Enabled(t *testing.T) {
	tests := []struct {
		doc, config string
		expected    bool
	}{
		{doc: "empty config", config: ``, expected: true},
		{doc: "empty json", config: `{}`, expected: true},
		{doc: "empty builder", config: `{"builder": {}}`, expected: true},
		{doc: "empty gc", config: `{"builder": {"gc": {}}}`, expected: true},
		{doc: "gc enabled", config: `{"builder": {"gc": {"enabled": true}}}`, expected: true},
		{doc: "gc disabled", config: `{"builder": {"gc": {"enabled": false}}}`, expected: false},
		{doc: "gc with policy", config: `{"builder": {"gc": {"policy": []}}}`, expected: true},
	}
	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			tempFile := fs.NewFile(t, "config", fs.WithContent(tc.config))
			configFile := tempFile.Path()

			cfg, err := MergeDaemonConfigurations(&Config{}, nil, configFile)
			assert.NilError(t, err)
			assert.Equal(t, cfg.Builder.GC.IsEnabled(), tc.expected)
		})
	}
}

func TestBuilderHistoryMaxEntries(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantSet bool
		want    int64
	}{
		{name: "unset", config: `{"builder":{"history":{"maxAge":"48h"}}}`},
		{name: "disabled", config: `{"builder":{"history":{"maxEntries":0}}}`, wantSet: true},
		{name: "configured", config: `{"builder":{"history":{"maxEntries":12}}}`, wantSet: true, want: 12},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tempFile := fs.NewFile(t, "config", fs.WithContent(tc.config))
			cfg, err := MergeDaemonConfigurations(&Config{}, nil, tempFile.Path())
			assert.NilError(t, err)
			assert.Assert(t, cfg.Builder.History != nil)
			if tc.wantSet {
				assert.Assert(t, cfg.Builder.History.MaxEntries != nil)
				assert.Equal(t, *cfg.Builder.History.MaxEntries, tc.want)
			} else {
				assert.Assert(t, cfg.Builder.History.MaxEntries == nil)
			}
		})
	}
}
