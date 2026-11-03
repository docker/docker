//go:build linux

package overlay

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"syscall"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/libnetwork/discoverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/overlay/overlayutils"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/nftables"
	"github.com/moby/moby/v2/daemon/libnetwork/iptables"
	"github.com/moby/moby/v2/daemon/libnetwork/ns"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/vishvananda/netlink"
)

/*
Encrypted overlay networks use IPsec in transport mode to encrypt and
authenticate the VXLAN UDP datagrams. This driver implements a bespoke control
plane which negotiates the security parameters for each peer-to-peer tunnel.

IPsec Terminology

 - ESP: IPSec Encapsulating Security Payload
 - SPI: Security Parameter Index
 - ICV: Integrity Check Value
 - SA: Security Association https://en.wikipedia.org/wiki/IPsec#Security_association


Developer documentation for Linux IPsec is rather sparse online. The following
slide deck provides a decent overview.
https://libreswan.org/wiki/images/e/e0/Netdev-0x12-ipsec-flow.pdf

The Linux IPsec stack is part of XFRM, the netlink packet transformation
interface.
https://man7.org/linux/man-pages/man8/ip-xfrm.8.html
*/

const (
	// Value used to mark outgoing packets which should have our IPsec
	// processing applied. It is also used as a label to identify XFRM
	// states (Security Associations) and policies (Security Policies)
	// programmed by us so we know which ones we can clean up without
	// disrupting other VPN connections on the system.
	mark = 0xD0C4E3

	pktExpansion = 26 // SPI(4) + SeqN(4) + IV(8) + PadLength(1) + NextHeader(1) + ICV(8)
)

const (
	forward = iota + 1
	reverse
	bidir
)

// Mark value for matching packets which should have our IPsec security policy
// applied.
var spMark = netlink.XfrmMark{Value: mark, Mask: 0xffffffff}

type key struct {
	value []byte
	tag   uint32
}

func (k *key) String() string {
	if k != nil {
		return fmt.Sprintf("(key: %s, tag: 0x%x)", hex.EncodeToString(k.value)[0:5], k.tag)
	}
	return ""
}

// Security Parameter Indices for the IPsec flows between local node and a
// remote peer, which identify the Security Associations (XFRM states) to be
// applied when encrypting and decrypting packets.
type spi struct {
	forward int
	reverse int
}

func (s *spi) String() string {
	return fmt.Sprintf("SPI(FWD: 0x%x, REV: 0x%x)", uint32(s.forward), uint32(s.reverse))
}

type encrNode struct {
	spi   []spi
	count int
}

// encrMap is a map of node IP addresses to their encryption parameters.
//
// Like all Go maps, it is not safe for concurrent use.
type encrMap map[netip.Addr]encrNode

func (e encrMap) String() string {
	b := new(bytes.Buffer)
	for k, v := range e {
		b.WriteString("\n")
		b.WriteString(k.String())
		b.WriteString(":")
		b.WriteString("[")
		for _, s := range v.spi {
			b.WriteString(s.String())
			b.WriteString(",")
		}
		b.WriteString("]")
	}
	return b.String()
}

