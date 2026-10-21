package networking

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	networktypes "github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/integration/internal/network"
	"github.com/moby/moby/v2/integration/internal/testutils/networking"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"github.com/moby/moby/v2/internal/testutil/request"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/icmd"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

const defaultFirewallBackend = "iptables"

func TestInfoFirewallBackend(t *testing.T) {
	ctx := setupTest(t)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	t.Cleanup(func() { d.Stop(t) })

	c := d.NewClientT(t)
	t.Cleanup(func() { c.Close() })

	expDriver := defaultFirewallBackend
	if val := os.Getenv("DOCKER_FIREWALL_BACKEND"); val != "" {
		expDriver = val
	}
	if !testEnv.IsRootless() && networking.FirewalldRunning() {
		expDriver += "+firewalld"
	}
	result, err := c.Info(ctx, client.InfoOptions{})
	assert.NilError(t, err)
	info := result.Info
	assert.Assert(t, info.FirewallBackend != nil, "expected firewall backend in info response")
	t.Log("FirewallBackend: Driver:", info.FirewallBackend.Driver)
	for _, kv := range info.FirewallBackend.Info {
		t.Logf("FirewallBackend: %s: %s", kv[0], kv[1])
	}
	assert.Check(t, is.Equal(info.FirewallBackend.Driver, expDriver))

	// Check FirewallBackend is omitted for API <= 1.48.
	t.Run("api 1.48", func(t *testing.T) {
		c148 := request.NewAPIClient(t, client.WithAPIVersion("1.48"))
		result, err := c148.Info(ctx, client.InfoOptions{})
		assert.NilError(t, err)
		info148 := result.Info
		assert.Check(t, is.Nil(info148.FirewallBackend))
	})
}

// TestFirewalldStop checks that the daemon restores its iptables rules when
// firewalld deletes them on stopping, and uses firewalld again when it is
// started.
func TestFirewalldStop(t *testing.T) {
	skip.If(t, testEnv.IsRootless, "rootless mode does not use firewalld")
	skip.If(t, testEnv.FirewallBackendDriver() != "iptables+firewalld", "test is for the iptables backend with firewalld")
	ctx := setupTest(t)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	t.Cleanup(func() { d.Stop(t) })
	c := d.NewClientT(t)
	t.Cleanup(func() { c.Close() })

	const bridgeName = "br-fwstop"
	want := setupPublishedPort(ctx, t, c, bridgeName)

	t.Cleanup(func() { startFirewalld(t) })
	if !firewalldDeletesRules(t, func() { stopFirewalld(t) }) {
		t.Skip("stopping firewalld does not delete iptables rules")
	}
	waitForRules(t, "after firewalld stopped", bridgeName, want)
	assert.Check(t, is.Equal(d.FirewallBackendDriver(t), "iptables"))

	deleted := firewalldDeletesRules(t, func() { startFirewalld(t) })
	if deleted {
		waitForRules(t, "after firewalld started", bridgeName, want)
	}
	assert.Check(t, is.Equal(d.FirewallBackendDriver(t), "iptables+firewalld"))
}

// TestFirewalldStartedAfterDaemon checks that the daemon starts using firewalld
// when firewalld is started after the daemon.
func TestFirewalldStartedAfterDaemon(t *testing.T) {
	skip.If(t, testEnv.IsRootless, "rootless mode does not use firewalld")
	skip.If(t, testEnv.FirewallBackendDriver() != "iptables+firewalld", "test is for the iptables backend with firewalld")
	ctx := setupTest(t)

	t.Cleanup(func() { startFirewalld(t) })
	stopFirewalld(t)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	t.Cleanup(func() { d.Stop(t) })
	c := d.NewClientT(t)
	t.Cleanup(func() { c.Close() })
	assert.Check(t, is.Equal(d.FirewallBackendDriver(t), "iptables"))

	const bridgeName = "br-fwlate"
	want := setupPublishedPort(ctx, t, c, bridgeName)

	deleted := firewalldDeletesRules(t, func() { startFirewalld(t) })
	poll.WaitOn(t, func(poll.LogT) poll.Result {
		if drv := d.FirewallBackendDriver(t); drv != "iptables+firewalld" {
			return poll.Continue("firewall backend driver is %q", drv)
		}
		return poll.Success()
	})
	poll.WaitOn(t, func(poll.LogT) poll.Result {
		res := icmd.RunCommand("firewall-cmd", "--zone=docker", "--list-interfaces")
		if !slices.Contains(strings.Fields(res.Stdout()), bridgeName) {
			return poll.Continue("%s is not in the docker zone: %s", bridgeName, res.Combined())
		}
		return poll.Success()
	})
	if deleted {
		waitForRules(t, "after firewalld started", bridgeName, want)
	}
}

