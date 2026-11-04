//go:build linux

package iptabler

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge/internal/firewaller"
	"github.com/moby/moby/v2/daemon/libnetwork/iptables"
)

type (
	iptableCleanFunc   func() error
	iptablesCleanFuncs []iptableCleanFunc
)

type network struct {
	config     firewaller.NetworkConfig
	ipt        *Iptabler
	cleanFuncs iptablesCleanFuncs
}

func (ipt *Iptabler) NewNetwork(ctx context.Context, nc firewaller.NetworkConfig) (_ firewaller.Network, retErr error) {
	n := &network{
		ipt:    ipt,
		config: nc,
	}
	defer func() {
		if retErr != nil {
			if err := n.DelNetworkLevelRules(ctx); err != nil {
				log.G(ctx).WithError(err).Warnf("Failed to delete network level rules following earlier error")
			}
		}
	}()

	if err := n.ReapplyNetworkLevelRules(ctx); err != nil {
		return nil, err
	}
	return n, nil
}

func (n *network) ReapplyNetworkLevelRules(ctx context.Context) error {
	if n.ipt.config.IPv4 {
		if err := n.configure(ctx, iptables.IPv4, n.config.Config4); err != nil {
			return err
		}
	}
	if n.ipt.config.IPv6 {
		if err := n.configure(ctx, iptables.IPv6, n.config.Config6); err != nil {
			return err
		}
	}
	return nil
}

func (n *network) DelNetworkLevelRules(_ context.Context) error {
	var errs []error
	for _, cleanFunc := range n.cleanFuncs {
		if err := cleanFunc(); err != nil {
			errs = append(errs, err)
		}
	}
	n.cleanFuncs = nil
	return errors.Join(errs...)
}

func (n *network) configure(ctx context.Context, ipv iptables.IPVersion, conf firewaller.NetworkConfigFam) error {
	if !conf.Prefix.IsValid() {
		return nil
	}
	return n.setupIPTables(ctx, ipv, conf)
}

func (n *network) registerCleanFunc(clean iptableCleanFunc) {
	n.cleanFuncs = append(n.cleanFuncs, clean)
}

func (n *network) setupIPTables(ctx context.Context, ipVersion iptables.IPVersion, config firewaller.NetworkConfigFam) error {
	if n.config.Internal {
		if err := setupInternalNetworkRules(ctx, n.config.IfName, config.Prefix, n.config.ICC, true); err != nil {
			return fmt.Errorf("Failed to Setup IP tables: %w", err)
		}
		n.registerCleanFunc(func() error {
			return setupInternalNetworkRules(ctx, n.config.IfName, config.Prefix, n.config.ICC, false)
		})
	} else {
		if err := n.setupNonInternalNetworkRules(ctx, ipVersion, config, true); err != nil {
			return fmt.Errorf("Failed to Setup IP tables: %w", err)
		}
		n.registerCleanFunc(func() error {
			return n.setupNonInternalNetworkRules(ctx, ipVersion, config, false)
		})

		if err := n.setSubnetProtection(ctx, ipVersion, config, true); err != nil {
			return fmt.Errorf("Failed to setup subnet protection: %w", err)
		}
		n.registerCleanFunc(func() error {
			return n.setSubnetProtection(ctx, ipVersion, config, false)
		})

		if err := deleteLegacyFilterRules(ipVersion, n.config.IfName); err != nil {
			return fmt.Errorf("failed to delete legacy rules in filter-FORWARD: %w", err)
		}

		err := setDefaultForwardRule(ipVersion, n.config.IfName, config.Unprotected, true)
		if err != nil {
			return err
		}
		n.registerCleanFunc(func() error {
			return setDefaultForwardRule(ipVersion, n.config.IfName, config.Unprotected, false)
		})

		ctRule := iptables.Rule{IPVer: ipVersion, Table: iptables.Filter, Chain: dockerCTChain, Args: []string{
			"-o", n.config.IfName,
			"-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED",
			"-j", "ACCEPT",
		}}
		if err := appendOrDelChainRule(ctRule, "bridge ct related", true); err != nil {
			return err
		}
		n.registerCleanFunc(func() error {
			return appendOrDelChainRule(ctRule, "bridge ct related", false)
		})
		jumpToDockerRule := iptables.Rule{IPVer: ipVersion, Table: iptables.Filter, Chain: dockerBridgeChain, Args: []string{
			"-o", n.config.IfName,
			"-j", dockerChain,
		}}
		if err := appendOrDelChainRule(jumpToDockerRule, "jump to docker", true); err != nil {
			return err
		}
		n.registerCleanFunc(func() error {
			return appendOrDelChainRule(jumpToDockerRule, "jump to docker", false)
		})
	}
	return nil
}