// setupEncryption programs the encryption parameters for secure communication
// between the local node and a remote node.
func (d *driver) setupEncryption(remoteIP netip.Addr) error {
	log.G(context.TODO()).Debugf("setupEncryption(%s)", remoteIP)

	d.encrMu.Lock()
	defer d.encrMu.Unlock()
	if len(d.keys) == 0 {
		return types.ForbiddenErrorf("encryption key is not present")
	}
	d.mu.Lock()
	localIP, advIP := d.bindAddress, d.advertiseAddress
	d.mu.Unlock()
	log.G(context.TODO()).Debugf("Programming encryption between %s and %s", localIP, remoteIP)

	// Block userspace from marking arbitrary packets for encryption using
	// the security association we are about to program into the kernel.
	// We only need to dynamically program this rule in iptables mode.
	// The same protection is built into our static nftables ruleset.
	if !nftables.Enabled() && len(d.secMap) == 0 {
		if err := d.programMangleMarkSpoofProtection(true); err != nil {
			return err
		}
	}

	indices := make([]spi, 0, len(d.keys))

	for i, k := range d.keys {
		spis := spi{buildSPI(advIP, remoteIP, k.tag), buildSPI(remoteIP, advIP, k.tag)}
		dir := reverse
		if i == 0 {
			dir = bidir
		}
		fSA, rSA, err := programSA(localIP.AsSlice(), remoteIP.AsSlice(), spis, k, dir, true)
		if err != nil {
			log.G(context.TODO()).Warn(err)
		}
		indices = append(indices, spis)
		if i != 0 {
			continue
		}
		err = programSP(fSA, rSA, true)
		if err != nil {
			log.G(context.TODO()).Warn(err)
		}
	}

	node := d.secMap[remoteIP]
	node.spi = indices
	node.count++
	d.secMap[remoteIP] = node

	return nil
}

func (d *driver) removeEncryption(remoteIP netip.Addr) error {
	log.G(context.TODO()).Debugf("removeEncryption(%s)", remoteIP)

	d.encrMu.Lock()
	defer d.encrMu.Unlock()

	var spi []spi
	node, ok := d.secMap[remoteIP]
	if !ok {
		return nil
	}
	if node.count == 1 {
		delete(d.secMap, remoteIP)
		spi = node.spi
	} else {
		node.count--
		d.secMap[remoteIP] = node
	}

	for i, idxs := range spi {
		dir := reverse
		if i == 0 {
			dir = bidir
		}
		fSA, rSA, err := programSA(d.bindAddress.AsSlice(), remoteIP.AsSlice(), idxs, nil, dir, false)
		if err != nil {
			log.G(context.TODO()).Warn(err)
		}
		if i != 0 {
			continue
		}
		err = programSP(fSA, rSA, false)
		if err != nil {
			log.G(context.TODO()).Warn(err)
		}
	}

	if !nftables.Enabled() && len(d.secMap) == 0 {
		if err := d.programMangleMarkSpoofProtection(false); err != nil {
			return err
		}
	}

	return nil
}

func (d *driver) transportIPTable() (*iptables.IPTable, error) {
	v6, err := d.isIPv6Transport()
	if err != nil {
		return nil, err
	}
	version := iptables.IPv4
	if v6 {
		version = iptables.IPv6
	}
	return iptables.GetIptable(version), nil
}

func (d *driver) programMangle(vni uint32, add bool) error {
	var (
		m      = strconv.FormatUint(mark, 10)
		chain  = "OUTPUT"
		rule   = append(matchVXLAN(overlayutils.VXLANUDPPort(), vni), "-j", "MARK", "--set-mark", m)
		a      = iptables.Append
		action = "install"
	)

	iptable, err := d.transportIPTable()
	if err != nil {
		// Fail closed if unsure. Better safe than cleartext.
		return err
	}

	// Clear out stale mangle rules which indiscriminately mark both
	// authentic and forged VXLAN datagrams for encryption.
	staleErr := iptable.ProgramRule(iptables.Mangle, chain, iptables.Delete, rule)
	if staleErr != nil {
		staleErr = fmt.Errorf("could not clear old mangle rule: %w", staleErr)
	}

	if !add {
		a = iptables.Delete
		action = "remove"
	}

	// Mark authentic VXLAN packets to be encrypted, skipping forged packets
	// with the same VNI.
	//
	// Only packets sent from this network namespace are evaluated by the
	// OUTPUT chain so we do not need to explicitly guard against marking
	// forged packets sent by processes from other network namespaces. We
	// only need to distinguish authentic packets encapsulated by our VXLAN
	// links (which are sent from this network namespace) from forged
	// packets sent by processes in this network namespace. All forged
	// packets processed by the OUTPUT chain will match --socket-exists. Our
	// authentic VXLAN packets will not match as they are associated with
	// foreign sockets from containers' namespaces or with no socket at all
	// (always the case on Linux v4.18 and earlier, which scrubbed socket
	// associations when crossing netns boundaries).
	rule = append([]string{"-m", "owner", "!", "--socket-exists"}, rule...)
	err = iptable.ProgramRule(iptables.Mangle, chain, a, rule)
	if err != nil {
		err = fmt.Errorf("could not %s mangle rule: %w", action, err)
	}
	return errors.Join(staleErr, err)
}

