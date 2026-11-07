package daemon

import (
	"net/netip"
	"os"
	"testing"

	"github.com/containerd/log"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/mitchellh/copystructure"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/images"
	"github.com/moby/moby/v2/daemon/libnetwork"
	"github.com/moby/moby/v2/daemon/pkg/registry"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

// muteLogs suppresses logs that are generated during the test
func muteLogs(t *testing.T) {
	t.Helper()
	err := log.SetLevel(log.ErrorLevel)
	if err != nil {
		t.Error(err)
	}
}

func TestCopystructureNetipAddr(t *testing.T) {
	// Verify that our custom copier for netip.Addr works correctly.
	// Without it, copystructure.Copy produces zero-value (invalid) addresses.
	original := []netip.Addr{netip.MustParseAddr("8.8.8.8")}
	copied, err := copystructure.Copy(original)
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(copied.([]netip.Addr), original, cmpopts.EquateComparable(netip.Addr{})))
}

func TestCopystructureNetipPrefix(t *testing.T) {
	// Verify that our custom copier for netip.Prefix works correctly.
	// Without it, copystructure.Copy produces zero-value (invalid) prefixes.
	original := []netip.Prefix{netip.MustParsePrefix("172.30.0.0/16")}
	copied, err := copystructure.Copy(original)
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(copied.([]netip.Prefix), original, cmpopts.EquateComparable(netip.Prefix{})))
}

func newDaemonForReloadT(t *testing.T, cfg *config.Config) *Daemon {
	t.Helper()
	daemon := &Daemon{
		imageService: images.NewImageService(t.Context(), images.ImageServiceConfig{}),
	}
	var err error
	daemon.registryService, err = registry.NewService(registry.ServiceOptions{})
	assert.Assert(t, err)
	daemon.configStore.Store(&configStore{Config: *cfg})
	return daemon
}

func TestDaemonReloadLabels(t *testing.T) {
	daemon := newDaemonForReloadT(t, &config.Config{
		CommonConfig: config.CommonConfig{
			Labels: []string{"foo:bar"},
		},
	})
	muteLogs(t)

	valuesSets := make(map[string]any)
	valuesSets["labels"] = "foo:baz"
	newConfig := &config.Config{
		CommonConfig: config.CommonConfig{
			Labels:    []string{"foo:baz"},
			ValuesSet: valuesSets,
		},
	}

	if err := daemon.Reload(newConfig); err != nil {
		t.Fatal(err)
	}

	label := daemon.config().Labels[0]
	if label != "foo:baz" {
		t.Fatalf("Expected daemon label `foo:baz`, got %s", label)
	}
}

