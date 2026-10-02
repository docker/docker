//go:build linux

package overlay

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/internal/countmap"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/hashable"
	"github.com/moby/moby/v2/daemon/libnetwork/ns"
	"github.com/moby/moby/v2/daemon/libnetwork/osl"
	"github.com/moby/moby/v2/internal/testutil/netnsutils"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

// newEncTestDriver returns a driver with the state setupEncryption and
// removeEncryption need to run: an encryption key, and a data-plane address so
// the driver can work out which iptables to program.
func newEncTestDriver() *driver {
	return &driver{
		networks: networkTable{},
		secMap:   encrMap{},
		keys: []*key{
			{value: []byte("0123456789abcdef0123456789abcdef"), tag: 1},
		},
		bindAddress:      netip.MustParseAddr("192.0.2.1"),
		advertiseAddress: netip.MustParseAddr("192.0.2.1"),
	}
}

func newEncTestNetwork(t *testing.T, d *driver, id string, secure bool) *network {
	t.Helper()
	n := &network{
		id:     id,
		driver: d,
		secure: secure,
		fdbCnt: countmap.Map[hashable.IPMAC]{},
	}
	d.networks[id] = n
	return n
}

func mustMAC(s string) hashable.MACAddr {
	mac, err := hashable.ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return mac
}

// addPeer performs the bookkeeping addNeighbor does for a peer it programmed
// successfully: one tunnel reference and one fdb reference.
func addPeer(t *testing.T, n *network, vtep netip.Addr, mac hashable.MACAddr) {
	t.Helper()
	if n.secure {
		assert.NilError(t, n.driver.setupEncryption(vtep))
	}
	n.fdbCnt.Add(hashable.IPMACFrom(vtep, mac), 1)
}

// withSandbox gives n a real network sandbox and a subnet, and marks both as
// already initialised so joinSandbox is a no-op. The subnet names a VXLAN link
// which does not exist, so programming a neighbor entry into the sandbox fails.
func withSandbox(t *testing.T, n *network, key string) {
	t.Helper()
	sbox, err := osl.NewSandbox(filepath.Join(t.TempDir(), key), true, false)
	assert.NilError(t, err)
	t.Cleanup(func() { _ = sbox.Destroy() })

	n.sbox = sbox
	n.sboxInit = true
	n.subnets = []*subnet{{
		sboxInit:  true,
		vxlanName: "vx-absent",
		subnetIP:  netip.MustParsePrefix("10.0.0.0/24"),
	}}
}

// TestAddNeighborRollsBackEncryptionRef checks that addNeighbor does not leave
// a tunnel reference behind when a later step fails.
func TestAddNeighborRollsBackEncryptionRef(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	d := newEncTestDriver()
	n := newEncTestNetwork(t, d, "nid", true)
	withSandbox(t, n, "addnbrollback")

	vtep := netip.MustParseAddr("192.0.2.2")
	err := n.addNeighbor(netip.MustParsePrefix("10.0.0.5/24"), mustMAC("02:42:0a:00:00:05"), vtep)

	// The tunnel reference is taken before the neighbor entry is programmed,
	// and the VXLAN link the entry needs does not exist.
	assert.Check(t, is.ErrorContains(err, "could not add neighbor entry into the sandbox"))
	assert.Check(t, is.Len(d.secMap, 0), "the tunnel reference should have been rolled back")
	assert.Check(t, is.Len(n.fdbCnt, 0))
}