func (d *driver) programMangleMarkSpoofProtection(add bool) error {
	a := iptables.Append
	action := "install"
	if !add {
		a = iptables.Delete
		action = "remove"
	}

	iptable, err := d.transportIPTable()
	if err != nil {
		// Fail closed if unsure. Better safe than cleartext.
		return err
	}

	// Block processes in the host netns with CAP_NET_RAW from using
	// setsockopt(SO_MARK) to encrypt forged packets and neutralize any
	// exploitable mangle rules earlier in the chain left over from old
	// daemon versions. We do not need to guard against forged packets from
	// other network namespaces as any packet marks are scrubbed when the
	// packet crosses the netns boundary.
	clearUserspaceMarkRule := []string{
		"-p", "udp", "--dport", strconv.FormatUint(uint64(overlayutils.VXLANUDPPort()), 10),
		"-m", "owner", "--socket-exists",
		"-m", "mark", "--mark", strconv.FormatUint(mark, 10),
		"-j", "MARK", "--set-mark", "0",
	}
	if err := iptable.ProgramRule(iptables.Mangle, "OUTPUT", a, clearUserspaceMarkRule); err != nil {
		return fmt.Errorf("could not %s mangle local-spoof-protection rule: %w", action, err)
	}
	return nil
}

func (d *driver) programInput(vni uint32, add bool) error {
	var (
		plainVxlan = matchVXLAN(overlayutils.VXLANUDPPort(), vni)
		chain      = "INPUT"
		msg        = "add"
	)

	rule := func(policy, jump string) []string {
		args := append([]string{"-m", "policy", "--dir", "in", "--pol", policy}, plainVxlan...)
		return append(args, "-j", jump)
	}

	iptable, err := d.transportIPTable()
	if err != nil {
		// Fail closed if unsure. Better safe than cleartext.
		return err
	}

	if !add {
		msg = "remove"
	}

	action := func(a iptables.Action) iptables.Action {
		if !add {
			return iptables.Delete
		}
		return a
	}

	// Drop incoming VXLAN datagrams for the VNI which were received in cleartext.
	// Insert at the top of the chain so the packets are dropped even if an
	// administrator-configured rule exists which would otherwise unconditionally
	// accept incoming VXLAN traffic.
	if err := iptable.ProgramRule(iptables.Filter, chain, action(iptables.Insert), rule("none", "DROP")); err != nil {
		return fmt.Errorf("could not %s input drop rule: %w", msg, err)
	}

	return nil
}

func (d *driver) programOverlayEncryptionFirewall(ctx context.Context, vni uint32, encrypted bool) error {
	if nftables.Enabled() {
		return d.programOverlayEncVNINft(ctx, vni, encrypted)
	}

	mangleErr := d.programMangle(vni, encrypted)
	if mangleErr != nil && encrypted {
		return mangleErr
	}
	err := d.programInput(vni, encrypted)
	if err != nil && encrypted {
		return errors.Join(err, d.programMangle(vni, false))
	}
	return errors.Join(mangleErr, err)
}

// restoreOverlayEncryptionFirewall restores the iptables rules for encryption,
// which a firewalld reload may have deleted.
func (d *driver) restoreOverlayEncryptionFirewall(ctx context.Context) {
	if nftables.Enabled() {
		return
	}

	d.encrMu.Lock()
	if len(d.secMap) > 0 {
		if err := d.programMangleMarkSpoofProtection(true); err != nil {
			log.G(ctx).WithError(err).Error("Failed to restore iptables local-spoof-protection rule on firewalld reload")
		}
	}
	d.encrMu.Unlock()

	d.mu.Lock()
	nids := slices.Collect(maps.Keys(d.networks))
	d.mu.Unlock()

	for _, nid := range nids {
		d.restoreNetworkEncryptionFirewall(ctx, nid)
	}
}

