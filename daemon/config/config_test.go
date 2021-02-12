package config // import "github.com/docker/docker/daemon/config"

import (
	"io/ioutil"
	"os"
	"strings"
	"testing"

	"github.com/docker/docker/daemon/discovery"
	"github.com/docker/docker/opts"
	"github.com/docker/libnetwork/ipamutils"
	"github.com/spf13/pflag"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/fs"
	"gotest.tools/skip"
)

func TestDaemonConfigurationNotFound(t *testing.T) {
	_, err := MergeDaemonConfigurations(&Config{}, nil, "/tmp/foo-bar-baz-docker")
	if err == nil || !os.IsNotExist(err) {
		t.Fatalf("expected does not exist error, got %v", err)
	}
}

func TestDaemonBrokenConfiguration(t *testing.T) {
	f, err := ioutil.TempFile("", "docker-config-")
	if err != nil {
		t.Fatal(err)
	}

	configFile := f.Name()
	f.Write([]byte(`{"Debug": tru`))
	f.Close()

	_, err = MergeDaemonConfigurations(&Config{}, nil, configFile)
	if err == nil {
		t.Fatalf("expected error, got %v", err)
	}
}

func TestParseClusterAdvertiseSettings(t *testing.T) {
	_, err := ParseClusterAdvertiseSettings("something", "")
	if err != discovery.ErrDiscoveryDisabled {
		t.Fatalf("expected discovery disabled error, got %v\n", err)
	}

	_, err = ParseClusterAdvertiseSettings("", "something")
	if err == nil {
		t.Fatalf("expected discovery store error, got %v\n", err)
	}

	_, err = ParseClusterAdvertiseSettings("etcd", "127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
}

func TestFindConfigurationConflicts(t *testing.T) {
	config := map[string]interface{}{"authorization-plugins": "foobar"}
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)

	flags.String("authorization-plugins", "", "")
	assert.Check(t, flags.Set("authorization-plugins", "asdf"))
	assert.Check(t, is.ErrorContains(findConfigurationConflicts(config, flags), "authorization-plugins: (from flag: asdf, from file: foobar)"))
}

func TestFindConfigurationConflictsWithNamedOptions(t *testing.T) {
	config := map[string]interface{}{"hosts": []string{"qwer"}}
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)

	var hosts []string
	flags.VarP(opts.NewNamedListOptsRef("hosts", &hosts, opts.ValidateHost), "host", "H", "Daemon socket(s) to connect to")
	assert.Check(t, flags.Set("host", "tcp://127.0.0.1:4444"))
	assert.Check(t, flags.Set("host", "unix:///var/run/docker.sock"))
	assert.Check(t, is.ErrorContains(findConfigurationConflicts(config, flags), "hosts"))
}

func TestDaemonConfigurationMergeConflicts(t *testing.T) {
	f, err := ioutil.TempFile("", "docker-config-")
	if err != nil {
		t.Fatal(err)
	}

	configFile := f.Name()
	f.Write([]byte(`{"debug": true}`))
	f.Close()

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.Bool("debug", false, "")
	flags.Set("debug", "false")

	_, err = MergeDaemonConfigurations(&Config{}, flags, configFile)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "debug") {
		t.Fatalf("expected debug conflict, got %v", err)
	}
}

func TestDaemonConfigurationMergeConcurrent(t *testing.T) {
	f, err := ioutil.TempFile("", "docker-config-")
	if err != nil {
		t.Fatal(err)
	}

	configFile := f.Name()
	f.Write([]byte(`{"max-concurrent-downloads": 1}`))
	f.Close()

	_, err = MergeDaemonConfigurations(&Config{}, nil, configFile)
	if err != nil {
		t.Fatal("expected error, got nil")
	}
}

func TestDaemonConfigurationMergeConcurrentError(t *testing.T) {
	f, err := ioutil.TempFile("", "docker-config-")
	if err != nil {
		t.Fatal(err)
	}

	configFile := f.Name()
	f.Write([]byte(`{"max-concurrent-downloads": -1}`))
	f.Close()

	_, err = MergeDaemonConfigurations(&Config{}, nil, configFile)
	if err == nil {
		t.Fatalf("expected no error, got error %v", err)
	}
}