func setICMP(ipv iptables.IPVersion, bridgeName string, enable bool) error {
	icmpProto := "icmp"
	if ipv == iptables.IPv6 {
		icmpProto = "icmpv6"
	}
	icmpRule := iptables.Rule{IPVer: ipv, Table: iptables.Filter, Chain: dockerChain, Args: []string{
		"-o", bridgeName,
		"-p", icmpProto,
		"-j", "ACCEPT",
	}}
	return appendOrDelChainRule(icmpRule, "ICMP", enable)
}

// setICMPv6ND accepts the IPv6 Neighbour Discovery messages that containers on
// the network need in order to address each other when ICC is disabled.
//
// Without them, a container can't resolve a peer's link-layer address, so it
// can't reply to a peer that reaches one of its published ports via a host
// address. IPv4 needs no equivalent, because ARP isn't filtered here.
//
// Only Neighbour Solicitation and Neighbour Advertisement are accepted. The
// other ICMPv6 messages that RFC 4890 section 4.4.1 says must not be dropped
// are exchanged between a host and its router. Here the router is the bridge
// itself, not another container, so those messages never reach the ICC rules.
// ICMPv6 errors for a permitted flow are already accepted as conntrack RELATED.
//
// The same section recommends checking that ND messages have a hop limit of
// 255, which a genuine on-link message always does. These rules don't check
// it. They only match packets bridged between two containers on this network,
// which is on-link by construction, and a container on the link can send a
// well-formed message with a hop limit of 255 as easily as a malformed one.
func setICMPv6ND(bridgeName string, enable bool) error {
	// Neighbour Solicitation and Neighbour Advertisement.
	for _, icmpType := range []string{"135", "136"} {
		rule := iptables.Rule{IPVer: iptables.IPv6, Table: iptables.Filter, Chain: dockerChain, Args: []string{
			"-i", bridgeName,
			"-o", bridgeName,
			"-p", "icmpv6",
			"--icmpv6-type", icmpType,
			"-j", "ACCEPT",
		}}
		if err := appendOrDelChainRule(rule, "ICMPv6 ND", enable); err != nil {
			return err
		}
	}
	return nil
}

func addNATJumpRules(ipVer iptables.IPVersion, hairpinMode, enable bool) error {
	preroute := iptables.Rule{IPVer: ipVer, Table: iptables.Nat, Chain: "PREROUTING", Args: []string{
		"-m", "addrtype",
		"--dst-type", "LOCAL",
		"-j", dockerChain,
	}}
	if enable {
		if err := preroute.Append(); err != nil {
			return fmt.Errorf("failed to append jump rules to nat-PREROUTING: %s", err)
		}
	} else {
		if err := preroute.Delete(); err != nil {
			return fmt.Errorf("failed to remove jump rules from nat-PREROUTING: %s", err)
		}
	}

	output := iptables.Rule{IPVer: ipVer, Table: iptables.Nat, Chain: "OUTPUT", Args: []string{
		"-m", "addrtype",
		"--dst-type", "LOCAL",
		"-j", dockerChain,
	}}
	if !hairpinMode {
		output.Args = append(output.Args, "!", "--dst", loopbackAddress(ipVer))
	}
	if enable {
		if err := output.Append(); err != nil {
			return fmt.Errorf("failed to append jump rules to nat-OUTPUT: %s", err)
		}
	} else {
		if err := output.Delete(); err != nil {
			return fmt.Errorf("failed to remove jump rules from nat-OUTPUT: %s", err)
		}
	}

	return nil
}