func (d *driver) restoreNetworkEncryptionFirewall(ctx context.Context, nid string) {
	n, unlock, err := d.lockNetwork(nid)
	if err != nil {
		// Network deleted since the reload started, not an error.
		return
	}
	defer unlock()

	if !n.secure {
		return
	}

	for _, s := range n.subnets {
		if !s.sboxInit {
			continue
		}
		// Unlike programOverlayEncryptionFirewall, do not roll back the
		// mangle rule if the input rule cannot be restored. The network
		// is still in use, and encrypting outgoing traffic is better
		// than sending it in cleartext.
		mangleErr := d.programMangle(s.vni, true)
		inputErr := d.programInput(s.vni, true)
		if err := errors.Join(mangleErr, inputErr); err != nil {
			log.G(ctx).WithFields(log.Fields{
				"network_id": n.id,
				"vni":        s.vni,
				"error":      err,
			}).Error("Failed to restore iptables rules for encrypted overlay network on firewalld reload")
		}
	}
}

func programSA(localIP, remoteIP net.IP, spi spi, k *key, dir int, add bool) (fSA *netlink.XfrmState, rSA *netlink.XfrmState, lastErr error) {
	var (
		action      = "Removing"
		xfrmProgram = ns.NlHandle().XfrmStateDel
	)

	if add {
		action = "Adding"
		xfrmProgram = ns.NlHandle().XfrmStateAdd
	}

	if dir&reverse > 0 {
		rSA = &netlink.XfrmState{
			Src:   remoteIP,
			Dst:   localIP,
			Proto: netlink.XFRM_PROTO_ESP,
			Spi:   spi.reverse,
			Mode:  netlink.XFRM_MODE_TRANSPORT,
			Reqid: mark,
		}
		if add {
			rSA.Aead = buildAeadAlgo(k, spi.reverse)
		}

		exists, err := saExists(rSA)
		if err != nil {
			lastErr = err
			exists = !add
		}

		if add != exists {
			log.G(context.TODO()).Debugf("%s: rSA{%s}", action, rSA)
			if err := xfrmProgram(rSA); err != nil {
				log.G(context.TODO()).Warnf("Failed %s rSA{%s}: %v", action, rSA, err)
			}
		}
	}

	if dir&forward > 0 {
		fSA = &netlink.XfrmState{
			Src:   localIP,
			Dst:   remoteIP,
			Proto: netlink.XFRM_PROTO_ESP,
			Spi:   spi.forward,
			Mode:  netlink.XFRM_MODE_TRANSPORT,
			Reqid: mark,
		}
		if add {
			fSA.Aead = buildAeadAlgo(k, spi.forward)
		}

		exists, err := saExists(fSA)
		if err != nil {
			lastErr = err
			exists = !add
		}

		if add != exists {
			log.G(context.TODO()).Debugf("%s fSA{%s}", action, fSA)
			if err := xfrmProgram(fSA); err != nil {
				log.G(context.TODO()).Warnf("Failed %s fSA{%s}: %v.", action, fSA, err)
			}
		}
	}

	return fSA, rSA, lastErr
}

// getMinimalIP returns the address in its shortest form
// If ip contains an IPv4-mapped IPv6 address, the 4-octet form of the IPv4 address will be returned.
// Otherwise ip is returned unchanged.
func getMinimalIP(ip net.IP) net.IP {
	if ip != nil && ip.To4() != nil {
		return ip.To4()
	}
	return ip
}