func TestDaemonReloadMirrors(t *testing.T) {
	daemon := &Daemon{
		imageService: images.NewImageService(t.Context(), images.ImageServiceConfig{}),
	}
	muteLogs(t)

	var err error
	daemon.registryService, err = registry.NewService(registry.ServiceOptions{
		InsecureRegistries: []string{},
		Mirrors: []string{
			"https://mirror.test1.example.com",
			"https://mirror.test2.example.com", // this will be removed when reloading
			"https://mirror.test3.example.com", // this will be removed when reloading
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	type pair struct {
		valid   bool
		mirrors []string
		after   []string
	}

	loadMirrors := []pair{
		{
			valid:   false,
			mirrors: []string{"10.10.1.11:5000"}, // this mirror is invalid
			after:   []string{},
		},
		{
			valid:   false,
			mirrors: []string{"mirror.test1.com"}, // this mirror is invalid
			after:   []string{},
		},
		{
			valid:   false,
			mirrors: []string{"10.10.1.11:5000", "mirror.test1.example.com"}, // mirrors are invalid
			after:   []string{},
		},
		{
			valid:   true,
			mirrors: []string{"https://mirror.test1.example.com", "https://mirror.test4.example.com"},
			after:   []string{"https://mirror.test1.example.com/", "https://mirror.test4.example.com/"},
		},
	}

	for _, value := range loadMirrors {
		valuesSets := make(map[string]any)
		valuesSets["registry-mirrors"] = value.mirrors

		newConfig := &config.Config{
			CommonConfig: config.CommonConfig{
				ServiceOptions: registry.ServiceOptions{
					Mirrors: value.mirrors,
				},
				ValuesSet: valuesSets,
			},
		}

		err := daemon.Reload(newConfig)
		if !value.valid && err == nil {
			// mirrors should be invalid, should be a non-nil error
			t.Fatalf("Expected daemon reload error with invalid mirrors: %s, while get nil", value.mirrors)
		}

		if value.valid {
			if err != nil {
				// mirrors should be valid, should be no error
				t.Fatal(err)
			}
			registryService := daemon.registryService.ServiceConfig()

			if len(registryService.Mirrors) != len(value.after) {
				t.Fatalf("Expected %d daemon mirrors %s while get %d with %s",
					len(value.after),
					value.after,
					len(registryService.Mirrors),
					registryService.Mirrors)
			}

			dataMap := map[string]struct{}{}

			for _, mirror := range registryService.Mirrors {
				if _, exist := dataMap[mirror]; !exist {
					dataMap[mirror] = struct{}{}
				}
			}

			for _, address := range value.after {
				if _, exist := dataMap[address]; !exist {
					t.Fatalf("Expected %s in daemon mirrors, while get none", address)
				}
			}
		}
	}
}

func TestDaemonReloadInsecureRegistries(t *testing.T) {
	daemon := &Daemon{
		imageService: images.NewImageService(t.Context(), images.ImageServiceConfig{}),
	}
	muteLogs(t)

	var err error
	// initialize daemon with existing insecure registries: "127.0.0.0/8", "10.10.1.11:5000", "10.10.1.22:5000"
	daemon.registryService, err = registry.NewService(registry.ServiceOptions{
		InsecureRegistries: []string{
			"::1/128",
			"127.0.0.0/8",
			"10.10.1.11:5000",
			"10.10.1.22:5000", // this will be removed when reloading
			"docker1.example.com",
			"docker2.example.com", // this will be removed when reloading
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	insecureRegistries := []string{
		"::1/128",             // this will be kept
		"127.0.0.0/8",         // this will be kept
		"10.10.1.11:5000",     // this will be kept
		"10.10.1.33:5000",     // this will be newly added
		"docker1.example.com", // this will be kept
		"docker3.example.com", // this will be newly added
	}

	mirrors := []string{
		"https://mirror.test.example.com",
	}

	valuesSets := make(map[string]any)
	valuesSets["insecure-registries"] = insecureRegistries
	valuesSets["registry-mirrors"] = mirrors

	newConfig := &config.Config{
		CommonConfig: config.CommonConfig{
			ServiceOptions: registry.ServiceOptions{
				InsecureRegistries: insecureRegistries,
				Mirrors:            mirrors,
			},
			ValuesSet: valuesSets,
		},
	}

	if err := daemon.Reload(newConfig); err != nil {
		t.Fatal(err)
	}

	// After Reload, daemon.RegistryService will be changed which is useful
	// for registry communication in daemon.
	registries := daemon.registryService.ServiceConfig()

	// After Reload(), newConfig has come to registries.InsecureRegistryCIDRs and registries.IndexConfigs in daemon.
	// Then collect registries.InsecureRegistryCIDRs in dataMap.
	// When collecting, we need to convert CIDRS into string as a key,
	// while the times of key appears as value.
	dataMap := map[string]int{}
	for _, value := range registries.InsecureRegistryCIDRs {
		if _, ok := dataMap[value.String()]; !ok {
			dataMap[value.String()] = 1
		} else {
			dataMap[value.String()]++
		}
	}

	for _, value := range registries.IndexConfigs {
		if _, ok := dataMap[value.Name]; !ok {
			dataMap[value.Name] = 1
		} else {
			dataMap[value.Name]++
		}
	}

	// Finally compare dataMap with the original insecureRegistries.
	// Each value in insecureRegistries should appear in daemon's insecure registries,
	// and each can only appear exactly ONCE.
	for _, r := range insecureRegistries {
		if value, ok := dataMap[r]; !ok {
			t.Fatalf("Expected daemon insecure registry %s, got none", r)
		} else if value != 1 {
			t.Fatalf("Expected only 1 daemon insecure registry %s, got %d", r, value)
		}
	}

	// assert if "10.10.1.22:5000" is removed when reloading
	if value, ok := dataMap["10.10.1.22:5000"]; ok {
		t.Fatalf("Expected no insecure registry of 10.10.1.22:5000, got %d", value)
	}

	// assert if "docker2.com" is removed when reloading
	if value, ok := dataMap["docker2.example.com"]; ok {
		t.Fatalf("Expected no insecure registry of docker2.com, got %d", value)
	}
}

func TestDaemonReloadNotAffectOthers(t *testing.T) {
	daemon := newDaemonForReloadT(t, &config.Config{
		CommonConfig: config.CommonConfig{
			Labels: []string{"foo:bar"},
			Debug:  true,
		},
	})
	muteLogs(t)

	valuesSets := make(map[string]any)
	valuesSets["labels"] = "foo:baz"
	newConfig := &config.Config{
		CommonConfig: config.CommonConfig{
			Labels:    []string{"foo:baz"},
			ValuesSet: valuesSets,
		},
	}

	if err := daemon.Reload(newConfig); err != nil {
		t.Fatal(err)
	}

	label := daemon.config().Labels[0]
	if label != "foo:baz" {
		t.Fatalf("Expected daemon label `foo:baz`, got %s", label)
	}
	debug := daemon.config().Debug
	if !debug {
		t.Fatal("Expected debug 'enabled', got 'disabled'")
	}
}

func TestDaemonReloadDefaultStopTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout int
	}{
		{name: "positive", timeout: 42},
		{name: "zero", timeout: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newDaemonForReloadT(t, &config.Config{
				CommonConfig: config.CommonConfig{
					ContainerDefaults: config.ContainerDefaults{
						DefaultStopTimeout: 10,
					},
				},
			})
			newConfig := &config.Config{
				CommonConfig: config.CommonConfig{
					ContainerDefaults: config.ContainerDefaults{
						DefaultStopTimeout: tc.timeout,
					},
					ValuesSet: map[string]any{"default-stop-timeout": tc.timeout},
				},
			}

			assert.NilError(t, daemon.Reload(newConfig))
			assert.Equal(t, daemon.config().DefaultStopTimeout, tc.timeout)
		})
	}
}

func TestDaemonReloadNetworkDiagnosticPort(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("root required")
	}
	daemon := newDaemonForReloadT(t, &config.Config{})

	enableConfig := &config.Config{
		CommonConfig: config.CommonConfig{
			NetworkDiagnosticPort: 2000,
			ValuesSet: map[string]any{
				"network-diagnostic-port": 2000,
			},
		},
	}

	netOptions, err := daemon.networkOptions(&config.Config{CommonConfig: config.CommonConfig{Root: t.TempDir(), ExecRoot: t.TempDir()}}, nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := libnetwork.New(t.Context(), netOptions...)
	if err != nil {
		t.Fatal(err)
	}
	daemon.netController = controller

	// Enable/Disable the server for some iterations
	for range 10 {
		enableConfig.CommonConfig.NetworkDiagnosticPort++
		if err := daemon.Reload(enableConfig); err != nil {
			t.Fatal(err)
		}
		// Check that the diagnostic is enabled
		if !daemon.netController.IsDiagnosticEnabled() {
			t.Fatalf("diagnostic should be enabled")
		}

		// Reload
		if err := daemon.Reload(&config.Config{}); err != nil {
			t.Fatal(err)
		}
		// Check that the diagnostic is disabled
		if daemon.netController.IsDiagnosticEnabled() {
			t.Fatalf("diagnostic should be disabled")
		}
	}

	enableConfig.CommonConfig.NetworkDiagnosticPort++
	// 2 times the enable should not create problems
	if err := daemon.Reload(enableConfig); err != nil {
		t.Fatal(err)
	}
	// Check that the diagnostic is enabled
	if !daemon.netController.IsDiagnosticEnabled() {
		t.Fatalf("diagnostic should be enable")
	}

	// Check that another reload does not cause issues
	if err := daemon.Reload(enableConfig); err != nil {
		t.Fatal(err)
	}
	// Check that the diagnostic is enable
	if !daemon.netController.IsDiagnosticEnabled() {
		t.Fatalf("diagnostic should be enable")
	}
}

func TestDaemonReloadPreservesDNSConfig(t *testing.T) {
	dnsServers := []netip.Addr{
		netip.MustParseAddr("8.8.8.8"),
		netip.MustParseAddr("8.8.4.4"),
	}
	hostGatewayIPs := []netip.Addr{
		netip.MustParseAddr("192.168.1.1"),
		netip.MustParseAddr("fd00::1"),
	}

	daemon := newDaemonForReloadT(t, &config.Config{
		CommonConfig: config.CommonConfig{
			Labels: []string{"foo:bar"},
			DNSConfig: config.DNSConfig{
				DNS:            dnsServers,
				HostGatewayIPs: hostGatewayIPs,
			},
		},
	})
	muteLogs(t)

	// Reload with a different config (labels change)
	newConfig := &config.Config{
		CommonConfig: config.CommonConfig{
			Labels:    []string{"foo:baz"},
			ValuesSet: map[string]any{"labels": "foo:baz"},
		},
	}

	err := daemon.Reload(newConfig)
	assert.NilError(t, err)

	// Verify DNS config is preserved after reload
	cfg := daemon.config()
	assert.Check(t, is.DeepEqual(cfg.DNS, dnsServers, cmpopts.EquateComparable(netip.Addr{})))
	assert.Check(t, is.DeepEqual(cfg.HostGatewayIPs, hostGatewayIPs, cmpopts.EquateComparable(netip.Addr{})))

	// Verify all addresses are valid (not zero-value)
	for i, addr := range cfg.DNS {
		assert.Check(t, addr.IsValid(), "DNS[%d] should be valid, got %q", i, addr)
	}
	for i, addr := range cfg.HostGatewayIPs {
		assert.Check(t, addr.IsValid(), "HostGatewayIPs[%d] should be valid, got %q", i, addr)
	}
}

func TestReloadTxn(t *testing.T) {
	// Count callback sets without making execution order part of the contract.
	type calls struct {
		commit   int
		rollback int
	}
	newTxn := func() (*reloadTxn, *calls) {
		var tx reloadTxn
		called := new(calls)
		commit := func() error {
			called.commit++
			return nil
		}
		rollback := func() error {
			called.rollback++
			return nil
		}
		// Interleave duplicates to catch replacement and cross-execution.
		tx.OnCommit(commit)
		tx.OnRollback(rollback)
		tx.OnCommit(commit)
		tx.OnRollback(rollback)
		return &tx, called
	}

	t.Run("Commit", func(t *testing.T) {
		tx, called := newTxn()
		assert.NilError(t, tx.Commit())
		assert.Equal(t, called.commit, 2)
		assert.Equal(t, called.rollback, 0)
	})

	t.Run("Rollback", func(t *testing.T) {
		tx, called := newTxn()
		assert.NilError(t, tx.Rollback())
		assert.Equal(t, called.commit, 0)
		assert.Equal(t, called.rollback, 2)
	})
}
