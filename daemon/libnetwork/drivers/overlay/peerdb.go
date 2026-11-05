//go:build linux

package overlay

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"syscall"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/hashable"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/setmatrix"
	"github.com/moby/moby/v2/daemon/libnetwork/osl"
)

type peerEntry struct {
	eid  string
	mac  hashable.MACAddr
	vtep netip.Addr
}

func (p *peerEntry) isLocal() bool {
	return !p.vtep.IsValid()
}

type peerMap struct {
	mp setmatrix.SetMatrix[netip.Prefix, peerEntry]
}

func (pm *peerMap) Walk(f func(netip.Prefix, peerEntry)) {
	for _, peerAddr := range pm.mp.Keys() {
		entry, ok := pm.Get(peerAddr)
		if ok {
			f(peerAddr, entry)
		}
	}
}

func (pm *peerMap) Get(peerIP netip.Prefix) (peerEntry, bool) {
	c, ok := pm.mp.Get(peerIP)
	if !ok || len(c) == 0 {
		return peerEntry{}, false
	}
	return c[0], true
}

func (pm *peerMap) Add(eid string, peerIP netip.Prefix, peerMac hashable.MACAddr, vtep netip.Addr) (bool, int) {
	b, i := pm.mp.Insert(peerIP, peerEntry{
		eid:  eid,
		mac:  peerMac,
		vtep: vtep,
	})
	if i != 1 {
		// Transient case, there is more than one endpoint that is using the same IP
		s, _ := pm.mp.String(peerIP)
		log.G(context.TODO()).WithFields(log.Fields{
			"peerIP":      peerIP.String(),
			"cardinality": i,
			"db-state":    s,
		}).Warn("peerDbAdd transient condition: there is more than one endpoint using the same IP")

	}
	return b, i
}

func (pm *peerMap) Delete(eid string, peerIP netip.Prefix, peerMac hashable.MACAddr, vtep netip.Addr) (bool, int) {
	b, i := pm.mp.Remove(peerIP, peerEntry{
		eid:  eid,
		mac:  peerMac,
		vtep: vtep,
	})
	if i != 0 {
		// Transient case, there is more than one endpoint that is using the same IP
		s, _ := pm.mp.String(peerIP)
		log.G(context.TODO()).WithFields(log.Fields{
			"peerIP":      peerIP.String(),
			"cardinality": i,
			"db-state":    s,
		}).Warn("peerDbDelete transient condition: there is more than one endpoint using the same IP")
	}
	return b, i
}

// The overlay uses a lazy initialization approach, this means that when a network is created
// and the driver registered the overlay does not allocate resources till the moment that a
// sandbox is actually created.
// At the moment of this call, that happens when a sandbox is initialized, is possible that
// networkDB has already delivered some events of peers already available on remote nodes,
// these peers are saved into the peerDB and this function is used to properly configure
// the network sandbox with all those peers that got previously notified.
//
// The caller is responsible for ensuring that peerAdd and peerDelete are not
// called concurrently with this function to guarantee consistency.
func (n *network) initSandboxPeerDB() error {
	var errs []error
	n.peerdb.Walk(func(peerIP netip.Prefix, pEntry peerEntry) {
		if !pEntry.isLocal() {
			if err := n.addNeighbor(peerIP, pEntry.mac, pEntry.vtep); err != nil {
				errs = append(errs, fmt.Errorf("failed to add neighbor entries for %s: %w", peerIP, err))
			}
		}
	})
	return errors.Join(errs...)
}

