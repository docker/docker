//go:build linux

package nftabler

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/nftables"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
)

func (n *network) AddPorts(ctx context.Context, pbs []types.PortBinding) error {
	return n.modPorts(ctx, pbs, true)
}

func (n *network) DelPorts(ctx context.Context, pbs []types.PortBinding) error {
	return n.modPorts(ctx, pbs, false)
}

func (n *network) modPorts(ctx context.Context, pbs []types.PortBinding, enable bool) error {
	if n.config.Internal {
		return nil
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(log.Fields{"bridge": n.config.IfName}))

	if enable && n.fw.cleaner != nil {
		n.fw.cleaner.DelPorts(ctx, n.config, pbs)
	}

	pbs4, pbs6 := splitByContainerFam(pbs)
	if n.fw.config.IPv4 && n.config.Config4.Prefix.IsValid() {
		if err := n.setPerPortRules(ctx, pbs4, n.fw.table4, nftables.IPv4, n.config.Config4.Unprotected, enable); err != nil {
			return err
		}
	}
	if n.fw.config.IPv6 && n.config.Config6.Prefix.IsValid() {
		if err := n.setPerPortRules(ctx, pbs6, n.fw.table6, nftables.IPv6, n.config.Config6.Unprotected, enable); err != nil {
			return err
		}
	}
	return nil
}

func splitByContainerFam(pbs []types.PortBinding) ([]types.PortBinding, []types.PortBinding) {
	var pbs4, pbs6 []types.PortBinding
	for _, pb := range pbs {
		if pb.IP.To4() != nil {
			pbs4 = append(pbs4, pb)
		} else {
			pbs6 = append(pbs6, pb)
		}
	}
	return pbs4, pbs6
}

func (n *network) setPerPortRules(ctx context.Context, pbs []types.PortBinding, table *nftables.Table, ipv nftables.Family, unprotected, enable bool) error {
	tm := nftables.Modifier{}
	updater := tm.Create
	if !enable {
		updater = tm.Delete
	}
	if !unprotected {
		n.setPerPortForwarding(pbs, updater, ipv)
	}
	// Also needed in nat-unprotected mode. That mode skips the per-port
	// forwarding rules above, and its blanket accept comes after the ICC rule,
	// so nothing else would accept hairpinned packets.
	n.setPerPortHairpinForwarding(pbs, updater, ipv)
	n.setPerPortDNAT(pbs, updater, ipv)
	n.setPerPortHairpinMasq(pbs, updater, ipv)
	n.filterPortMappedOnLoopback(pbs, updater, ipv)
	if err := table.Apply(ctx, tm); err != nil {
		return fmt.Errorf("adding rules for bridge %s: %w", n.config.IfName, err)
	}
	return nil
}

func (n *network) setPerPortForwarding(pbs []types.PortBinding, updater func(nftables.Obj), ipv nftables.Family) {
	chainName := chainFilterFwdIn(n.config.IfName)
	for _, pb := range pbs {
		// When more than one host port is mapped to a single container port, this will
		// generate the same rule for each host port. So, ignore duplicates when adding,
		// and missing rules when removing. (No ref-counting is currently needed because
		// when bindings are added or removed for an endpoint, they're all added or
		// removed for an address family. So, a rule that's added more than once will
		// also be deleted more than once.)
		//
		// TODO(robmry) - track port mappings, use that to edit nftables sets when bindings are added/removed.
		updater(nftables.Rule{
			Chain: chainName,
			Group: fwdInPortsRuleGroup,
			Rule: []string{
				string(ipv), "daddr", pb.IP.String(), pb.Proto.String(),
				"dport", strconv.Itoa(int(pb.Port)), "counter accept",
			},
			IgnoreExist: true,
		})
	}
}

// setPerPortHairpinForwarding opens a container's published port to the other
// containers on its own network, for packets that reached it via one of the
// host's addresses.
//
// When ICC is disabled, the ICC rule drops packets from other containers on
// this network. But a published port is meant to be reachable via the host's
// addresses from anywhere that can route to them, including the port's own
// network. So these rules come before the ICC rule, and only match packets
// arriving on the bridge. Packets for the port from anywhere else are accepted
// after the ICC rule.
//
// A rule also only matches packets that were DNATed, by requiring that the
// connection's original destination is not the container's own address.
// Otherwise it would allow direct access to the container's address, which is
// what disabling ICC prevents. By the time they get here, the two kinds of
// packet are otherwise the same: same input interface, destination and port.
func (n *network) setPerPortHairpinForwarding(pbs []types.PortBinding, updater func(nftables.Obj), ipv nftables.Family) {
	if n.config.ICC {
		return
	}
	chainName := chainFilterFwdIn(n.config.IfName)
	for _, pb := range pbs {
		if pb.HostPort == 0 {
			continue
		}
		updater(nftables.Rule{
			Chain: chainName,
			Group: fwdInHairpinPortsRuleGroup,
			Rule: []string{
				"iifname ==", n.config.IfName,
				string(ipv), "daddr", pb.IP.String(), pb.Proto.String(),
				"dport", strconv.Itoa(int(pb.Port)),
				"ct original", string(ipv), "daddr !=", pb.IP.String(),
				"counter accept",
			},
			IgnoreExist: true,
		})
	}
}

