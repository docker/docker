package libnetwork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/moby/moby/v2/daemon/libnetwork/config"
	"github.com/moby/moby/v2/daemon/libnetwork/driverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/setmatrix"
	"github.com/moby/moby/v2/daemon/libnetwork/ipams/defaultipam"
	"github.com/moby/moby/v2/daemon/libnetwork/ipamutils"
	"github.com/moby/moby/v2/daemon/libnetwork/netlabel"
	"github.com/moby/moby/v2/daemon/libnetwork/netutils"
	"github.com/moby/moby/v2/daemon/libnetwork/scope"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/moby/moby/v2/internal/testutil/libnetworkutils"
	"github.com/moby/moby/v2/internal/testutil/netnsutils"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

func TestNetworkMarshalling(t *testing.T) {
	n := &Network{
		name:        "Miao",
		id:          "abccba",
		ipamType:    "default",
		addrSpace:   "viola",
		networkType: "bridge",
		enableIPv4:  true,
		enableIPv6:  true,
		persist:     true,
		configOnly:  true,
		configFrom:  "configOnlyX",
		ipamOptions: map[string]string{
			netlabel.MacAddress: "a:b:c:d:e:f",
			"primary":           "",
		},
		ipamV4Config: []*IpamConf{
			{
				PreferredPool: "10.2.0.0/16",
				SubPool:       "10.2.0.0/24",
				Gateway:       "",
				AuxAddresses:  nil,
			},
			{
				PreferredPool: "10.2.0.0/16",
				SubPool:       "10.2.1.0/24",
				Gateway:       "10.2.1.254",
			},
		},
		ipamV6Config: []*IpamConf{
			{
				PreferredPool: "abcd::/64",
				SubPool:       "abcd:abcd:abcd:abcd:abcd::/80",
				Gateway:       "abcd::29/64",
				AuxAddresses:  nil,
			},
		},
		ipamV4Info: []*IpamInfo{
			{
				PoolID: "ipoolverde123",
				Meta: map[string]string{
					netlabel.Gateway: "10.2.1.255/16",
				},
				IPAMData: driverapi.IPAMData{
					AddressSpace: "viola",
					Pool: &net.IPNet{
						IP:   net.IP{10, 2, 0, 0},
						Mask: net.IPMask{255, 255, 255, 0},
					},
					Gateway:      nil,
					AuxAddresses: nil,
				},
			},
			{
				PoolID: "ipoolblue345",
				Meta: map[string]string{
					netlabel.Gateway: "10.2.1.255/16",
				},
				IPAMData: driverapi.IPAMData{
					AddressSpace: "viola",
					Pool: &net.IPNet{
						IP:   net.IP{10, 2, 1, 0},
						Mask: net.IPMask{255, 255, 255, 0},
					},
					Gateway: &net.IPNet{IP: net.IP{10, 2, 1, 254}, Mask: net.IPMask{255, 255, 255, 0}},
					AuxAddresses: map[string]*net.IPNet{
						"ip3": {IP: net.IP{10, 2, 1, 3}, Mask: net.IPMask{255, 255, 255, 0}},
						"ip5": {IP: net.IP{10, 2, 1, 55}, Mask: net.IPMask{255, 255, 255, 0}},
					},
				},
			},
			{
				PoolID: "weirdinfo",
				IPAMData: driverapi.IPAMData{
					Gateway: &net.IPNet{
						IP:   net.IP{11, 2, 1, 255},
						Mask: net.IPMask{255, 0, 0, 0},
					},
				},
			},
		},
		ipamV6Info: []*IpamInfo{
			{
				PoolID: "ipoolv6",
				IPAMData: driverapi.IPAMData{
					AddressSpace: "viola",
					Pool: &net.IPNet{
						IP:   net.IP{0xab, 0xcd, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
						Mask: net.IPMask{255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 0, 0, 0, 0, 0, 0},
					},
					Gateway: &net.IPNet{
						IP:   net.IP{0xab, 0xcd, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 29},
						Mask: net.IPMask{255, 255, 255, 255, 255, 255, 255, 255, 255, 255, 0, 0, 0, 0, 0, 0},
					},
					AuxAddresses: nil,
				},
			},
		},
		labels: map[string]string{
			"color":        "blue",
			"superimposed": "",
		},
		created: time.Now(),
	}

	b, err := json.Marshal(n)
	if err != nil {
		t.Fatal(err)
	}

	nn := &Network{}
	err = json.Unmarshal(b, nn)
	if err != nil {
		t.Fatal(err)
	}

	if n.name != nn.name || n.id != nn.id || n.networkType != nn.networkType || n.ipamType != nn.ipamType ||
		n.addrSpace != nn.addrSpace || n.enableIPv4 != nn.enableIPv4 || n.enableIPv6 != nn.enableIPv6 ||
		n.persist != nn.persist || !compareIpamConfList(n.ipamV4Config, nn.ipamV4Config) ||
		!compareIpamInfoList(n.ipamV4Info, nn.ipamV4Info) || !compareIpamConfList(n.ipamV6Config, nn.ipamV6Config) ||
		!compareIpamInfoList(n.ipamV6Info, nn.ipamV6Info) ||
		!compareStringMaps(n.ipamOptions, nn.ipamOptions) ||
		!compareStringMaps(n.labels, nn.labels) ||
		!n.created.Equal(nn.created) ||
		n.configOnly != nn.configOnly || n.configFrom != nn.configFrom {
		t.Fatalf("JSON marsh/unmarsh failed."+
			"\nOriginal:\n%#v\nDecoded:\n%#v"+
			"\nOriginal ipamV4Conf: %#v\n\nDecoded ipamV4Conf: %#v"+
			"\nOriginal ipamV4Info: %s\n\nDecoded ipamV4Info: %s"+
			"\nOriginal ipamV6Conf: %#v\n\nDecoded ipamV6Conf: %#v"+
			"\nOriginal ipamV6Info: %s\n\nDecoded ipamV6Info: %s",
			n, nn, printIpamConf(n.ipamV4Config), printIpamConf(nn.ipamV4Config),
			printIpamInfo(n.ipamV4Info), printIpamInfo(nn.ipamV4Info),
			printIpamConf(n.ipamV6Config), printIpamConf(nn.ipamV6Config),
			printIpamInfo(n.ipamV6Info), printIpamInfo(nn.ipamV6Info))
	}
}

func printIpamConf(list []*IpamConf) string {
	s := "\n[]*IpamConfig{"
	for _, i := range list {
		s = fmt.Sprintf("%s %v,", s, i)
	}
	s = s + "}"
	return s
}

func printIpamInfo(list []*IpamInfo) string {
	s := "\n[]*IpamInfo{"
	for _, i := range list {
		s = fmt.Sprintf("%s\n{\n%s\n}", s, i)
	}
	s = s + "\n}"
	return s
}

func TestEndpointMarshalling(t *testing.T) {
	ip, nw6, err := net.ParseCIDR("2001:db8:4003::122/64")
	if err != nil {
		t.Fatal(err)
	}
	nw6.IP = ip

	var lla []*net.IPNet
	for _, nw := range []string{"169.254.0.1/16", "169.254.1.1/16", "169.254.2.2/16"} {
		ll, _ := types.ParseCIDR(nw)
		lla = append(lla, ll)
	}

	e := &Endpoint{
		name:      "Bau",
		id:        "efghijklmno",
		sandboxID: "ambarabaciccicocco",
		iface: &EndpointInterface{
			mac: []byte{11, 12, 13, 14, 15, 16},
			addr: &net.IPNet{
				IP:   net.IP{10, 0, 1, 23},
				Mask: net.IPMask{255, 255, 255, 0},
			},
			addrv6:    nw6,
			srcName:   "veth12ab1314",
			dstPrefix: "eth",
			v4PoolID:  "poolpool",
			v6PoolID:  "poolv6",
			llAddrs:   lla,
		},
		dnsNames: []string{"test", "foobar", "baz"},
	}

	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}

	ee := &Endpoint{}
	err = json.Unmarshal(b, ee)
	if err != nil {
		t.Fatal(err)
	}

	if e.name != ee.name || e.id != ee.id || e.sandboxID != ee.sandboxID || !reflect.DeepEqual(e.dnsNames, ee.dnsNames) || !compareEndpointInterface(e.iface, ee.iface) {
		t.Fatalf("JSON marsh/unmarsh failed.\nOriginal:\n%#v\nDecoded:\n%#v\nOriginal iface: %#v\nDecodediface:\n%#v", e, ee, e.iface, ee.iface)
	}
}

