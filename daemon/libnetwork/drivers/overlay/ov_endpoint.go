//go:build linux

package overlay

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/internal/netiputil"
	"github.com/moby/moby/v2/daemon/libnetwork/driverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/hashable"
	"github.com/moby/moby/v2/daemon/libnetwork/netutils"
	"github.com/moby/moby/v2/daemon/libnetwork/ns"
)

type endpointTable map[string]*endpoint

type endpoint struct {
	id     string
	nid    string
	ifName string
	mac    hashable.MACAddr
	addr   netip.Prefix
	joined bool
}

func (d *driver) CreateEndpoint(_ context.Context, nid, eid string, ifInfo driverapi.InterfaceInfo, epOptions map[string]any) error {
	var err error
	if err = validateID(nid, eid); err != nil {
		return err
	}

	// Since we perform lazy configuration make sure we try
	// configuring the driver when we enter CreateEndpoint since
	// CreateNetwork may not be called in every node.
	if err := d.configure(); err != nil {
		return err
	}

	n, unlock, err := d.lockNetwork(nid)
	if err != nil {
		return err
	}
	defer unlock()

	ep := &endpoint{
		id:  eid,
		nid: n.id,
	}
	var ok bool
	ep.addr, ok = netiputil.ToPrefix(ifInfo.Address())
	if !ok {
		return errors.New("create endpoint was not passed interface IP address")
	}

	if s := n.getSubnetforIP(ep.addr); s == nil {
		return fmt.Errorf("no matching subnet for IP %q in network %q", ep.addr, nid)
	}

	if ifmac := ifInfo.MacAddress(); ifmac != nil {
		var ok bool
		ep.mac, ok = hashable.MACAddrFromSlice(ifInfo.MacAddress())
		if !ok {
			return fmt.Errorf("invalid MAC address %q assigned to endpoint: unexpected length", ifmac)
		}
	} else {
		var ok bool
		ep.mac, ok = hashable.MACAddrFromSlice(netutils.GenerateMACFromIP(ep.addr.Addr().AsSlice()))
		if !ok {
			panic("GenerateMACFromIP returned a HardwareAddress that is not a MAC-48")
		}
		if err := ifInfo.SetMacAddress(ep.mac.AsSlice()); err != nil {
			return err
		}
	}

	n.endpoints[ep.id] = ep

	return nil
}

func (d *driver) DeleteEndpoint(nid, eid string) error {
	nlh := ns.NlHandle()

	if err := validateID(nid, eid); err != nil {
		return err
	}

	n, unlock, err := d.lockNetwork(nid)
	if err != nil {
		return err
	}
	defer unlock()

	ep := n.endpoints[eid]
	if ep == nil {
		return fmt.Errorf("endpoint id %q not found", eid)
	}

	delete(n.endpoints, eid)

	if ep.ifName == "" {
		return nil
	}

	link, err := nlh.LinkByName(ep.ifName)
	if err != nil {
		log.G(context.TODO()).Debugf("Failed to retrieve interface (%s)'s link on endpoint (%s) delete: %v", ep.ifName, ep.id, err)
		return nil
	}
	if err := nlh.LinkDel(link); err != nil {
		log.G(context.TODO()).Debugf("Failed to delete interface (%s)'s link on endpoint (%s) delete: %v", ep.ifName, ep.id, err)
	}

	return nil
}

func (d *driver) EndpointOperInfo(nid, eid string) (map[string]any, error) {
	return make(map[string]any), nil
}
