package command

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/containerd/log"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/spf13/pflag"
	"go.opentelemetry.io/otel"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/fs"
)

func defaultOptions(t *testing.T, configFile string) *daemonOptions {
	cfg, err := config.New()
	assert.NilError(t, err)
	opts := newDaemonOptions(cfg)
	opts.flags = &pflag.FlagSet{}
	opts.installFlags(opts.flags)
	installConfigFlags(opts.daemonConfig, opts.flags)
	opts.flags.StringVar(&opts.configFile, "config-file", opts.configFile, "")
	opts.configFile = configFile
	err = opts.flags.Parse([]string{})
	assert.NilError(t, err)
	return opts
}

func TestLoadDaemonCliConfigWithoutOverriding(t *testing.T) {
	opts := defaultOptions(t, "")
	opts.Debug = true

	loadedConfig, err := loadDaemonCliConfig(opts)
	assert.NilError(t, err)
	assert.Assert(t, loadedConfig != nil)
	if !loadedConfig.Debug {
		t.Fatalf("expected debug to be copied from the common flags, got false")
	}
}

func TestLoadDaemonCliConfigWithTLS(t *testing.T) {
	opts := defaultOptions(t, "")
	opts.TLSOptions.CAFile = "/tmp/ca.pem"
	opts.TLS = true

	loadedConfig, err := loadDaemonCliConfig(opts)
	assert.NilError(t, err)
	assert.Assert(t, loadedConfig != nil)
	assert.Check(t, is.Equal("/tmp/ca.pem", loadedConfig.TLSOptions.CAFile))
}

func TestLoadDaemonCliConfigWithConflicts(t *testing.T) {
	tempFile := fs.NewFile(t, "config", fs.WithContent(`{"labels": ["l3=foo"]}`))
	configFile := tempFile.Path()

	opts := defaultOptions(t, configFile)
	flags := opts.flags

	assert.Check(t, flags.Set("config-file", configFile))
	assert.Check(t, flags.Set("label", "l1=bar"))
	assert.Check(t, flags.Set("label", "l2=baz"))

	_, err := loadDaemonCliConfig(opts)
	assert.Check(t, is.ErrorContains(err, "as a flag and in the configuration file: labels"))
}

func TestLoadDaemonCliConfigWithExtensions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		config  string
		flags   []string
		want    config.ExtensionsConfig
		wantErr string
	}{
		{
			name: "nested settings",
			config: `{"extensions": {
				"enable": ["org.example.optional.v1"],
				"disable": ["org.example.default.v1"],
				"dirs": ["/opt/extensions"],
				"configs": {"org.example.test.v1": {"dirs": "opaque"}}
			}}`,
			want: config.ExtensionsConfig{
				Enable: []string{"org.example.optional.v1"}, Disable: []string{"org.example.default.v1"},
				Dirs:    []string{"/opt/extensions"},
				Configs: config.ExtensionConfigs{{ID: "org.example.test.v1", Config: map[string]any{"dirs": "opaque"}}},
			},
		},
		{
			name:   "merge different settings",
			config: `{"extensions": {"configs": {"org.example.test.v1": {"workers": 4}}}}`,
			flags:  []string{"--extension-dir", "/opt/extensions", "--extension-enable", "org.example.optional.v1", "--extension-disable", "org.example.default.v1"},
			want: config.ExtensionsConfig{
				Enable: []string{"org.example.optional.v1"}, Disable: []string{"org.example.default.v1"},
				Dirs:    []string{"/opt/extensions"},
				Configs: config.ExtensionConfigs{{ID: "org.example.test.v1", Config: map[string]any{"workers": float64(4)}}},
			},
		},
		{
			name: "enable flag conflict", config: `{"extensions": {"enable": []}}`,
			flags: []string{"--extension-enable", "org.example.optional.v1"}, wantErr: "as a flag and in the configuration file: extensions.enable",
		},
		{
			name: "disable flag conflict", config: `{"extensions": {"disable": []}}`,
			flags: []string{"--extension-disable", "org.example.default.v1"}, wantErr: "as a flag and in the configuration file: extensions.disable",
		},
		{
			name: "conflicting activation", config: `{"extensions": {"disable": ["org.example.optional.v1"]}}`,
			flags: []string{"--extension-enable", "org.example.optional.v1"}, wantErr: "listed in both extensions.enable and extensions.disable",
		},
		{
			name: "invalid enable ID", config: `{"extensions": {"enable": ["invalid"]}}`, wantErr: "extensions.enable: invalid extension id",
		},
		{
			name: "invalid disable ID", config: `{"extensions": {"disable": ["invalid"]}}`, wantErr: "extensions.disable: invalid extension id",
		},
		{
			name: "directory flag conflict", config: `{"extensions": {"dirs": []}}`,
			flags: []string{"--extension-dir", "/opt/extensions"}, wantErr: "as a flag and in the configuration file: extensions.dirs",
		},
		{
			name: "unknown nested key", config: `{"extensions": {"directories": []}}`, wantErr: "don't match any configuration option: extensions.directories",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempFile := fs.NewFile(t, "config", fs.WithContent(tc.config))
			opts := defaultOptions(t, tempFile.Path())
			assert.NilError(t, opts.flags.Parse(tc.flags))
			cfg, err := loadDaemonCliConfig(opts)
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			assert.NilError(t, err)
			assert.DeepEqual(t, cfg.Extensions, tc.want)
		})
	}
}

