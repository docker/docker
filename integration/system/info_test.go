package system

import (
	"fmt"
	"net/netip"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

func TestInfoAPI(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	result, err := apiClient.Info(ctx, client.InfoOptions{})
	assert.NilError(t, err)

	info := result.Info
	// TODO(thaJeztah): make sure we have other tests that run a local daemon and check other fields based on known state.
	assert.Check(t, info.ID != "")
	assert.Check(t, is.Equal(info.Containers, info.ContainersRunning+info.ContainersPaused+info.ContainersStopped))
	assert.Check(t, info.LoggingDriver != "")
	assert.Check(t, info.OperatingSystem != "")
	assert.Check(t, info.NCPU != 0)
	assert.Check(t, info.OSType != "")
	assert.Check(t, info.Architecture != "")
	assert.Check(t, info.MemTotal != 0)
	assert.Check(t, info.KernelVersion != "")
	assert.Check(t, info.Driver != "")
	assert.Check(t, info.ServerVersion != "")
	assert.Check(t, info.SystemTime != "")
	if testEnv.DaemonInfo.OSType != "windows" {
		// Windows currently doesn't have security-options in the info response.
		assert.Check(t, len(info.SecurityOptions) != 0)
	}
}

func TestInfoAPIWarnings(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon, "cannot run daemon when remote daemon")
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "FIXME")

	t.Parallel()

	ctx := testutil.StartSpan(baseContext, t)

	d := daemon.New(t)
	c := d.NewClientT(t)

	d.Start(t, "-H=0.0.0.0:23756", "-H="+d.Sock(), "--iptables=false", "--ip6tables=false")
	defer d.Stop(t)

	result, err := c.Info(ctx, client.InfoOptions{})
	assert.NilError(t, err)

	info := result.Info

	stringsToCheck := []string{
		"Access to the remote API is equivalent to root access",
		"http://0.0.0.0:23756",
	}

	out := fmt.Sprintf("%+v", info)
	for _, linePrefix := range stringsToCheck {
		assert.Check(t, is.Contains(out, linePrefix))
	}
}

func TestInfoDebug(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon, "cannot run daemon when remote daemon")
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "FIXME: test starts daemon with -H unix://.....")

	t.Parallel()

	_ = testutil.StartSpan(baseContext, t)

	d := daemon.New(t)
	d.Start(t, "--debug", "--iptables=false", "--ip6tables=false")
	defer d.Stop(t)

	info := d.Info(t)
	assert.Equal(t, info.Debug, true)

	// Note that the information below is not tied to debug-mode being enabled.
	assert.Check(t, info.NFd != 0)

	// TODO need a stable way to generate event listeners
	// assert.Check(t, info.NEventsListener != 0)
	assert.Check(t, info.NGoroutines != 0)
	assert.Equal(t, info.DockerRootDir, d.Root)
}

func TestInfoInsecureRegistries(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon, "cannot run daemon when remote daemon")
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "FIXME: test starts daemon with -H unix://.....")

	t.Parallel()

	const (
		registryCIDR = "192.168.1.0/24"
		registryHost = "insecurehost.com:5000"
	)

	t.Run("defaults", func(t *testing.T) {
		t.Parallel()
		d := daemon.New(t)
		defer d.Stop(t)
		d.Start(t, "--iptables=false", "--ip6tables=false")
		info := d.Info(t)

		expectedCIDRS := []netip.Prefix{
			netip.MustParsePrefix("127.0.0.0/8"),
			netip.MustParsePrefix("::1/128"),
		}
		assert.DeepEqual(t, info.RegistryConfig.InsecureRegistryCIDRs, expectedCIDRS, cmpopts.EquateComparable(netip.Prefix{}))
	})

	t.Run("insecure-registries", func(t *testing.T) {
		t.Parallel()
		d := daemon.New(t)
		defer d.Stop(t)
		d.Start(t, "--insecure-registry="+registryCIDR, "--insecure-registry="+registryHost, "--iptables=false", "--ip6tables=false")
		info := d.Info(t)
		d.Stop(t)

		expectedCIDRS := []netip.Prefix{netip.MustParsePrefix(registryCIDR)}
		assert.DeepEqual(t, info.RegistryConfig.InsecureRegistryCIDRs, expectedCIDRS, cmpopts.EquateComparable(netip.Prefix{}))
		assert.DeepEqual(t, *info.RegistryConfig.IndexConfigs["docker.io"], registry.IndexInfo{Name: "docker.io", Mirrors: []string{}, Secure: true, Official: true})
		assert.DeepEqual(t, *info.RegistryConfig.IndexConfigs[registryHost], registry.IndexInfo{Name: registryHost, Mirrors: []string{}, Secure: false, Official: false})
	})
}

func TestInfoRegistryMirrors(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon, "cannot run daemon when remote daemon")
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "FIXME: test starts daemon with -H unix://.....")

	t.Parallel()

	const (
		registryMirror1 = "https://192.168.1.2"
		registryMirror2 = "http://registry-mirror.example.com:5000"
	)

	d := daemon.New(t)
	d.Start(t, "--registry-mirror="+registryMirror1, "--registry-mirror="+registryMirror2, "--iptables=false", "--ip6tables=false")
	defer d.Stop(t)

	info := d.Info(t)
	slices.Sort(info.RegistryConfig.Mirrors)
	assert.DeepEqual(t, info.RegistryConfig.Mirrors, []string{registryMirror2 + "/", registryMirror1 + "/"})
}
