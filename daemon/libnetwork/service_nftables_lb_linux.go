package libnetwork

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/maputil"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/nftables"
)

const (
	natServiceVipMap  = "nat-service-vip"
	natPublishPortMap = "nat-publish-port"
	dsrVipSet         = "vip"
	dsrRealServerMap  = "real-server"

	// numgenModulus is the size of the random bucket space a VIP's or a
	// published port's backends are partitioned over.
	numgenModulus = 65536
	numgenExpr    = "numgen random mod 65536"
)

// nftLBState is the nftables load-balancer state for a load-balancer sandbox's
// tables. It lives on the [Sandbox] (see osSandbox) so that its lifetime is the
// sandbox's: the tables it describes go when the sandbox does, and so does it,
// without anything having to remember to discard it.
//
// Those tables have exactly one writer: each loadBalancer records the
// contribution it wants here, and applyNFTLB renders the complete element set
// from every recorded contribution. That's not a stylistic choice, it's forced
// by the map types. nat-publish-port is an interval map keyed on
// `l4proto . dport . numgen`, so the numgen space for one published port must be
// partitioned by whoever owns that port: two loadBalancers each partitioning it
// independently would emit overlapping intervals, which nftables rejects.
//
// And loadBalancers do legitimately share a published port and a VIP.
// Controller.serviceBindings is keyed on serviceKey{id, ports}, so a service
// whose published-port set changes has a separate binding - and so a separate
// loadBalancer per network - for each generation, coexisting for as long as the
// rolling update takes to drain. Composing their contributions here is what
// lets a shared port be partitioned once, across every backend that serves it,
// instead of each generation clobbering the other's elements.
//
// The iptables/IPVS implementation has no equivalent: it names each
// loadBalancer's data plane by lb.fwMark, and one mark cannot route different
// published ports to different backend sets, so there it remains one generation
// per mark.
type nftLBState struct {
	// contrib is what each loadBalancer using this sandbox wants programmed. It's
	// keyed by serviceKey rather than by *loadBalancer so a stale entry can't
	// keep a loadBalancer alive, and so it's loggable.
	contrib map[serviceKey]nftLBContribution

	// aliasedVIPs is the set of VIPs currently added to the LB endpoint's
	// interface, keyed by the VIP's string form. Diffing the composed set
	// against it is what tells a sync which aliases to add and which to remove,
	// so no sync has to know what an earlier one did.
	aliasedVIPs map[string]struct{}
}

// nftLBContribution is the data plane one loadBalancer wants: its VIP, the
// backends currently able to serve it, and the ingress ports its generation of
// the service publishes.
type nftLBContribution struct {
	vip      net.IP
	backends []net.IP
	ports    []*PortConfig
}

// portKey identifies a host-published ingress port within a network's
// load-balancer sandbox. Two generations of one service commonly publish the
// same one.
type portKey struct {
	proto PortConfig_Protocol
	port  uint32
}

// syncLBBackendsNFTables records lb's desired data plane and re-applies the
// network's load-balancer tables from every loadBalancer's contribution.
//
// It reports whether the NAT path is in place, which is what host-published
// ingress ports are load-balanced by. On false the caller must not publish those
// ports or record the service as programmed, so that the next backend event
// retries. A DSR failure is only logged: that table serves overlay VIP traffic
// alone, and every sync rebuilds it, so it heals on the next backend event.
func (n *Network) syncLBBackendsNFTables(ctx context.Context, ep *Endpoint, sb *Sandbox, lb *loadBalancer, rmService bool) bool {
	ifName := findIfaceDstName(sb, ep)
	if ifName == "" {
		log.G(ctx).Errorf("Failed find interface name for endpoint %s(%s) to create LB alias", ep.ID(), ep.Name())
		return false
	}

	// Which backend ends up in which of the numgen buckets doesn't matter: the
	// buckets are equal-weight and selected at random per connection, so any
	// assignment gives the same distribution. So this is left in whatever order
	// FilterValues produced.
	enabled := maputil.FilterValues(lb.backEnds, func(b *lbBackend) bool { return !b.disabled })
	ips := make([]net.IP, 0, len(enabled))
	for _, b := range enabled {
		ips = append(ips, b.ip)
	}

	key := serviceKey{id: lb.service.id, ports: lb.service.ingressPorts.String()}

	sb.nftLBMu.Lock()
	defer sb.nftLBMu.Unlock()

	state := &sb.nftLB
	if state.contrib == nil {
		state.contrib = map[serviceKey]nftLBContribution{}
		state.aliasedVIPs = map[string]struct{}{}
	}

	// A service being removed that has no backends left contributes nothing.
	// Dropping its entry is what releases its share of a published port it was
	// partitioning, and what removes its VIP alias.
	if rmService && len(ips) == 0 {
		delete(state.contrib, key)
	} else {
		state.contrib[key] = nftLBContribution{
			vip:      lb.vip,
			backends: ips,
			ports:    lb.service.ingressPorts,
		}
	}

	return n.applyNFTLB(ctx, sb, ifName, state)
}