// deleteLegacyFilterRules removes the legacy per-bridge rules from the filter-FORWARD
// chain. This is required for users upgrading the Engine to v28.0.
// TODO(aker): drop this function once Mirantis latest LTS is v28.0 (or higher).
func deleteLegacyFilterRules(ipVer iptables.IPVersion, bridgeName string) error {
	iptable := iptables.GetIptable(ipVer)
	// Delete legacy per-bridge jump to the DOCKER chain from the FORWARD chain, if it exists.
	// These rules have been replaced by an ipset-matching rule.
	link := []string{
		"-o", bridgeName,
		"-j", dockerChain,
	}
	if iptable.Exists(iptables.Filter, "FORWARD", link...) {
		del := append([]string{string(iptables.Delete), "FORWARD"}, link...)
		if output, err := iptable.Raw(del...); err != nil {
			return err
		} else if len(output) != 0 {
			return fmt.Errorf("could not delete linking rule from %s-%s: %s", iptables.Filter, dockerChain, output)
		}
	}

	// Delete legacy per-bridge related/established rule if it exists. These rules
	// have been replaced by an ipset-matching rule.
	establish := []string{
		"-o", bridgeName,
		"-m", "conntrack",
		"--ctstate", "RELATED,ESTABLISHED",
		"-j", "ACCEPT",
	}
	if iptable.Exists(iptables.Filter, "FORWARD", establish...) {
		del := append([]string{string(iptables.Delete), "FORWARD"}, establish...)
		if output, err := iptable.Raw(del...); err != nil {
			return err
		} else if len(output) != 0 {
			return fmt.Errorf("could not delete establish rule from %s-%s: %s", iptables.Filter, dockerChain, output)
		}
	}

	return nil
}

// loopbackAddress returns the loopback address for the given IP version.
func loopbackAddress(version iptables.IPVersion) string {
	switch version {
	case iptables.IPv4, "":
		// IPv4 (default for backward-compatibility)
		return "127.0.0.0/8"
	case iptables.IPv6:
		return "::1/128"
	default:
		panic("unknown IP version: " + version)
	}
}

func setDefaultForwardRule(ipVersion iptables.IPVersion, ifName string, unprotected bool, enable bool) error {
	// Normally, DROP anything that hasn't been ACCEPTed by a per-port/protocol
	// rule. This prevents direct access to un-mapped ports from remote hosts
	// that can route directly to the container's address (by setting up a
	// route via the host's address).
	action := "DROP"
	if unprotected {
		// If the user really wants to allow all access from the wider network,
		// explicitly ACCEPT anything so that the filter-FORWARD chain's
		// default policy can't interfere.
		action = "ACCEPT"
	}

	rule := iptables.Rule{IPVer: ipVersion, Table: iptables.Filter, Chain: dockerChain, Args: []string{
		"!", "-i", ifName,
		"-o", ifName,
		"-j", action,
	}}

	// Append to the filter table's DOCKER chain (the default rule must follow
	// per-port ACCEPT rules, which will be inserted at the top of the chain).
	if err := appendOrDelChainRule(rule, "DEFAULT FWD", enable); err != nil {
		return fmt.Errorf("failed to add default-drop rule: %w", err)
	}
	return nil
}

