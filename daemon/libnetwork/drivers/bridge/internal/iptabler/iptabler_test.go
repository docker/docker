//go:build linux

package iptabler

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge/internal/firewaller"
	"github.com/moby/moby/v2/daemon/libnetwork/iptables"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/moby/moby/v2/internal/testutil/netnsutils"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/golden"
	"gotest.tools/v3/icmd"
	"gotest.tools/v3/skip"
)

func TestCleanupIptableRules(t *testing.T) {
	skip.If(t, iptables.UsingFirewalld(), "firewalld is running in the host netns, it can't modify rules in the test's netns")

	defer netnsutils.SetupTestOSContext(t)()
	bridgeChains := []struct {
		name       string
		table      iptables.Table
		expRemoved bool
	}{
		{name: dockerChain, table: iptables.Nat, expRemoved: true},
		// The filter-FORWARD chain has a reference to dockerForwardChain, so it won't be
		// removed - but it should be flushed. (This has long/always been the case for
		// the daemon, its filter-FORWARD rules aren't removed.)
		{name: dockerForwardChain, table: iptables.Filter},
		{name: dockerCTChain, table: iptables.Filter, expRemoved: true},
		{name: dockerBridgeChain, table: iptables.Filter, expRemoved: true},
		{name: dockerChain, table: iptables.Filter, expRemoved: true},
		{name: dockerInternalChain, table: iptables.Filter, expRemoved: true},
	}

	ipVersions := []iptables.IPVersion{iptables.IPv4, iptables.IPv6}

	for _, version := range ipVersions {
		err := setupIPChains(t.Context(), version, firewaller.Config{Hairpin: true})
		assert.NilError(t, err, "version:%s", version)

		iptable := iptables.GetIptable(version)
		for _, chainInfo := range bridgeChains {
			exists := iptable.ExistChain(chainInfo.name, chainInfo.table)
			assert.Check(t, exists, "version:%s chain:%s table:%v",
				version, chainInfo.name, chainInfo.table)
		}

		// Insert RETURN rules so that there's something to flush.
		for _, chainInfo := range bridgeChains {
			out, err := iptable.Raw("-t", string(chainInfo.table), "-A", chainInfo.name, "-j", "RETURN")
			assert.NilError(t, err, "version:%s chain:%s table:%v out:%s",
				version, chainInfo.name, chainInfo.table, out)
		}

		removeIPChains(t.Context(), version)

		for _, chainInfo := range bridgeChains {
			exists := iptable.Exists(chainInfo.table, chainInfo.name, "-A", chainInfo.name, "-j", "RETURN")
			assert.Check(t, !exists, "version:%s chain:%s table:%v",
				version, chainInfo.name, chainInfo.table)
			if chainInfo.expRemoved {
				exists := iptable.ExistChain(chainInfo.name, chainInfo.table)
				assert.Check(t, !exists, "version:%s chain:%s table:%v",
					version, chainInfo.name, chainInfo.table)
			}
		}
	}
}

// TestCleanupOldIngressChains checks that removeIPChains removes the DOCKER-INGRESS
// chains, and the jumps to them, created by daemons older than 28.0.1.
func TestCleanupOldIngressChains(t *testing.T) {
	skip.If(t, iptables.UsingFirewalld(), "firewalld is running in the host netns, it can't modify rules in the test's netns")

	defer netnsutils.SetupTestOSContext(t)()
	iptable := iptables.GetIptable(iptables.IPv4)
	for _, args := range [][]string{
		{"-t", "nat", "-N", oldIngressChain},
		{"-t", "nat", "-A", oldIngressChain, "-j", "RETURN"},
		{"-t", "nat", "-I", "PREROUTING", "-m", "addrtype", "--dst-type", "LOCAL", "-j", oldIngressChain},
		{"-t", "nat", "-I", "OUTPUT", "-m", "addrtype", "--dst-type", "LOCAL", "-j", oldIngressChain},
		{"-N", oldIngressChain},
		{"-A", oldIngressChain, "-j", "RETURN"},
		{"-I", "FORWARD", "-j", oldIngressChain},
	} {
		out, err := iptable.Raw(args...)
		assert.NilError(t, err, "args:%v out:%s", args, out)
	}

	removeIPChains(t.Context(), iptables.IPv4)

	assert.Check(t, !iptable.Exists(iptables.Filter, "FORWARD", "-j", oldIngressChain))
	for _, table := range []iptables.Table{iptables.Nat, iptables.Filter} {
		assert.Check(t, !iptable.ExistChain(oldIngressChain, table), "table:%v", table)
	}
}