func programSP(fSA *netlink.XfrmState, rSA *netlink.XfrmState, add bool) error {
	action := "Removing"
	xfrmProgram := ns.NlHandle().XfrmPolicyDel
	if add {
		action = "Adding"
		xfrmProgram = ns.NlHandle().XfrmPolicyAdd
	}

	// Create a congruent cidr
	s := getMinimalIP(fSA.Src)
	d := getMinimalIP(fSA.Dst)
	fullMask := net.CIDRMask(8*len(s), 8*len(s))

	fPol := &netlink.XfrmPolicy{
		Src:     &net.IPNet{IP: s, Mask: fullMask},
		Dst:     &net.IPNet{IP: d, Mask: fullMask},
		Dir:     netlink.XFRM_DIR_OUT,
		Proto:   syscall.IPPROTO_UDP,
		DstPort: int(overlayutils.VXLANUDPPort()),
		Mark:    &spMark,
		Tmpls: []netlink.XfrmPolicyTmpl{
			{
				Src:   fSA.Src,
				Dst:   fSA.Dst,
				Proto: netlink.XFRM_PROTO_ESP,
				Mode:  netlink.XFRM_MODE_TRANSPORT,
				Spi:   fSA.Spi,
				Reqid: mark,
			},
		},
	}

	exists, err := spExists(fPol)
	if err != nil {
		exists = !add
	}

	if add != exists {
		log.G(context.TODO()).Debugf("%s fSP{%s}", action, fPol)
		if err := xfrmProgram(fPol); err != nil {
			log.G(context.TODO()).Warnf("%s fSP{%s}: %v", action, fPol, err)
		}
	}

	return nil
}

func saExists(sa *netlink.XfrmState) (bool, error) {
	_, err := ns.NlHandle().XfrmStateGet(sa)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		err = fmt.Errorf("Error while checking for SA existence: %v", err)
		log.G(context.TODO()).Warn(err)
		return false, err
	}
}

func spExists(sp *netlink.XfrmPolicy) (bool, error) {
	_, err := ns.NlHandle().XfrmPolicyGet(sp)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.ENOENT):
		return false, nil
	default:
		err = fmt.Errorf("Error while checking for SP existence: %v", err)
		log.G(context.TODO()).Warn(err)
		return false, err
	}
}

func buildSPI(src, dst netip.Addr, st uint32) int {
	h := fnv.New32a()
	v := src.As16()
	h.Write(v[:])
	binary.Write(h, binary.BigEndian, st)
	v = dst.As16()
	h.Write(v[:])
	return int(h.Sum32())
}

func buildAeadAlgo(k *key, s int) *netlink.XfrmStateAlgo {
	salt := make([]byte, 4)
	binary.BigEndian.PutUint32(salt, uint32(s))
	return &netlink.XfrmStateAlgo{
		Name:   "rfc4106(gcm(aes))",
		Key:    append(k.value, salt...),
		ICVLen: 64,
	}
}

func (d *driver) setKeys(ctx context.Context, encrData discoverapi.DriverEncryptionConfig) error {
	keys := make([]*key, 0, len(encrData.Keys))
	for i := 0; i < len(encrData.Keys); i++ {
		keys = append(keys, &key{
			value: encrData.Keys[i],
			tag:   uint32(encrData.Tags[i]),
		})
	}
	d.encrMu.Lock()
	defer d.encrMu.Unlock()

	// Remove any stale policy, state
	clearEncryptionStates()
	if !nftables.Enabled() && len(d.secMap) > 0 {
		if err := d.programMangleMarkSpoofProtection(false); err != nil {
			log.G(ctx).WithError(err).Warn("Failed to clear iptables local-spoof-protection rule")
		}
	}
	// Accept the encryption keys and clear any stale encryption map
	d.secMap = encrMap{}
	d.keys = keys

	log.G(ctx).WithFields(log.Fields{
		"driver": "overlay",
		"keys":   d.keys,
	}).Debug("Set initial encryption keys")
	return nil
}