func (n *network) setupNonInternalNetworkRules(ctx context.Context, ipVer iptables.IPVersion, config firewaller.NetworkConfigFam, enable bool) error {
	if n.config.AcceptFwMark != "" {
		fwm, err := iptablesFwMark(n.config.AcceptFwMark)
		if err != nil {
			return err
		}
		if err := programChainRule(iptables.Rule{IPVer: ipVer, Table: iptables.Filter, Chain: dockerForwardChain, Args: []string{
			"-m", "mark", "--mark", fwm, "-j", "ACCEPT",
		}}, "ALLOW FW MARK", enable); err != nil {
			return err
		}
	}

	var natArgs, hpNatArgs []string
	if config.HostIP.IsValid() {
		// The user wants IPv4/IPv6 SNAT with the given address.
		hostAddr := config.HostIP.String()
		natArgs = []string{"-s", config.Prefix.String(), "!", "-o", n.config.IfName, "-j", "SNAT", "--to-source", hostAddr}
		hpNatArgs = []string{"-m", "addrtype", "--src-type", "LOCAL", "-o", n.config.IfName, "-j", "SNAT", "--to-source", hostAddr}
	} else {
		// Use MASQUERADE, which picks the src-ip based on next-hop from the route table
		natArgs = []string{"-s", config.Prefix.String(), "!", "-o", n.config.IfName, "-j", "MASQUERADE"}
		hpNatArgs = []string{"-m", "addrtype", "--src-type", "LOCAL", "-o", n.config.IfName, "-j", "MASQUERADE"}
	}
	natRule := iptables.Rule{IPVer: ipVer, Table: iptables.Nat, Chain: "POSTROUTING", Args: natArgs}
	hpNatRule := iptables.Rule{IPVer: ipVer, Table: iptables.Nat, Chain: "POSTROUTING", Args: hpNatArgs}

	// Set NAT.
	nat := !config.Routed
	if n.config.Masquerade {
		if nat {
			if err := programChainRule(natRule, "NAT", enable); err != nil {
				return err
			}
		}
	}

	// In hairpin mode, masquerade traffic from localhost. If hairpin is disabled or if we're tearing down
	// that bridge, make sure the iptables rule isn't lying around.
	if err := programChainRule(hpNatRule, "MASQ LOCAL HOST", enable && n.ipt.config.Hairpin); err != nil {
		return err
	}

	// Set Inter Container Communication.
	if err := setIcc(ctx, ipVer, n.config.IfName, n.config.ICC, false, enable); err != nil {
		return err
	}

	// Allow ICMP in routed mode.
	if !nat {
		if err := setICMP(ipVer, n.config.IfName, enable); err != nil {
			return err
		}
	}

	// Accept Neighbour Discovery between containers, which the ICC rules would
	// otherwise drop. In routed mode, the rule above already accepts all ICMPv6.
	if ipVer == iptables.IPv6 && nat {
		if err := setICMPv6ND(n.config.IfName, enable && !n.config.ICC); err != nil {
			return err
		}
	}

	// Handle outgoing packets. This rule was previously added unconditionally
	// to ACCEPT packets that weren't ICC - an extra rule was needed to enable
	// ICC if needed. Those rules are now combined. So, outRuleNoICC is only
	// needed for ICC=false, along with the DROP rule for ICC added by setIcc.
	outRuleNoICC := iptables.Rule{IPVer: ipVer, Table: iptables.Filter, Chain: dockerForwardChain, Args: []string{
		"-i", n.config.IfName,
		"!", "-o", n.config.IfName,
		"-j", "ACCEPT",
	}}
	// If there's a version of outRuleNoICC in the FORWARD chain, created by moby 28.0.0 or older, delete it.
	if enable {
		if err := outRuleNoICC.WithChain("FORWARD").Delete(); err != nil {
			return fmt.Errorf("deleting FORWARD chain outRuleNoICC: %w", err)
		}
	}
	if n.config.ICC {
		// Accept outgoing traffic to anywhere, including other containers on this bridge.
		outRuleICC := iptables.Rule{IPVer: ipVer, Table: iptables.Filter, Chain: dockerForwardChain, Args: []string{
			"-i", n.config.IfName,
			"-j", "ACCEPT",
		}}
		if err := appendOrDelChainRule(outRuleICC, "ACCEPT OUTGOING", enable); err != nil {
			return err
		}
		// If there's a version of outRuleICC in the FORWARD chain, created by moby 28.0.0 or older, delete it.
		if enable {
			if err := outRuleICC.WithChain("FORWARD").Delete(); err != nil {
				return fmt.Errorf("deleting FORWARD chain outRuleICC: %w", err)
			}
		}
	} else {
		// Accept outgoing traffic to anywhere, apart from other containers on this bridge.
		// setIcc added a DROP rule for ICC traffic.
		if err := appendOrDelChainRule(outRuleNoICC, "ACCEPT NON_ICC OUTGOING", enable); err != nil {
			return err
		}
	}

	return nil
}

