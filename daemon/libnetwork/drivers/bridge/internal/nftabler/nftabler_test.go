//go:build linux

package nftabler

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge/internal/firewaller"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/nftables"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/moby/moby/v2/internal/testutil/netnsutils"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/golden"
	"gotest.tools/v3/icmd"
)

func TestNftabler(t *testing.T) {
	const (
		ipv4 uint64 = 1 << iota
		ipv6
		hairpin
		internal
		icc
		masq
		snat
		bindLocalhost
		wsl2Mirrored
		numBoolParams = iota
	)
	nftables.Enable()
	t.Cleanup(func() { nftables.Disable() }) // Cleanup instead of defer, this func returns before the parallel subtests finish.
	for i := range uint64(1) << numBoolParams {
		p := func(n uint64) bool { return (i & n) == n }
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
				// If updating results, don't run in parallel because some of the results files are shared.
				// Tests are dynamically linked, so the nftables code under test uses cgo to call libnftables
				// and they run faster without t.Parallel(). Strangely, when statically linked and exec-ing
				// 'nft', they run faster in parallel.
				// if !golden.FlagUpdate() { t.Parallel() }

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
				testNftabler(t, tn, config, netConfig, p(bindLocalhost), tn+"/"+resName)
			})
		}
	}
}

func testNftabler(t *testing.T, tn string, config firewaller.Config, netConfig firewaller.NetworkConfig, bindLocalhost bool, resName string) {
	defer netnsutils.SetupTestOSContext(t, netnsutils.WithSetNsHandles(false))()

	checkResults := func(family, name string, en bool) {
		t.Helper()
		res := icmd.RunCommand("nft", "list", "table", family, dockerTable)
		out := res.Combined()
		if !en {
			assert.Assert(t, is.Contains(out, "No such file or directory"))
			return
		}
		// nftables before v1.0.9 don't stringify the dstnat priority for an
		// output hook, and print its numeric value (-100) instead. Normalize the
		// output for stable test results.
		//
		// - https://git.netfilter.org/nftables/commit/?id=8beafab74c391130fbb9111bfccab8613644e3b9
		// - https://github.com/moby/moby/pull/50745 / https://github.com/moby/moby/commit/fbde2bcb9a8a8fcf06cc6fe1675560cf46237e7b
		out = strings.ReplaceAll(out, "type nat hook output priority -100", "type nat hook output priority dstnat")
		assert.Assert(t, res.Error, out)
		golden.Assert(t, out, name+"__"+family+".golden")
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
	fw, err := NewNftabler(t.Context(), config)
	assert.NilError(t, err)
	defer fw.Close()
	checkResults("ip", rnWSL2Mirrored(fmt.Sprintf("%s/cleaned,hairpin=%v", tn, config.Hairpin)), config.IPv4)
	checkResults("ip6", fmt.Sprintf("%s/cleaned,hairpin=%v", tn, config.Hairpin), config.IPv6)

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
	checkResults("ip", rnWSL2Mirrored(resName), config.IPv4)
	checkResults("ip6", resName, config.IPv6)

	// Remove the port mappings and the network, and check the result (should be the same
	// for all tests with the same "hairpin" setting).
	err = nw.DelPorts(t.Context(), []types.PortBinding{pb4, pb6})
	assert.NilError(t, err)
	err = nw.DelEndpoint(t.Context(), epAddr4, epAddr6)
	assert.NilError(t, err)
	err = nw.DelNetworkLevelRules(t.Context())
	assert.NilError(t, err)
	checkResults("ip", rnWSL2Mirrored(fmt.Sprintf("%s/cleaned,hairpin=%v", tn, config.Hairpin)), config.IPv4)
	checkResults("ip6", fmt.Sprintf("%s/cleaned,hairpin=%v", tn, config.Hairpin), config.IPv6)
}

// TestHairpinMultipleHostPorts checks that, with hairpin enabled (no userland proxy),
// more than one host port can be mapped to the same container port. The per-port
// "MASQ TO OWN PORT" rule is keyed on container address/port, so the same rule is
// generated for each host port.
func TestHairpinMultipleHostPorts(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t, netnsutils.WithSetNsHandles(false))()
	nftables.Enable()
	defer nftables.Disable()

	fw, err := NewNftabler(t.Context(), firewaller.Config{IPv4: true, Hairpin: true})
	assert.NilError(t, err)
	defer fw.Close()

	nw, err := fw.NewNetwork(t.Context(), firewaller.NetworkConfig{
		IfName:     "br-dummy",
		ICC:        true,
		Masquerade: true,
		Config4:    firewaller.NetworkConfigFam{Prefix: netip.MustParsePrefix("192.168.0.0/24")},
	})
	assert.NilError(t, err)

	epAddr4 := netip.MustParseAddr("192.168.0.2")
	err = nw.AddEndpoint(t.Context(), epAddr4, netip.Addr{})
	assert.NilError(t, err)

	makePB := func(hostPort uint16) types.PortBinding {
		return types.PortBinding{
			Proto:       types.TCP,
			IP:          epAddr4.AsSlice(),
			Port:        80,
			HostIP:      net.IPv4zero,
			HostPort:    hostPort,
			HostPortEnd: hostPort,
		}
	}
	pb8080, pb8081, pb8082 := makePB(8080), makePB(8081), makePB(8082)

	countInChain := func(chain, s string) int {
		t.Helper()
		res := icmd.RunCommand("nft", "list", "chain", "ip", dockerTable, chain)
		assert.NilError(t, res.Error, res.Combined())
		return strings.Count(res.Combined(), s)
	}
	const masqComment = `comment "MASQ TO OWN PORT"`
	const dnatComment = `comment "DNAT"`
	postRtChain := chainNatPostRtIn("br-dummy")

	// Two host ports for the same container port, in a single call.
	err = nw.AddPorts(t.Context(), []types.PortBinding{pb8080, pb8081})
	assert.NilError(t, err)
	// Another host port for the same container port, in a separate call.
	err = nw.AddPorts(t.Context(), []types.PortBinding{pb8082})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(countInChain(postRtChain, masqComment), 1))
	assert.Check(t, is.Equal(countInChain(natChain, dnatComment), 3))

	// Remove all bindings in a single call, as happens when an endpoint's ports
	// are released.
	err = nw.DelPorts(t.Context(), []types.PortBinding{pb8080, pb8081, pb8082})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(countInChain(postRtChain, masqComment), 0))
	assert.Check(t, is.Equal(countInChain(natChain, dnatComment), 0))
}