// updateKeys allows to add a new key and/or change the primary key and/or prune an existing key
// The primary key is the key used in transmission and will go in first position in the list.
func (d *driver) updateKeys(ctx context.Context, encrData discoverapi.DriverEncryptionUpdate) error {
	var newKey, primaryKey, pruneKey *key
	if encrData.Key != nil {
		newKey = &key{
			value: encrData.Key,
			tag:   uint32(encrData.Tag),
		}
	}
	if encrData.Primary != nil {
		primaryKey = &key{
			value: encrData.Primary,
			tag:   uint32(encrData.PrimaryTag),
		}
	}
	if encrData.Prune != nil {
		pruneKey = &key{
			value: encrData.Prune,
			tag:   uint32(encrData.PruneTag),
		}
	}

	d.encrMu.Lock()
	defer d.encrMu.Unlock()

	log.G(ctx).WithFields(log.Fields{
		"driver":  "overlay",
		"current": d.keys,
		"new":     newKey,
		"primary": primaryKey,
		"prune":   pruneKey,
	}).Debug("Updating encryption keys")

	var (
		newIdx = -1
		priIdx = -1
		delIdx = -1
		lIP    = d.bindAddress
		aIP    = d.advertiseAddress
	)

	// add new
	if newKey != nil {
		d.keys = append(d.keys, newKey)
		newIdx += len(d.keys)
	}
	for i, k := range d.keys {
		if primaryKey != nil && k.tag == primaryKey.tag {
			priIdx = i
		}
		if pruneKey != nil && k.tag == pruneKey.tag {
			delIdx = i
		}
	}

	if (newKey != nil && newIdx == -1) ||
		(primaryKey != nil && priIdx == -1) ||
		(pruneKey != nil && delIdx == -1) {
		return types.InvalidParameterErrorf("cannot find proper key indices while processing key update:"+
			"(newIdx,priIdx,delIdx):(%d, %d, %d)", newIdx, priIdx, delIdx)
	}

	if priIdx != -1 && priIdx == delIdx {
		return types.InvalidParameterErrorf("attempting to both make a key (index %d) primary and delete it", priIdx)
	}

	for rIP, node := range d.secMap {
		idxs := updateNodeKey(lIP, aIP, rIP, node.spi, d.keys, newIdx, priIdx, delIdx)
		if idxs != nil {
			d.secMap[rIP] = encrNode{idxs, node.count}
		}
	}

	// swap primary
	if priIdx != -1 {
		d.keys[0], d.keys[priIdx] = d.keys[priIdx], d.keys[0]
	}
	// prune
	if delIdx != -1 {
		if delIdx == 0 {
			delIdx = priIdx
		}
		d.keys = append(d.keys[:delIdx], d.keys[delIdx+1:]...)
	}

	log.G(ctx).WithFields(log.Fields{
		"driver": "overlay",
		"keys":   d.keys,
	}).Debug("Updated encryption keys")
	return nil
}

/********************************************************
 * Steady state: rSA0, rSA1, rSA2, fSA1, fSP1
 * Rotation --> -rSA0, +rSA3, +fSA2, +fSP2/-fSP1, -fSA1
 * Steady state: rSA1, rSA2, rSA3, fSA2, fSP2
 *********************************************************/

