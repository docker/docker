package osl

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/internal/nftables"
	"github.com/moby/moby/v2/daemon/libnetwork/nlwrap"
	"github.com/moby/moby/v2/daemon/libnetwork/ns"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/moby/moby/v2/internal/testutil/netnsutils"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
)

const (
	vethName1       = "wierdlongname1"
	vethName2       = "wierdlongname2"
	vethName3       = "wierdlongname3"
	vethName4       = "wierdlongname4"
	sboxIfacePrefix = "containername"
)

func generateRandomName(prefix string, size int) (string, error) {
	id := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, id); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(id)[:size], nil
}

func newKey(t *testing.T) (string, error) {
	t.Helper()
	name, err := generateRandomName("netns", 12)
	if err != nil {
		return "", err
	}

	name = filepath.Join("/tmp", name)
	f, err := os.Create(name)
	if err != nil {
		return "", err
	}
	_ = f.Close()

	return name, nil
}

func newInfo(t *testing.T, hnd nlwrap.Handle) (*Namespace, error) {
	t.Helper()
	err := hnd.LinkAdd(&netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: vethName1, TxQLen: 0},
		PeerName:  vethName2,
	})
	if err != nil {
		return nil, err
	}

	ip4, addr, err := net.ParseCIDR("192.168.1.100/24")
	if err != nil {
		return nil, err
	}
	addr.IP = ip4

	ip6, addrv6, err := net.ParseCIDR("fdac:97b4:dbcc::2/64")
	if err != nil {
		return nil, err
	}
	addrv6.IP = ip6

	_, route, err := net.ParseCIDR("192.168.2.1/32")
	if err != nil {
		return nil, err
	}

	// Store the sandbox side pipe interface
	// This is needed for cleanup on DeleteEndpoint()
	intf1 := &Interface{
		srcName:     vethName2,
		dstPrefix:   sboxIfacePrefix,
		address:     addr,
		addressIPv6: addrv6,
		routes:      []*net.IPNet{route},
	}

	intf2 := &Interface{
		srcName:   "testbridge",
		dstPrefix: sboxIfacePrefix,
		bridge:    true,
	}

	err = hnd.LinkAdd(&netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: vethName3, TxQLen: 0},
		PeerName:  vethName4,
	})
	if err != nil {
		return nil, err
	}

	intf3 := &Interface{
		srcName:   vethName4,
		dstPrefix: sboxIfacePrefix,
		master:    "testbridge",
	}

	return &Namespace{
		iFaces: []*Interface{intf1, intf2, intf3},
		gw:     net.ParseIP("192.168.1.1"),
		gwv6:   net.ParseIP("fdac:97b4:dbcc::1/64"),
	}, nil
}

func verifySandbox(t *testing.T, ns *Namespace, ifaceSuffixes []string) {
	sbNs, err := netns.GetFromPath(ns.Key())
	if err != nil {
		t.Fatalf("Failed top open network namespace path %q: %v", ns.Key(), err)
	}
	defer sbNs.Close()

	nh, err := nlwrap.NewHandleAt(sbNs)
	if err != nil {
		t.Fatal(err)
	}
	defer nh.Close()

	for _, suffix := range ifaceSuffixes {
		_, err = nh.LinkByName(sboxIfacePrefix + suffix)
		if err != nil {
			t.Fatalf("Could not find the interface %s inside the sandbox: %v",
				sboxIfacePrefix+suffix, err)
		}
	}
}

func verifyCleanup(t *testing.T, ns *Namespace) {
	if _, err := os.Stat(ns.Key()); err == nil {
		t.Fatalf("The sandbox path %s is not cleaned up after running gc", ns.Key())
	}
}

