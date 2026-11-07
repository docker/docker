package config

import (
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"dario.cat/mergo"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/moby/moby/v2/daemon/libnetwork/ipamutils"
	"github.com/moby/moby/v2/daemon/pkg/opts"
	"github.com/moby/moby/v2/daemon/pkg/registry"
	"github.com/spf13/pflag"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/unicode"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

func makeConfigFile(t *testing.T, content string) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "daemon.json")
	err := os.WriteFile(name, []byte(content), 0o666)
	assert.NilError(t, err)
	return name
}

func TestDaemonConfigurationNotFound(t *testing.T) {
	_, err := MergeDaemonConfigurations(&Config{}, nil, "/tmp/foo-bar-baz-docker")
	assert.Check(t, os.IsNotExist(err), "got: %[1]T: %[1]v", err)
}

func TestDaemonConfigurationMalformedJSON(t *testing.T) {
	configFile := makeConfigFile(t, `{"debug": tru`)

	_, err := MergeDaemonConfigurations(&Config{}, nil, configFile)
	assert.Check(t, is.ErrorContains(err, "invalid JSON"))
	var syntaxErr *json.SyntaxError
	assert.Check(t, errors.As(err, &syntaxErr), `got: %[1]T: %[1]v`, err)
}

func TestDaemonConfigurationExtensionConfig(t *testing.T) {
	configFile := makeConfigFile(t, `{
		"extensions": {
			"configs": {
				"org.example.foo.v1": {"plugin_path": "/opt/foo", "enabled": true},
				"com.docker.compose.v1": {"workers": 4}
			}
		}
	}`)
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	cfg, err := MergeDaemonConfigurations(&Config{}, flags, configFile)
	assert.NilError(t, err)
	assert.DeepEqual(t, cfg.Extensions.Configs, ExtensionConfigs{
		{ID: "org.example.foo.v1", Config: map[string]any{
			"plugin_path": "/opt/foo",
			"enabled":     true,
		}},
		{ID: "com.docker.compose.v1", Config: map[string]any{
			"workers": float64(4),
		}},
	})
}

