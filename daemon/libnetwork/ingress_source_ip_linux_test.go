package libnetwork

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/iptables"
	"github.com/moby/moby/v2/daemon/libnetwork/nlwrap"
	"github.com/moby/moby/v2/daemon/libnetwork/osl"
	"github.com/moby/moby/v2/internal/testutil/netnsutils"
	"github.com/vishvananda/netlink"
	"gotest.tools/v3/assert"
)

func TestIngressSourceIPGateway(t *testing.T) {
	_, pool, err := net.ParseCIDR("10.66.0.0/24")
	assert.NilError(t, err)
	info := &IpamInfo{}
	info.Pool = pool
	n := &Network{ipamV4Info: []*IpamInfo{info}}
	for _, tc := range []struct {
		name, ip, mac string
		valid         bool
	}{
		{"valid", "10.66.0.2", "02:42:0a:42:00:02", true},
		{"outside pool", "10.67.0.2", "02:42:0a:42:00:02", false},
		{"network address", "10.66.0.0", "02:42:0a:42:00:02", false},
		{"invalid address", "invalid", "02:42:0a:42:00:02", false},
		{"IPv6", "2001:db8::1", "02:42:0a:42:00:02", false},
		{"invalid MAC", "10.66.0.2", "invalid", false},
		{"multicast MAC", "10.66.0.2", "01:00:5e:00:00:02", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(ingressGatewayRecord{Address: tc.ip, MAC: tc.mac})
			assert.NilError(t, err)
			gateway, err := n.parseIngressGateway(data)
			if tc.valid {
				assert.NilError(t, err)
				assert.Equal(t, gateway.id, uint32(2))
				assert.Equal(t, ingressReturnNextHop(gateway.id).String(), "169.254.0.2")
			} else {
				assert.Assert(t, err != nil)
			}
		})
	}
	_, err = n.parseIngressGateway([]byte("not JSON"))
	assert.Assert(t, err != nil)
}

func TestIngressSourceIPRules(t *testing.T) {
	mac, err := net.ParseMAC("02:42:0a:42:00:02")
	assert.NilError(t, err)
	gateways := []ingressSourceGateway{{address: net.ParseIP("10.66.0.2"), mac: mac, id: 2}}
	ports := []*PortConfig{{Protocol: ProtocolTCP, PublishedPort: 8080, TargetPort: 80}, {Protocol: ProtocolUDP, PublishedPort: 5353, TargetPort: 53}}
	rules, err := ingressGatewayRules(net.ParseIP("10.66.0.4"), net.ParseIP("10.66.0.100"), gateways, ports, 1450)
	assert.NilError(t, err)
	for _, want := range []string{
		"-p 4 -s 10.66.0.2 -d 10.66.0.4 -j MARK --set-xmark 0x20000/0xffff0000",
		"-d 10.66.0.2 -p tcp --dport 8080 -j DNAT --to-destination :80",
		"-d 10.66.0.100 -p udp --dport 5353 -j DNAT --to-destination :53",
		"--nfmask 0xffff0000 --ctmask 0xffff0000", "--set-mss 1390",
		"--ctstate NEW,ESTABLISHED -m connmark ! --mark 0/0xffff0000 -j ACCEPT",
	} {
		assert.Assert(t, strings.Contains(rules, want), "missing rule %q", want)
	}
	assert.Assert(t, !strings.Contains(rules, "SNAT"))
	assert.Assert(t, !strings.Contains(rules, "REDIRECT"))
	assert.Equal(t, strings.Count(rules, "COMMIT"), 3)
	for _, invalid := range []*PortConfig{nil, {Protocol: ProtocolTCP, PublishedPort: 65536, TargetPort: 80},
		{Protocol: ProtocolTCP, PublishedPort: 8080, TargetPort: 0}, {Protocol: 123, PublishedPort: 8080, TargetPort: 80}} {
		_, err := ingressGatewayRules(net.ParseIP("10.66.0.4"), nil, gateways, []*PortConfig{invalid}, 1450)
		assert.Assert(t, err != nil)
	}
	_, err = ingressGatewayRules(net.ParseIP("10.66.0.4"), nil, gateways, ports, 500)
	assert.Assert(t, err != nil)
}

func TestIngressSourceIPRouteOwnership(t *testing.T) {
	_, pool, err := net.ParseCIDR("10.66.0.0/24")
	assert.NilError(t, err)
	rule := ingressReturnRule(pool, 2)
	assert.Assert(t, ownedIngressReturnRule(*rule, pool))
	assert.Equal(t, rule.Mark, uint32(0x20000))
	assert.Equal(t, *rule.Mask, uint32(0xffff0000))
	assert.Equal(t, rule.Table, int(0x444b0002))
	foreign := *rule
	foreign.Priority++
	assert.Assert(t, !ownedIngressReturnRule(foreign, pool))
	foreign = *rule
	foreign.Table++
	assert.Assert(t, !ownedIngressReturnRule(foreign, pool))
	foreign = *rule
	foreign.Mark++
	assert.Assert(t, !ownedIngressReturnRule(foreign, pool))
	_, other, err := net.ParseCIDR("10.67.0.0/24")
	assert.NilError(t, err)
	assert.Assert(t, !ownedIngressReturnRule(*rule, other))
}

