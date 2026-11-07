package overlay

import (
	"maps"
	"net/netip"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/internal/countmap"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/hashable"
	"github.com/moby/moby/v2/daemon/libnetwork/nlwrap"
	"github.com/moby/moby/v2/daemon/libnetwork/ns"
	"github.com/moby/moby/v2/daemon/libnetwork/osl"
	"github.com/moby/moby/v2/internal/testutil/netnsutils"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

// newPeerTestNetwork returns a network with a sandbox which holds a VXLAN
// link, so that peers can be programmed into it. The caller must be in a test
// OS context.
func newPeerTestNetwork(t *testing.T) *network {
	t.Helper()
	n := &network{
		id:     "peertestnet",
		driver: &driver{},
		fdbCnt: countmap.Map[hashable.IPMAC]{},
	}

	sbox, err := osl.NewSandbox(filepath.Join(t.TempDir(), "netns"), true, false)
	assert.NilError(t, err)
	t.Cleanup(func() { _ = sbox.Destroy() })

	const vxlanName = "vxpeertest"
	err = ns.NlHandle().LinkAdd(&netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{Name: vxlanName},
		VxlanId:   4242,
	})
	assert.NilError(t, err)
	assert.NilError(t, sbox.AddInterface(t.Context(), vxlanName, "vxlan", ""))

	n.sbox = sbox
	n.sboxInit = true
	n.subnets = []*subnet{{
		initDone:  true,
		vxlanName: vxlanName,
		vni:       4242,
		subnetIP:  netip.MustParsePrefix("10.0.0.0/24"),
		gwIP:      netip.MustParsePrefix("10.0.0.1/24"),
	}}
	return n
}

// assertKernelPeers checks the IP neighbor entries and the permanent FDB
// entries of the VXLAN link in the sandbox of n, and that the FDB reference
// counts match the FDB entries.
func assertKernelPeers(t *testing.T, n *network, wantNeighs, wantFDB map[string]string) {
	t.Helper()
	nsh, err := netns.GetFromPath(n.sbox.Key())
	assert.NilError(t, err)
	defer nsh.Close()
	nlh, err := nlwrap.NewHandleAt(nsh, syscall.NETLINK_ROUTE)
	assert.NilError(t, err)
	defer nlh.Close()

	var link netlink.Link
	for _, iface := range n.sbox.Interfaces() {
		if iface.SrcName() == n.subnets[0].vxlanName {
			link, err = nlh.LinkByName(iface.DstName())
			assert.NilError(t, err)
		}
	}
	assert.Assert(t, link != nil)

	neighs := map[string]string{}
	entries, err := nlh.NeighList(link.Attrs().Index, syscall.AF_INET)
	assert.NilError(t, err)
	for _, e := range entries {
		neighs[e.IP.String()] = e.HardwareAddr.String()
	}
	fdb := map[string]string{}
	entries, err = nlh.NeighList(link.Attrs().Index, syscall.AF_BRIDGE)
	assert.NilError(t, err)
	for _, e := range entries {
		if e.State&netlink.NUD_PERMANENT != 0 && e.IP != nil {
			fdb[e.HardwareAddr.String()] = e.IP.String()
		}
	}
	assert.Check(t, is.DeepEqual(neighs, wantNeighs))
	assert.Check(t, is.DeepEqual(fdb, wantFDB))

	wantCnt := countmap.Map[hashable.IPMAC]{}
	for mac, vtep := range fdb {
		wantCnt.Add(hashable.IPMACFrom(netip.MustParseAddr(vtep), mustMAC(mac)), 1)
	}
	assert.Check(t, maps.Equal(n.fdbCnt, wantCnt), "fdbCnt=%v", n.fdbCnt)
}

// TestPeerLocalAndRemote checks that no remote peer is programmed for an IP
// address which a local endpoint uses, in whichever order the peer events for
// the address arrive.
func TestPeerLocalAndRemote(t *testing.T) {
	ip := netip.MustParsePrefix("10.0.0.5/24")
	mac := mustMAC("02:42:0a:00:00:05")
	vtepA := netip.MustParseAddr("192.0.2.10")
	vtepB := netip.MustParseAddr("192.0.2.11")
	var local netip.Addr // Local peers are signified by an invalid vtep.

	none := map[string]string{}
	neigh := map[string]string{ip.Addr().String(): mac.String()}
	fdbVia := func(vtep netip.Addr) map[string]string {
		return map[string]string{mac.String(): vtep.String()}
	}

	t.Run("remote deleted after local join", func(t *testing.T) {
		defer netnsutils.SetupTestOSContext(t)()
		n := newPeerTestNetwork(t)

		assert.NilError(t, n.peerAdd("remote", ip, mac, vtepA))
		assertKernelPeers(t, n, neigh, fdbVia(vtepA))
		assert.NilError(t, n.peerAdd("local", ip, mac, local))
		assertKernelPeers(t, n, none, none)
		assert.NilError(t, n.peerDelete("remote", ip, mac, vtepA))
		assertKernelPeers(t, n, none, none)
		assert.NilError(t, n.peerDelete("local", ip, mac, local))
		assertKernelPeers(t, n, none, none)

		// The next remote user of the IP address is programmed.
		assert.NilError(t, n.peerAdd("next", ip, mac, vtepB))
		assertKernelPeers(t, n, neigh, fdbVia(vtepB))
	})

	t.Run("local leaves before remote is deleted", func(t *testing.T) {
		defer netnsutils.SetupTestOSContext(t)()
		n := newPeerTestNetwork(t)

		assert.NilError(t, n.peerAdd("local", ip, mac, local))
		assert.NilError(t, n.peerAdd("remote", ip, mac, vtepA))
		assertKernelPeers(t, n, none, none)
		assert.NilError(t, n.peerDelete("local", ip, mac, local))
		assertKernelPeers(t, n, neigh, fdbVia(vtepA))
		assert.NilError(t, n.peerDelete("remote", ip, mac, vtepA))
		assertKernelPeers(t, n, none, none)
	})

	t.Run("sandbox initialized after both were added", func(t *testing.T) {
		defer netnsutils.SetupTestOSContext(t)()
		n := newPeerTestNetwork(t)
		sbox := n.sbox
		n.sbox = nil

		assert.NilError(t, n.peerAdd("remote", ip, mac, vtepA))
		assert.NilError(t, n.peerAdd("local", ip, mac, local))
		n.sbox = sbox
		assert.NilError(t, n.initSandboxPeerDB())
		assertKernelPeers(t, n, none, none)
	})
}