func TestDaemonConfigurationRejectsTopLevelExtensionKeys(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		config string
		key    string
	}{
		{
			name: "activation", key: "extensions.disable",
			config: `{"extensions.disable": ["org.mobyproject.namesgenerator.legacy.v1"]}`,
		},
		{
			name: "enable flag spelling", key: "extension-enable",
			config: `{"extension-enable": ["org.example.optional.v1"]}`,
		},
		{
			name: "disable flag spelling", key: "extension-disable",
			config: `{"extension-disable": ["org.mobyproject.namesgenerator.legacy.v1"]}`,
		},
		{
			name: "configuration", key: "extensions.configs",
			config: `{"extensions.configs": {"org.example.test.v1": {"workers": 4}}}`,
		},
		{
			name: "unknown empty object", key: "extensions.unknown",
			config: `{"extensions.unknown": {}}`,
		},
		{
			name: "mixed nested and dotted settings", key: "extensions.disable",
			config: `{"extensions": {"disable": []}, "extensions.disable": ["org.mobyproject.namesgenerator.legacy.v1"]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			configFile := makeConfigFile(t, tc.config)
			cfg := &Config{}
			var enabled, disabled []string
			flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
			flags.Var(opts.NewNamedListOptsRef("extensions.enable", &enabled, nil), "extension-enable", "")
			flags.Var(opts.NewNamedListOptsRef("extensions.disable", &disabled, nil), "extension-disable", "")
			for name, flagSet := range map[string]*pflag.FlagSet{"with flags": flags, "without flags": nil} {
				t.Run(name, func(t *testing.T) {
					_, err := MergeDaemonConfigurations(cfg, flagSet, configFile)
					assert.ErrorContains(t, err, `invalid configuration directive "`+tc.key+`": use the nested "extensions" object`)
				})
			}
		})
	}
}

func TestExtensionConfigsMarshalOrder(t *testing.T) {
	configs := ExtensionConfigs{
		{ID: "org.example.first.v1", Config: map[string]any{"name": "first"}},
		{ID: "org.example.second.v1", Config: map[string]any{"name": "second"}},
	}

	encoded, err := json.Marshal(configs)
	assert.NilError(t, err)
	assert.Equal(t, string(encoded), `{"org.example.first.v1":{"name":"first"},"org.example.second.v1":{"name":"second"}}`)
}

func TestExtensionConfigsNullAndEmpty(t *testing.T) {
	var configs ExtensionConfigs
	assert.NilError(t, json.Unmarshal([]byte(`null`), &configs))
	assert.Assert(t, configs == nil)

	assert.NilError(t, json.Unmarshal([]byte(`{}`), &configs))
	assert.Assert(t, configs != nil)
	assert.Equal(t, len(configs), 0)

	encoded, err := json.Marshal(configs)
	assert.NilError(t, err)
	assert.Equal(t, string(encoded), `{}`)

	configs = nil
	encoded, err = json.Marshal(configs)
	assert.NilError(t, err)
	assert.Equal(t, string(encoded), `null`)
}

func TestDaemonConfigurationInvalidExtensionConfigID(t *testing.T) {
	configFile := makeConfigFile(t, `{
		"extensions": {
			"configs": {
				"org.example.foo": {"enabled": true}
			}
		}
	}`)

	_, err := MergeDaemonConfigurations(&Config{}, nil, configFile)
	assert.ErrorContains(t, err, `invalid extensions.configs key "org.example.foo"`)
}

func TestDaemonConfigurationExtensionDirs(t *testing.T) {
	configFile := makeConfigFile(t, `{
		"extensions": {
			"dirs": ["/opt/extensions/one", "/opt/extensions/two"]
		}
	}`)

	cfg, err := MergeDaemonConfigurations(&Config{}, nil, configFile)
	assert.NilError(t, err)
	assert.DeepEqual(t, cfg.Extensions.Dirs, []string{"/opt/extensions/one", "/opt/extensions/two"})
}

// TestDaemonConfigurationUnicodeVariations feeds various variations of Unicode into the JSON parser, ensuring that we
// respect a BOM and otherwise default to UTF-8.
func TestDaemonConfigurationUnicodeVariations(t *testing.T) {
	jsonData := `{"debug": true}`

	testCases := []struct {
		name     string
		encoding encoding.Encoding
	}{
		{
			name:     "UTF-8",
			encoding: unicode.UTF8,
		},
		{
			name:     "UTF-8 (with BOM)",
			encoding: unicode.UTF8BOM,
		},
		{
			name:     "UTF-16 (BE with BOM)",
			encoding: unicode.UTF16(unicode.BigEndian, unicode.UseBOM),
		},
		{
			name:     "UTF-16 (LE with BOM)",
			encoding: unicode.UTF16(unicode.LittleEndian, unicode.UseBOM),
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			encodedJson, err := tc.encoding.NewEncoder().String(jsonData)
			assert.NilError(t, err)
			configFile := makeConfigFile(t, encodedJson)
			_, err = MergeDaemonConfigurations(&Config{}, nil, configFile)
			assert.NilError(t, err)
		})
	}
}

// TestDaemonConfigurationInvalidUnicode ensures that the JSON parser returns a useful error message if malformed UTF-8
// is provided.
func TestDaemonConfigurationInvalidUnicode(t *testing.T) {
	configFileBOM := makeConfigFile(t, "\xef\xbb\xbf{\"debug\": true}\xff")
	_, err := MergeDaemonConfigurations(&Config{}, nil, configFileBOM)
	assert.ErrorIs(t, err, encoding.ErrInvalidUTF8)

	configFileNoBOM := makeConfigFile(t, "{\"debug\": true}\xff")
	_, err = MergeDaemonConfigurations(&Config{}, nil, configFileNoBOM)
	assert.ErrorIs(t, err, encoding.ErrInvalidUTF8)
}

func TestFindConfigurationConflicts(t *testing.T) {
	config := map[string]any{"authorization-plugins": "foobar"}
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)

	flags.String("authorization-plugins", "", "")
	assert.Check(t, flags.Set("authorization-plugins", "asdf"))
	assert.Check(t, is.ErrorContains(findConfigurationConflicts(config, flags), "authorization-plugins: (from flag: asdf, from file: foobar)"))
}

func TestFindConfigurationConflictsWithNamedOptions(t *testing.T) {
	config := map[string]any{"hosts": []string{"qwer"}}
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)

	var hosts []string
	flags.VarP(opts.NewNamedListOptsRef("hosts", &hosts, opts.ValidateHost), "host", "H", "Daemon socket(s) to connect to")
	assert.Check(t, flags.Set("host", "tcp://127.0.0.1:4444"))
	assert.Check(t, flags.Set("host", "unix:///var/run/docker.sock"))
	assert.Check(t, is.ErrorContains(findConfigurationConflicts(config, flags), "hosts"))
}

func TestDaemonConfigurationMergeConflicts(t *testing.T) {
	configFile := makeConfigFile(t, `{"debug": true}`)

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.Bool("debug", false, "")
	assert.Check(t, flags.Set("debug", "false"))

	_, err := MergeDaemonConfigurations(&Config{}, flags, configFile)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "debug") {
		t.Fatalf("expected debug conflict, got %v", err)
	}
}

func TestDaemonConfigurationMergeConcurrent(t *testing.T) {
	configFile := makeConfigFile(t, `{"max-concurrent-downloads": 1}`)

	_, err := MergeDaemonConfigurations(&Config{}, nil, configFile)
	assert.NilError(t, err)
}

func TestDaemonConfigurationMergeConcurrentError(t *testing.T) {
	configFile := makeConfigFile(t, `{"max-concurrent-downloads": -1}`)

	_, err := MergeDaemonConfigurations(&Config{}, nil, configFile)
	assert.ErrorContains(t, err, `invalid max concurrent downloads: -1`)
}

func TestDefaultStopTimeout(t *testing.T) {
	for _, tc := range []struct {
		name     string
		config   string
		expected int
	}{
		{
			name:     "positive",
			config:   `{"default-stop-timeout": 42}`,
			expected: 42,
		},
		{
			name:     "zero",
			config:   `{"default-stop-timeout": 0}`,
			expected: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configFile := makeConfigFile(t, tc.config)
			flagsConfig := &Config{
				CommonConfig: CommonConfig{
					ContainerDefaults: ContainerDefaults{
						DefaultStopTimeout: 20,
					},
				},
			}
			flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
			flags.IntVar(&flagsConfig.DefaultStopTimeout, "default-stop-timeout", flagsConfig.DefaultStopTimeout, "")

			merged, err := MergeDaemonConfigurations(flagsConfig, flags, configFile)
			assert.NilError(t, err)
			assert.Equal(t, merged.DefaultStopTimeout, tc.expected)
		})
	}
}

func TestDaemonConfigurationMergeConflictsWithInnerStructs(t *testing.T) {
	configFile := makeConfigFile(t, `{"tlscacert": "/etc/certificates/ca.pem"}`)

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("tlscacert", "", "")
	assert.Check(t, flags.Set("tlscacert", "~/.docker/ca.pem"))

	_, err := MergeDaemonConfigurations(&Config{}, flags, configFile)
	assert.ErrorContains(t, err, `the following directives are specified both as a flag and in the configuration file: tlscacert`)
}

// TestDaemonConfigurationMergeDefaultAddressPools is a regression test for #40711.
func TestDaemonConfigurationMergeDefaultAddressPools(t *testing.T) {
	emptyConfigFile := makeConfigFile(t, `{}`)
	configFile := makeConfigFile(t, `{"default-address-pools":[{"base": "10.123.0.0/16", "size": 24 }]}`)

	expected := []*ipamutils.NetworkToSplit{{Base: netip.MustParsePrefix("10.123.0.0/16"), Size: 24}}

	t.Run("empty config file", func(t *testing.T) {
		conf := Config{}
		flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
		flags.Var(&conf.NetworkConfig.DefaultAddressPools, "default-address-pool", "")
		assert.Check(t, flags.Set("default-address-pool", "base=10.123.0.0/16,size=24"))

		config, err := MergeDaemonConfigurations(&conf, flags, emptyConfigFile)
		assert.NilError(t, err)
		assert.DeepEqual(t, config.DefaultAddressPools.Value(), expected, cmpopts.EquateComparable(netip.Prefix{}))
	})

	t.Run("config file", func(t *testing.T) {
		conf := Config{}
		flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
		flags.Var(&conf.NetworkConfig.DefaultAddressPools, "default-address-pool", "")

		config, err := MergeDaemonConfigurations(&conf, flags, configFile)
		assert.NilError(t, err)
		assert.DeepEqual(t, config.DefaultAddressPools.Value(), expected, cmpopts.EquateComparable(netip.Prefix{}))
	})

	t.Run("with conflicting options", func(t *testing.T) {
		conf := Config{}
		flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
		flags.Var(&conf.NetworkConfig.DefaultAddressPools, "default-address-pool", "")
		assert.Check(t, flags.Set("default-address-pool", "base=10.123.0.0/16,size=24"))

		_, err := MergeDaemonConfigurations(&conf, flags, configFile)
		assert.ErrorContains(t, err, "the following directives are specified both as a flag and in the configuration file")
		assert.ErrorContains(t, err, "default-address-pools")
	})
}

func TestFindConfigurationConflictsWithUnknownKeys(t *testing.T) {
	config := map[string]any{"tls-verify": "true"}
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)

	flags.Bool("tlsverify", false, "")
	err := findConfigurationConflicts(config, flags)
	assert.ErrorContains(t, err, "the following directives don't match any configuration option: tls-verify")
}

func TestFindConfigurationConflictsWithMergedValues(t *testing.T) {
	var hosts []string
	config := map[string]any{"hosts": "tcp://127.0.0.1:2345"}
	flags := pflag.NewFlagSet("base", pflag.ContinueOnError)
	flags.VarP(opts.NewNamedListOptsRef("hosts", &hosts, nil), "host", "H", "")

	err := findConfigurationConflicts(config, flags)
	assert.NilError(t, err)

	assert.Check(t, flags.Set("host", "unix:///var/run/docker.sock"))
	err = findConfigurationConflicts(config, flags)
	assert.ErrorContains(t, err, "hosts: (from flag: [unix:///var/run/docker.sock], from file: tcp://127.0.0.1:2345)")
}

func TestValidateConfigurationErrors(t *testing.T) {
	testCases := []struct {
		name        string
		field       string
		config      *Config
		platform    string
		expectedErr string
	}{
		{
			name: "single label without value",
			config: &Config{
				CommonConfig: CommonConfig{
					Labels: []string{"one"},
				},
			},
			expectedErr: "bad attribute format: one",
		},
		{
			name: "embedded-containerd with cri-containerd",
			config: &Config{
				CommonConfig: CommonConfig{
					Features:      map[string]bool{"embedded-containerd": true},
					CriContainerd: true,
				},
			},
			expectedErr: errEmbeddedContainerdWithCRI.Error(),
		},
		{
			name: "multiple label without value",
			config: &Config{
				CommonConfig: CommonConfig{
					Labels: []string{"foo=bar", "one"},
				},
			},
			expectedErr: "bad attribute format: one",
		},
		{
			name: "single DNSSearch",
			config: &Config{
				CommonConfig: CommonConfig{
					DNSConfig: DNSConfig{
						DNSSearch: []string{"123456"},
					},
				},
			},
			expectedErr: "123456 is not a valid domain",
		},
		{
			name: "multiple DNSSearch",
			config: &Config{
				CommonConfig: CommonConfig{
					DNSConfig: DNSConfig{
						DNSSearch: []string{"a.b.c", "123456"},
					},
				},
			},
			expectedErr: "123456 is not a valid domain",
		},
		{
			name: "negative MTU",
			config: &Config{
				CommonConfig: CommonConfig{
					BridgeConfig: BridgeConfig{
						DefaultBridgeConfig: DefaultBridgeConfig{
							MTU: -10,
						},
					},
				},
			},
			expectedErr: "invalid default MTU: -10",
		},
		{
			name: "negative max-concurrent-downloads",
			config: &Config{
				CommonConfig: CommonConfig{
					MaxConcurrentDownloads: -10,
				},
			},
			expectedErr: "invalid max concurrent downloads: -10",
		},
		{
			name: "negative max-concurrent-uploads",
			config: &Config{
				CommonConfig: CommonConfig{
					MaxConcurrentUploads: -10,
				},
			},
			expectedErr: "invalid max concurrent uploads: -10",
		},
		{
			name: "negative max-download-attempts",
			config: &Config{
				CommonConfig: CommonConfig{
					MaxDownloadAttempts: -10,
				},
			},
			expectedErr: "invalid max download attempts: -10",
		},
		{
			name: "invalid default-stop-timeout",
			config: &Config{
				CommonConfig: CommonConfig{
					ContainerDefaults: ContainerDefaults{
						DefaultStopTimeout: -1,
					},
				},
			},
			expectedErr: "invalid default stop timeout: -1",
		},
		// TODO(thaJeztah) temporarily excluding this test as it assumes defaults are set before validating and applying updated configs
		/*
			{
				name:  "zero max-download-attempts",
				field: "MaxDownloadAttempts",
				config: &Config{
					CommonConfig: CommonConfig{
						MaxDownloadAttempts: 0,
					},
				},
				expectedErr: "invalid max download attempts: 0",
			},
		*/
		{
			name: "negative network-diagnostic-port",
			config: &Config{
				CommonConfig: CommonConfig{
					NetworkDiagnosticPort: -1,
				},
			},
			expectedErr: "invalid network-diagnostic-port (-1): value must be between 0 and 65535",
		},
		{
			name: "network-diagnostic-port out of range",
			config: &Config{
				CommonConfig: CommonConfig{
					NetworkDiagnosticPort: 65536,
				},
			},
			expectedErr: "invalid network-diagnostic-port (65536): value must be between 0 and 65535",
		},
		{
			name: "generic resource without =",
			config: &Config{
				CommonConfig: CommonConfig{
					NodeGenericResources: []string{"foo"},
				},
			},
			expectedErr: "could not parse GenericResource: incorrect term foo, missing '=' or malformed expression",
		},
		{
			name: "generic resource mixed named and discrete",
			config: &Config{
				CommonConfig: CommonConfig{
					NodeGenericResources: []string{"foo=bar", "foo=1"},
				},
			},
			expectedErr: "could not parse GenericResource: mixed discrete and named resources in expression 'foo=[bar 1]'",
		},
		{
			name: "with invalid hosts",
			config: &Config{
				CommonConfig: CommonConfig{
					Hosts: []string{"127.0.0.1:2375/path"},
				},
			},
			expectedErr: "invalid bind address (127.0.0.1:2375/path): should not contain a path element",
		},
		{
			name: "with invalid log-level",
			config: &Config{
				CommonConfig: CommonConfig{
					DaemonLogConfig: DaemonLogConfig{LogLevel: "foobar"},
				},
			},
			expectedErr: "invalid logging level: foobar",
		},
		{
			name: "exec-opt without value",
			config: &Config{
				CommonConfig: CommonConfig{
					ExecOptions: []string{"no-value"},
				},
			},
			expectedErr: "invalid exec-opt (no-value): must be formatted 'opt=value'",
		},
		{
			name: "exec-opt with empty value",
			config: &Config{
				CommonConfig: CommonConfig{
					ExecOptions: []string{"empty-value="},
				},
			},
			expectedErr: "invalid exec-opt (empty-value=): must be formatted 'opt=value'",
		},
		{
			name: "exec-opt without key",
			config: &Config{
				CommonConfig: CommonConfig{
					ExecOptions: []string{"=empty-key"},
				},
			},
			expectedErr: "invalid exec-opt (=empty-key): must be formatted 'opt=value'",
		},
		{
			name: "exec-opt unknown option",
			config: &Config{
				CommonConfig: CommonConfig{
					ExecOptions: []string{"unknown-option=any-value"},
				},
			},
			expectedErr: "invalid exec-opt (unknown-option=any-value): unknown option: 'unknown-option'",
		},
		{
			name: "exec-opt invalid on linux",
			config: &Config{
				CommonConfig: CommonConfig{
					ExecOptions: []string{"isolation=default"},
				},
			},
			platform:    "linux",
			expectedErr: "invalid exec-opt (isolation=default): option 'isolation' is only supported on windows",
		},
		{
			name: "exec-opt invalid on windows",
			config: &Config{
				CommonConfig: CommonConfig{
					ExecOptions: []string{"native.cgroupdriver=systemd"},
				},
			},
			platform:    "windows",
			expectedErr: "invalid exec-opt (native.cgroupdriver=systemd): option 'native.cgroupdriver' is only supported on linux",
		},
		{
			name: "invalid mirror",
			config: &Config{
				CommonConfig: CommonConfig{
					ServiceOptions: registry.ServiceOptions{
						Mirrors: []string{"ftp://example.com"},
					},
				},
			},
			expectedErr: `invalid mirror: unsupported scheme "ftp" in "ftp://example.com": must use either 'https://' or 'http://'`,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := New()
			assert.NilError(t, err)
			if tc.field != "" {
				assert.Check(t, mergo.Merge(cfg, tc.config, mergo.WithOverride, withForceOverwrite(tc.field)))
			} else {
				assert.Check(t, mergo.Merge(cfg, tc.config, mergo.WithOverride))
			}
			err = Validate(cfg)
			if tc.platform != "" && tc.platform != runtime.GOOS {
				assert.NilError(t, err)
			} else {
				assert.Error(t, err, tc.expectedErr)
			}
		})
	}
}

func TestValidateEmbeddedContainerdAllowsExplicitAddr(t *testing.T) {
	cfg := &Config{
		CommonConfig: CommonConfig{
			Features:       map[string]bool{"embedded-containerd": true},
			ContainerdAddr: "/run/containerd/containerd.sock",
		},
	}

	assert.NilError(t, Validate(cfg))
}

func withForceOverwrite(fieldName string) func(config *mergo.Config) {
	return mergo.WithTransformers(overwriteTransformer{fieldName: fieldName})
}

type overwriteTransformer struct {
	fieldName string
}

func (tf overwriteTransformer) Transformer(typ reflect.Type) func(dst, src reflect.Value) error {
	if typ == reflect.TypeFor[CommonConfig]() {
		return func(dst, src reflect.Value) error {
			dst.FieldByName(tf.fieldName).Set(src.FieldByName(tf.fieldName))
			return nil
		}
	}
	return nil
}

func TestValidateConfiguration(t *testing.T) {
	testCases := []struct {
		name   string
		field  string
		config *Config
	}{
		{
			name:  "with label",
			field: "Labels",
			config: &Config{
				CommonConfig: CommonConfig{
					Labels: []string{"one=two"},
				},
			},
		},
		{
			name:  "with dns-search",
			field: "DNSConfig",
			config: &Config{
				CommonConfig: CommonConfig{
					DNSConfig: DNSConfig{
						DNSSearch: []string{"a.b.c"},
					},
				},
			},
		},
		{
			name:  "with mtu",
			field: "MTU",
			config: &Config{
				CommonConfig: CommonConfig{
					BridgeConfig: BridgeConfig{
						DefaultBridgeConfig: DefaultBridgeConfig{
							MTU: 1234,
						},
					},
				},
			},
		},
		{
			name:  "with max-concurrent-downloads",
			field: "MaxConcurrentDownloads",
			config: &Config{
				CommonConfig: CommonConfig{
					MaxConcurrentDownloads: 4,
				},
			},
		},
		{
			name:  "with max-concurrent-uploads",
			field: "MaxConcurrentUploads",
			config: &Config{
				CommonConfig: CommonConfig{
					MaxConcurrentUploads: 4,
				},
			},
		},
		{
			name:  "with max-download-attempts",
			field: "MaxDownloadAttempts",
			config: &Config{
				CommonConfig: CommonConfig{
					MaxDownloadAttempts: 4,
				},
			},
		},
		{
			name:  "with multiple node generic resources",
			field: "NodeGenericResources",
			config: &Config{
				CommonConfig: CommonConfig{
					NodeGenericResources: []string{"foo=bar", "foo=baz"},
				},
			},
		},
		{
			name:  "with node generic resources",
			field: "NodeGenericResources",
			config: &Config{
				CommonConfig: CommonConfig{
					NodeGenericResources: []string{"foo=1"},
				},
			},
		},
		{
			name:  "with hosts",
			field: "Hosts",
			config: &Config{
				CommonConfig: CommonConfig{
					Hosts: []string{"tcp://127.0.0.1:2375"},
				},
			},
		},
		{
			name:  "with log-level warn",
			field: "LogLevel",
			config: &Config{
				CommonConfig: CommonConfig{
					DaemonLogConfig: DaemonLogConfig{LogLevel: "warn"},
				},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Start with a config with all defaults set, so that we only
			cfg, err := New()
			assert.NilError(t, err)
			assert.Check(t, mergo.Merge(cfg, tc.config, mergo.WithOverride))

			// Check that the override happened :)
			assert.Check(t, is.DeepEqual(cfg, tc.config, field(tc.field)))
			err = Validate(cfg)
			assert.NilError(t, err)
		})
	}
}

func TestValidateMinAPIVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		doc         string
		input       string
		expectedErr string
	}{
		{
			doc:         "empty",
			expectedErr: "value is empty",
		},
		{
			doc:         "with prefix",
			input:       "v1.43",
			expectedErr: `API version must be provided without "v" prefix`,
		},
		{
			doc:         "major only",
			input:       "1",
			expectedErr: `minimum supported API version is`,
		},
		{
			doc:         "too low",
			input:       "1.0",
			expectedErr: `minimum supported API version is`,
		},
		{
			doc:         "minor too high",
			input:       "1.99",
			expectedErr: `maximum supported API version is`,
		},
		{
			doc:         "major too high",
			input:       "9.0",
			expectedErr: `maximum supported API version is`,
		},
		{
			doc:   "current version",
			input: MaxAPIVersion,
		},
	}

	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			err := ValidateMinAPIVersion(tc.input)
			if tc.expectedErr != "" {
				assert.Check(t, is.ErrorContains(err, tc.expectedErr))
			} else {
				assert.Check(t, err)
			}
		})
	}
}

func TestConfigDNS(t *testing.T) {
	tests := []struct {
		doc   string
		input string
	}{
		{
			doc:   "IPv6s with scope IDs",
			input: `{"dns": ["::1%eth0", "::1%2"]}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			var cfg Config
			err := json.Unmarshal([]byte(tc.input), &cfg)
			assert.Check(t, err == nil, "type: %T", err)
		})
	}
}