// setupPublishedPort creates a bridge network with the given bridge name, and
// a container on it with a published port. It returns the iptables rules which
// refer to the bridge.
func setupPublishedPort(ctx context.Context, t *testing.T, c client.APIClient, bridgeName string) []string {
	t.Helper()
	const netName = "testnet"
	network.CreateNoError(ctx, t, c, netName,
		network.WithDriver("bridge"),
		network.WithOption(bridge.BridgeName, bridgeName),
	)
	t.Cleanup(func() { network.RemoveNoError(ctx, t, c, netName) })
	ctrID := container.Run(ctx, t, c,
		container.WithNetworkMode(netName),
		container.WithExposedPorts("80/tcp"),
		container.WithPortMap(networktypes.PortMap{
			networktypes.MustParsePort("80/tcp"): {{HostPort: "8080"}},
		}),
	)
	t.Cleanup(func() { c.ContainerRemove(ctx, ctrID, client.ContainerRemoveOptions{Force: true}) })

	rules := bridgeRules(t, bridgeName)
	assert.Assert(t, len(rules) > 0, "no iptables rules refer to %s", bridgeName)
	return rules
}

// bridgeRules returns the iptables rules which refer to bridgeName.
func bridgeRules(t *testing.T, bridgeName string) []string {
	t.Helper()
	var rules []string
	for _, cmd := range []string{"iptables-save", "ip6tables-save"} {
		res := icmd.RunCommand(cmd)
		res.Assert(t, icmd.Success)
		for _, l := range strings.Split(res.Stdout(), "\n") {
			if strings.Contains(l, bridgeName) {
				rules = append(rules, cmd+": "+l)
			}
		}
	}
	return rules
}

// waitForRules waits until every rule in want is present.
func waitForRules(t *testing.T, when, bridgeName string, want []string) {
	t.Helper()
	poll.WaitOn(t, func(poll.LogT) poll.Result {
		got := bridgeRules(t, bridgeName)
		var missing []string
		for _, r := range want {
			if !slices.Contains(got, r) {
				missing = append(missing, r)
			}
		}
		if len(missing) > 0 {
			return poll.Continue("%s, rules are missing:\n%s", when, strings.Join(missing, "\n"))
		}
		return poll.Success()
	}, poll.WithTimeout(30*time.Second))
}

// firewalldDeletesRules reports whether f deletes an iptables chain that the
// test adds before calling it.
func firewalldDeletesRules(t *testing.T, f func()) bool {
	t.Helper()
	const markerChain = "TEST-FIREWALLD-MARKER"
	icmd.RunCommand("iptables", "-N", markerChain).Assert(t, icmd.Success)
	t.Cleanup(func() { icmd.RunCommand("iptables", "-X", markerChain) })
	f()
	return icmd.RunCommand("iptables", "-S", markerChain).ExitCode != 0
}

func stopFirewalld(t *testing.T) {
	t.Helper()
	icmd.RunCommand("systemctl", "stop", "firewalld").Assert(t, icmd.Success)
}

func startFirewalld(t *testing.T) {
	t.Helper()
	icmd.RunCommand("systemctl", "start", "firewalld").Assert(t, icmd.Success)
	poll.WaitOn(t, func(poll.LogT) poll.Result {
		if !networking.FirewalldRunning() {
			return poll.Continue("firewalld is not running")
		}
		return poll.Success()
	}, poll.WithTimeout(30*time.Second))
}
