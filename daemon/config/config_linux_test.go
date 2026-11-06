package config

import (
	"net/netip"
	"testing"

	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/moby/moby/api/types/container"
	dopts "github.com/moby/moby/v2/daemon/internal/opts"
	"github.com/moby/moby/v2/daemon/pkg/opts"
	"github.com/spf13/pflag"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestGetConflictFreeConfiguration(t *testing.T) {
	configFile := makeConfigFile(t, `
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
			},
			"default-network-opts": {
				"overlay": {
					"com.docker.network.driver.mtu": "1337"
				}
			}
		}`)

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	var debug bool
	flags.BoolVarP(&debug, "debug", "D", false, "")
	flags.Var(opts.NewNamedUlimitOpt("default-ulimits", nil), "default-ulimit", "")
	flags.Var(opts.NewNamedMapOpts("log-opts", nil, nil), "log-opt", "")
	flags.Var(opts.NewNamedMapMapOpts("default-network-opts", nil, nil), "default-network-opt", "")

	cc, err := getConflictFreeConfiguration(configFile, flags)
	assert.NilError(t, err)

	assert.Check(t, cc.Debug)

	expectedUlimits := map[string]*container.Ulimit{
		"nofile": {
			Name: "nofile",
			Hard: 2048,
			Soft: 1024,
		},
	}

	assert.Check(t, is.DeepEqual(expectedUlimits, cc.Ulimits))
}

func TestDaemonConfigurationMerge(t *testing.T) {
	configFile := makeConfigFile(t, `
		{
			"debug": true,
			"default-ulimits": {
				"nofile": {
					"Name": "nofile",
					"Hard": 2048,
					"Soft": 1024
				}
			}
		}`)

	conf, err := New()
	assert.NilError(t, err)

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.BoolVarP(&conf.Debug, "debug", "D", false, "")
	flags.BoolVarP(&conf.AutoRestart, "restart", "r", true, "")
	flags.Var(opts.NewNamedUlimitOpt("default-ulimits", &conf.Ulimits), "default-ulimit", "")
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

	expectedUlimits := map[string]*container.Ulimit{
		"nofile": {
			Name: "nofile",
			Hard: 2048,
			Soft: 1024,
		},
	}

	assert.Check(t, is.DeepEqual(expectedUlimits, cc.Ulimits))
}

func TestDaemonConfigurationMergeShmSize(t *testing.T) {
	configFile := makeConfigFile(t, `{"default-shm-size": "1g"}`)

	c, err := New()
	assert.NilError(t, err)

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	shmSize := opts.MemBytes(DefaultShmSize)
	flags.Var(&shmSize, "default-shm-size", "")

	cc, err := MergeDaemonConfigurations(c, flags, configFile)
	assert.NilError(t, err)

	expectedValue := 1 * 1024 * 1024 * 1024
	assert.Check(t, is.Equal(int64(expectedValue), cc.ShmSize.Value()))
}

func TestDaemonConfigurationFeatures(t *testing.T) {
	tests := []struct {
		name, config, flags string
		expectedValue       map[string]bool
		expectedErr         string
	}{
		{
			name:          "enable from file",
			config:        `{"features": {"containerd-snapshotter": true}}`,
			expectedValue: map[string]bool{"containerd-snapshotter": true},
		},
		{
			name:          "enable from flags",
			config:        `{}`,
			flags:         "containerd-snapshotter=true",
			expectedValue: map[string]bool{"containerd-snapshotter": true},
		},
		{
			name:          "disable from file",
			config:        `{"features": {"containerd-snapshotter": false}}`,
			expectedValue: map[string]bool{"containerd-snapshotter": false},
		},
		{
			name:          "disable from flags",
			config:        `{}`,
			flags:         "containerd-snapshotter=false",
			expectedValue: map[string]bool{"containerd-snapshotter": false},
		},
		{
			name:        "conflict",
			config:      `{"features": {"containerd-snapshotter": true}}`,
			flags:       "containerd-snapshotter=true",
			expectedErr: `the following directives are specified both as a flag and in the configuration file: features: (from flag: map[containerd-snapshotter:true], from file: map[containerd-snapshotter:true])`,
		},
		{
			name:        "invalid config value",
			config:      `{"features": {"containerd-snapshotter": "not-a-boolean"}}`,
			expectedErr: `json: cannot unmarshal string into Go struct field`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New()
			assert.NilError(t, err)

			configFile := makeConfigFile(t, tc.config)
			flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
			flags.Var(dopts.NewNamedSetOpts("features", c.Features), "feature", "Enable feature in the daemon")
			if tc.flags != "" {
				err = flags.Set("feature", tc.flags)
				assert.NilError(t, err)
			}
			cc, err := MergeDaemonConfigurations(c, flags, configFile)
			if tc.expectedErr != "" {
				assert.ErrorContains(t, err, tc.expectedErr)
			} else {
				assert.NilError(t, err)
				assert.Check(t, is.DeepEqual(tc.expectedValue, cc.Features))
			}
		})
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
		assert.Equal(t, tc.config.GetInitPath(), tc.expectedInitPath)
	}
}

