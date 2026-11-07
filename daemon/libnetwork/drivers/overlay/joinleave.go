//go:build linux

package overlay

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"syscall"

	"github.com/containerd/log"
	"github.com/gogo/protobuf/proto"
	"github.com/moby/moby/v2/daemon/internal/netiputil"
	"github.com/moby/moby/v2/daemon/libnetwork/driverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/netlabel"
	"github.com/moby/moby/v2/daemon/libnetwork/ns"
	"github.com/moby/moby/v2/daemon/libnetwork/osl"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Join method is invoked when a Sandbox is attached to an endpoint.
func (d *driver) Join(ctx context.Context, nid, eid string, sboxKey string, jinfo driverapi.JoinInfo, epOpts, _ map[string]any) error {
	ctx, span := otel.Tracer("").Start(ctx, "libnetwork.drivers.overlay.Join", trace.WithAttributes(
		attribute.String("nid", nid),
		attribute.String("eid", eid),
		attribute.String("sboxKey", sboxKey)))
	defer span.End()

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
		return fmt.Errorf("could not find endpoint with id %s", eid)
	}

	if n.secure {
		d.encrMu.Lock()
		nkeys := len(d.keys)
		d.encrMu.Unlock()
		if nkeys == 0 {
			return errors.New("cannot join secure network: encryption keys not present")
		}
	}

	nlh := ns.NlHandle()

	if n.secure && !nlh.SupportsNetlinkFamily(syscall.NETLINK_XFRM) {
		return errors.New("cannot join secure network: required modules to install IPSEC rules are missing on host")
	}

	s := n.getSubnetforIP(ep.addr)
	if s == nil {
		return fmt.Errorf("could not find subnet for endpoint %s", eid)
	}

	if err := n.joinSandbox(s, true); err != nil {
		return fmt.Errorf("network sandbox join failed: %v", err)
	}
	ep.joined = true

	overlayIfName, containerIfName, err := createVethPair()
	if err != nil {
		return err
	}

	ep.ifName = containerIfName

	// Set the container interface and its peer MTU to allow for the
	// VXLAN encapsulation overhead.
	mtu := n.maxMTU()

	veth, err := nlh.LinkByName(overlayIfName)
	if err != nil {
		return fmt.Errorf("could not find link by name %s: %v", overlayIfName, err)
	}
	err = nlh.LinkSetMTU(veth, mtu)
	if err != nil {
		return err
	}

	if err = n.sbox.AddInterface(ctx, overlayIfName, "veth", "", osl.WithMaster(s.brName)); err != nil {
		return fmt.Errorf("could not add veth pair inside the network sandbox: %v", err)
	}

	veth, err = nlh.LinkByName(containerIfName)
	if err != nil {
		return fmt.Errorf("could not find link by name %s: %v", containerIfName, err)
	}
	err = nlh.LinkSetMTU(veth, mtu)
	if err != nil {
		return err
	}

	if err = nlh.LinkSetHardwareAddr(veth, ep.mac.AsSlice()); err != nil {
		return fmt.Errorf("could not set mac address (%v) to the container interface: %v", ep.mac, err)
	}

	for _, sub := range n.subnets {
		if sub == s {
			continue
		}
		if err = jinfo.AddStaticRoute(netiputil.ToIPNet(sub.subnetIP), types.NEXTHOP, s.gwIP.Addr().AsSlice()); err != nil {
			log.G(ctx).Errorf("Adding subnet %s static route in network %q failed\n", s.subnetIP, n.id)
		}
	}

	if iNames := jinfo.InterfaceName(); iNames != nil {
		err = iNames.SetNames(containerIfName, "eth", netlabel.GetIfname(epOpts))
		if err != nil {
			return err
		}
	}

	if err := n.peerAdd(eid, ep.addr, ep.mac, netip.Addr{}); err != nil {
		return fmt.Errorf("overlay: failed to add local endpoint to network peer db: %w", err)
	}

	buf, err := proto.Marshal(&PeerRecord{
		EndpointIP:       ep.addr.String(),
		EndpointMAC:      ep.mac.String(),
		TunnelEndpointIP: d.advertiseAddress.String(),
	})
	if err != nil {
		return err
	}

	if err := jinfo.AddTableEntry(OverlayPeerTable, eid, buf); err != nil {
		log.G(ctx).Errorf("overlay: Failed adding table entry to joininfo: %v", err)
	}

	return nil
}