func TestDisableIPv6DAD(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	key, err := newKey(t)
	if err != nil {
		t.Fatalf("Failed to obtain a key: %v", err)
	}

	n, err := NewSandbox(key, true, false)
	if err != nil {
		t.Fatalf("Failed to create a new sandbox: %v", err)
	}
	defer destroyTest(t, n)

	nlh := n.nlHandle

	ipv6, _ := types.ParseCIDR("2001:db8::44/64")
	iface := &Interface{addressIPv6: ipv6, ns: n, dstName: "sideA"}

	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: "sideA"},
		PeerName:  "sideB",
	}

	err = nlh.LinkAdd(veth)
	if err != nil {
		t.Fatal(err)
	}

	link, err := nlh.LinkByName("sideA")
	if err != nil {
		t.Fatal(err)
	}

	err = setInterfaceIPv6(t.Context(), nlh, link, iface)
	if err != nil {
		t.Fatal(err)
	}

	addrList, err := nlh.AddrList(link, nl.FAMILY_V6)
	if err != nil {
		t.Fatal(err)
	}

	if addrList[0].Flags&syscall.IFA_F_NODAD == 0 {
		t.Fatalf("Unexpected interface flags: 0x%x. Expected to contain 0x%x", addrList[0].Flags, syscall.IFA_F_NODAD)
	}
}

// TestApplyOSTweaksNFTablesForwarding checks that, with nftables, an ingress or
// load-balancer sandbox gets IPv4 forwarding, which the nftables load balancer
// depends on. A new namespace copies ip_forward from the initial namespace, so
// it's switched off first rather than relying on the host's value.
func TestApplyOSTweaksNFTablesForwarding(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()
	if err := nftables.Enable(); err != nil {
		t.Skipf("nftables not available: %v", err)
	}
	defer nftables.Disable()

	const ipForward = "/proc/sys/net/ipv4/ip_forward"
	for _, typ := range []SandboxType{SandboxTypeIngress, SandboxTypeLoadBalancer} {
		key, err := newKey(t)
		if err != nil {
			t.Fatalf("Failed to obtain a key: %v", err)
		}
		n, err := NewSandbox(key, true, false)
		if err != nil {
			t.Fatalf("Failed to create a new sandbox: %v", err)
		}
		defer destroyTest(t, n)

		var got []byte
		var ferr error
		if err := n.InvokeFunc(func() {
			if ferr = os.WriteFile(ipForward, []byte("0"), 0o644); ferr != nil {
				return
			}
			n.ApplyOSTweaks([]SandboxType{typ})
			got, ferr = os.ReadFile(ipForward)
		}); err != nil {
			t.Fatal(err)
		}
		if ferr != nil {
			t.Fatal(ferr)
		}
		if v := strings.TrimSpace(string(got)); v != "1" {
			t.Errorf("sandbox type %d: ip_forward = %q, want \"1\"", typ, v)
		}
	}
}

func destroyTest(t *testing.T, ns *Namespace) {
	if err := ns.Destroy(); err != nil {
		t.Log(err)
	}
}

func TestSetInterfaceIP(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	key, err := newKey(t)
	if err != nil {
		t.Fatalf("Failed to obtain a key: %v", err)
	}

	n, err := NewSandbox(key, true, false)
	if err != nil {
		t.Fatalf("Failed to create a new sandbox: %v", err)
	}
	defer destroyTest(t, n)

	nlh := n.nlHandle

	ipv4, _ := types.ParseCIDR("172.30.0.33/24")
	ipv6, _ := types.ParseCIDR("2001:db8::44/64")
	iface := &Interface{address: ipv4, addressIPv6: ipv6, ns: n, dstName: "sideA"}

	if err := nlh.LinkAdd(&netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: "sideA"},
		PeerName:  "sideB",
	}); err != nil {
		t.Fatal(err)
	}

	linkA, err := nlh.LinkByName("sideA")
	if err != nil {
		t.Fatal(err)
	}

	linkB, err := nlh.LinkByName("sideB")
	if err != nil {
		t.Fatal(err)
	}

	if err := nlh.LinkSetUp(linkA); err != nil {
		t.Fatal(err)
	}

	if err := nlh.LinkSetUp(linkB); err != nil {
		t.Fatal(err)
	}

	if err := setInterfaceIP(t.Context(), nlh, linkA, iface); err != nil {
		t.Fatal(err)
	}

	if err := setInterfaceIPv6(t.Context(), nlh, linkA, iface); err != nil {
		t.Fatal(err)
	}

	err = setInterfaceIP(t.Context(), nlh, linkB, iface)
	if err == nil {
		t.Fatalf("Expected route conflict error, but succeeded")
	}
	if !strings.Contains(err.Error(), "conflicts with existing route") {
		t.Fatalf("Unexpected error: %v", err)
	}

	err = setInterfaceIPv6(t.Context(), nlh, linkB, iface)
	if err == nil {
		t.Fatalf("Expected route conflict error, but succeeded")
	}
	if !strings.Contains(err.Error(), "conflicts with existing route") {
		t.Fatalf("Unexpected error: %v", err)
	}
}