func TestLoadDaemonCliWithConflictingNodeGenericResources(t *testing.T) {
	tempFile := fs.NewFile(t, "config", fs.WithContent(`{"node-generic-resources": ["foo=bar", "bar=baz"]}`))
	configFile := tempFile.Path()

	opts := defaultOptions(t, configFile)
	flags := opts.flags

	assert.Check(t, flags.Set("config-file", configFile))
	assert.Check(t, flags.Set("node-generic-resource", "r1=bar"))
	assert.Check(t, flags.Set("node-generic-resource", "r2=baz"))

	_, err := loadDaemonCliConfig(opts)
	assert.Check(t, is.ErrorContains(err, "as a flag and in the configuration file: node-generic-resources"))
}

func TestLoadDaemonCliWithConflictingLabels(t *testing.T) {
	opts := defaultOptions(t, "")
	flags := opts.flags

	assert.Check(t, flags.Set("label", "foo=bar"))
	assert.Check(t, flags.Set("label", "foo=baz"))

	_, err := loadDaemonCliConfig(opts)
	assert.Check(t, is.Error(err, "conflict labels for foo=baz and foo=bar"))
}

func TestLoadDaemonCliWithDuplicateLabels(t *testing.T) {
	opts := defaultOptions(t, "")
	flags := opts.flags

	assert.Check(t, flags.Set("label", "foo=the-same"))
	assert.Check(t, flags.Set("label", "foo=the-same"))

	_, err := loadDaemonCliConfig(opts)
	assert.Check(t, err)
}

func TestLoadDaemonCliConfigWithTLSVerify(t *testing.T) {
	tempFile := fs.NewFile(t, "config", fs.WithContent(`{"tlsverify": true}`))

	opts := defaultOptions(t, tempFile.Path())
	opts.TLSOptions.CAFile = "/tmp/ca.pem"

	loadedConfig, err := loadDaemonCliConfig(opts)
	assert.NilError(t, err)
	assert.Assert(t, loadedConfig != nil)
	assert.Check(t, is.Equal(*loadedConfig.TLS, true))
}

func TestLoadDaemonCliConfigWithExplicitTLSVerifyFalse(t *testing.T) {
	tempFile := fs.NewFile(t, "config", fs.WithContent(`{"tlsverify": false}`))

	opts := defaultOptions(t, tempFile.Path())
	opts.TLSOptions.CAFile = "/tmp/ca.pem"

	loadedConfig, err := loadDaemonCliConfig(opts)
	assert.NilError(t, err)
	assert.Assert(t, loadedConfig != nil)
	assert.Check(t, *loadedConfig.TLS)
}

func TestLoadDaemonCliConfigWithoutTLSVerify(t *testing.T) {
	tempFile := fs.NewFile(t, "config", fs.WithContent(`{}`))

	opts := defaultOptions(t, tempFile.Path())
	opts.TLSOptions.CAFile = "/tmp/ca.pem"

	loadedConfig, err := loadDaemonCliConfig(opts)
	assert.NilError(t, err)
	assert.Assert(t, loadedConfig != nil)
	assert.Check(t, is.Nil(loadedConfig.TLS))
}