// setSubnetProtection drops packets addressed directly to any IP in the bridge
// subnet from interfaces other than the bridge or loopback. This prevents external
// hosts that have a route to the bridge subnet from accessing containers on
// unpublished ports, or services on the host bound to the gateway address.
//
// It is a no-op if:
//   - gateway mode is "nat-unprotected" or "routed" (direct access is intentional).
//   - direct routing is enabled at the daemon level.
//   - "raw" rules are disabled.
func (n *network) setSubnetProtection(ctx context.Context, ipv iptables.IPVersion, config firewaller.NetworkConfigFam, enable bool) error {
	if config.Unprotected || config.Routed || !config.Prefix.IsValid() {
		return nil
	}
	if n.ipt.config.AllowDirectRouting || rawRulesDisabled(ctx) {
		enable = false
	}
	subnet := config.Prefix.String()
	// Accept loopback traffic to the subnet. This is needed for host-local access
	// to the gateway IP, which is assigned to the bridge and reachable via lo.
	loAccept := iptables.Rule{IPVer: ipv, Table: iptables.Raw, Chain: "PREROUTING", Args: []string{
		"-d", subnet, "-i", "lo", "-j", "ACCEPT",
	}}
	if err := appendOrDelChainRule(loAccept, "SUBNET PROTECTION - ACCEPT LO", enable); err != nil {
		return err
	}
	// Accept traffic from trusted interfaces (e.g. flannel.1 in VXLAN setups).
	for _, ifName := range n.config.TrustedHostInterfaces {
		accept := iptables.Rule{IPVer: ipv, Table: iptables.Raw, Chain: "PREROUTING", Args: []string{
			"-d", subnet, "-i", ifName, "-j", "ACCEPT",
		}}
		if err := appendOrDelChainRule(accept, "SUBNET PROTECTION - ACCEPT TRUSTED", enable); err != nil {
			return err
		}
	}
	// Drop traffic from any other non-bridge interface to the subnet.
	extDrop := iptables.Rule{IPVer: ipv, Table: iptables.Raw, Chain: "PREROUTING", Args: []string{
		"-d", subnet, "!", "-i", n.config.IfName, "-j", "DROP",
	}}
	return appendOrDelChainRule(extDrop, "SUBNET PROTECTION - DROP", enable)
}

func setIcc(ctx context.Context, version iptables.IPVersion, bridgeIface string, iccEnable, internal, insert bool) error {
	args := []string{"-i", bridgeIface, "-o", bridgeIface, "-j"}
	acceptRule := iptables.Rule{IPVer: version, Table: iptables.Filter, Chain: dockerForwardChain, Args: append(args, "ACCEPT")}
	dropRule := iptables.Rule{IPVer: version, Table: iptables.Filter, Chain: dockerForwardChain, Args: append(args, "DROP")}

	// The accept rule is no longer required for a bridge with external connectivity, because
	// ICC traffic is allowed by the outgoing-packets rule created by setupIptablesInternal.
	// The accept rule is still required for a --internal network because it has no outgoing
	// rule. If insert and the rule is not required, an ACCEPT rule for an external network
	// may have been left behind by an older version of the daemon so, delete it.
	if insert && iccEnable && internal {
		if err := acceptRule.Append(); err != nil {
			return fmt.Errorf("Unable to allow intercontainer communication: %w", err)
		}
	} else {
		if err := acceptRule.Delete(); err != nil {
			log.G(ctx).WithError(err).Warn("Failed to delete legacy ICC accept rule")
		}
	}

	if insert && !iccEnable {
		if err := dropRule.Append(); err != nil {
			return fmt.Errorf("Unable to prevent intercontainer communication: %w", err)
		}
	} else {
		if err := dropRule.Delete(); err != nil {
			log.G(ctx).WithError(err).Warn("Failed to delete ICC drop rule")
		}
	}

	// Delete rules that may have been inserted into the FORWARD chain by moby 28.0.0 or older.
	if insert {
		if err := acceptRule.WithChain("FORWARD").Delete(); err != nil {
			return fmt.Errorf("deleting FORWARD chain accept rule: %w", err)
		}
		if err := dropRule.WithChain("FORWARD").Delete(); err != nil {
			return fmt.Errorf("deleting FORWARD chain drop rule: %w", err)
		}
	}
	return nil
}

// Obsolete chains from previous docker versions
const (
	oldIsolationChain = "DOCKER-ISOLATION"
	oldIngressChain   = "DOCKER-INGRESS"
)