// composeNFTLB gathers, for each VIP and each published port, every backend that
// serves it. The caller partitions the numgen space over each of those sets.
//
// The returned slices are in no particular order, and don't need to be: the
// buckets are equal-weight and drawn at random per connection, and the rendered
// nftables commands are ordered by element key rather than by the order the
// elements are enqueued in.
func composeNFTLB(state *nftLBState) (vipBackends map[string][]net.IP, portBackends map[portKey][]net.IP) {
	vipBackends = map[string][]net.IP{}
	portBackends = map[portKey][]net.IP{}

	// Assigning to a map key creates it whatever the value, so a contribution with
	// no backends still claims its VIP and its ports - append just yields a nil
	// slice for them. That's wanted: a service whose backends are all deweighted
	// keeps its VIP claimed, so its alias stays while the service exists, and
	// simply gets no elements.
	for _, c := range state.contrib {
		vip := c.vip.String()
		vipBackends[vip] = append(vipBackends[vip], c.backends...)

		for _, p := range c.ports {
			pk := portKey{proto: p.Protocol, port: p.PublishedPort}
			portBackends[pk] = append(portBackends[pk], c.backends...)
		}
	}

	return vipBackends, portBackends
}

// nftLBNATElements returns every element the docker-lb-nat maps should hold,
// partitioning the numgen bucket space over each VIP's and each published port's
// backends.
//
// It's separate from applying them so that the element set - which is the whole
// observable output of the load balancer's data plane - can be asserted without a
// network namespace or an nft binary.
func nftLBNATElements(vipBackends map[string][]net.IP, portBackends map[portKey][]net.IP) []nftables.MapElement {
	var elems []nftables.MapElement
	for vip, backends := range vipBackends {
		for iv, ip := range nftables.EqualWeightIntervals(backends, numgenModulus) {
			elems = append(elems, nftables.MapElement{
				MapName: natServiceVipMap,
				Key:     fmt.Sprintf("%s . %s", vip, iv),
				Value:   ip.String(),
			})
		}
	}
	for pk, backends := range portBackends {
		proto := strings.ToLower(pk.proto.String())
		for iv, ip := range nftables.EqualWeightIntervals(backends, numgenModulus) {
			elems = append(elems, nftables.MapElement{
				MapName: natPublishPortMap,
				Key:     fmt.Sprintf("%s . %d . %s", proto, pk.port, iv),
				Value:   ip.String(),
			})
		}
	}
	return elems
}