func TestLiveRestore(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	key, err := newKey(t)
	if err != nil {
		t.Fatalf("Failed to obtain a key: %v", err)
	}

	n, err := NewSandbox(key, true, false)
	if err != nil {
		t.Fatalf("Failed to create a new sandbox: %v", err)
	}
	defer destroyTest(t, n)

	nlh := n.nlHandle

	ipv4, _ := types.ParseCIDR("172.30.0.33/24")
	ipv6, _ := types.ParseCIDR("2001:db8::44/64")
	iface := &Interface{address: ipv4, addressIPv6: ipv6, ns: n, dstName: "sideA"}

	if err := nlh.LinkAdd(&netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: "sideA"},
		PeerName:  "sideB",
	}); err != nil {
		t.Fatal(err)
	}

	linkA, err := nlh.LinkByName("sideA")
	if err != nil {
		t.Fatal(err)
	}

	linkB, err := nlh.LinkByName("sideB")
	if err != nil {
		t.Fatal(err)
	}

	if err := nlh.LinkSetUp(linkA); err != nil {
		t.Fatal(err)
	}

	if err := nlh.LinkSetUp(linkB); err != nil {
		t.Fatal(err)
	}

	if err := setInterfaceIP(t.Context(), nlh, linkA, iface); err != nil {
		t.Fatal(err)
	}

	if err := setInterfaceIPv6(t.Context(), nlh, linkA, iface); err != nil {
		t.Fatal(err)
	}

	err = setInterfaceIP(t.Context(), nlh, linkB, iface)
	if err == nil {
		t.Fatalf("Expected route conflict error, but succeeded")
	}
	if !strings.Contains(err.Error(), "conflicts with existing route") {
		t.Fatalf("Unexpected error: %v", err)
	}

	err = setInterfaceIPv6(t.Context(), nlh, linkB, iface)
	if err == nil {
		t.Fatalf("Expected route conflict error, but succeeded")
	}
	if !strings.Contains(err.Error(), "conflicts with existing route") {
		t.Fatalf("Unexpected error: %v", err)
	}

	// Create newsandbox with Restore - TRUE
	n2, err := NewSandbox(key, true, true)
	if err != nil {
		t.Fatalf("Failed to create a new sandbox: %v", err)
	}
	defer destroyTest(t, n2)

	// Check if the IPV4 & IPV6 entry present
	// If present , we should get error in below call
	// It shows us , we don't delete any config in live-restore case
	if err := setInterfaceIPv6(t.Context(), nlh, linkA, iface); err == nil {
		t.Fatalf("Expected route conflict error, but succeeded for IPV6 ")
	}
	if err := setInterfaceIP(t.Context(), nlh, linkA, iface); err == nil {
		t.Fatalf("Expected route conflict error, but succeeded for IPV4 ")
	}
}