// TestIptabler tests combinations of firewaller options against golden results.
func TestIptabler(t *testing.T) {
	skip.If(t, iptables.UsingFirewalld(), "firewalld is running in the host netns, it can't modify rules in the test's netns")

	const (
		ipv4 int64 = iota
		ipv6
		hairpin
		internal
		icc
		masq
		snat
		bindLocalhost
		wsl2Mirrored
		numBoolParams
	)
	for i := range 1 << numBoolParams {
		p := func(n int64) bool { return (i & (1 << n)) != 0 }
		for _, gwmode := range []string{"nat", "nat-unprotected", "routed"} {
			config := firewaller.Config{
				IPv4:         p(ipv4),
				IPv6:         p(ipv6),
				Hairpin:      p(hairpin),
				WSL2Mirrored: p(wsl2Mirrored),
			}
			netConfig := firewaller.NetworkConfig{
				IfName:     "br-dummy",
				Internal:   p(internal),
				ICC:        p(icc),
				Masquerade: p(masq),
				Config4: firewaller.NetworkConfigFam{
					HostIP:      netip.Addr{},
					Prefix:      netip.MustParsePrefix("192.168.0.0/24"),
					Routed:      gwmode == "routed",
					Unprotected: gwmode == "nat-unprotected",
				},
				Config6: firewaller.NetworkConfigFam{
					HostIP:      netip.Addr{},
					Prefix:      netip.MustParsePrefix("fd49:efd7:54aa::/64"),
					Routed:      gwmode == "routed",
					Unprotected: gwmode == "nat-unprotected",
				},
			}
			if p(snat) {
				netConfig.Config4.HostIP = netip.MustParseAddr("192.168.123.0")
				netConfig.Config6.HostIP = netip.MustParseAddr("fd34:d0d4:672f::123")
			}
			tn := t.Name()
			t.Run(fmt.Sprintf("ipv4=%v/ipv6=%v/hairpin=%v/internal=%v/icc=%v/masq=%v/snat=%v/gwm=%v/bindlh=%v/wsl2mirrored=%v",
				p(ipv4), p(ipv6), p(hairpin), p(internal), p(icc), p(masq), p(snat), gwmode, p(bindLocalhost), p(wsl2Mirrored)), func(t *testing.T) {
				// Run in parallel, unless updating results (because tests share golden results files, so
				// they trample each other's output).
				if !golden.FlagUpdate() {
					t.Parallel()
				}
				// Combine results (golden output files) where possible to:
				// - check params that should have no effect when made irrelevant by other params, and
				// - minimise the number of results files.
				var resName string
				if p(internal) {
					// Port binding params should have no effect on an internal network.
					resName = fmt.Sprintf("hairpin=%v,internal=true,icc=%v", p(hairpin), p(icc))
				} else {
					resName = fmt.Sprintf("hairpin=%v,internal=%v,icc=%v,masq=%v,snat=%v,gwm=%v,bindlh=%v",
						p(hairpin), p(internal), p(icc), p(masq), p(snat), gwmode, p(bindLocalhost))
				}
				testIptabler(t, tn, config, netConfig, p(bindLocalhost), tn+"/"+resName)
			})
		}
	}
}