// applyNFTLB renders and applies the sandbox's load-balancer tables from every
// recorded contribution, and reconciles the VIP aliases on the LB endpoint's
// interface. Callers must hold sb.nftLBMu.
func (n *Network) applyNFTLB(ctx context.Context, sb *Sandbox, ifName string, state *nftLBState) bool {
	vipBackends, portBackends := composeNFTLB(state)

	wantVIPs := slices.Collect(maps.Keys(vipBackends))

	// VIPs this network no longer serves. Both their interface alias and their
	// membership of the DSR set have to go, and both are driven from the same
	// diff so neither can be left behind by the other.
	var goneVIPs []string
	for vip := range state.aliasedVIPs {
		if _, ok := vipBackends[vip]; !ok {
			goneVIPs = append(goneVIPs, vip)
		}
	}

	// Add aliases for VIPs that have appeared before programming the maps that
	// direct traffic at them, and remove the ones that have gone only after - so
	// a VIP is never routed to without its address being present.
	for _, vip := range wantVIPs {
		if _, ok := state.aliasedVIPs[vip]; ok {
			continue
		}
		if !addVIPAlias(ctx, n, sb, ifName, vip) {
			return false
		}
		state.aliasedVIPs[vip] = struct{}{}
	}

	var natTM nftables.Batch
	// This is the only writer of these maps, so replace their contents
	// wholesale. Selecting the elements from the map's own contents means no
	// record of what was last written can fall out of step with the table, and
	// anything an earlier failed update left behind is cleaned up here.
	for _, mapName := range []string{natServiceVipMap, natPublishPortMap} {
		natTM.Append(nftables.MapElementDeleteFunc{
			MapName: mapName,
			Fn:      func(nftables.MapElement) bool { return true },
		})
	}
	for _, e := range nftLBNATElements(vipBackends, portBackends) {
		natTM.Append(nftables.Create(e))
	}

	err := sb.osSbox.ApplyNFTable(ctx, nftables.IPv4, "docker-lb-nat", initNFTLBNAT, natTM)
	if err != nil {
		// The backend NAT map wasn't programmed, so report failure: the caller
		// must not publish ingress ports, or record a service as programmed,
		// for a data plane that is missing.
		log.G(ctx).WithError(err).Error("Failed to apply changes to nftables nat table")
		return false
	}

	if n.loadBalancerMode == loadBalancerModeDSR {
		n.applyNFTLBDSR(ctx, sb, ifName, wantVIPs, goneVIPs, vipBackends)
	}

	// The maps no longer direct anything at the VIPs that have gone, so their
	// aliases can be dropped. A VIP stays recorded unless removal succeeded, so
	// a failure here is retried by the next sync rather than forgotten.
	for _, vip := range goneVIPs {
		if removeVIPAlias(ctx, n, sb, ifName, vip) {
			delete(state.aliasedVIPs, vip)
		}
	}

	return true
}

// addVIPAlias adds vip as a /32 alias on the LB endpoint's interface. It reports
// whether the alias is in place.
func addVIPAlias(ctx context.Context, n *Network, sb *Sandbox, ifName, vip string) bool {
	err := sb.osSbox.AddAliasIP(ifName, &net.IPNet{IP: net.ParseIP(vip), Mask: net.CIDRMask(32, 32)})
	// A previous sync may have added the alias and then failed before recording
	// it, so tolerate it already being present rather than aborting before the
	// backends are programmed.
	if err != nil && !errors.Is(err, os.ErrExist) {
		log.G(ctx).Errorf("Failed add IP alias %s to network %s LB endpoint interface %s: %v", vip, n.ID(), ifName, err)
		return false
	}
	return true
}

// removeVIPAlias drops vip from the LB endpoint's interface. It reports whether
// the alias is gone, so the caller can keep the VIP recorded and retry on the
// next sync rather than losing track of it.
func removeVIPAlias(ctx context.Context, n *Network, sb *Sandbox, ifName, vip string) bool {
	err := sb.osSbox.RemoveAliasIP(ifName, &net.IPNet{IP: net.ParseIP(vip), Mask: net.CIDRMask(32, 32)})
	// The kernel reports a missing address as EADDRNOTAVAIL - treat that as
	// already-removed.
	if err != nil && !errors.Is(err, syscall.EADDRNOTAVAIL) {
		log.G(ctx).Errorf("Failed remove IP alias %s from network %s LB endpoint interface %s: %v", vip, n.ID(), ifName, err)
		return false
	}
	return true
}

