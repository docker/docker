package daemon

import (
	"testing"

	"github.com/moby/extensions"
	"github.com/moby/moby/v2/daemon/config"
	"gotest.tools/v3/assert"
)

func TestSelectBuiltinExtensions(t *testing.T) {
	t.Parallel()

	defaultExtension := extensions.New(extensions.Declaration{
		ID: "org.example.default.v1",
		Providers: []extensions.Provider{
			{Point: "org.example.first.v0", Impl: "default-first"},
			{Point: "org.example.second.v0", Impl: "default-second"},
		},
	})
	optionalExtension := extensions.New(extensions.Declaration{
		ID: "org.example.optional.v1",
		Providers: []extensions.Provider{
			{Point: "org.example.first.v0", Impl: "optional-first"},
			{Point: "org.example.second.v0", Impl: "optional-second"},
		},
	})
	defaults := []extensions.Extension{defaultExtension}
	optional := []extensions.Extension{optionalExtension}

	for _, tc := range []struct {
		name    string
		config  config.ExtensionsConfig
		want    []extensions.Extension
		wantErr string
	}{
		{
			name: "defaults", want: defaults,
		},
		{
			name:   "configuration does not enable an optional extension",
			config: config.ExtensionsConfig{Configs: config.ExtensionConfigs{{ID: "org.example.optional.v1", Config: map[string]any{"enabled": true}}}},
			want:   defaults,
		},
		{
			name:   "enable does not implicitly replace defaults",
			config: config.ExtensionsConfig{Enable: []string{"org.example.optional.v1", "org.example.optional.v1", "org.example.default.v1"}},
			want:   []extensions.Extension{defaultExtension, optionalExtension},
		},
		{
			name:   "replace whole extension",
			config: config.ExtensionsConfig{Enable: []string{"org.example.optional.v1"}, Disable: []string{"org.example.default.v1"}},
			want:   optional,
		},
		{
			name:   "disable all",
			config: config.ExtensionsConfig{Disable: []string{"org.example.default.v1", "org.example.optional.v1"}},
		},
		{
			name:    "conflicting activation",
			config:  config.ExtensionsConfig{Enable: []string{"org.example.optional.v1"}, Disable: []string{"org.example.optional.v1"}},
			wantErr: "listed in both extensions.enable and extensions.disable",
		},
		{
			name:    "unknown enable ID",
			config:  config.ExtensionsConfig{Enable: []string{"org.example.unknown.v1"}},
			wantErr: `unknown built-in extension "org.example.unknown.v1" (available: org.example.default.v1, org.example.optional.v1)`,
		},
		{
			name:    "unknown disable ID",
			config:  config.ExtensionsConfig{Disable: []string{"org.example.unknown.v1"}},
			wantErr: `unknown built-in extension "org.example.unknown.v1" (available: org.example.default.v1, org.example.optional.v1)`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := selectBuiltinExtensions(tc.config, defaults, optional)
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			assert.NilError(t, err)
			assert.Equal(t, len(got), len(tc.want))
			for i, ext := range got {
				assert.DeepEqual(t, ext.Declaration(), tc.want[i].Declaration())
			}
		})
	}
}
