package libnetwork

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/moby/moby/v2/daemon/libnetwork/internal/nftables"
)

func (sb *Sandbox) addRedirectRulesNFTables(ctx context.Context, eIP *net.IPNet, ingressPorts []*PortConfig) error {
	if sb.osSbox == nil {
		return nil
	}

	// The iptables implementation skips programming the rules to drop
	// packets addressed to eIP on unpublished ports when the list of
	// ingress ports is empty. It's a very strange behavior, but it's been
	// like that for a decade, so we are replicating it here for
	// consistency.
	if len(ingressPorts) == 0 {
		return nil
	}

	const (
		publishedPortsMap = "published-ports"
		ingressIPsSet     = "ingress-ips"
	)
	var tm nftables.Modifier
	ingressIP := eIP.IP.String()
	tm.Create(nftables.SetElement{
		SetName: ingressIPsSet,
		Element: ingressIP,
	})
	for _, p := range ingressPorts {
		// No comment: PortConfig.Name is an optional, unvalidated string straight
		// from the service spec, and validateComment rejects quotes and newlines.
		// A rejected element fails the whole table, which would leave the task with
		// neither its port redirect nor the input/output filtering below - open
		// rather than closed. The key already identifies the element completely,
		// and addRedirectRulesIPTables ignores Name too.
		tm.Create(nftables.MapElement{
			MapName: publishedPortsMap,
			Key:     fmt.Sprintf("%s . %s . %d", ingressIP, strings.ToLower(p.Protocol.String()), p.PublishedPort),
			Value:   strconv.FormatUint(uint64(p.TargetPort), 10),
		})
	}

	err := sb.osSbox.ApplyNFTable(ctx, nftables.IPv4, "docker-container-ingress", func(initIngress *nftables.Modifier) error {
		// Map of ingress-IP . publishPort -> targetPort.
		// Packets with a destination address of ingress-IP and a
		// destination port in this map are redirected to the target
		// port.
		initIngress.Create(nftables.Map{
			Name:        publishedPortsMap,
			ElementType: nftables.IPv4Addr.Concat(nftables.InetProto).Concat(nftables.InetService).MapTo(nftables.InetService),
		})
		initIngress.Create(nftables.Set{
			Name:        ingressIPsSet,
			ElementType: nftables.IPv4Addr,
		})

		nftables.BaseChain{
			Name:      "prerouting",
			ChainType: nftables.BaseChainTypeNAT,
			Hook:      nftables.BaseChainHookPrerouting,
			Priority:  nftables.BaseChainPriorityDstNAT,
			Policy:    nftables.BaseChainPolicyAccept,
		}.Builder().
			Rule("meta l4proto { tcp, udp, sctp } redirect to ip daddr . meta l4proto . th dport map @" + publishedPortsMap).
			Create(initIngress)

		nftables.BaseChain{
			Name:      "input",
			ChainType: nftables.BaseChainTypeFilter,
			Hook:      nftables.BaseChainHookInput,
			Priority:  nftables.BaseChainPriorityFilter,
			Policy:    nftables.BaseChainPolicyAccept,
		}.Builder().
			Rule("ip daddr != @"+ingressIPsSet, "counter accept").
			Rule("icmp type { destination-unreachable, time-exceeded } counter accept").
			// Only allow incoming connections to exposed ports
			// from the ingress network.
			Rule("ct state { established, related } counter accept").
			Rule("ct status dnat counter accept").
			Rule("counter reject").
			Create(initIngress)

		nftables.BaseChain{
			Name:      "output",
			ChainType: nftables.BaseChainTypeFilter,
			Hook:      nftables.BaseChainHookOutput,
			Priority:  nftables.BaseChainPriorityFilter,
			Policy:    nftables.BaseChainPolicyAccept,
		}.Builder().
			Rule("ip saddr != @"+ingressIPsSet, "counter accept").
			Rule("icmp type { destination-unreachable, time-exceeded } counter accept").
			// Only allow outgoing replies for incoming connections
			// to be transmitted over the ingress network.
			Rule("ct status dnat ct state { established, related } counter accept").
			Rule("counter reject").
			Create(initIngress)

		return nil
	}, tm)
	if err != nil {
		return fmt.Errorf("failed to add redirect rules to nftables ingress table: %v", err)
	}
	return nil
}