func TestConfigInvalidDNS(t *testing.T) {
	tests := []struct {
		doc         string
		input       string
		expectedErr string
	}{
		{
			doc:         "single DNS, invalid IP-address",
			input:       `{"dns": ["1.1.1.1o"]}`,
			expectedErr: `ParseAddr("1.1.1.1o"): unexpected character (at "o")`,
		},
		{
			doc:         "multiple DNS, invalid IP-address",
			input:       `{"dns": ["2.2.2.2", "1.1.1.1o"]}`,
			expectedErr: `ParseAddr("1.1.1.1o"): unexpected character (at "o")`,
		},
		{
			doc:         "IPv4 with scope ID",
			input:       `{"dns": ["1.1.1.1%eth0"]}`,
			expectedErr: `ParseAddr("1.1.1.1%eth0"): unexpected character (at "%eth0")`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			var cfg Config
			err := json.Unmarshal([]byte(tc.input), &cfg)
			assert.Check(t, is.Error(err, tc.expectedErr), "type: %T", err)
		})
	}
}

func field(field string) cmp.Option {
	tmp := reflect.TypeFor[Config]()
	ignoreFields := make([]string, 0, tmp.NumField())
	for i := 0; i < tmp.NumField(); i++ {
		if tmp.Field(i).Name != field {
			ignoreFields = append(ignoreFields, tmp.Field(i).Name)
		}
	}
	return cmpopts.IgnoreFields(Config{}, ignoreFields...)
}

