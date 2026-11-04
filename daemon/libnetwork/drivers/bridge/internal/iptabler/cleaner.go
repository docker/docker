//go:build linux

package iptabler

import (
	"context"
	"net/netip"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge/internal/firewaller"
	"github.com/moby/moby/v2/daemon/libnetwork/iptables"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
)

type iptablesCleaner struct {
	config firewaller.Config
}

// NewCleaner checks for iptables rules left behind by an old daemon that was using
// the Iptabler.
//
// If there are old rules present, it deletes as much as possible straight away
// (user-defined chains and jumps from the built-in chains).
//
// But, it can't delete network or port-specific rules from built-in chains
// without flushing those chains which would be very antisocial - because, at
// this stage, interface names, addresses, etc. are unknown. So, also return
// FirewallCleaner that the new Firewaller can use to delete those rules while
// it's setting up those networks/ports (probably on replay from persistent
// storage).
//
// If there are no old rules to clean up, return nil.
func NewCleaner(ctx context.Context, config firewaller.Config) firewaller.FirewallCleaner {
	clean := func(ipv iptables.IPVersion, enabled bool) bool {
		if !enabled {
			return false
		}
		t := iptables.GetIptable(ipv)
		// Since 28.0, the jump in the filter-FORWARD chain is DOCKER-FORWARD.
		// In earlier releases, there was a jump to DOCKER-ISOLATION-STAGE-1.
		if !t.Exists("filter", "FORWARD", "-j", dockerForwardChain) &&
			!t.Exists("filter", "FORWARD", "-j", isolationChain1) {
			return false
		}
		log.G(ctx).WithField("ipv", ipv).Info("Cleaning iptables")
		_ = t.DeleteJumpRule(iptables.Filter, "FORWARD", dockerForwardChain)
		_ = deleteLegacyTopLevelRules(ctx, t, ipv)
		removeIPChains(ctx, ipv)
		// The iptables chains will no longer have Docker's ACCEPT rules. So, if the
		// filter-FORWARD chain has policy DROP (possibly set by Docker when it enabled
		// IP forwarding), packets accepted by nftables chains will still be processed by
		// iptables and dropped. It's the user's responsibility to sort that out.
		if t.HasPolicy("filter", "FORWARD", iptables.Drop) {
			log.G(ctx).WithField("ipv", ipv).Warn("Network traffic for published ports may be dropped, iptables chain FORWARD has policy DROP.")
		}
		return true
	}
	cleaned4 := clean(iptables.IPv4, config.IPv4)
	cleaned6 := clean(iptables.IPv6, config.IPv6)
	if !cleaned4 && !cleaned6 {
		return nil
	}
	return &iptablesCleaner{config: config}
}

func (ic iptablesCleaner) DelNetwork(ctx context.Context, nc firewaller.NetworkConfig) {
	if nc.Internal {
		return
	}
	n := network{
		config: nc,
		ipt:    &Iptabler{config: ic.config},
	}
	if ic.config.IPv4 && nc.Config4.Prefix.IsValid() {
		_ = deleteLegacyFilterRules(iptables.IPv4, nc.IfName)
		_ = n.setupNonInternalNetworkRules(ctx, iptables.IPv4, nc.Config4, false)
		_ = n.setSubnetProtection(ctx, iptables.IPv4, nc.Config4, false)
	}
	if ic.config.IPv6 && nc.Config6.Prefix.IsValid() {
		_ = deleteLegacyFilterRules(iptables.IPv6, nc.IfName)
		_ = n.setupNonInternalNetworkRules(ctx, iptables.IPv6, nc.Config6, false)
		_ = n.setSubnetProtection(ctx, iptables.IPv6, nc.Config6, false)
	}
}

func (ic iptablesCleaner) DelEndpoint(ctx context.Context, nc firewaller.NetworkConfig, epIPv4, epIPv6 netip.Addr) {
	n := network{
		config: nc,
		ipt:    &Iptabler{config: ic.config},
	}
	if n.ipt.config.IPv4 && epIPv4.IsValid() {
		_ = n.deleteLegacyDirectAccess(ctx, iptables.IPv4, n.config.Config4, epIPv4)
	}
	if n.ipt.config.IPv6 && epIPv6.IsValid() {
		_ = n.deleteLegacyDirectAccess(ctx, iptables.IPv6, n.config.Config6, epIPv6)
	}
}

func (ic iptablesCleaner) DelPorts(ctx context.Context, nc firewaller.NetworkConfig, pbs []types.PortBinding) {
	n := network{
		config: nc,
		ipt:    &Iptabler{config: ic.config},
	}
	_ = n.DelPorts(ctx, pbs)
}