// TestDeleteNeighborUnprogrammedPeer checks that removing a peer entry whose
// addNeighbor never succeeded does not release a tunnel reference it never
// took. Releasing one would tear the tunnel down under an endpoint on the same
// peer node which is still using it, putting its traffic on the wire in the
// clear.
func TestDeleteNeighborUnprogrammedPeer(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	d := newEncTestDriver()
	n := newEncTestNetwork(t, d, "nid", true)
	withSandbox(t, n, "delnbunprog")

	// A sibling endpoint on the same peer node was programmed successfully.
	vtep := netip.MustParseAddr("192.0.2.2")
	addPeer(t, n, vtep, mustMAC("02:42:0a:00:00:05"))
	assert.Check(t, is.Equal(d.secMap[vtep].count, 1))

	// This peer holds no references: its addNeighbor never got that far.
	err := n.deleteNeighbor(netip.MustParsePrefix("10.0.0.6/24"), mustMAC("02:42:0a:00:00:06"), vtep)

	// The reference check must run before any kernel state is touched, so the
	// failure has to come from it and not from the neighbor delete below it.
	assert.Check(t, is.ErrorContains(err, "fdb entry was not programmed into sandbox"))

	var nserr osl.NeighborSearchError
	assert.Check(t, errors.As(err, &nserr), "peerDelete keys its transient-case handling off this error type")
	assert.Check(t, !nserr.Present)
	assert.Check(t, is.Equal(d.secMap[vtep].count, 1), "the sibling's tunnel must not be torn down")
	assert.Check(t, is.Len(n.fdbCnt, 1))
}

func TestReleaseEncryptionRefs(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	d := newEncTestDriver()
	n := newEncTestNetwork(t, d, "nid", true)

	vtepA := netip.MustParseAddr("192.0.2.2")
	vtepB := netip.MustParseAddr("192.0.2.3")

	// Two endpoints hosted on peer A, one on peer B.
	addPeer(t, n, vtepA, mustMAC("02:42:c0:00:02:0a"))
	addPeer(t, n, vtepA, mustMAC("02:42:c0:00:02:0b"))
	addPeer(t, n, vtepB, mustMAC("02:42:c0:00:02:0c"))

	assert.Check(t, is.Equal(d.secMap[vtepA].count, 2))
	assert.Check(t, is.Equal(d.secMap[vtepB].count, 1))
	assert.Check(t, is.Len(n.fdbCnt, 3))

	n.releaseEncryptionRefs()

	assert.Check(t, is.Len(d.secMap, 0), "every tunnel reference should have been released")
	assert.Check(t, is.Len(n.fdbCnt, 0))

	// A second call must not release references the network no longer holds.
	n.releaseEncryptionRefs()
	assert.Check(t, is.Len(d.secMap, 0))
}

// TestReleaseEncryptionRefsScopedToNetwork checks that a network releases only
// the references it took, not every reference outstanding for the peer. secMap
// is scoped to the driver, so peers shared with another overlay network must
// keep their tunnel.
func TestReleaseEncryptionRefsScopedToNetwork(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	d := newEncTestDriver()
	n1 := newEncTestNetwork(t, d, "nid1", true)
	n2 := newEncTestNetwork(t, d, "nid2", true)

	vtep := netip.MustParseAddr("192.0.2.2")
	addPeer(t, n1, vtep, mustMAC("02:42:c0:00:02:0a"))
	addPeer(t, n2, vtep, mustMAC("02:42:c0:00:02:0b"))
	assert.Check(t, is.Equal(d.secMap[vtep].count, 2))

	n1.releaseEncryptionRefs()

	assert.Check(t, is.Equal(d.secMap[vtep].count, 1), "the other network's reference should survive")
	assert.Check(t, is.Len(n1.fdbCnt, 0))
	assert.Check(t, is.Len(n2.fdbCnt, 1))
}

// TestReleaseEncryptionRefsUnencryptedNetwork checks that an unencrypted
// network releases nothing: it never took a reference, and secMap is shared
// with the encrypted networks on the same driver.
func TestReleaseEncryptionRefsUnencryptedNetwork(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	d := newEncTestDriver()
	secure := newEncTestNetwork(t, d, "secure", true)
	plain := newEncTestNetwork(t, d, "plain", false)

	// Both networks have an endpoint on the same peer node.
	vtep := netip.MustParseAddr("192.0.2.2")
	addPeer(t, secure, vtep, mustMAC("02:42:c0:00:02:0a"))
	addPeer(t, plain, vtep, mustMAC("02:42:c0:00:02:0b"))
	assert.Check(t, is.Equal(d.secMap[vtep].count, 1))

	plain.releaseEncryptionRefs()

	assert.Check(t, is.Equal(d.secMap[vtep].count, 1), "an unencrypted network must not release tunnel references")
	assert.Check(t, is.Len(plain.fdbCnt, 0))
}