func TestDaemonConfigurationMergeConflictsWithInnerStructs(t *testing.T) {
	f, err := ioutil.TempFile("", "docker-config-")
	if err != nil {
		t.Fatal(err)
	}

	configFile := f.Name()
	f.Write([]byte(`{"tlscacert": "/etc/certificates/ca.pem"}`))
	f.Close()

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("tlscacert", "", "")
	flags.Set("tlscacert", "~/.docker/ca.pem")

	_, err = MergeDaemonConfigurations(&Config{}, flags, configFile)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "tlscacert") {
		t.Fatalf("expected tlscacert conflict, got %v", err)
	}
}

// Test for #40711
func TestDaemonConfigurationMergeDefaultAddressPools(t *testing.T) {
	emptyConfigFile := fs.NewFile(t, "config", fs.WithContent(`{}`))
	defer emptyConfigFile.Remove()
	configFile := fs.NewFile(t, "config", fs.WithContent(`{"default-address-pools":[{"base": "10.123.0.0/16", "size": 24 }]}`))
	defer configFile.Remove()

	expected := []*ipamutils.NetworkToSplit{{Base: "10.123.0.0/16", Size: 24}}

	t.Run("empty config file", func(t *testing.T) {
		var conf = Config{}
		flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
		flags.Var(&conf.NetworkConfig.DefaultAddressPools, "default-address-pool", "")
		flags.Set("default-address-pool", "base=10.123.0.0/16,size=24")

		config, err := MergeDaemonConfigurations(&conf, flags, emptyConfigFile.Path())
		assert.NilError(t, err)
		assert.DeepEqual(t, config.DefaultAddressPools.Value(), expected)
	})

	t.Run("config file", func(t *testing.T) {
		var conf = Config{}
		flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
		flags.Var(&conf.NetworkConfig.DefaultAddressPools, "default-address-pool", "")

		config, err := MergeDaemonConfigurations(&conf, flags, configFile.Path())
		assert.NilError(t, err)
		assert.DeepEqual(t, config.DefaultAddressPools.Value(), expected)
	})

	t.Run("with conflicting options", func(t *testing.T) {
		var conf = Config{}
		flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
		flags.Var(&conf.NetworkConfig.DefaultAddressPools, "default-address-pool", "")
		flags.Set("default-address-pool", "base=10.123.0.0/16,size=24")

		_, err := MergeDaemonConfigurations(&conf, flags, configFile.Path())
		assert.ErrorContains(t, err, "the following directives are specified both as a flag and in the configuration file")
		assert.ErrorContains(t, err, "default-address-pools")
	})
}

func TestFindConfigurationConflictsWithUnknownKeys(t *testing.T) {
	config := map[string]interface{}{"tls-verify": "true"}
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)

	flags.Bool("tlsverify", false, "")
	err := findConfigurationConflicts(config, flags)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "the following directives don't match any configuration option: tls-verify") {
		t.Fatalf("expected tls-verify conflict, got %v", err)
	}
}

func TestFindConfigurationConflictsWithMergedValues(t *testing.T) {
	var hosts []string
	config := map[string]interface{}{"hosts": "tcp://127.0.0.1:2345"}
	flags := pflag.NewFlagSet("base", pflag.ContinueOnError)
	flags.VarP(opts.NewNamedListOptsRef("hosts", &hosts, nil), "host", "H", "")

	err := findConfigurationConflicts(config, flags)
	if err != nil {
		t.Fatal(err)
	}

	flags.Set("host", "unix:///var/run/docker.sock")
	err = findConfigurationConflicts(config, flags)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "hosts: (from flag: [unix:///var/run/docker.sock], from file: tcp://127.0.0.1:2345)") {
		t.Fatalf("expected hosts conflict, got %v", err)
	}
}

func TestValidateReservedNamespaceLabels(t *testing.T) {
	for _, validLabels := range [][]string{
		nil, // no error if there are no labels
		{ // no error if there aren't any reserved namespace labels
			"hello=world",
			"label=me",
		},
		{ // only reserved namespaces that end with a dot are invalid
			"com.dockerpsychnotreserved.label=value",
			"io.dockerproject.not=reserved",
			"org.docker.not=reserved",
		},
	} {
		assert.Check(t, ValidateReservedNamespaceLabels(validLabels))
	}

	for _, invalidLabel := range []string{
		"com.docker.feature=enabled",
		"io.docker.configuration=0",
		"org.dockerproject.setting=on",
		// casing doesn't matter
		"COM.docker.feature=enabled",
		"io.DOCKER.CONFIGURATION=0",
		"Org.Dockerproject.Setting=on",
	} {
		err := ValidateReservedNamespaceLabels([]string{
			"valid=label",
			invalidLabel,
			"another=valid",
		})
		assert.Check(t, is.ErrorContains(err, invalidLabel))
	}
}