func TestIngressSourceIPRoutingLifecycle(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()
	handle, err := nlwrap.NewHandle(syscall.NETLINK_ROUTE)
	assert.NilError(t, err)
	defer handle.Close()
	iface := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "fixture", MTU: 1450}}
	assert.NilError(t, handle.LinkAdd(iface))
	_, pool, err := net.ParseCIDR("10.66.0.0/24")
	assert.NilError(t, err)
	address := net.ParseIP("10.66.0.4").To4()
	ifaceAddress := &net.IPNet{IP: address, Mask: pool.Mask}
	sandbox, err := osl.GetSandboxForExternalKey("/proc/thread-self/ns/net", filepath.Join(t.TempDir(), "sandbox"))
	assert.NilError(t, err)
	defer sandbox.Destroy()
	assert.NilError(t, sandbox.AddInterface(t.Context(), "fixture", "eth", "eth0", osl.WithCreatedInContainer(true), osl.WithIPv4Address(ifaceAddress)))
	ifaceLink, err := handle.LinkByName("eth0")
	assert.NilError(t, err)
	mac1, err := net.ParseMAC("02:42:0a:42:00:02")
	assert.NilError(t, err)
	mac2, err := net.ParseMAC("02:42:0a:42:00:03")
	assert.NilError(t, err)
	gateways := []ingressSourceGateway{{address: net.ParseIP("10.66.0.2"), mac: mac1, id: 2}, {address: net.ParseIP("10.66.0.3"), mac: mac2, id: 3}}
	ports := []*PortConfig{{Protocol: ProtocolTCP, PublishedPort: 8080, TargetPort: 80}}
	iptable := iptables.GetIptable(iptables.IPv4)
	assert.NilError(t, iptable.RawCombinedOutputNative("-t", "mangle", "-N", "APP-OWNED"))
	foreign := netlink.NewRule()
	foreign.Priority = 30000
	foreign.Table = 12345
	assert.NilError(t, handle.RuleAdd(foreign))
	// Namespace defaults vary between distributions. Check the prerequisite
	// explicitly before setting up the successful lifecycle.
	for _, value := range []string{"1\n", "2\n"} {
		assert.NilError(t, os.WriteFile("/proc/sys/net/ipv4/conf/all/rp_filter", []byte(value), 0o644))
		err = configureIngressReturnRoutes("eth0", address, nil, pool, gateways, ports)
		assert.ErrorContains(t, err, "requires net.ipv4.conf.all.rp_filter=0")
		_, err = handle.LinkByName(ingressSourceTunnel)
		assert.Assert(t, errors.As(err, new(netlink.LinkNotFoundError)))
	}
	assert.NilError(t, os.WriteFile("/proc/sys/net/ipv4/conf/all/rp_filter", []byte("0\n"), 0o644))
	for range 2 {
		assert.NilError(t, configureIngressReturnRoutes("eth0", address, nil, pool, gateways, ports))
	}
	rpf, err := os.ReadFile("/proc/sys/net/ipv4/conf/" + ingressSourceTunnel + "/rp_filter")
	assert.NilError(t, err)
	assert.Equal(t, strings.TrimSpace(string(rpf)), "0")
	assert.NilError(t, os.WriteFile("/proc/sys/net/ipv4/conf/all/rp_filter", []byte("2\n"), 0o644))
	err = configureIngressReturnRoutes("eth0", address, nil, pool, gateways, ports)
	assert.ErrorContains(t, err, "requires net.ipv4.conf.all.rp_filter=0")
	assert.NilError(t, os.WriteFile("/proc/sys/net/ipv4/conf/all/rp_filter", []byte("0\n"), 0o644))
	rules, err := handle.RuleList(netlink.FAMILY_V4)
	assert.NilError(t, err)
	owned := 0
	for _, rule := range rules {
		if ownedIngressReturnRule(rule, pool) {
			owned++
		}
	}
	assert.Equal(t, owned, 2)
	neighbors, err := handle.NeighList(ifaceLink.Attrs().Index, netlink.FAMILY_V4)
	assert.NilError(t, err)
	assert.Equal(t, len(neighbors), 2)
	assert.NilError(t, configureIngressReturnRoutes("eth0", address, nil, pool, gateways[1:], ports))
	rules, err = handle.RuleList(netlink.FAMILY_V4)
	assert.NilError(t, err)
	for _, rule := range rules {
		if ownedIngressReturnRule(rule, pool) {
			assert.Equal(t, rule.Table, int(ingressReturnTables|3))
		}
	}
	neighbors, err = handle.NeighList(ifaceLink.Attrs().Index, netlink.FAMILY_V4)
	assert.NilError(t, err)
	assert.Equal(t, len(neighbors), 1)
	info := &IpamInfo{}
	info.Pool = pool
	network := &Network{loadBalancerMode: loadBalancerModeSourceIP, ipamV4Info: []*IpamInfo{info}}
	endpoint := &Endpoint{network: network, iface: &EndpointInterface{srcName: "fixture", addr: ifaceAddress}}
	assert.NilError(t, clearIngressSourceIPRouting(sandbox, endpoint))
	assert.NilError(t, clearIngressSourceIPRouting(sandbox, endpoint))
	rules, err = handle.RuleList(netlink.FAMILY_V4)
	assert.NilError(t, err)
	foreignPresent := false
	for _, rule := range rules {
		assert.Assert(t, !ownedIngressReturnRule(rule, pool))
		foreignPresent = foreignPresent || rule.Table == foreign.Table
	}
	assert.Assert(t, foreignPresent)
	assert.Assert(t, iptable.ExistChain("APP-OWNED", iptables.Mangle))
	_, err = handle.LinkByName(ingressSourceTunnel)
	assert.Assert(t, err != nil)
	neighbors, err = handle.NeighList(ifaceLink.Attrs().Index, netlink.FAMILY_V4)
	assert.NilError(t, err)
	assert.Equal(t, len(neighbors), 0)
	loopback, err := handle.LinkByName("lo")
	assert.NilError(t, err)
	aliases, err := handle.AddrList(loopback, netlink.FAMILY_V4)
	assert.NilError(t, err)
	for _, alias := range aliases {
		assert.Assert(t, alias.Label != ingressSourceAlias)
	}
}