// TestReloadSetConfigFileNotExist tests that when `--config-file` is set, and it doesn't exist the `Reload` function
// returns an error.
func TestReloadSetConfigFileNotExist(t *testing.T) {
	configFile := "/tmp/blabla/not/exists/config.json"
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("config-file", "", "")
	assert.Check(t, flags.Set("config-file", configFile))

	err := Reload(configFile, flags, func(c *Config) {})
	assert.Check(t, is.ErrorContains(err, "unable to configure the Docker daemon with file"))
}

// TestReloadDefaultConfigNotExist tests that if the default configuration file doesn't exist the daemon still will
// still be reloaded.
func TestReloadDefaultConfigNotExist(t *testing.T) {
	skip.If(t, os.Getuid() != 0, "skipping test that requires root")
	defaultConfigFile := "/tmp/blabla/not/exists/daemon.json"
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("config-file", defaultConfigFile, "")
	reloaded := false
	err := Reload(defaultConfigFile, flags, func(c *Config) {
		reloaded = true
	})
	assert.Check(t, err)
	assert.Check(t, reloaded)
}

// TestReloadBadDefaultConfig tests that when `--config-file` is not set and the default configuration file exists and
// is bad, an error is returned.
func TestReloadBadDefaultConfig(t *testing.T) {
	configFile := makeConfigFile(t, `{wrong: "configuration"}`)

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("config-file", configFile, "")
	reloaded := false
	err := Reload(configFile, flags, func(c *Config) {
		reloaded = true
	})
	assert.Check(t, is.ErrorContains(err, "unable to configure the Docker daemon with file"))
	assert.Check(t, reloaded == false)
}