func TestLoadDaemonCliConfigWithLogLevel(t *testing.T) {
	tempFile := fs.NewFile(t, "config", fs.WithContent(`{"log-level": "warn"}`))

	opts := defaultOptions(t, tempFile.Path())
	loadedConfig, err := loadDaemonCliConfig(opts)
	assert.NilError(t, err)
	assert.Assert(t, loadedConfig != nil)
	assert.Check(t, is.Equal("warn", loadedConfig.LogLevel))
}

func TestLoadDaemonCliConfigWithLogFormat(t *testing.T) {
	tempFile := fs.NewFile(t, "config", fs.WithContent(`{"log-format": "json"}`))
	defer tempFile.Remove()

	opts := defaultOptions(t, tempFile.Path())
	loadedConfig, err := loadDaemonCliConfig(opts)
	assert.NilError(t, err)
	assert.Assert(t, loadedConfig != nil)
	assert.Check(t, is.Equal(log.JSONFormat, loadedConfig.DaemonLogConfig.LogFormat))
}

func TestLoadDaemonCliConfigWithInvalidLogFormat(t *testing.T) {
	tempFile := fs.NewFile(t, "config", fs.WithContent(`{"log-format": "foo"}`))
	defer tempFile.Remove()

	opts := defaultOptions(t, tempFile.Path())
	_, err := loadDaemonCliConfig(opts)
	assert.Check(t, is.ErrorContains(err, "invalid log format: foo"))
}

func TestLoadDaemonConfigWithEmbeddedOptions(t *testing.T) {
	content := `{"tlscacert": "/etc/certs/ca.pem", "log-driver": "syslog"}`
	tempFile := fs.NewFile(t, "config", fs.WithContent(content))

	opts := defaultOptions(t, tempFile.Path())
	loadedConfig, err := loadDaemonCliConfig(opts)
	assert.NilError(t, err)
	assert.Assert(t, loadedConfig != nil)
	assert.Check(t, is.Equal("/etc/certs/ca.pem", loadedConfig.TLSOptions.CAFile))
	assert.Check(t, is.Equal("syslog", loadedConfig.LogConfig.Type))
}

func TestContainerdRootDir(t *testing.T) {
	cfg := config.Config{CommonConfig: config.CommonConfig{Root: t.TempDir()}}
	t.Setenv(containerdRootEnv, "")
	root, err := containerdRootDir(t.Context(), &cfg)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(root, filepath.Join(cfg.Root, "containerd", "daemon")))

	override := t.TempDir()
	t.Setenv(containerdRootEnv, override)
	root, err = containerdRootDir(t.Context(), &cfg)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(root, override))

	t.Setenv(containerdRootEnv, filepath.Join("relative", "containerd"))
	_, err = containerdRootDir(t.Context(), &cfg)
	assert.Check(t, is.ErrorContains(err, "must be an absolute path"))
}

func TestLoadDaemonConfigWithRegistryOptions(t *testing.T) {
	t.Run("empty config", func(t *testing.T) {
		const content = `{}`
		cfgFile := filepath.Join(t.TempDir(), "daemon.json")
		assert.NilError(t, os.WriteFile(cfgFile, []byte(content), 0o600))

		opts := defaultOptions(t, cfgFile)
		loadedConfig, err := loadDaemonCliConfig(opts)
		assert.NilError(t, err)
		assert.Assert(t, loadedConfig != nil)

		assert.Check(t, is.Nil(loadedConfig.Mirrors))
		assert.Check(t, is.Nil(loadedConfig.InsecureRegistries))
	})

	t.Run("no insecure registries", func(t *testing.T) {
		const content = `{"registry-mirrors": [], "insecure-registries": []}`
		cfgFile := filepath.Join(t.TempDir(), "daemon.json")
		assert.NilError(t, os.WriteFile(cfgFile, []byte(content), 0o600))

		opts := defaultOptions(t, cfgFile)
		loadedConfig, err := loadDaemonCliConfig(opts)
		assert.NilError(t, err)
		assert.Assert(t, loadedConfig != nil)

		assert.Check(t, is.Len(loadedConfig.Mirrors, 0))
		assert.Check(t, is.Len(loadedConfig.InsecureRegistries, 0))
	})

	t.Run("registry options", func(t *testing.T) {
		const content = `{"registry-mirrors": ["https://mirrors.example.com"],"insecure-registries": ["https://insecure-registry.example.com"]}`
		cfgFile := filepath.Join(t.TempDir(), "daemon.json")
		assert.NilError(t, os.WriteFile(cfgFile, []byte(content), 0o600))

		opts := defaultOptions(t, cfgFile)
		loadedConfig, err := loadDaemonCliConfig(opts)
		assert.NilError(t, err)
		assert.Assert(t, loadedConfig != nil)

		assert.Check(t, is.Len(loadedConfig.Mirrors, 1))
		assert.Check(t, is.Len(loadedConfig.InsecureRegistries, 1))
	})
}

