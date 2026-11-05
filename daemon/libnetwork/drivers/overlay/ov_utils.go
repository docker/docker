//go:build linux

package overlay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/overlay/overlayutils"
	"github.com/moby/moby/v2/daemon/libnetwork/netutils"
	"github.com/moby/moby/v2/daemon/libnetwork/nlwrap"
	"github.com/moby/moby/v2/daemon/libnetwork/ns"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

var soTimeout = ns.NetlinkSocketsTimeout

func validateID(nid, eid string) error {
	if nid == "" {
		return errors.New("invalid network id")
	}

	if eid == "" {
		return errors.New("invalid endpoint id")
	}

	return nil
}

func createVethPair() (string, string, error) {
	nlh := ns.NlHandle()

	// Generate a name for what will be the host side pipe interface
	name1, err := netutils.GenerateIfaceName(nlh, vethPrefix, vethLen)
	if err != nil {
		return "", "", fmt.Errorf("error generating veth name1: %v", err)
	}

	// Generate a name for what will be the sandbox side pipe interface
	name2, err := netutils.GenerateIfaceName(nlh, vethPrefix, vethLen)
	if err != nil {
		return "", "", fmt.Errorf("error generating veth name2: %v", err)
	}

	// Generate and add the interface pipe host <-> sandbox
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: name1, TxQLen: 0},
		PeerName:  name2,
	}
	if err := nlh.LinkAdd(veth); err != nil {
		return "", "", fmt.Errorf("error creating veth pair: %v", err)
	}

	return name1, name2, nil
}

func createVxlan(name string, vni uint32, mtu int, vtepIPv6 bool) error {
	vxlan := &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{Name: name, MTU: mtu},
		VxlanId:   int(vni),
		Learning:  true,
		Port:      int(overlayutils.VXLANUDPPort()),
		Proxy:     true,
		L3miss:    true,
		L2miss:    true,
	}

	// The kernel restricts the destination VTEP (virtual tunnel endpoint) in
	// VXLAN forwarding database entries to a single address family, defaulting
	// to IPv4 unless either an IPv6 group or default remote destination address
	// is configured when the VXLAN link is created.
	//
	// Set up the VXLAN link for IPv6 destination addresses by setting the VXLAN
	// group address to the IPv6 unspecified address, like iproute2.
	// https://github.com/iproute2/iproute2/commit/97d564b90ccb1e4a3c756d9caae161f55b2b63a2
	// https://patchwork.ozlabs.org/project/netdev/patch/20180917171325.GA2660@localhost.localdomain/
	if vtepIPv6 {
		vxlan.Group = net.IPv6unspecified
	}

	if err := ns.NlHandle().LinkAdd(vxlan); err != nil {
		return fmt.Errorf("error creating vxlan interface: %v", err)
	}

	return nil
}

func deleteInterface(name string) error {
	link, err := ns.NlHandle().LinkByName(name)
	if err != nil {
		return fmt.Errorf("failed to find interface with name %s: %v", name, err)
	}

	if err := ns.NlHandle().LinkDel(link); err != nil {
		return fmt.Errorf("error deleting interface with name %s: %v", name, err)
	}

	return nil
}

// deleteVxlans deletes every VXLAN link in the network namespace at path.
func deleteVxlans(path string) error {
	ns, err := netns.GetFromPath(path)
	if err != nil {
		return fmt.Errorf("failed to get ns handle for %s: %v", path, err)
	}
	defer ns.Close()

	nlh, err := nlwrap.NewHandleAt(ns, syscall.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("failed to get netlink handle for ns %s: %v", path, err)
	}
	defer nlh.Close()
	if err := nlh.SetSocketTimeout(soTimeout); err != nil {
		log.G(context.TODO()).Warnf("Failed to set the timeout on the netlink handle sockets for vxlan deletion: %v", err)
	}

	links, err := nlh.LinkList()
	if err != nil {
		return fmt.Errorf("failed to list interfaces while deleting vxlan interfaces: %v", err)
	}

	var errs []error
	for _, l := range links {
		if l.Type() != "vxlan" {
			continue
		}
		if err := nlh.LinkDel(l); err != nil {
			errs = append(errs, fmt.Errorf("error deleting vxlan interface %s: %v", l.Attrs().Name, err))
		}
	}
	return errors.Join(errs...)
}