func TestDaemonConfigurationHostGatewayIP(t *testing.T) {
	tests := []struct {
		name      string
		config    string
		flags     []string
		expVal    []string
		expSetErr string
		expErr    string
	}{
		{
			name:   "flag IPv4 only",
			config: `{}`,
			flags:  []string{"192.0.2.1"},
			expVal: []string{"192.0.2.1"},
		},
		{
			name:   "flag IPv6 only",
			config: `{}`,
			flags:  []string{"2001:db8::1234"},
			expVal: []string{"2001:db8::1234"},
		},
		{
			name:   "flag IPv4 and IPv6",
			config: `{}`,
			flags:  []string{"2001:db8::1234", "192.0.2.1"},
			expVal: []string{"2001:db8::1234", "192.0.2.1"},
		},
		{
			name:   "flag two IPv4",
			config: `{}`,
			flags:  []string{"192.0.2.1", "192.0.2.2"},
			expErr: "merged configuration validation from file and command line flags failed: only one IPv4 host gateway IP address can be specified",
		},
		{
			name:   "flag two IPv6",
			config: `{}`,
			flags:  []string{"2001:db8::1234", "2001:db8::5678"},
			expErr: "merged configuration validation from file and command line flags failed: only one IPv6 host gateway IP address can be specified",
		},
		{
			name:   "legacy config",
			config: `{"host-gateway-ip": "2001:db8::1234"}`,
			expVal: []string{"2001:db8::1234"},
		},
		{
			name:   "config ipv4",
			config: `{"host-gateway-ips": ["192.0.2.1"]}`,
			expVal: []string{"192.0.2.1"},
		},
		{
			name:   "config ipv6",
			config: `{"host-gateway-ips": ["2001:db8::1234"]}`,
			expVal: []string{"2001:db8::1234"},
		},
		{
			name:   "config ipv4 and ipv6",
			config: `{"host-gateway-ips": ["2001:db8::1234", "192.0.2.1"]}`,
			expVal: []string{"2001:db8::1234", "192.0.2.1"},
		},
		{
			name:   "config two ipv4",
			config: `{"host-gateway-ips": ["192.0.2.1", "192.0.2.2"]}`,
			expErr: "merged configuration validation from file and command line flags failed: only one IPv4 host gateway IP address can be specified",
		},
		{
			name:   "config two ipv6",
			config: `{"host-gateway-ips": ["2001:db8::1234", "2001:db8::5678"]}`,
			expErr: "merged configuration validation from file and command line flags failed: only one IPv6 host gateway IP address can be specified",
		},
		{
			name:      "flag bad address",
			flags:     []string{"hello"},
			expSetErr: `invalid argument "hello" for "--host-gateway-ip" flag: ParseAddr("hello"): unable to parse IP`,
		},
		{
			name:   "config bad address",
			config: `{"host-gateway-ips": ["hello"]}`,
			expErr: `ParseAddr("hello"): unable to parse IP`,
		},
		{
			name:   "config not array",
			config: `{"host-gateway-ips": "192.0.2.1"}`,
			expErr: `json: cannot unmarshal string into Go struct field`,
		},
		{
			name:   "config old and new",
			config: `{"host-gateway-ip": "192.0.2.1", "host-gateway-ips": ["192.0.2.1"]}`,
			expErr: "host-gateway-ip and host-gateway-ips must not both be specified in the config file",
		},
		{
			name:   "config old and flag",
			flags:  []string{"192.0.2.1"},
			config: `{"host-gateway-ip": "192.0.2.2"}`,
			expErr: "the following directives are specified both as a flag and in the configuration file: host-gateway-ip: (from flag: [192.0.2.1], from file: 192.0.2.2)",
		},
		{
			name:   "config new and flag",
			flags:  []string{"192.0.2.1"},
			config: `{"host-gateway-ips": ["192.0.2.2", "2001:db8::1234"]}`,
			expErr: "the following directives are specified both as a flag and in the configuration file: host-gateway-ips: (from flag: [192.0.2.1], from file: [192.0.2.2 2001:db8::1234])",
		},
		{
			name:   "config new and old and flag",
			flags:  []string{"192.0.2.1"},
			config: `{"host-gateway-ip": "192.0.2.2", "host-gateway-ips": ["192.0.2.3"]}`,
			expErr: "host-gateway-ip and host-gateway-ips must not both be specified in the config file\n" +
				"the following directives are specified both as a flag and in the configuration file: host-gateway-ips: (from flag: [192.0.2.1], from file: [192.0.2.3]), host-gateway-ip: (from flag: [192.0.2.1], from file: 192.0.2.2)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New()
			assert.NilError(t, err)

			configFile := makeConfigFile(t, tc.config)
			flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
			flags.Var(dopts.NewNamedIPListOptsRef("host-gateway-ips", &c.HostGatewayIPs),
				"host-gateway-ip", "a usage message")
			for _, flagVal := range tc.flags {
				err := flags.Set("host-gateway-ip", flagVal)
				if tc.expSetErr != "" {
					assert.Check(t, is.Error(err, tc.expSetErr))
					return
				}
				assert.NilError(t, err)
			}
			cc, err := MergeDaemonConfigurations(c, flags, configFile)
			if tc.expErr != "" {
				assert.Check(t, is.ErrorContains(err, tc.expErr))
				assert.Check(t, is.Nil(cc))
			} else {
				assert.NilError(t, err)
				var expVal []netip.Addr
				for _, ev := range tc.expVal {
					expVal = append(expVal, netip.MustParseAddr(ev))
				}
				assert.Check(t, is.DeepEqual(cc.HostGatewayIPs, expVal, cmpopts.EquateComparable(netip.Addr{})))
				assert.Check(t, is.Nil(cc.HostGatewayIP)) //nolint:staticcheck // ignore SA1019: deprecated field should be nil
			}
		})
	}
}