func (d *driver) DecodeTableEntry(tablename string, key string, value []byte) (string, map[string]string) {
	if tablename != OverlayPeerTable {
		log.G(context.TODO()).Errorf("DecodeTableEntry: unexpected table name %s", tablename)
		return "", nil
	}

	var peer PeerRecord
	if err := proto.Unmarshal(value, &peer); err != nil {
		log.G(context.TODO()).Errorf("DecodeTableEntry: failed to unmarshal peer record for key %s: %v", key, err)
		return "", nil
	}

	return key, map[string]string{
		"Host IP": peer.TunnelEndpointIP,
	}
}

func (d *driver) EventNotify(nid, tableName, key string, prev, value []byte) {
	if tableName != OverlayPeerTable {
		log.G(context.TODO()).Errorf("Unexpected table notification for table %s received", tableName)
		return
	}

	eid := key

	var prevPeer, newPeer *Peer
	if prev != nil {
		var err error
		prevPeer, err = UnmarshalPeerRecord(prev)
		if err != nil {
			log.G(context.TODO()).WithError(err).Error("Failed to unmarshal previous peer record")
		} else if prevPeer.TunnelEndpointIP == d.advertiseAddress {
			// Ignore local peers. We don't add them to the VXLAN
			// FDB so don't need to remove them.
			prevPeer = nil
		}
	}
	if value != nil {
		var err error
		newPeer, err = UnmarshalPeerRecord(value)
		if err != nil {
			log.G(context.TODO()).WithError(err).Error("Failed to unmarshal peer record")
		} else if newPeer.TunnelEndpointIP == d.advertiseAddress {
			newPeer = nil
		}
	}

	if prevPeer == nil && newPeer == nil {
		// Nothing to do! Either the event was for a local peer,
		// or unmarshaling failed.
		return
	}
	if prevPeer != nil && newPeer != nil && *prevPeer == *newPeer {
		// The update did not materially change the FDB entry.
		return
	}

	n, unlock, err := d.lockNetwork(nid)
	if err != nil {
		log.G(context.TODO()).WithFields(log.Fields{
			"error": err,
			"nid":   nid,
		}).Error("overlay: handling peer event")
		return
	}
	defer unlock()

	if prevPeer != nil {
		if err := n.peerDelete(eid, prevPeer.EndpointIP, prevPeer.EndpointMAC, prevPeer.TunnelEndpointIP); err != nil {
			log.G(context.TODO()).WithFields(log.Fields{
				"error": err,
				"nid":   n.id,
				"peer":  prevPeer,
			}).Warn("overlay: failed to delete peer entry")
		}
	}
	if newPeer != nil {
		if err := n.peerAdd(eid, newPeer.EndpointIP, newPeer.EndpointMAC, newPeer.TunnelEndpointIP); err != nil {
			log.G(context.TODO()).WithFields(log.Fields{
				"error": err,
				"nid":   n.id,
				"peer":  newPeer,
			}).Warn("overlay: failed to add peer entry")
		}
	}
}

// Leave method is invoked when a Sandbox detaches from an endpoint.
func (d *driver) Leave(nid, eid string) error {
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
		return types.InternalMaskableErrorf("could not find endpoint with id %s", eid)
	}

	// libnetwork also calls Leave to roll back a failed join: once if Join
	// failed, and twice if a later step of the join failed.
	if !ep.joined {
		return nil
	}
	ep.joined = false

	err = n.peerDelete(eid, ep.addr, ep.mac, netip.Addr{})
	n.leaveSandbox()
	if err != nil {
		return fmt.Errorf("overlay: failed to delete local endpoint eid:%s from network peer db: %w", eid, err)
	}
	return nil
}