func TestReloadWithConflictingLabels(t *testing.T) {
	configFile := makeConfigFile(t, `{"labels": ["foo=bar", "foo=baz"]}`)

	var lbls []string
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("config-file", configFile, "")
	flags.StringSlice("labels", lbls, "")
	reloaded := false
	err := Reload(configFile, flags, func(c *Config) {
		reloaded = true
	})
	assert.Check(t, is.ErrorContains(err, "conflict labels for foo=baz and foo=bar"))
	assert.Check(t, reloaded == false)
}

func TestReloadWithDuplicateLabels(t *testing.T) {
	configFile := makeConfigFile(t, `{"labels": ["foo=the-same", "foo=the-same"]}`)

	var lbls []string
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("config-file", configFile, "")
	flags.StringSlice("labels", lbls, "")
	reloaded := false
	err := Reload(configFile, flags, func(c *Config) {
		reloaded = true
		assert.Check(t, is.DeepEqual(c.Labels, []string{"foo=the-same"}))
	})
	assert.Check(t, err)
	assert.Check(t, reloaded)
}

func TestMaskURLCredentials(t *testing.T) {
	tests := []struct {
		rawURL    string
		maskedURL string
	}{
		{
			rawURL:    "",
			maskedURL: "",
		}, {
			rawURL:    "invalidURL",
			maskedURL: "invalidURL",
		}, {
			rawURL:    "http://proxy.example.com:80/",
			maskedURL: "http://proxy.example.com:80/",
		}, {
			rawURL:    "http://USER:PASSWORD@proxy.example.com:80/",
			maskedURL: "http://xxxxx:xxxxx@proxy.example.com:80/",
		}, {
			rawURL:    "http://PASSWORD:PASSWORD@proxy.example.com:80/",
			maskedURL: "http://xxxxx:xxxxx@proxy.example.com:80/",
		}, {
			rawURL:    "http://USER:@proxy.example.com:80/",
			maskedURL: "http://xxxxx:xxxxx@proxy.example.com:80/",
		}, {
			rawURL:    "http://:PASSWORD@proxy.example.com:80/",
			maskedURL: "http://xxxxx:xxxxx@proxy.example.com:80/",
		}, {
			rawURL:    "http://USER@docker:password@proxy.example.com:80/",
			maskedURL: "http://xxxxx:xxxxx@proxy.example.com:80/",
		}, {
			rawURL:    "http://USER%40docker:password@proxy.example.com:80/",
			maskedURL: "http://xxxxx:xxxxx@proxy.example.com:80/",
		}, {
			rawURL:    "http://USER%40docker:pa%3Fsword@proxy.example.com:80/",
			maskedURL: "http://xxxxx:xxxxx@proxy.example.com:80/",
		}, {
			rawURL:    "http://USER%40docker:pa%3Fsword@proxy.example.com:80/hello%20world",
			maskedURL: "http://xxxxx:xxxxx@proxy.example.com:80/hello%20world",
		},
	}
	for _, test := range tests {
		maskedURL := MaskCredentials(test.rawURL)
		assert.Equal(t, maskedURL, test.maskedURL)
	}
}