// initNFTLBNAT declares the docker-lb-nat table's maps and chains.
func initNFTLBNAT(natInit *nftables.Modifier) error {
	natInit.Create(nftables.Map{
		Name:        natServiceVipMap,
		ElementType: nftables.Typeof("ip daddr").Concat(numgenExpr).MapTo("ip daddr"),
		Flags:       []string{"interval"},
		Counter:     true,
	})

	natInit.Create(nftables.Map{
		Name:        natPublishPortMap,
		ElementType: nftables.Typeof("meta l4proto . th dport").Concat(numgenExpr).MapTo("ip daddr"),
		Flags:       []string{"interval"},
		Counter:     true,
	})

	nftables.BaseChain{
		Name:      "prerouting",
		ChainType: nftables.BaseChainTypeNAT,
		Hook:      nftables.BaseChainHookPrerouting,
		Priority:  nftables.BaseChainPriorityDstNAT,
		Policy:    nftables.BaseChainPolicyAccept,
	}.Builder().
		Rule("dnat to ip daddr .", numgenExpr, "map @"+natServiceVipMap).
		Rule("dnat to meta l4proto . th dport .", numgenExpr, "map @"+natPublishPortMap).
		Create(natInit)

	nftables.BaseChain{
		Name:      "postrouting",
		ChainType: nftables.BaseChainTypeNAT,
		Hook:      nftables.BaseChainHookPostrouting,
		Priority:  nftables.BaseChainPrioritySrcNAT,
		Policy:    nftables.BaseChainPolicyAccept,
	}.Builder().
		Rule("ct status dnat counter masquerade").
		Create(natInit)
	return nil
}

// applyNFTLBDSR programs the netdev table that direct-routes overlay VIP traffic.
//
// Only overlay VIP traffic from other containers takes this path: the netdev
// chains are bound to the overlay interface and match on the VIP. Host-published
// ingress ports reach this sandbox DNATed to the gwbridge address and are
// load-balanced by natPublishPortMap, so they work with or without this table -
// a failure here is logged rather than reported, so it doesn't hold them up. The
// whole ruleset is rebuilt by every sync, so a transient failure heals on the
// next backend event.
func (n *Network) applyNFTLBDSR(ctx context.Context, sb *Sandbox, ifName string, wantVIPs, goneVIPs []string, vipBackends map[string][]net.IP) {
	var dsrTM nftables.Batch

	// As for the NAT maps, this is the only writer, so replace wholesale.
	dsrTM.Append(nftables.MapElementDeleteFunc{
		MapName: dsrRealServerMap,
		Fn:      func(nftables.MapElement) bool { return true },
	})
	// There is no delete-func for set elements, so the VIP set is maintained by
	// diff rather than replaced. A VIP left in it would keep the ingress chain
	// matching traffic that the real-server map can no longer resolve, which
	// falls through to `counter drop`.
	for _, vip := range goneVIPs {
		dsrTM.Append(nftables.Delete(nftables.SetElement{
			SetName:    dsrVipSet,
			Element:    vip,
			Idempotent: true,
		}))
	}
	for _, vip := range wantVIPs {
		dsrTM.Append(nftables.Create(nftables.SetElement{
			SetName:    dsrVipSet,
			Element:    vip,
			Idempotent: true,
		}))
		for iv, ip := range nftables.EqualWeightIntervals(vipBackends[vip], numgenModulus) {
			dsrTM.Append(nftables.Create(nftables.MapElement{
				MapName: dsrRealServerMap,
				Key:     fmt.Sprintf("%s . %s", vip, iv),
				Value:   ip.String(),
			}))
		}
	}

	// Say what the operator actually gets, not just that a table failed to apply.
	// Without the netdev egress hook the whole table is rejected, which is the safe
	// outcome: the ingress chain resolves established sessions from a conntrack map
	// that only the egress chain writes, so a table applied in parts would draw a new
	// backend per packet. Rejected outright, the service is left load-balanced by the
	// NAT path alone - correct, but not the direct routing that was asked for.
	//
	// There is deliberately no up-front capability check. DSR is an overlay network
	// option, so the request is cluster-wide while the kernel support is node-local:
	// there is no API call left to fail, and refusing the network here would take the
	// node out of service rather than degrade it. That leaves the daemon log, as it
	// does for the ingress ports - and a check would report once, which cannot tell
	// an ongoing degradation from one an operator has since resolved.
	if err := sb.osSbox.ApplyNFTable(ctx, nftables.Netdev, "docker-lb-dsr", func(dsrInit *nftables.Modifier) error {
		return initNFTLBDSR(dsrInit, ifName)
	}, dsrTM); err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{"network": n.ID()}).Error(
			"Failed to apply nftables dsr table; this network's overlay VIP traffic will be " +
				"load-balanced via NAT rather than direct server return (DSR needs the nftables " +
				"netdev egress hook, Linux 5.16 or later)")
	}
}