func (n *network) setPerPortDNAT(pbs []types.PortBinding, updater func(nftables.Obj), ipv nftables.Family) {
	var v6LLSkip string
	if ipv == nftables.IPv6 {
		v6LLSkip = "ip6 saddr != fe80::/10 "
	}
	for _, pb := range pbs {
		// Nothing to do if NAT is disabled.
		if pb.HostPort == 0 {
			continue
		}
		// If the binding is between containerV4 and hostV6, NAT isn't possible (the mapping
		// is handled by docker-proxy).
		if (pb.IP.To4() != nil) != (pb.HostIP.To4() != nil) {
			continue
		}
		var proxySkip string
		if !n.config.DNATFromOwnBridge(n.fw.config, pb) {
			proxySkip = fmt.Sprintf("iifname != %s ", n.config.IfName)
		}
		var daddrMatch string
		if !pb.HostIP.IsUnspecified() {
			daddrMatch = fmt.Sprintf("%s daddr %s ", ipv, pb.HostIP)
		}
		updater(nftables.Rule{
			Chain: natChain,
			Group: initialRuleGroup,
			Rule: []string{
				proxySkip, v6LLSkip, daddrMatch, pb.Proto.String(), "dport", strconv.Itoa(int(pb.HostPort)), "counter dnat to",
				net.JoinHostPort(pb.IP.String(), strconv.Itoa(int(pb.Port))), "comment DNAT",
			},
		})
	}
}

// setPerPortHairpinMasq allows containers to access their own published ports on the host
// when hairpin is enabled (no docker-proxy), by masquerading.
func (n *network) setPerPortHairpinMasq(pbs []types.PortBinding, updater func(nftables.Obj), ipv nftables.Family) {
	if !n.fw.config.Hairpin {
		return
	}
	chainName := chainNatPostRtIn(n.config.IfName)
	for _, pb := range pbs {
		// Nothing to do if NAT is disabled.
		if pb.HostPort == 0 {
			continue
		}
		// If the binding is between containerV4 and hostV6, NAT isn't possible (it's
		// handled by docker-proxy).
		if (pb.IP.To4() != nil) != (pb.HostIP.To4() != nil) {
			continue
		}
		// When more than one host port is mapped to a single container port, this will
		// generate the same rule for each host port. So, ignore duplicates when adding,
		// and missing rules when removing. (No ref-counting is currently needed because
		// when bindings are added or removed for an endpoint, they're all added or
		// removed. So, a rule that's added more than once will also be deleted more
		// than once.)
		//
		// TODO(robmry) - track port mappings, use that to edit nftables sets when bindings are added/removed.
		updater(nftables.Rule{
			Chain: chainName,
			Group: initialRuleGroup,
			Rule: []string{
				string(ipv), "saddr", pb.IP.String(), string(ipv),
				"daddr", pb.IP.String(), pb.Proto.String(),
				"dport", strconv.Itoa(int(pb.Port)),
				`counter masquerade comment "MASQ TO OWN PORT"`,
			},
			IgnoreExist: true,
		})
	}
}

// filterPortMappedOnLoopback adds a rule that drops remote connections to ports
// mapped to loopback addresses.
//
// This is a no-op if the portBinding is for IPv6 (IPv6 loopback address is
// non-routable), or over a network with gw_mode=routed (PBs in routed mode
// don't map ports on the host).
func (n *network) filterPortMappedOnLoopback(pbs []types.PortBinding, updater func(nftables.Obj), ipv nftables.Family) {
	if ipv == nftables.IPv6 {
		return
	}
	for _, pb := range pbs {
		// Nothing to do if not binding to the loopback address.
		if pb.HostPort == 0 || !pb.HostIP.IsLoopback() {
			continue
		}
		// Mappings from host IPv6 to container IPv4 are handled by docker-proxy.
		if pb.HostIP.To4() == nil {
			continue
		}
		if n.fw.config.WSL2Mirrored {
			updater(nftables.Rule{
				Chain: rawPreroutingChain,
				Group: rawPreroutingPortsRuleGroup,
				Rule: []string{
					"iifname loopback0 ip daddr", pb.HostIP.String(), pb.Proto.String(),
					"dport", strconv.Itoa(int(pb.HostPort)),
					`counter accept comment "ACCEPT WSL2 LOOPBACK"`,
				},
			})
		}
		updater(nftables.Rule{
			Chain: rawPreroutingChain,
			Group: rawPreroutingPortsRuleGroup,
			Rule: []string{
				`iifname != lo ip daddr`, pb.HostIP.String(), pb.Proto.String(),
				"dport", strconv.Itoa(int(pb.HostPort)),
				`counter drop comment "DROP REMOTE LOOPBACK"`,
			},
		})
	}
}