func testIptabler(t *testing.T, tn string, config firewaller.Config, netConfig firewaller.NetworkConfig, bindLocalhost bool, resName string) {
	defer netnsutils.SetupTestOSContext(t, netnsutils.WithSetNsHandles(false))()

	stripComments := func(text string) string {
		lines := strings.Split(text, "\n")
		lines = slices.DeleteFunc(lines, func(l string) bool { return l != "" && l[0] == '#' })
		return strings.Join(lines, "\n")
	}

	checkResults := func(cmd, name string, en bool) {
		t.Helper()
		// Explicitly save each table that might be used because:
		// - iptables-nft and iptables-legacy pick a different order when dumping all tables
		// - if the raw table isn't used it's not included in the all-tables dump but, once it's been used, it's always
		//   included ... so, "cleaned" results would differ only in the empty raw table.
		var dump strings.Builder
		for _, table := range []string{"raw", "filter", "nat"} {
			res := icmd.RunCommand(cmd+"-save", "-t", table)
			assert.Assert(t, res.Error)
			if !en {
				name = tn + "/no"
			}
			dump.WriteString(res.Combined())
		}
		assert.Check(t, golden.String(stripComments(dump.String()), name+"__"+cmd+".golden"))
	}

	makePB := func(hip string, cip netip.Addr) types.PortBinding {
		return types.PortBinding{
			Proto:       types.TCP,
			IP:          cip.AsSlice(),
			Port:        80,
			HostIP:      net.ParseIP(hip),
			HostPort:    8080,
			HostPortEnd: 8080,
		}
	}

	// WSL2Mirrored should only affect IPv4 results, and only if there's a port binding
	// to a loopback address or docker-proxy is disabled. Share other results files.
	rnWSL2Mirrored := func(resName string) string {
		if config.IPv4 && config.WSL2Mirrored && (bindLocalhost || !config.Hairpin) {
			return resName + ",wsl2mirrored=true"
		}
		return resName
	}

	// Initialise iptables, check the iptables config looks like it should look at the
	// end of the test (after deleting per-network and per-port rules).
	fw, err := NewIptabler(t.Context(), config)
	assert.NilError(t, err)
	checkResults("iptables", rnWSL2Mirrored(fmt.Sprintf("%s/cleaned,hairpin=%v", tn, config.Hairpin)), config.IPv4)
	checkResults("ip6tables", fmt.Sprintf("%s/cleaned,hairpin=%v", tn, config.Hairpin), config.IPv6)

	// Add the network.
	nw, err := fw.NewNetwork(t.Context(), netConfig)
	assert.NilError(t, err)

	// Add an endpoint.
	epAddr4 := netip.MustParseAddr("192.168.0.2")
	epAddr6 := netip.MustParseAddr("fd49:efd7:54aa::1")
	err = nw.AddEndpoint(t.Context(), epAddr4, epAddr6)
	assert.NilError(t, err)

	// Add IPv4 and IPv6 port mappings.
	var pb4, pb6 types.PortBinding
	if bindLocalhost {
		pb4 = makePB("127.0.0.1", epAddr4)
		pb6 = makePB("::1", epAddr6)
	} else {
		pb4 = makePB("0.0.0.0", epAddr4)
		pb6 = makePB("::", epAddr6)
	}
	err = nw.AddPorts(t.Context(), []types.PortBinding{pb4, pb6})
	assert.NilError(t, err)

	// Check the resulting iptables config.
	checkResults("iptables", rnWSL2Mirrored(resName), config.IPv4)
	checkResults("ip6tables", resName, config.IPv6)

	// Remove the port mappings and the network, and check the result.
	err = nw.DelPorts(t.Context(), []types.PortBinding{pb4, pb6})
	assert.NilError(t, err)
	err = nw.DelEndpoint(t.Context(), epAddr4, epAddr6)
	assert.NilError(t, err)
	err = nw.DelNetworkLevelRules(t.Context())
	assert.NilError(t, err)
	checkResults("iptables", rnWSL2Mirrored(fmt.Sprintf("%s/cleaned,hairpin=%v", tn, config.Hairpin)), config.IPv4)
	checkResults("ip6tables", fmt.Sprintf("%s/cleaned,hairpin=%v", tn, config.Hairpin), config.IPv6)
}

// TestDNATFromOwnBridge checks when a port binding's DNAT rule leaves packets
// arriving from the network's own bridge to the userland proxy, and that the
// rule is deleted with the binding.
func TestDNATFromOwnBridge(t *testing.T) {
	skip.If(t, iptables.UsingFirewalld(), "firewalld is running in the host netns, it can't modify rules in the test's netns")

	testcases := []struct {
		hairpin, icc, noSelfConnect bool
		expProxySkip                bool
	}{
		{hairpin: true, icc: true},
		{hairpin: true, icc: false, noSelfConnect: true},
		{icc: true, expProxySkip: true},
		{icc: true, noSelfConnect: true, expProxySkip: true},
		{icc: false, expProxySkip: true},
		{icc: false, noSelfConnect: true},
	}
	for _, tc := range testcases {
		t.Run(fmt.Sprintf("hairpin=%v/icc=%v/noSelfConnect=%v", tc.hairpin, tc.icc, tc.noSelfConnect), func(t *testing.T) {
			defer netnsutils.SetupTestOSContext(t, netnsutils.WithSetNsHandles(false))()

			fw, err := NewIptabler(t.Context(), firewaller.Config{IPv4: true, Hairpin: tc.hairpin})
			assert.NilError(t, err)
			nw, err := fw.NewNetwork(t.Context(), firewaller.NetworkConfig{
				IfName:     "br-dummy",
				ICC:        tc.icc,
				Masquerade: true,
				Config4:    firewaller.NetworkConfigFam{Prefix: netip.MustParsePrefix("192.168.0.0/24")},
			})
			assert.NilError(t, err)
			pb := types.PortBinding{
				Proto:         types.TCP,
				IP:            net.ParseIP("192.168.0.2"),
				Port:          80,
				HostIP:        net.IPv4zero,
				HostPort:      8080,
				HostPortEnd:   8080,
				NoSelfConnect: tc.noSelfConnect,
			}

			dnatRules := func() []string {
				t.Helper()
				out, err := iptables.GetIptable(iptables.IPv4).Raw("-t", "nat", "-S", dockerChain)
				assert.NilError(t, err)
				return slices.DeleteFunc(strings.Split(string(out), "\n"), func(r string) bool {
					return !strings.Contains(r, "-j DNAT")
				})
			}

			assert.NilError(t, nw.AddPorts(t.Context(), []types.PortBinding{pb}))
			rules := dnatRules()
			assert.Assert(t, is.Len(rules, 1))
			assert.Check(t, strings.Contains(rules[0], "! -i br-dummy") == tc.expProxySkip, "DNAT rule: %s", rules[0])

			assert.NilError(t, nw.DelPorts(t.Context(), []types.PortBinding{pb}))
			assert.Check(t, is.Len(dnatRules(), 0))
		})
	}
}