func TestSandboxCreate(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	key, err := newKey(t)
	if err != nil {
		t.Fatalf("Failed to obtain a key: %v", err)
	}

	s, err := NewSandbox(key, true, false)
	if err != nil {
		t.Fatalf("Failed to create a new sandbox: %v", err)
	}

	if s.Key() != key {
		t.Fatalf("s.Key() returned %s. Expected %s", s.Key(), key)
	}

	tbox, err := newInfo(t, ns.NlHandle())
	if err != nil {
		t.Fatalf("Failed to generate new sandbox info: %v", err)
	}

	for _, i := range tbox.Interfaces() {
		err = s.AddInterface(t.Context(), i.SrcName(), i.dstPrefix, i.DstName(),
			WithIsBridge(i.Bridge()),
			WithIPv4Address(i.Address()),
			WithIPv6Address(i.AddressIPv6()),
			WithAdvertiseAddrNMsgs(0), // Made-up IP addresses, can't sent ARP/NA messages.
		)
		if err != nil {
			t.Fatalf("Failed to add interfaces to sandbox: %v", err)
		}
	}

	err = s.SetGateway(tbox.Gateway())
	if err != nil {
		t.Fatalf("Failed to set gateway to sandbox: %v", err)
	}

	err = s.SetGatewayIPv6(tbox.GatewayIPv6())
	if err != nil {
		t.Fatalf("Failed to set ipv6 gateway to sandbox: %v", err)
	}

	verifySandbox(t, s, []string{"0", "1", "2"})

	err = s.Destroy()
	if err != nil {
		t.Fatal(err)
	}
	verifyCleanup(t, s)
}

func TestSandboxCreateTwice(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	key, err := newKey(t)
	if err != nil {
		t.Fatalf("Failed to obtain a key: %v", err)
	}

	_, err = NewSandbox(key, true, false)
	if err != nil {
		t.Fatalf("Failed to create a new sandbox: %v", err)
	}

	// Create another sandbox with the same key to see if we handle it
	// gracefully.
	s, err := NewSandbox(key, true, false)
	if err != nil {
		t.Fatalf("Failed to create a new sandbox: %v", err)
	}

	err = s.Destroy()
	if err != nil {
		t.Fatal(err)
	}
	verifyCleanup(t, s)
}

func TestSandboxGC(t *testing.T) {
	key, err := newKey(t)
	if err != nil {
		t.Fatalf("Failed to obtain a key: %v", err)
	}

	s, err := NewSandbox(key, true, false)
	if err != nil {
		t.Fatalf("Failed to create a new sandbox: %v", err)
	}

	err = s.Destroy()
	if err != nil {
		t.Fatal(err)
	}

	verifyCleanup(t, s)
}

func TestAddRemoveInterface(t *testing.T) {
	defer netnsutils.SetupTestOSContext(t)()

	key, err := newKey(t)
	if err != nil {
		t.Fatalf("Failed to obtain a key: %v", err)
	}

	s, err := NewSandbox(key, true, false)
	if err != nil {
		t.Fatalf("Failed to create a new sandbox: %v", err)
	}

	if s.Key() != key {
		t.Fatalf("s.Key() returned %s. Expected %s", s.Key(), key)
	}

	tbox, err := newInfo(t, ns.NlHandle())
	if err != nil {
		t.Fatalf("Failed to generate new sandbox info: %v", err)
	}

	for _, i := range tbox.Interfaces() {
		err = s.AddInterface(t.Context(), i.SrcName(), i.dstPrefix, i.DstName(),
			WithIsBridge(i.Bridge()),
			WithIPv4Address(i.Address()),
			WithIPv6Address(i.AddressIPv6()),
			WithAdvertiseAddrNMsgs(0), // Made-up IP addresses, can't sent ARP/NA messages.
		)
		if err != nil {
			t.Fatalf("Failed to add interfaces to sandbox: %v", err)
		}
	}

	verifySandbox(t, s, []string{"0", "1", "2"})

	interfaces := s.Interfaces()
	if err := interfaces[0].Remove(); err != nil {
		t.Fatalf("Failed to remove interfaces from sandbox: %v", err)
	}

	verifySandbox(t, s, []string{"1", "2"})

	i := tbox.Interfaces()[0]
	err = s.AddInterface(t.Context(), i.SrcName(), i.dstPrefix, i.DstName(),
		WithIsBridge(i.Bridge()),
		WithIPv4Address(i.Address()),
		WithIPv6Address(i.AddressIPv6()),
		WithAdvertiseAddrNMsgs(0), // Made-up IP addresses, can't sent ARP/NA messages.
	)
	if err != nil {
		t.Fatalf("Failed to add interfaces to sandbox: %v", err)
	}

	verifySandbox(t, s, []string{"0", "1", "2"})

	err = s.Destroy()
	if err != nil {
		t.Fatal(err)
	}

	verifyCleanup(t, s)
}
