//go:build linux

package iptabler

import (
	"context"
	"net/netip"

	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge/internal/firewaller"
	"github.com/moby/moby/v2/daemon/libnetwork/iptables"
)

func (n *network) AddEndpoint(ctx context.Context, epIPv4, epIPv6 netip.Addr) error {
	if n.ipt.config.IPv4 && epIPv4.IsValid() {
		if err := n.deleteLegacyDirectAccess(ctx, iptables.IPv4, n.config.Config4, epIPv4); err != nil {
			return err
		}
	}
	if n.ipt.config.IPv6 && epIPv6.IsValid() {
		if err := n.deleteLegacyDirectAccess(ctx, iptables.IPv6, n.config.Config6, epIPv6); err != nil {
			return err
		}
	}
	return nil
}

func (n *network) DelEndpoint(_ context.Context, _, _ netip.Addr) error {
	return nil
}

// deleteLegacyDirectAccess removes per-container raw PREROUTING rules that may
// have been created by an older daemon version. Current versions use a single
// subnet-level rule per network instead (see setSubnetProtection).
func (n *network) deleteLegacyDirectAccess(ctx context.Context, ipv iptables.IPVersion, config firewaller.NetworkConfigFam, epIP netip.Addr) error {
	if n.config.Internal || config.Unprotected || config.Routed {
		return nil
	}
	for _, ifName := range n.config.TrustedHostInterfaces {
		accept := iptables.Rule{IPVer: ipv, Table: iptables.Raw, Chain: "PREROUTING", Args: []string{
			"-d", epIP.String(),
			"-i", ifName,
			"-j", "ACCEPT",
		}}
		if err := appendOrDelChainRule(accept, "DIRECT ACCESS FILTERING - ACCEPT", false); err != nil {
			return err
		}
	}
	drop := iptables.Rule{IPVer: ipv, Table: iptables.Raw, Chain: "PREROUTING", Args: []string{
		"-d", epIP.String(),
		"!", "-i", n.config.IfName,
		"-j", "DROP",
	}}
	return appendOrDelChainRule(drop, "DIRECT ACCESS FILTERING - DROP", false)
}