// Spis and keys are sorted in such away the one in position 0 is the primary
func updateNodeKey(lIP, aIP, rIP netip.Addr, idxs []spi, curKeys []*key, newIdx, priIdx, delIdx int) []spi {
	log.G(context.TODO()).Debugf("Updating keys for node: %s (%d,%d,%d)", rIP, newIdx, priIdx, delIdx)

	spis := idxs
	log.G(context.TODO()).Debugf("Current: %v", spis)

	// add new
	if newIdx != -1 {
		spis = append(spis, spi{
			forward: buildSPI(aIP, rIP, curKeys[newIdx].tag),
			reverse: buildSPI(rIP, aIP, curKeys[newIdx].tag),
		})
	}

	if delIdx != -1 {
		// -rSA0
		programSA(lIP.AsSlice(), rIP.AsSlice(), spis[delIdx], nil, reverse, false)
	}

	if newIdx > -1 {
		// +rSA2
		programSA(lIP.AsSlice(), rIP.AsSlice(), spis[newIdx], curKeys[newIdx], reverse, true)
	}

	if priIdx > 0 {
		// +fSA2
		fSA2, _, _ := programSA(lIP.AsSlice(), rIP.AsSlice(), spis[priIdx], curKeys[priIdx], forward, true)

		// +fSP2, -fSP1
		s := getMinimalIP(fSA2.Src)
		d := getMinimalIP(fSA2.Dst)
		fullMask := net.CIDRMask(8*len(s), 8*len(s))

		fSP1 := &netlink.XfrmPolicy{
			Src:     &net.IPNet{IP: s, Mask: fullMask},
			Dst:     &net.IPNet{IP: d, Mask: fullMask},
			Dir:     netlink.XFRM_DIR_OUT,
			Proto:   syscall.IPPROTO_UDP,
			DstPort: int(overlayutils.VXLANUDPPort()),
			Mark:    &spMark,
			Tmpls: []netlink.XfrmPolicyTmpl{
				{
					Src:   fSA2.Src,
					Dst:   fSA2.Dst,
					Proto: netlink.XFRM_PROTO_ESP,
					Mode:  netlink.XFRM_MODE_TRANSPORT,
					Spi:   fSA2.Spi,
					Reqid: mark,
				},
			},
		}
		log.G(context.TODO()).Debugf("Updating fSP{%s}", fSP1)
		if err := ns.NlHandle().XfrmPolicyUpdate(fSP1); err != nil {
			log.G(context.TODO()).Warnf("Failed to update fSP{%s}: %v", fSP1, err)
		}

		// -fSA1
		programSA(lIP.AsSlice(), rIP.AsSlice(), spis[0], nil, forward, false)
	}

	// swap
	if priIdx > 0 {
		swp := spis[0]
		spis[0] = spis[priIdx]
		spis[priIdx] = swp
	}
	// prune
	if delIdx != -1 {
		if delIdx == 0 {
			delIdx = priIdx
		}
		spis = append(spis[:delIdx], spis[delIdx+1:]...)
	}

	log.G(context.TODO()).Debugf("Updated: %v", spis)

	return spis
}

func (n *network) maxMTU() int {
	mtu := 1500
	if n.mtu != 0 {
		mtu = n.mtu
	}
	// Assume an IPv6 transport if the transport family is not known, as
	// its larger overhead gives an MTU that fits either family.
	if v6, err := n.driver.isIPv6Transport(); err == nil && !v6 {
		mtu -= vxlanEncap
	} else {
		mtu -= vxlan6Encap
	}
	if n.secure {
		// In case of encryption account for the
		// esp packet expansion and padding
		mtu -= pktExpansion
		mtu -= (mtu % 4)
	}
	return mtu
}

func clearEncryptionStates() {
	nlh := ns.NlHandle()
	spList, err := nlh.XfrmPolicyList(netlink.FAMILY_ALL)
	if err != nil {
		log.G(context.TODO()).Warnf("Failed to retrieve SP list for cleanup: %v", err)
	}
	saList, err := nlh.XfrmStateList(netlink.FAMILY_ALL)
	if err != nil {
		log.G(context.TODO()).Warnf("Failed to retrieve SA list for cleanup: %v", err)
	}
	for _, sp := range spList {
		if sp.Mark != nil && sp.Mark.Value == spMark.Value {
			if err := nlh.XfrmPolicyDel(&sp); err != nil {
				log.G(context.TODO()).Warnf("Failed to delete stale SP %s: %v", sp, err)
				continue
			}
			log.G(context.TODO()).Debugf("Removed stale SP: %s", sp)
		}
	}
	for _, sa := range saList {
		if sa.Reqid == mark {
			if err := nlh.XfrmStateDel(&sa); err != nil {
				log.G(context.TODO()).Warnf("Failed to delete stale SA %s: %v", sa, err)
				continue
			}
			log.G(context.TODO()).Debugf("Removed stale SA: %s", sa)
		}
	}
}