func TestSanitize(t *testing.T) {
	const (
		userPass        = "myuser:mypassword@"
		proxyRawURL     = "https://" + userPass + "example.org"
		proxyURL        = "https://xxxxx:xxxxx@example.org"
		extensionID     = "org.example.test.v1"
		extensionSecret = "unique-extension-secret"
	)
	cfg := Config{
		CommonConfig: CommonConfig{
			Proxies: Proxies{
				HTTPProxy:  proxyRawURL,
				HTTPSProxy: proxyRawURL,
				NoProxy:    proxyRawURL,
			},
			Extensions: ExtensionsConfig{
				Configs: ExtensionConfigs{{
					ID: extensionID,
					Config: map[string]any{
						"secret": extensionSecret,
					},
				}},
			},
		},
	}
	sanitizedCfg := Sanitize(cfg)
	expectedProxies := Proxies{
		HTTPProxy:  proxyURL,
		HTTPSProxy: proxyURL,
		NoProxy:    proxyURL,
	}
	assert.Check(t, is.DeepEqual(sanitizedCfg.Proxies, expectedProxies))

	sanitizedJSON, err := json.Marshal(sanitizedCfg)
	assert.NilError(t, err)
	assert.Assert(t, !strings.Contains(string(sanitizedJSON), extensionSecret))
	assert.Assert(t, !strings.Contains(string(sanitizedJSON), `"configs"`))
	assert.Check(t, is.DeepEqual(cfg.Extensions.Configs[0].Config, map[string]any{
		"secret": extensionSecret,
	}))
}