func TestConfigureDaemonLogs(t *testing.T) {
	ctx := t.Context()
	err := configureDaemonLogs(ctx, config.DaemonLogConfig{})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(log.InfoLevel, log.GetLevel()))

	// log level should not be changed when passing an invalid value
	err = configureDaemonLogs(ctx, config.DaemonLogConfig{LogLevel: "foobar"})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(log.InfoLevel, log.GetLevel()))

	err = configureDaemonLogs(ctx, config.DaemonLogConfig{LogLevel: "warn"})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(log.WarnLevel, log.GetLevel()))
}

func TestCDISpecDirs(t *testing.T) {
	testCases := []struct {
		description         string
		configContent       string
		specDirs            []string
		expectedCDISpecDirs []string
	}{
		{
			description:         "CDI enabled by default",
			specDirs:            nil,
			expectedCDISpecDirs: []string{"/etc/cdi", "/var/run/cdi"},
		},
		{
			description:         "CDI explicitly enabled and no spec dirs specified returns default",
			specDirs:            nil,
			configContent:       `{"features": {"cdi": true}}`,
			expectedCDISpecDirs: []string{"/etc/cdi", "/var/run/cdi"},
		},
		{
			description:         "CDI enabled and specified spec dirs are returned",
			specDirs:            []string{"/foo/bar", "/baz/qux"},
			configContent:       `{"features": {"cdi": true}}`,
			expectedCDISpecDirs: []string{"/foo/bar", "/baz/qux"},
		},
		{
			description:         "CDI enabled and empty string as spec dir returns empty slice",
			specDirs:            []string{""},
			configContent:       `{"features": {"cdi": true}}`,
			expectedCDISpecDirs: []string{},
		},
		{
			description:         "CDI enabled and empty config option returns empty slice",
			configContent:       `{"cdi-spec-dirs": [], "features": {"cdi": true}}`,
			expectedCDISpecDirs: []string{},
		},
		{
			description:         "CDI disabled and no spec dirs specified returns no cdi spec dirs",
			specDirs:            nil,
			configContent:       `{"features": {"cdi": false}}`,
			expectedCDISpecDirs: nil,
		},
		{
			description:         "CDI disabled and specified spec dirs returns no cdi spec dirs",
			specDirs:            []string{"/foo/bar", "/baz/qux"},
			configContent:       `{"features": {"cdi": false}}`,
			expectedCDISpecDirs: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			tempFile := fs.NewFile(t, "config", fs.WithContent(tc.configContent))
			defer tempFile.Remove()

			opts := defaultOptions(t, tempFile.Path())

			flags := opts.flags
			for _, specDir := range tc.specDirs {
				assert.Check(t, flags.Set("cdi-spec-dir", specDir))
			}

			loadedConfig, err := loadDaemonCliConfig(opts)
			assert.NilError(t, err)

			assert.Check(t, is.DeepEqual(tc.expectedCDISpecDirs, loadedConfig.CDISpecDirs, cmpopts.EquateEmpty()))
		})
	}
}

// TestOtelMeterLeak is a regression test for a memory leak in the OTEL meter
// implementation that was fixed in OTEL v1.30.0.
//
// See:
// - https://github.com/open-telemetry/opentelemetry-go-contrib/issues/5190
// - https://github.com/moby/moby/pull/48690
// - https://github.com/moby/moby/issues/48144
func TestOtelMeterLeak(t *testing.T) {
	meter := otel.Meter("foo")

	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	const counters = 10 * 1000 * 1000
	for range counters {
		_, _ = meter.Int64Counter("bar")
	}

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	allocs := after.Mallocs - before.Mallocs
	t.Log("Allocations:", allocs)

	// currently, with OTel v1.31.0, allocations is 3; add some margin to
	// check for unexpectedly more than that.
	if allocs > 10 {
		t.Fatalf("Possible OTel leak; got more than 10 allocations (allocs: %d).", allocs)
	}
}