// peerAdd adds a new entry to the peer database.
//
// Local peers are signified by an invalid vtep (i.e. netip.Addr{}).
func (n *network) peerAdd(eid string, peerIP netip.Prefix, peerMac hashable.MACAddr, vtep netip.Addr) error {
	if eid == "" {
		return errors.New("invalid endpoint id")
	}

	inserted, dbEntries := n.peerdb.Add(eid, peerIP, peerMac, vtep)
	if !inserted {
		log.G(context.TODO()).WithFields(log.Fields{
			"nid":     n.id,
			"eid":     eid,
			"peerIP":  peerIP,
			"peerMac": peerMac,
			"vtep":    vtep,
		}).Warn("peerAdd: entry already present in db")
	}
	if vtep.IsValid() {
		if err := n.addNeighbor(peerIP, peerMac, vtep); err != nil {
			var nserr osl.NeighborSearchError
			if dbEntries > 1 && errors.As(err, &nserr) && nserr.Present {
				// Conflicting neighbor entries are already programmed into the kernel and we are in the transient case.
				// Upon deletion if the active configuration is deleted the next one from the database will be restored.
				return nil
			}
			return fmt.Errorf("peer add operation failed: %w", err)
		}
	}
	return nil
}

// addNeighbor programs the kernel so the given peer is reachable through the VXLAN tunnel.
func (n *network) addNeighbor(peerIP netip.Prefix, peerMac hashable.MACAddr, vtep netip.Addr) (retErr error) {
	if n.sbox == nil {
		// We are hitting this case for all the events that are arriving before that the sandbox
		// is being created. The peer got already added into the database and the sandbox init will
		// call the peerDbUpdateSandbox that will configure all these peers from the database
		return nil
	}

	s := n.getSubnetforIP(peerIP)
	if s == nil {
		return fmt.Errorf("couldn't find the subnet %q in network %q", peerIP.String(), n.id)
	}

	if err := n.joinSandbox(s, false); err != nil {
		return fmt.Errorf("network sandbox join failed for subnet %q: %v", s.subnetIP.String(), err)
	}

	if n.secure {
		if err := n.driver.setupEncryption(vtep); err != nil {
			return fmt.Errorf("could not setup encryption for peer %v: %w", vtep, err)
		}
		defer func() {
			if retErr != nil {
				if err := n.driver.removeEncryption(vtep); err != nil {
					retErr = errors.Join(retErr, fmt.Errorf("could not roll back encryption for peer %v: %w", vtep, err))
				}
			}
		}()
	}

	// Add neighbor entry for the peer IP
	if err := n.sbox.AddNeighbor(peerIP.Addr().AsSlice(), peerMac.AsSlice(), osl.WithLinkName(s.vxlanName)); err != nil {
		return fmt.Errorf("could not add neighbor entry into the sandbox: %w", err)
	}
	defer func() {
		if retErr != nil {
			if err := n.sbox.DeleteNeighbor(peerIP.Addr().AsSlice(), peerMac.AsSlice(), osl.WithLinkName(s.vxlanName)); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("could not roll back sandbox neighbor entry for %v: %w", peerIP, err))
			}
		}
	}()

	// Add fdb entry to the bridge for the peer mac
	if v := hashable.IPMACFrom(vtep, peerMac); n.fdbCnt.Add(v, 1) == 1 {
		if err := n.sbox.AddNeighbor(vtep.AsSlice(), peerMac.AsSlice(), osl.WithLinkName(s.vxlanName), osl.WithFamily(syscall.AF_BRIDGE)); err != nil {
			n.fdbCnt.Add(v, -1)
			return fmt.Errorf("could not add fdb entry into the sandbox: %w", err)
		}
	}

	return nil
}