// initNFTLBDSR declares the docker-lb-dsr table's sets, maps and netdev chains.
func initNFTLBDSR(dsrInit *nftables.Modifier, ifName string) error {
	l4protos := []string{"tcp", "udp", "sctp"}

	dsrInit.Create(nftables.Set{
		Name:        dsrVipSet,
		ElementType: nftables.IPv4Addr,
		Counter:     true,
	})
	dsrInit.Create(nftables.Map{
		Name:        dsrRealServerMap,
		ElementType: nftables.Typeof("ip daddr").Concat(numgenExpr).MapTo("ip daddr"),
		Flags:       []string{"interval"},
		Counter:     true,
	})

	// Sticky overlay DSR sessions.
	// Split by L4 protocol as nftables 1.0.6 crashes when attempting to
	// compile a ruleset that contains a map update on a 5-tuple key.
	for _, l4proto := range l4protos {
		dsrInit.Create(nftables.Map{
			Name:        "dsr-conntrack-" + l4proto,
			ElementType: nftables.Typeof("ip saddr . th sport . ip daddr . th dport").MapTo("ether daddr"),
			Flags:       []string{"dynamic"},
			Size:        65536,
			Timeout:     60 * time.Second,
		})
	}

	b := nftables.BaseChain{
		Name:      "ingress",
		ChainType: nftables.BaseChainTypeFilter,
		Hook:      nftables.BaseChainHookIngress,
		Device:    ifName,
		Priority:  nftables.BaseChainPriorityFilter,
		Policy:    nftables.BaseChainPolicyAccept,
	}.Builder().
		Rule("meta protocol arp counter accept").
		Rule("ip daddr != @"+dsrVipSet, "counter accept").
		Rule("notrack ether saddr set ether daddr counter")

	for _, l4proto := range l4protos {
		// Established session: reuse MAC from conntrack map.
		b.Rule("ip protocol", l4proto,
			"ether daddr set ip saddr . th sport . ip daddr . th dport",
			"map @dsr-conntrack-"+l4proto, "counter fwd to", ifName)
	}
	b.
		// New session: random bucket lookup. The session is
		// persisted to the conntrack map in the egress chain.
		Rule("ip protocol {", strings.Join(l4protos, ", "), "}",
			"fwd ip to ip daddr .", numgenExpr, "map @"+dsrRealServerMap, "device", ifName).
		// The service is defined but the packet does not correspond to
		// an established session and no backends are available for a new
		// session.
		Rule("counter drop").
		Create(dsrInit)

	b = nftables.BaseChain{
		Name:      "egress",
		ChainType: nftables.BaseChainTypeFilter,
		Hook:      nftables.BaseChainHookEgress,
		Device:    ifName,
		Priority:  nftables.BaseChainPriorityFilter,
		Policy:    nftables.BaseChainPolicyAccept,
	}.Builder().
		Rule("meta protocol arp counter accept").
		Rule("ip daddr != @"+dsrVipSet, "counter accept").
		// We can confidently stop tracking a TCP session that has been reset.
		Rule("tcp flags rst update @dsr-conntrack-tcp",
			"{ ip saddr . th sport . ip daddr . th dport : ether daddr timeout 0s }",
			"counter accept")
	for _, l4proto := range l4protos {
		b.Rule("ip protocol", l4proto, "update @dsr-conntrack-"+l4proto,
			"{ ip saddr . th sport . ip daddr . th dport : ether daddr }",
			"counter accept")
	}
	b.Create(dsrInit)
	return nil
}