func TestValidateConfigurationErrors(t *testing.T) {
	minusNumber := -10
	testCases := []struct {
		config *Config
	}{
		{
			config: &Config{
				CommonConfig: CommonConfig{
					Labels: []string{"one"},
				},
			},
		},
		{
			config: &Config{
				CommonConfig: CommonConfig{
					Labels: []string{"foo=bar", "one"},
				},
			},
		},
		{
			config: &Config{
				CommonConfig: CommonConfig{
					DNSConfig: DNSConfig{
						DNS: []string{"1.1.1.1o"},
					},
				},
			},
		},
		{
			config: &Config{
				CommonConfig: CommonConfig{
					DNSConfig: DNSConfig{
						DNS: []string{"2.2.2.2", "1.1.1.1o"},
					},
				},
			},
		},
		{
			config: &Config{
				CommonConfig: CommonConfig{
					DNSConfig: DNSConfig{
						DNSSearch: []string{"123456"},
					},
				},
			},
		},
		{
			config: &Config{
				CommonConfig: CommonConfig{
					DNSConfig: DNSConfig{
						DNSSearch: []string{"a.b.c", "123456"},
					},
				},
			},
		},
		{
			config: &Config{
				CommonConfig: CommonConfig{
					MaxConcurrentDownloads: &minusNumber,
					// This is weird...
					ValuesSet: map[string]interface{}{
						"max-concurrent-downloads": -1,
					},
				},
			},
		},
		{
			config: &Config{
				CommonConfig: CommonConfig{
					MaxConcurrentUploads: &minusNumber,
					// This is weird...
					ValuesSet: map[string]interface{}{
						"max-concurrent-uploads": -1,
					},
				},
			},
		},
		{
			config: &Config{
				CommonConfig: CommonConfig{
					NodeGenericResources: []string{"foo"},
				},
			},
		},
		{
			config: &Config{
				CommonConfig: CommonConfig{
					NodeGenericResources: []string{"foo=bar", "foo=1"},
				},
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

func TestValidateConfiguration(t *testing.T) {
	minusNumber := 4
	testCases := []struct {
		config *Config
	}{
		{
			config: &Config{
				CommonConfig: CommonConfig{
					Labels: []string{"one=two"},
				},
			},
		},
		{
			config: &Config{
				CommonConfig: CommonConfig{
					DNSConfig: DNSConfig{
						DNS: []string{"1.1.1.1"},
					},
				},
			},
		},
		{
			config: &Config{
				CommonConfig: CommonConfig{
					DNSConfig: DNSConfig{
						DNSSearch: []string{"a.b.c"},
					},
				},
			},
		},
		{
			config: &Config{
				CommonConfig: CommonConfig{
					MaxConcurrentDownloads: &minusNumber,
					// This is weird...
					ValuesSet: map[string]interface{}{
						"max-concurrent-downloads": -1,
					},
				},
			},
		},
		{
			config: &Config{
				CommonConfig: CommonConfig{
					MaxConcurrentUploads: &minusNumber,
					// This is weird...
					ValuesSet: map[string]interface{}{
						"max-concurrent-uploads": -1,
					},
				},
			},
		},
		{
			config: &Config{
				CommonConfig: CommonConfig{
					NodeGenericResources: []string{"foo=bar", "foo=baz"},
				},
			},
		},
		{
			config: &Config{
				CommonConfig: CommonConfig{
					NodeGenericResources: []string{"foo=1"},
				},
			},
		},
	}
	for _, tc := range testCases {
		err := Validate(tc.config)
		if err != nil {
			t.Fatalf("expected no error, got error %v", err)
		}
	}
}

func TestModifiedDiscoverySettings(t *testing.T) {
	cases := []struct {
		current  *Config
		modified *Config
		expected bool
	}{
		{
			current:  discoveryConfig("foo", "bar", map[string]string{}),
			modified: discoveryConfig("foo", "bar", map[string]string{}),
			expected: false,
		},
		{
			current:  discoveryConfig("foo", "bar", map[string]string{"foo": "bar"}),
			modified: discoveryConfig("foo", "bar", map[string]string{"foo": "bar"}),
			expected: false,
		},
		{
			current:  discoveryConfig("foo", "bar", map[string]string{}),
			modified: discoveryConfig("foo", "bar", nil),
			expected: false,
		},
		{
			current:  discoveryConfig("foo", "bar", nil),
			modified: discoveryConfig("foo", "bar", map[string]string{}),
			expected: false,
		},
		{
			current:  discoveryConfig("foo", "bar", nil),
			modified: discoveryConfig("baz", "bar", nil),
			expected: true,
		},
		{
			current:  discoveryConfig("foo", "bar", nil),
			modified: discoveryConfig("foo", "baz", nil),
			expected: true,
		},
		{
			current:  discoveryConfig("foo", "bar", nil),
			modified: discoveryConfig("foo", "bar", map[string]string{"foo": "bar"}),
			expected: true,
		},
	}

	for _, c := range cases {
		got := ModifiedDiscoverySettings(c.current, c.modified.ClusterStore, c.modified.ClusterAdvertise, c.modified.ClusterOpts)
		if c.expected != got {
			t.Fatalf("expected %v, got %v: current config %v, new config %v", c.expected, got, c.current, c.modified)
		}
	}
}

func discoveryConfig(backendAddr, advertiseAddr string, opts map[string]string) *Config {
	return &Config{
		CommonConfig: CommonConfig{
			ClusterStore:     backendAddr,
			ClusterAdvertise: advertiseAddr,
			ClusterOpts:      opts,
		},
	}
}

// TestReloadSetConfigFileNotExist tests that when `--config-file` is set
// and it doesn't exist the `Reload` function returns an error.
func TestReloadSetConfigFileNotExist(t *testing.T) {
	configFile := "/tmp/blabla/not/exists/config.json"
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("config-file", "", "")
	flags.Set("config-file", configFile)

	err := Reload(configFile, flags, func(c *Config) {})
	assert.Check(t, is.ErrorContains(err, "unable to configure the Docker daemon with file"))
}

// TestReloadDefaultConfigNotExist tests that if the default configuration file
// doesn't exist the daemon still will be reloaded.
func TestReloadDefaultConfigNotExist(t *testing.T) {
	skip.If(t, os.Getuid() != 0, "skipping test that requires root")
	reloaded := false
	configFile := "/etc/docker/daemon.json"
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("config-file", configFile, "")
	err := Reload(configFile, flags, func(c *Config) {
		reloaded = true
	})
	assert.Check(t, err)
	assert.Check(t, reloaded)
}

// TestReloadBadDefaultConfig tests that when `--config-file` is not set
// and the default configuration file exists and is bad return an error
func TestReloadBadDefaultConfig(t *testing.T) {
	f, err := ioutil.TempFile("", "docker-config-")
	if err != nil {
		t.Fatal(err)
	}

	configFile := f.Name()
	f.Write([]byte(`{wrong: "configuration"}`))
	f.Close()

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("config-file", configFile, "")
	err = Reload(configFile, flags, func(c *Config) {})
	assert.Check(t, is.ErrorContains(err, "unable to configure the Docker daemon with file"))
}

func TestReloadWithConflictingLabels(t *testing.T) {
	tempFile := fs.NewFile(t, "config", fs.WithContent(`{"labels":["foo=bar","foo=baz"]}`))
	defer tempFile.Remove()
	configFile := tempFile.Path()

	var lbls []string
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("config-file", configFile, "")
	flags.StringSlice("labels", lbls, "")
	err := Reload(configFile, flags, func(c *Config) {})
	assert.Check(t, is.ErrorContains(err, "conflict labels for foo=baz and foo=bar"))
}

func TestReloadWithDuplicateLabels(t *testing.T) {
	tempFile := fs.NewFile(t, "config", fs.WithContent(`{"labels":["foo=the-same","foo=the-same"]}`))
	defer tempFile.Remove()
	configFile := tempFile.Path()

	var lbls []string
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.String("config-file", configFile, "")
	flags.StringSlice("labels", lbls, "")
	err := Reload(configFile, flags, func(c *Config) {})
	assert.Check(t, err)
}