// peerDelete removes an entry from the peer database.
//
// Local peers are signified by an invalid vtep (i.e. netip.Addr{}).
func (n *network) peerDelete(eid string, peerIP netip.Prefix, peerMac hashable.MACAddr, vtep netip.Addr) error {
	if eid == "" {
		return errors.New("invalid endpoint id")
	}

	logger := log.G(context.TODO()).WithFields(log.Fields{
		"nid":  n.id,
		"eid":  eid,
		"ip":   peerIP,
		"mac":  peerMac,
		"vtep": vtep,
	})
	deleted, dbEntries := n.peerdb.Delete(eid, peerIP, peerMac, vtep)
	if !deleted {
		logger.Warn("Peer entry was not in db")
	}
	if vtep.IsValid() {
		err := n.deleteNeighbor(peerIP, peerMac, vtep)
		if err != nil {
			if dbEntries > 0 && errors.As(err, &osl.NeighborSearchError{}) {
				// We fall in here if there is a transient state and if the neighbor that is being deleted
				// was never been configured into the kernel (we allow only 1 configuration at the time per <ip,mac> mapping)
				return nil
			}
			logger.WithError(err).Warn("Peer delete operation failed")
		}
	}

	if dbEntries > 0 {
		// If there is still an entry into the database and the deletion went through without errors means that there is now no
		// configuration active in the kernel.
		// Restore one configuration for the ip directly from the database, note that is guaranteed that there is one
		peer, ok := n.peerdb.Get(peerIP)
		if !ok {
			return fmt.Errorf("peerDelete: unable to restore a configuration: no entry for %v found in the database", peerIP)
		}
		if err := n.addNeighbor(peerIP, peer.mac, peer.vtep); err != nil {
			return fmt.Errorf("peer delete operation failed: %w", err)
		}
	}
	return nil
}

// deleteNeighbor removes programming from the kernel for the given peer to be
// reachable through the VXLAN tunnel. It is the inverse of [driver.addNeighbor].
func (n *network) deleteNeighbor(peerIP netip.Prefix, peerMac hashable.MACAddr, vtep netip.Addr) error {
	if n.sbox == nil {
		return nil
	}

	s := n.getSubnetforIP(peerIP)
	if s == nil {
		return fmt.Errorf("could not find the subnet %q in network %q", peerIP.String(), n.id)
	}

	fdbKey := hashable.IPMACFrom(vtep, peerMac)
	if n.fdbCnt[fdbKey] == 0 {
		err := osl.NeighborSearchError{IP: vtep.AsSlice(), MAC: peerMac.AsSlice(), LinkName: s.vxlanName, Present: false}
		return fmt.Errorf("fdb entry was not programmed into sandbox: %w", err)
	}

	// Delete neighbor entry for the peer IP
	if err := n.sbox.DeleteNeighbor(peerIP.Addr().AsSlice(), peerMac.AsSlice(), osl.WithLinkName(s.vxlanName)); err != nil {
		// If the entry is missing we can't be sure if it is because the
		// neighbor entry we programmed disappeared out from under us,
		// or because this entry was never programmed into the kernel
		// and the fdbCnt reference belongs to another peer entry with
		// the same MAC and VTEP. That entry would have to be for a
		// different peer IP as the kernel matches neighbor deletes on
		// the IP alone; the delete would have succeeded if the entry
		// was for this peer's IP.
		//
		// Assume the latter and return early, without decrementing
		// fdbCnt, so we don't risk decrementing another entry's
		// reference and tear down an IPsec tunnel that is still in use.
		// We leak the tunnel if our assumption is wrong, which is safer
		// than breaking confidentiality.
		return fmt.Errorf("could not delete neighbor entry in the sandbox: %w", err)
	}

	// Remove fdb entry to the bridge for the peer mac
	if n.fdbCnt.Add(fdbKey, -1) == 0 {
		err := n.sbox.DeleteNeighbor(vtep.AsSlice(), peerMac.AsSlice(), osl.WithLinkName(s.vxlanName), osl.WithFamily(syscall.AF_BRIDGE))
		if err != nil && !errors.As(err, &osl.NeighborSearchError{}) {
			// Deletion failed for a reason other than the entry
			// being absent from the fdb.
			n.fdbCnt.Add(fdbKey, 1)
			return fmt.Errorf("could not delete fdb entry in the sandbox: %w", err)
		}
	}

	// Decrement the reference count for the encrypted tunnel last so there
	// is no opportunity for VXLAN datagrams to be sent in the clear. Note
	// that control flow can only reach here if we succeeded in deleting the
	// neighbor entry (and fdb entry, if applicable).
	if n.secure {
		if err := n.driver.removeEncryption(vtep); err != nil {
			return fmt.Errorf("could not remove encryption for peer %v: %w", vtep, err)
		}
	}

	return nil
}