// TestDestroySandboxReleasesEncryptionRefs checks that the references are
// released even when there is no sandbox to tear down, so a network which
// failed to create one cannot hold them forever.
func TestDestroySandboxReleasesEncryptionRefs(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	d := newEncTestDriver()
	n := newEncTestNetwork(t, d, "nid", true)

	vtep := netip.MustParseAddr("192.0.2.2")
	addPeer(t, n, vtep, mustMAC("02:42:c0:00:02:0a"))
	assert.Check(t, is.Equal(d.secMap[vtep].count, 1))

	assert.Check(t, is.Nil(n.sbox))
	n.destroySandbox()

	assert.Check(t, is.Len(d.secMap, 0))
	assert.Check(t, is.Len(n.fdbCnt, 0))
}

// TestDeleteNetworkReleasesEncryptionRefs checks the belt-and-braces release in
// DeleteNetwork: the network object becomes unreachable once it is dropped from
// d.networks, so anything it still holds has to go with it.
func TestDeleteNetworkReleasesEncryptionRefs(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	d := newEncTestDriver()
	n := newEncTestNetwork(t, d, "nid", true)

	vtep := netip.MustParseAddr("192.0.2.2")
	addPeer(t, n, vtep, mustMAC("02:42:c0:00:02:0a"))
	assert.Check(t, is.Equal(d.secMap[vtep].count, 1))

	assert.NilError(t, d.DeleteNetwork("nid"))

	assert.Check(t, is.Len(d.secMap, 0))
	assert.Check(t, is.Len(d.networks, 0))
}

func TestRemoveStaleSandboxes(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	netnsDir := t.TempDir()
	d := &driver{
		sandboxDir:       filepath.Join(netnsDir, "overlay"),
		legacySandboxDir: netnsDir,
	}
	stale := filepath.Join(d.sandboxDir, "0-0123456789abcdef")
	legacy := filepath.Join(netnsDir, "1-0123456789")
	other := filepath.Join(netnsDir, "0123456789ab")
	assert.NilError(t, os.Mkdir(d.sandboxDir, 0o755))
	for _, key := range []string{stale, legacy, other} {
		_, err := osl.NewSandbox(key, true, false)
		assert.NilError(t, err)
	}
	t.Cleanup(func() {
		_ = unix.Unmount(other, unix.MNT_DETACH)
		_ = os.Remove(other)
	})

	// VXLAN links left in a stale sandbox keep their VNIs claimed. A
	// network sandbox holds one link for each subnet of the network.
	vnis := []uint32{4242, 4243}
	legacyNS, err := netns.GetFromPath(legacy)
	assert.NilError(t, err)
	for _, vni := range vnis {
		name := fmt.Sprintf("vx-stale-%d", vni)
		assert.NilError(t, createVxlan(name, vni, 1450, false))
		link, err := ns.NlHandle().LinkByName(name)
		assert.NilError(t, err)
		assert.NilError(t, ns.NlHandle().LinkSetNsFd(link, int(legacyNS)))
	}
	_ = legacyNS.Close()

	d.removeStaleSandboxes()

	for _, key := range []string{stale, legacy} {
		_, err := os.Lstat(key)
		assert.Check(t, is.ErrorIs(err, os.ErrNotExist), "stale sandbox %s was not removed", key)
	}
	_, err = os.Lstat(other)
	assert.Check(t, err, "a network namespace the driver did not create must not be removed")

	// The VNIs can be reused straight away.
	for _, vni := range vnis {
		name := fmt.Sprintf("vx-new-%d", vni)
		assert.Check(t, createVxlan(name, vni, 1450, false), "VNI %d is still claimed", vni)
		_ = deleteInterface(name)
	}
}

func TestInitSandboxWithoutNetnsDir(t *testing.T) {
	dt := &driverTester{t: t}
	assert.NilError(t, Register(dt, ""))
	n := &network{id: "0123456789abcdef", driver: dt.d}

	assert.Check(t, is.ErrorContains(n.initSandbox(), "no netns directory is configured"))
	assert.Check(t, is.Equal(dt.d.sandboxDir, ""))
	dt.d.removeStaleSandboxes()
}