func compareEndpointInterface(a, b *EndpointInterface) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.srcName == b.srcName && a.dstPrefix == b.dstPrefix && a.dstName == b.dstName && a.v4PoolID == b.v4PoolID && a.v6PoolID == b.v6PoolID &&
		types.CompareIPNet(a.addr, b.addr) && types.CompareIPNet(a.addrv6, b.addrv6) && compareNwLists(a.llAddrs, b.llAddrs)
}

func compareIpamConfList(listA, listB []*IpamConf) bool {
	var a, b *IpamConf
	if len(listA) != len(listB) {
		return false
	}
	for i := range listA {
		a = listA[i]
		b = listB[i]
		if a.PreferredPool != b.PreferredPool ||
			a.SubPool != b.SubPool ||
			a.Gateway != b.Gateway || !compareStringMaps(a.AuxAddresses, b.AuxAddresses) {
			return false
		}
	}
	return true
}

func compareIpamInfoList(listA, listB []*IpamInfo) bool {
	var a, b *IpamInfo
	if len(listA) != len(listB) {
		return false
	}
	for i := range listA {
		a = listA[i]
		b = listB[i]
		if a.PoolID != b.PoolID || !compareStringMaps(a.Meta, b.Meta) ||
			!types.CompareIPNet(a.Gateway, b.Gateway) ||
			a.AddressSpace != b.AddressSpace ||
			!types.CompareIPNet(a.Pool, b.Pool) ||
			!compareAddresses(a.AuxAddresses, b.AuxAddresses) {
			return false
		}
	}
	return true
}

func compareStringMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) > 0 {
		for k := range a {
			if a[k] != b[k] {
				return false
			}
		}
	}
	return true
}

func compareAddresses(a, b map[string]*net.IPNet) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) > 0 {
		for k := range a {
			if !types.CompareIPNet(a[k], b[k]) {
				return false
			}
		}
	}
	return true
}

func compareNwLists(a, b []*net.IPNet) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !types.CompareIPNet(a[k], b[k]) {
			return false
		}
	}
	return true
}

func TestAuxAddresses(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	c, err := New(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	n := &Network{
		enableIPv4:  true,
		ipamType:    defaultipam.DriverName,
		networkType: "bridge",
		controller:  c,
	}

	input := []struct {
		masterPool   string
		subPool      string
		auxAddresses map[string]string
		good         bool
	}{
		{"192.168.0.0/16", "", map[string]string{"goodOne": "192.168.2.2"}, true},
		{"192.168.0.0/16", "", map[string]string{"badOne": "192.169.2.3"}, false},
		{"192.168.0.0/16", "192.168.1.0/24", map[string]string{"goodOne": "192.168.1.2"}, true},
		{"192.168.0.0/16", "192.168.1.0/24", map[string]string{"stillGood": "192.168.2.4"}, true},
		{"192.168.0.0/16", "192.168.1.0/24", map[string]string{"badOne": "192.169.2.4"}, false},
	}

	for _, i := range input {
		n.ipamV4Config = []*IpamConf{{PreferredPool: i.masterPool, SubPool: i.subPool, AuxAddresses: i.auxAddresses}}

		err = n.ipamAllocate()

		if i.good != (err == nil) {
			t.Fatalf("Unexpected result for %v: %v", i, err)
		}

		n.ipamRelease()
	}
}

func TestEndpointNameLabel(t *testing.T) {
	skip.If(t, runtime.GOOS == "windows", "test causes sync issue with Windows HNS")
	defer netnsutils.SetupTestOSContext(t)()

	c, err := New(t.Context())
	assert.NilError(t, err)

	ipamOpt := NetworkOptionIpam(defaultipam.DriverName, "", []*IpamConf{{PreferredPool: "10.35.0.0/16", Gateway: "10.35.255.253"}}, nil, nil)
	gnw, err := c.NewNetwork(t.Context(), "bridge", "label-test", "",
		NetworkOptionEnableIPv4(true),
		ipamOpt,
	)
	assert.NilError(t, err)
	defer func() {
		err := gnw.Delete()
		assert.NilError(t, err)
	}()

	createOptions := CreateOptionIPAM(net.ParseIP("10.35.0.10"), nil, nil)
	ep, err := gnw.CreateEndpoint(t.Context(), "ep1", createOptions)
	assert.NilError(t, err)

	assert.Check(t, is.Equal(ep.ipamOptions[netlabel.EndpointName], "ep1"), "got: %s; expected: ep1", ep.ipamOptions[netlabel.EndpointName])

	defer ep.Delete(t.Context(), false) //nolint:errcheck
}

func TestUpdateSvcRecord(t *testing.T) {
	skip.If(t, runtime.GOOS == "windows", "bridge driver and IPv6, only works on linux")

	tests := []struct {
		name     string
		epName   string
		addr4    string
		addr6    string
		expAddrs []netip.Addr
	}{
		{
			name:     "v4only",
			epName:   "ep4",
			addr4:    "172.16.0.2/24",
			expAddrs: []netip.Addr{netip.MustParseAddr("172.16.0.2")},
		},
		{
			name:     "v6only",
			epName:   "ep6",
			addr6:    "fde6:045d:b2aa::2/64",
			expAddrs: []netip.Addr{netip.MustParseAddr("fde6:45d:b2aa::2")},
		},
		{
			name:   "dual-stack",
			epName: "ep46",
			addr4:  "172.16.1.2/24",
			addr6:  "fd60:8677:5a4c::2/64",
			expAddrs: []netip.Addr{
				netip.MustParseAddr("172.16.1.2"),
				netip.MustParseAddr("fd60:8677:5a4c::2"),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer netnsutils.SetupTestOSContext(t)()
			ctrlr, err := New(t.Context())
			assert.NilError(t, err)

			var ipam4, ipam6 []*IpamConf
			var ip4, ip6 net.IP
			if tc.addr4 != "" {
				var net4 *net.IPNet
				ip4, net4, err = net.ParseCIDR(tc.addr4)
				assert.NilError(t, err)
				ipam4 = []*IpamConf{{PreferredPool: net4.String()}}
			}
			if tc.addr6 != "" {
				var net6 *net.IPNet
				ip6, net6, err = net.ParseCIDR(tc.addr6)
				assert.NilError(t, err)
				ipam6 = []*IpamConf{{PreferredPool: net6.String()}}
			}
			n, err := ctrlr.NewNetwork(t.Context(), "bridge", "net1", "", nil,
				NetworkOptionEnableIPv4(tc.addr4 != ""),
				NetworkOptionEnableIPv6(tc.addr6 != ""),
				NetworkOptionIpam(defaultipam.DriverName, "", ipam4, ipam6, nil),
			)
			assert.NilError(t, err)
			dnsName := "id-" + tc.epName
			ep, err := n.CreateEndpoint(t.Context(), tc.epName,
				CreateOptionDNSNames([]string{tc.epName, dnsName}),
				CreateOptionIPAM(ip4, ip6, nil),
			)
			assert.NilError(t, err)

			n.updateSvcRecord(t.Context(), ep, true)
			for _, name := range []string{tc.epName, dnsName} {
				addrs, found4, found6 := getSvcRecords(t, n, name)
				assert.Check(t, found4 == (tc.addr4 != ""), "name:%s", name)
				assert.Check(t, found6 == (tc.addr6 != ""), "name:%s", name)
				assert.Check(t, is.DeepEqual(addrs, tc.expAddrs, cmpopts.EquateComparable(netip.Addr{})))
			}

			n.updateSvcRecord(t.Context(), ep, false)
			for _, name := range []string{tc.epName, dnsName} {
				addrs, found4, found6 := getSvcRecords(t, n, tc.epName)
				assert.Check(t, !found4, "name:%s", name)
				assert.Check(t, !found6, "name:%s", name)
				assert.Check(t, is.Len(addrs, 0))
			}
		})
	}
}

func getSvcRecords(t *testing.T, n *Network, key string) (addrs []netip.Addr, found4, found6 bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.controller.mu.Lock()
	defer n.controller.mu.Unlock()

	sr, ok := n.controller.svcRecords[n.id]
	assert.Assert(t, ok)

	lookup := func(svcMap *setmatrix.SetMatrix[string, svcMapEntry]) bool {
		mapEntryList, ok := svcMap.Get(key)
		if !ok {
			return false
		}
		assert.Assert(t, len(mapEntryList) > 0,
			"Found empty list of IP addresses: key:%s, net:%s, nid:%s", key, n.name, n.id)
		addr, err := netip.ParseAddr(mapEntryList[0].ip)
		assert.NilError(t, err)
		addrs = append(addrs, addr)
		return true
	}
	return addrs, lookup(&sr.svcMap), lookup(&sr.svcIPv6Map)
}

func TestSRVServiceQuery(t *testing.T) {
	skip.If(t, runtime.GOOS == "windows", "test only works on linux")

	defer netnsutils.SetupTestOSContext(t)()

	c, err := New(t.Context(), libnetworkutils.OptionTempNetnsDir(t),
		config.OptionDefaultAddressPoolConfig(ipamutils.GetLocalScopeDefaultNetworks()))
	if err != nil {
		t.Fatal(err)
	}

	n, err := c.NewNetwork(t.Context(), "bridge", "net1", "",
		NetworkOptionEnableIPv4(true),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := n.Delete(); err != nil {
			t.Fatal(err)
		}
	}()

	ep, err := n.CreateEndpoint(t.Context(), "testep")
	if err != nil {
		t.Fatal(err)
	}

	sb, err := c.NewSandbox(t.Context(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := sb.Delete(t.Context()); err != nil {
			t.Fatal(err)
		}
	}()

	err = ep.Join(t.Context(), sb)
	if err != nil {
		t.Fatal(err)
	}

	sr := &svcInfo{
		service: make(map[string][]servicePorts),
	}
	// backing container for the service
	cTarget := serviceTarget{
		name: "task1.web.swarm",
		ip:   net.ParseIP("192.168.10.2"),
		port: 80,
	}
	// backing host for the service
	hTarget := serviceTarget{
		name: "node1.docker-cluster",
		ip:   net.ParseIP("10.10.10.2"),
		port: 45321,
	}
	httpPort := servicePorts{
		portName: "_http",
		proto:    "_tcp",
		target:   []serviceTarget{cTarget},
	}

	extHTTPPort := servicePorts{
		portName: "_host_http",
		proto:    "_tcp",
		target:   []serviceTarget{hTarget},
	}
	sr.service["web.swarm"] = append(sr.service["web.swarm"], httpPort)
	sr.service["web.swarm"] = append(sr.service["web.swarm"], extHTTPPort)

	c.svcRecords[n.ID()] = sr

	ctx := t.Context()
	_, ip := ep.Info().Sandbox().ResolveService(ctx, "_http._tcp.web.swarm")

	if len(ip) == 0 {
		t.Fatal(err)
	}
	if ip[0].String() != "192.168.10.2" {
		t.Fatal(err)
	}

	_, ip = ep.Info().Sandbox().ResolveService(ctx, "_host_http._tcp.web.swarm")

	if len(ip) == 0 {
		t.Fatal(err)
	}
	if ip[0].String() != "10.10.10.2" {
		t.Fatal(err)
	}

	// Service name with invalid protocol name. Should fail without error
	_, ip = ep.Info().Sandbox().ResolveService(ctx, "_http._icmp.web.swarm")
	if len(ip) != 0 {
		t.Fatal("Valid response for invalid service name")
	}
}

func TestServiceVIPReuse(t *testing.T) {
	skip.If(t, runtime.GOOS == "windows", "test only works on linux")

	defer netnsutils.SetupTestOSContext(t)()

	c, err := New(t.Context(), libnetworkutils.OptionTempNetnsDir(t),
		config.OptionDefaultAddressPoolConfig(ipamutils.GetLocalScopeDefaultNetworks()))
	if err != nil {
		t.Fatal(err)
	}

	n, err := c.NewNetwork(t.Context(), "bridge", "net1", "", nil,
		NetworkOptionEnableIPv4(true),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := n.Delete(); err != nil {
			t.Fatal(err)
		}
	}()

	ep, err := n.CreateEndpoint(t.Context(), "testep")
	if err != nil {
		t.Fatal(err)
	}

	sb, err := c.NewSandbox(t.Context(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := sb.Delete(t.Context()); err != nil {
			t.Fatal(err)
		}
	}()

	err = ep.Join(t.Context(), sb)
	if err != nil {
		t.Fatal(err)
	}

	// Add 2 services with same name but different service ID to share the same VIP
	n.addSvcRecords("ep1", "service_test", "serviceID1", net.ParseIP("192.168.0.1"), net.IP{}, true, "test")
	n.addSvcRecords("ep2", "service_test", "serviceID2", net.ParseIP("192.168.0.1"), net.IP{}, true, "test")

	ipToResolve := netutils.ReverseIP("192.168.0.1")

	ctx := t.Context()
	ipList, _ := n.ResolveName(ctx, "service_test", types.IPv4)
	if len(ipList) == 0 {
		t.Fatal("There must be the VIP")
	}
	if len(ipList) != 1 {
		t.Fatal("It must return only 1 VIP")
	}
	if ipList[0].String() != "192.168.0.1" {
		t.Fatal("The service VIP is 192.168.0.1")
	}
	name := n.ResolveIP(ctx, ipToResolve)
	if name == "" {
		t.Fatal("It must return a name")
	}
	if name != "service_test.net1" {
		t.Fatalf("It must return the service_test.net1 != %s", name)
	}

	// Delete service record for one of the services, the IP should remain because one service is still associated with it
	n.deleteSvcRecords("ep1", "service_test", "serviceID1", net.ParseIP("192.168.0.1"), net.IP{}, true, "test")
	ipList, _ = n.ResolveName(ctx, "service_test", types.IPv4)
	if len(ipList) == 0 {
		t.Fatal("There must be the VIP")
	}
	if len(ipList) != 1 {
		t.Fatal("It must return only 1 VIP")
	}
	if ipList[0].String() != "192.168.0.1" {
		t.Fatal("The service VIP is 192.168.0.1")
	}
	name = n.ResolveIP(ctx, ipToResolve)
	if name == "" {
		t.Fatal("It must return a name")
	}
	if name != "service_test.net1" {
		t.Fatalf("It must return the service_test.net1 != %s", name)
	}

	// Delete again the service using the previous service ID, nothing should happen
	n.deleteSvcRecords("ep2", "service_test", "serviceID1", net.ParseIP("192.168.0.1"), net.IP{}, true, "test")
	ipList, _ = n.ResolveName(ctx, "service_test", types.IPv4)
	if len(ipList) == 0 {
		t.Fatal("There must be the VIP")
	}
	if len(ipList) != 1 {
		t.Fatal("It must return only 1 VIP")
	}
	if ipList[0].String() != "192.168.0.1" {
		t.Fatal("The service VIP is 192.168.0.1")
	}
	name = n.ResolveIP(ctx, ipToResolve)
	if name == "" {
		t.Fatal("It must return a name")
	}
	if name != "service_test.net1" {
		t.Fatalf("It must return the service_test.net1 != %s", name)
	}

	// Delete now using the second service ID, now all the entries should be gone
	n.deleteSvcRecords("ep2", "service_test", "serviceID2", net.ParseIP("192.168.0.1"), net.IP{}, true, "test")
	ipList, _ = n.ResolveName(ctx, "service_test", types.IPv4)
	if len(ipList) != 0 {
		t.Fatal("All the VIPs should be gone now")
	}
	name = n.ResolveIP(ctx, ipToResolve)
	if name != "" {
		t.Fatalf("It must return empty no more services associated, instead:%s", name)
	}
}

func TestIpamReleaseOnNetDriverFailures(t *testing.T) {
	skip.If(t, runtime.GOOS == "windows", "test only works on linux")

	defer netnsutils.SetupTestOSContext(t)()

	c, err := New(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	const badDriverName = "bad network driver"
	bd := &badDriver{failNetworkCreation: true}
	if err := c.drvRegistry.RegisterDriver(badDriverName, bd, driverapi.Capability{DataScope: scope.Local}); err != nil {
		t.Fatal(err)
	}

	// Test whether ipam state release is invoked  on network create failure from net driver
	// by checking whether subsequent network creation requesting same gateway IP succeeds
	ipamOpt := NetworkOptionIpam(defaultipam.DriverName, "", []*IpamConf{{PreferredPool: "10.34.0.0/16", Gateway: "10.34.255.254"}}, nil, nil)
	_, err = c.NewNetwork(t.Context(), badDriverName, "badnet1", "", ipamOpt)
	assert.Check(t, is.ErrorContains(err, "I will not create any network"))

	gnw, err := c.NewNetwork(t.Context(), "bridge", "goodnet1", "",
		NetworkOptionEnableIPv4(true),
		ipamOpt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := gnw.Delete(); err != nil {
		t.Fatal(err)
	}

	// Now check whether ipam release works on endpoint creation failure
	bd.failNetworkCreation = false
	bnw, err := c.NewNetwork(t.Context(), badDriverName, "badnet2", "",
		NetworkOptionEnableIPv4(true),
		ipamOpt,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := bnw.Delete(); err != nil {
			t.Fatal(err)
		}
	}()

	if _, err := bnw.CreateEndpoint(t.Context(), "ep0"); err == nil {
		t.Fatalf("bad network driver should have failed endpoint creation")
	}

	// Now create good bridge network with different gateway
	ipamOpt2 := NetworkOptionIpam(defaultipam.DriverName, "", []*IpamConf{{PreferredPool: "10.35.0.0/16", Gateway: "10.35.255.253"}}, nil, nil)
	gnw, err = c.NewNetwork(t.Context(), "bridge", "goodnet2", "",
		NetworkOptionEnableIPv4(true),
		ipamOpt2,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := gnw.Delete(); err != nil {
			t.Fatal(err)
		}
	}()

	ep, err := gnw.CreateEndpoint(t.Context(), "ep1")
	if err != nil {
		t.Fatal(err)
	}
	defer ep.Delete(t.Context(), false) //nolint:errcheck

	expectedIP, _ := types.ParseCIDR("10.35.0.1/16")
	if !types.CompareIPNet(ep.Info().Iface().Address(), expectedIP) {
		t.Fatalf("Ipam release must have failed, endpoint has unexpected address: %v", ep.Info().Iface().Address())
	}
}

type badDriver struct {
	failNetworkCreation bool
}

func (b *badDriver) CreateNetwork(ctx context.Context, nid string, options map[string]any, nInfo driverapi.NetworkInfo, ipV4Data, ipV6Data []driverapi.IPAMData) error {
	if b.failNetworkCreation {
		return errors.New("I will not create any network")
	}
	return nil
}

func (b *badDriver) DeleteNetwork(nid string) error {
	return nil
}

func (b *badDriver) CreateEndpoint(_ context.Context, nid, eid string, ifInfo driverapi.InterfaceInfo, options map[string]any) error {
	return errors.New("I will not create any endpoint")
}

func (b *badDriver) DeleteEndpoint(nid, eid string) error {
	return nil
}

func (b *badDriver) EndpointOperInfo(nid, eid string) (map[string]any, error) {
	return nil, nil
}

func (b *badDriver) Join(_ context.Context, nid, eid string, sboxKey string, jinfo driverapi.JoinInfo, _, _ map[string]any) error {
	return errors.New("I will not allow any join")
}

func (b *badDriver) Leave(nid, eid string) error {
	return nil
}

func (b *badDriver) IsBuiltIn() bool {
	return false
}