func removeIPChains(ctx context.Context, version iptables.IPVersion) {
	ipt := iptables.GetIptable(version)

	// Remove obsolete rules from default chains
	ipt.ProgramRule(iptables.Filter, "FORWARD", iptables.Delete, []string{"-j", oldIsolationChain})
	ipt.ProgramRule(iptables.Filter, "FORWARD", iptables.Delete, []string{"-j", oldIngressChain})

	// Remove chains
	for _, chainInfo := range []iptables.ChainInfo{
		{Name: dockerChain, Table: iptables.Nat, IPVersion: version},
		{Name: dockerForwardChain, Table: iptables.Filter, IPVersion: version},
		{Name: dockerBridgeChain, Table: iptables.Filter, IPVersion: version},
		{Name: dockerChain, Table: iptables.Filter, IPVersion: version},
		{Name: dockerCTChain, Table: iptables.Filter, IPVersion: version},
		{Name: dockerInternalChain, Table: iptables.Filter, IPVersion: version},
		{Name: isolationChain1, Table: iptables.Filter, IPVersion: version},
		{Name: isolationChain2, Table: iptables.Filter, IPVersion: version},
		{Name: oldIsolationChain, Table: iptables.Filter, IPVersion: version},
		{Name: oldIngressChain, Table: iptables.Filter, IPVersion: version},
		{Name: oldIngressChain, Table: iptables.Nat, IPVersion: version},
	} {
		if err := chainInfo.Remove(); err != nil {
			log.G(ctx).Warnf("Failed to remove existing iptables entries in table %s chain %s : %v", chainInfo.Table, chainInfo.Name, err)
		}
	}
}

func setupInternalNetworkRules(ctx context.Context, bridgeIface string, prefix netip.Prefix, icc, insert bool) error {
	var version iptables.IPVersion
	var inDropRule, outDropRule iptables.Rule

	if prefix.Addr().Is4() {
		version = iptables.IPv4
		inDropRule = iptables.Rule{
			IPVer: version,
			Table: iptables.Filter,
			Chain: dockerInternalChain,
			Args:  []string{"-i", bridgeIface, "!", "-d", prefix.String(), "-j", "DROP"},
		}
		outDropRule = iptables.Rule{
			IPVer: version,
			Table: iptables.Filter,
			Chain: dockerInternalChain,
			Args:  []string{"-o", bridgeIface, "!", "-s", prefix.String(), "-j", "DROP"},
		}
	} else {
		version = iptables.IPv6
		inDropRule = iptables.Rule{
			IPVer: version,
			Table: iptables.Filter,
			Chain: dockerInternalChain,
			Args:  []string{"-i", bridgeIface, "!", "-o", bridgeIface, "!", "-d", prefix.String(), "-j", "DROP"},
		}
		outDropRule = iptables.Rule{
			IPVer: version,
			Table: iptables.Filter,
			Chain: dockerInternalChain,
			Args:  []string{"!", "-i", bridgeIface, "-o", bridgeIface, "!", "-s", prefix.String(), "-j", "DROP"},
		}
	}

	if err := programChainRule(inDropRule, "DROP INCOMING", insert); err != nil {
		return err
	}
	if err := programChainRule(outDropRule, "DROP OUTGOING", insert); err != nil {
		return err
	}

	// Set Inter Container Communication.
	return setIcc(ctx, version, bridgeIface, icc, true, insert)
}

// iptablesFwMark takes a string representing a firewall mark with an optional
// "/mask" parses the mark and mask, and returns the same "mark/mask" with the
// numbers converted to decimal, because strings.ParseUint accepts more integer
// formats than iptables.
func iptablesFwMark(val string) (string, error) {
	markStr, maskStr, haveMask := strings.Cut(val, "/")
	mark, err := strconv.ParseUint(markStr, 0, 32)
	if err != nil {
		return "", fmt.Errorf("invalid firewall mark %q: %w", val, err)
	}
	if haveMask {
		mask, err := strconv.ParseUint(maskStr, 0, 32)
		if err != nil {
			return "", fmt.Errorf("invalid firewall mask %q: %w", val, err)
		}
		return fmt.Sprintf("%d/%d", mark, mask), nil
	}
	return strconv.FormatUint(mark, 10), nil
}