// TestDaemonLegacyOptions verifies that loading config files containing
// deprecated / legacy options does not prevent the daemon from loading
// the config (some options may have an explicit error, and can be tested
// separately).
func TestDaemonLegacyOptions(t *testing.T) {
	tests := []struct {
		name       string
		configJSON string
	}{
		{
			name:       "deprecated-key-path",
			configJSON: `{"deprecated-key-path": "/etc/docker/key.json"}`,
		},
		{
			name:       "allow-nondistributable-artifacts",
			configJSON: `{"allow-nondistributable-artifacts": ["127.0.0.0/8", "10.10.1.11:5000", "10.10.1.22:5000", "registry.example.com", "registry.example.com"]}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			configFile := makeConfigFile(t, tc.configJSON)
			flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
			c, err := New()
			assert.NilError(t, err)
			_, err = MergeDaemonConfigurations(c, flags, configFile)
			assert.NilError(t, err)
		})
	}
}

func TestValidateAcceptFwMarkMark(t *testing.T) {
	tests := []struct {
		name   string
		val    string
		expErr string
	}{
		{
			name: "empty",
			val:  "",
		},
		{
			name: "dec/no-mask",
			val:  "1",
		},
		{
			name: "hex/no-mask",
			val:  "0x1",
		},
		{
			name: "dec/mask",
			val:  "1/2",
		},
		{
			name: "hex/mask",
			val:  "0x1/0x2",
		},
		{
			name: "octal/mask",
			val:  "010/0xff",
		},
		{
			name:   "bad/mark",
			val:    "hello/0x2",
			expErr: `invalid firewall mark "hello/0x2": strconv.ParseUint: parsing "hello": invalid syntax`,
		},
		{
			name:   "bad/mark",
			val:    "1/hello",
			expErr: `invalid firewall mask "1/hello": strconv.ParseUint: parsing "hello": invalid syntax`,
		},
		{
			name:   "bad/sep",
			val:    "1+hello",
			expErr: `invalid firewall mark "1+hello": strconv.ParseUint: parsing "1+hello": invalid syntax`,
		},
		{
			name:   "bad/no-mask",
			val:    "1/",
			expErr: `invalid firewall mask "1/": strconv.ParseUint: parsing "": invalid syntax`,
		},
		{
			name:   "bad/negative",
			val:    "-1",
			expErr: `invalid firewall mark "-1": strconv.ParseUint: parsing "-1": invalid syntax`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFwMarkMask(tc.val)
			if tc.expErr == "" {
				assert.NilError(t, err)
			} else {
				assert.Check(t, is.ErrorContains(err, tc.expErr))
			}
		})
	}
}