// TestDNATFromOwnBridge checks when a port binding's DNAT rule leaves packets
// arriving from the network's own bridge to the userland proxy, and that the
// rule is deleted with the binding.
func TestDNATFromOwnBridge(t *testing.T) {
	nftables.Enable()
	defer nftables.Disable()

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

			fw, err := NewNftabler(t.Context(), firewaller.Config{IPv4: true, Hairpin: tc.hairpin})
			assert.NilError(t, err)
			defer fw.Close()
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
				res := icmd.RunCommand("nft", "list", "table", "ip", dockerTable)
				assert.Assert(t, res.Error, res.Combined())
				var rules []string
				for _, r := range strings.Split(res.Stdout(), "\n") {
					if strings.Contains(r, "dnat to") {
						rules = append(rules, r)
					}
				}
				return rules
			}

			assert.NilError(t, nw.AddPorts(t.Context(), []types.PortBinding{pb}))
			rules := dnatRules()
			assert.Assert(t, is.Len(rules, 1))
			assert.Check(t, strings.Contains(rules[0], `iifname != "br-dummy"`) == tc.expProxySkip, "DNAT rule: %s", rules[0])

			assert.NilError(t, nw.DelPorts(t.Context(), []types.PortBinding{pb}))
			assert.Check(t, is.Len(dnatRules(), 0))
		})
	}
}
