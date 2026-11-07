package overlay

import (
	"net"
	"net/netip"
	"path/filepath"
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/driverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/countmap"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/hashable"
	"github.com/moby/moby/v2/daemon/libnetwork/ns"
	"github.com/moby/moby/v2/daemon/libnetwork/osl"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/moby/moby/v2/internal/testutil/netnsutils"
	"github.com/vishvananda/netlink"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

type fakeJoinInfo struct{}

func (fakeJoinInfo) InterfaceName() driverapi.InterfaceNameInfo               { return nil }
func (fakeJoinInfo) SetGateway(net.IP) error                                  { return nil }
func (fakeJoinInfo) SetGatewayIPv6(net.IP) error                              { return nil }
func (fakeJoinInfo) AddStaticRoute(*net.IPNet, types.RouteType, net.IP) error { return nil }
func (fakeJoinInfo) DisableGatewayService()                                   {}
func (fakeJoinInfo) ForceGw4()                                                {}
func (fakeJoinInfo) ForceGw6()                                                {}
func (fakeJoinInfo) AddTableEntry(string, string, []byte) error               { return nil }

// newJoinTestNetwork must be called in a test OS context.
func newJoinTestNetwork(t *testing.T) *network {
	t.Helper()
	d := &driver{networks: networkTable{}}
	n := &network{
		id:        "jointestnet",
		driver:    d,
		endpoints: endpointTable{},
		fdbCnt:    countmap.Map[hashable.IPMAC]{},
	}
	d.networks[n.id] = n

	sbox, err := osl.NewSandbox(filepath.Join(t.TempDir(), "netns"), true, false)
	assert.NilError(t, err)
	t.Cleanup(func() { _ = sbox.Destroy() })

	const vxlanName = "vxjointest"
	err = ns.NlHandle().LinkAdd(&netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{Name: vxlanName},
		VxlanId:   4243,
	})
	assert.NilError(t, err)
	assert.NilError(t, sbox.AddInterface(t.Context(), vxlanName, "vxlan", ""))

	n.sbox = sbox
	n.sboxInit = true
	n.subnets = []*subnet{{
		sboxInit:  true,
		vxlanName: vxlanName,
		vni:       4243,
		subnetIP:  netip.MustParsePrefix("10.0.0.0/24"),
		gwIP:      netip.MustParsePrefix("10.0.0.1/24"),
	}}
	return n
}

func addJoinTestEndpoint(t *testing.T, n *network, eid, addr, mac string) *endpoint {
	t.Helper()
	hwAddr, err := hashable.ParseMAC(mac)
	assert.NilError(t, err)
	ep := &endpoint{id: eid, nid: n.id, addr: netip.MustParsePrefix(addr), mac: hwAddr}
	n.endpoints[eid] = ep
	return ep
}

func TestJoinLeave(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()
	n := newJoinTestNetwork(t)
	d := n.driver

	ep1 := addJoinTestEndpoint(t, n, "ep1", "10.0.0.5/24", "02:42:0a:00:00:05")
	assert.NilError(t, d.Join(t.Context(), n.id, ep1.id, "", fakeJoinInfo{}, nil, nil))
	assert.Check(t, is.Equal(n.joinCnt, 1))

	// Join fails after the endpoint joined the sandbox.
	ep2 := addJoinTestEndpoint(t, n, "ep2", "10.0.0.6/24", "02:42:0a:00:00:06")
	n.subnets[0].brName = "nosuchbridge"
	err := d.Join(t.Context(), n.id, ep2.id, "", fakeJoinInfo{}, nil, nil)
	n.subnets[0].brName = ""
	assert.Check(t, is.ErrorContains(err, "could not add veth pair"))
	assert.NilError(t, d.Leave(n.id, ep2.id))
	assert.NilError(t, d.Leave(n.id, ep2.id))
	assert.Check(t, is.Equal(n.joinCnt, 1))

	// Join fails before the endpoint joined the sandbox.
	ep3 := addJoinTestEndpoint(t, n, "ep3", "10.0.0.7/24", "02:42:0a:00:00:07")
	n.secure = true
	err = d.Join(t.Context(), n.id, ep3.id, "", fakeJoinInfo{}, nil, nil)
	n.secure = false
	assert.Check(t, is.ErrorContains(err, "encryption keys not present"))
	assert.NilError(t, d.Leave(n.id, ep3.id))
	assert.Check(t, is.Equal(n.joinCnt, 1))
	assert.Check(t, n.sbox != nil, "the sandbox must stay while an endpoint is joined")

	assert.NilError(t, d.Leave(n.id, ep1.id))
	assert.Check(t, is.Equal(n.joinCnt, 0))
	assert.Check(t, n.sbox == nil, "the sandbox must be destroyed when the last endpoint leaves")
}
