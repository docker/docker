package libnetwork

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"

	"github.com/containerd/log"
	"github.com/docker/go-events"
	"github.com/moby/moby/v2/daemon/libnetwork/iptables"
	"github.com/moby/moby/v2/daemon/libnetwork/networkdb"
	"github.com/moby/moby/v2/daemon/libnetwork/nlwrap"
	"github.com/moby/moby/v2/daemon/libnetwork/osl"
	"github.com/vishvananda/netlink"
)

const (
	ingressReturnChain    = "DOCKER-INGRESS-SOURCE-IP"
	ingressReplyChain     = "DOCKER-INGRESS-REPLY"
	ingressSourceTunnel   = "ingress-source"
	ingressSourceAlias    = "lo:ingress-src"
	ingressReturnMask     = uint32(0xffff0000)
	ingressReturnTables   = uint32(0x444b0000)
	ingressReturnPriority = 20000
)

// IPVS tunnels the packet to the task with the client's source address and
// the entry gateway's destination address intact. Distinct gateway VIPs keep
// connections to different public nodes distinct at the task's TCP stack.
// The outer IP source identifies the entry gateway. Tasks retain that identity
// in the upper 16 connmark bits and route replies through that same gateway.
//
// The gateway's offset in the ingress subnet supplies a stable identifier. This
// is deliberately restricted to one IPv4 subnet no larger than /16. It does not
// encode identifiers in DSCP/ECN, or mark unrelated outbound connections.
type ingressGatewayRecord struct {
	Address string `json:"address"`
	MAC     string `json:"mac"`
}

type ingressSourceGateway struct {
	address net.IP
	mac     net.HardwareAddr
	id      uint32
}

func (n *Network) parseIngressGateway(data []byte) (ingressSourceGateway, error) {
	var record ingressGatewayRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return ingressSourceGateway{}, err
	}
	ip := net.ParseIP(record.Address).To4()
	mac, err := net.ParseMAC(record.MAC)
	if ip == nil || err != nil || len(mac) != 6 || mac[0]&1 != 0 {
		return ingressSourceGateway{}, errors.New("invalid ingress gateway address or MAC")
	}
	pool, err := n.ingressSourceIPPool()
	if err != nil {
		return ingressSourceGateway{}, err
	}
	if !pool.Contains(ip) {
		return ingressSourceGateway{}, fmt.Errorf("ingress gateway %s is outside subnet %s", ip, pool)
	}
	id := binary.BigEndian.Uint32(ip) - binary.BigEndian.Uint32(pool.IP.Mask(pool.Mask).To4())
	if id == 0 || id > 0xffff {
		return ingressSourceGateway{}, fmt.Errorf("invalid ingress gateway identifier for %s", ip)
	}
	return ingressSourceGateway{address: ip, mac: mac, id: id}, nil
}

func (n *Network) ingressSourceGateways() ([]ingressSourceGateway, error) {
	agent, ok := n.clusterAgent()
	if !ok {
		return nil, errors.New("ingress gateway discovery is not available")
	}
	records := agent.networkDB.GetTableByNetwork(ingressGatewayTable, n.ID())
	gateways := make([]ingressSourceGateway, 0, len(records))
	seen := make(map[uint32]struct{}, len(records))
	for _, record := range records {
		gateway, err := n.parseIngressGateway(record.Value)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[gateway.id]; exists {
			return nil, fmt.Errorf("duplicate ingress gateway address %s", gateway.address)
		}
		seen[gateway.id] = struct{}{}
		gateways = append(gateways, gateway)
	}
	slices.SortFunc(gateways, func(a, b ingressSourceGateway) int {
		return int(a.id) - int(b.id)
	})
	return gateways, nil
}

func (ep *Endpoint) addIngressGatewayInfoToCluster() error {
	n := ep.getNetwork()
	if n.loadBalancerMode != loadBalancerModeSourceIP || !ep.loadBalancer {
		return nil
	}
	agent, ok := n.clusterAgent()
	if !ok {
		return errors.New("ingress gateway discovery is not available")
	}
	ep.mu.Lock()
	record := ingressGatewayRecord{Address: ep.iface.addr.IP.String(), MAC: ep.iface.mac.String()}
	ep.mu.Unlock()
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if _, err := n.parseIngressGateway(data); err != nil {
		return err
	}
	if old, err := agent.networkDB.GetEntry(ingressGatewayTable, n.ID(), ep.ID()); err == nil {
		if !bytes.Equal(old, data) {
			if err := agent.networkDB.UpdateEntry(ingressGatewayTable, n.ID(), ep.ID(), data); err != nil {
				return err
			}
		}
	} else if err := agent.networkDB.CreateEntry(ingressGatewayTable, n.ID(), ep.ID(), data); err != nil {
		return err
	}
	// Local mutations do not notify NetworkDB watchers.
	n.refreshIngressSourceIPRouting()
	return nil
}

func (ep *Endpoint) deleteIngressGatewayInfoFromCluster() error {
	n := ep.getNetwork()
	if n.loadBalancerMode != loadBalancerModeSourceIP || !ep.loadBalancer {
		return nil
	}
	agent, ok := n.clusterAgent()
	if !ok {
		return nil
	}
	if _, err := agent.networkDB.GetEntry(ingressGatewayTable, n.ID(), ep.ID()); err != nil {
		return nil
	}
	if err := agent.networkDB.DeleteEntry(ingressGatewayTable, n.ID(), ep.ID()); err != nil {
		return err
	}
	n.refreshIngressSourceIPRouting()
	return nil
}

func (c *Controller) handleIngressGatewayTableEvent(ev events.Event) {
	event := ev.(networkdb.WatchEvent)
	n, err := c.NetworkByID(event.NetworkID)
	if err != nil || n.loadBalancerMode != loadBalancerModeSourceIP {
		return
	}
	n.refreshIngressSourceIPRouting()
}

func (n *Network) refreshIngressSourceIPRouting() {
	for _, ep := range n.Endpoints() {
		if ep.loadBalancer {
			continue
		}
		sb, ok := ep.getSandbox()
		if !ok {
			continue
		}
		// Serialize reconciliation against task joins/leaves. Use the endpoint
		// from the sandbox, whose interface names are current.
		sb.joinLeaveMu.Lock()
		if current := sb.GetEndpoint(ep.ID()); current != nil {
			if err := sb.configureIngressSourceIPRouting(current); err != nil {
				log.G(context.TODO()).WithError(err).WithField("endpoint", ep.ID()).Error("Failed to refresh ingress return routing")
			}
		}
		sb.joinLeaveMu.Unlock()
	}
}

func (n *Network) restoreIngressSourceIPRouting() {
	if n.loadBalancerMode != loadBalancerModeSourceIP {
		return
	}
	for _, ep := range n.Endpoints() {
		if ep.loadBalancer {
			if err := ep.addIngressGatewayInfoToCluster(); err != nil {
				log.G(context.TODO()).WithError(err).Error("Failed to restore ingress gateway information")
			}
		}
	}
	n.refreshIngressSourceIPRouting()
}

func ingressReturnNextHop(id uint32) net.IP {
	return net.IPv4(169, 254, byte(id>>8), byte(id))
}

func ingressReturnHooks(iface string, address net.IP, pool *net.IPNet) map[string][]string {
	return map[string][]string{
		"mangle/INPUT": {"-i", iface, "-d", address.String(), "-p", "4", "-j", ingressReturnChain},
		"mangle/PREROUTING": {
			"-i", ingressSourceTunnel, "-m", "conntrack", "--ctdir", "ORIGINAL", "--ctstate", "NEW",
			"-m", "mark", "!", "--mark", "0/0xffff0000", "-j", "CONNMARK",
			"--save-mark", "--nfmask", "0xffff0000", "--ctmask", "0xffff0000",
		},
		"mangle/OUTPUT": {
			"-s", pool.String(), "-m", "conntrack", "--ctdir", "REPLY",
			"-m", "connmark", "!", "--mark", "0/0xffff0000", "-j", ingressReplyChain,
		},
		"nat/PREROUTING": {"-i", ingressSourceTunnel, "-j", ingressReturnChain},
		"filter/INPUT":   {"-i", ingressSourceTunnel, "-j", ingressReturnChain},
	}
}

func ingressReturnRule(pool *net.IPNet, id uint32) *netlink.Rule {
	rule := netlink.NewRule()
	rule.Family = netlink.FAMILY_V4
	rule.Priority = ingressReturnPriority
	rule.Table = int(ingressReturnTables | id)
	rule.Mark = id << 16
	rule.Mask = new(uint32)
	*rule.Mask = ingressReturnMask
	rule.Src = pool
	return rule
}

func ownedIngressReturnRule(rule netlink.Rule, pool *net.IPNet) bool {
	return rule.Priority == ingressReturnPriority && rule.Mask != nil && *rule.Mask == ingressReturnMask &&
		uint32(rule.Table)&ingressReturnMask == ingressReturnTables && rule.Mark != 0 &&
		rule.Mark == (uint32(rule.Table)&0xffff)<<16 && rule.Src != nil &&
		rule.Src.IP.Equal(pool.IP) && bytes.Equal(rule.Src.Mask, pool.Mask)
}

func removeIngressReturnRoute(handle nlwrap.Handle, rule netlink.Rule, linkIndex int) error {
	var errs []error
	if err := handle.RuleDel(&rule); err != nil && !errors.Is(err, syscall.ENOENT) {
		errs = append(errs, err)
	}
	routes, err := handle.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: rule.Table}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, route := range routes {
		if err := handle.RouteDel(&route); err != nil && !errors.Is(err, syscall.ESRCH) {
			errs = append(errs, err)
		}
	}
	neighbor := &netlink.Neigh{LinkIndex: linkIndex, Family: netlink.FAMILY_V4, IP: ingressReturnNextHop(uint32(rule.Table) & 0xffff)}
	if err := handle.NeighDel(neighbor); err != nil && !errors.Is(err, syscall.ENOENT) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func ingressGatewayRules(address net.IP, vip net.IP, gateways []ingressSourceGateway, ports []*PortConfig, mtu int) (string, error) {
	if mtu < 576 {
		return "", errors.New("ingress MTU must be at least 576")
	}
	for _, port := range ports {
		if port == nil || port.PublishedPort == 0 || port.PublishedPort > 65535 || port.TargetPort == 0 || port.TargetPort > 65535 {
			return "", errors.New("invalid ingress port")
		}
		switch port.Protocol {
		case ProtocolTCP, ProtocolUDP, ProtocolSCTP:
		default:
			return "", fmt.Errorf("invalid ingress port protocol %d", port.Protocol)
		}
	}
	var rules strings.Builder
	fmt.Fprintf(&rules, "*mangle\n:%s - [0:0]\n-F %s\n:%s - [0:0]\n-F %s\n", ingressReturnChain, ingressReturnChain, ingressReplyChain, ingressReplyChain)
	for _, gateway := range gateways {
		// ipip preserves skb->mark while decapsulating in this namespace.
		// Each inner connection saves the entry identity in its own connmark.
		fmt.Fprintf(&rules, "-A %s -p 4 -s %s -d %s -j MARK --set-xmark 0x%x/0xffff0000\n",
			ingressReturnChain, gateway.address, address, gateway.id<<16)
	}
	fmt.Fprintf(&rules, "-A %s -j CONNMARK --restore-mark --nfmask 0xffff0000 --ctmask 0xffff0000\n", ingressReplyChain)
	// The request gains an IP-in-IP header. Advertise an MSS which fits
	// the ingress link without depending on external ICMP delivery.
	fmt.Fprintf(&rules, "-A %s -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss %d\nCOMMIT\n", ingressReplyChain, mtu-60)
	fmt.Fprintf(&rules, "*nat\n:%s - [0:0]\n-F %s\n", ingressReturnChain, ingressReturnChain)
	addresses := make([]net.IP, 0, len(gateways)+1)
	for _, gateway := range gateways {
		addresses = append(addresses, gateway.address)
	}
	if vip.To4() != nil {
		addresses = append(addresses, vip)
	}
	for _, target := range addresses {
		for _, port := range ports {
			fmt.Fprintf(&rules, "-A %s -d %s -p %s --dport %d -j DNAT --to-destination :%d\n",
				ingressReturnChain, target, strings.ToLower(port.Protocol.String()), port.PublishedPort, port.TargetPort)
		}
	}
	rules.WriteString("COMMIT\n")
	fmt.Fprintf(&rules, "*filter\n:%s - [0:0]\n-F %s\n", ingressReturnChain, ingressReturnChain)
	for _, target := range addresses {
		for _, port := range ports {
			fmt.Fprintf(&rules, "-A %s -d %s -p %s --dport %d -m conntrack --ctstate NEW,ESTABLISHED -m connmark ! --mark 0/0xffff0000 -j ACCEPT\n",
				ingressReturnChain, target, strings.ToLower(port.Protocol.String()), port.TargetPort)
		}
	}
	fmt.Fprintf(&rules, "-A %s -m conntrack --ctstate RELATED,ESTABLISHED -p icmp -j ACCEPT\n", ingressReturnChain)
	fmt.Fprintf(&rules, "-A %s -j DROP\nCOMMIT\n", ingressReturnChain)
	return rules.String(), nil
}

func replaceIngressGatewayRules(rules string) error {
	command := exec.CommandContext(context.TODO(), "iptables-restore", "--wait", "--noflush")
	command.Stdin = strings.NewReader(rules)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("updating ingress gateway rules: %w: %s", err, output)
	}
	return nil
}

// Caller serializes configuration with sb.joinLeaveMu.
func (sb *Sandbox) configureIngressSourceIPRouting(ep *Endpoint) error {
	n := ep.getNetwork()
	if n.loadBalancerMode != loadBalancerModeSourceIP || ep.loadBalancer || len(ep.ingressPorts) == 0 {
		return nil
	}
	gateways, err := n.ingressSourceGateways()
	if err != nil {
		return err
	}
	if len(gateways) == 0 {
		return errors.New("no ingress gateway is available for return routing")
	}
	pool, err := n.ingressSourceIPPool()
	if err != nil {
		return err
	}
	sb.mu.Lock()
	ns := sb.osSbox
	sb.mu.Unlock()
	if ns == nil {
		return errors.New("task namespace is unavailable")
	}
	iface := findIfaceDstName(sb, ep)
	if iface == "" {
		return errors.New("task ingress interface is unavailable")
	}
	address := ep.Iface().Address().IP.To4()
	if address == nil {
		return errors.New("task ingress address is not IPv4")
	}
	var innerErr error
	if err := ns.InvokeFunc(func() {
		innerErr = configureIngressReturnRoutes(iface, address, ep.virtualIP, pool, gateways, ep.ingressPorts)
	}); err != nil {
		return err
	}
	return innerErr
}

func configureIngressReturnRoutes(iface string, address net.IP, vip net.IP, pool *net.IPNet, gateways []ingressSourceGateway, ports []*PortConfig) error {
	// A task's ordinary route to the client may differ from the ingress
	// tunnel. The tunnel has its own connection-scoped return policy, so
	// reverse-path filtering must not run on the decapsulated request.
	// Do not weaken an application-wide setting on the other interfaces.
	if value, err := os.ReadFile("/proc/sys/net/ipv4/conf/all/rp_filter"); err != nil {
		return err
	} else if strings.TrimSpace(string(value)) != "0" {
		return errors.New("preserving ingress source IPs requires net.ipv4.conf.all.rp_filter=0 in the task namespace")
	}
	handle, err := nlwrap.NewHandle(syscall.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer handle.Close()
	link, err := handle.LinkByName(iface)
	if err != nil {
		return err
	}
	rules, err := ingressGatewayRules(address, vip, gateways, ports, link.Attrs().MTU)
	if err != nil {
		return err
	}
	tunnel, err := handle.LinkByName(ingressSourceTunnel)
	if errors.As(err, new(netlink.LinkNotFoundError)) {
		tunnel = &netlink.Iptun{LinkAttrs: netlink.LinkAttrs{Name: ingressSourceTunnel, MTU: link.Attrs().MTU - 20},
			Local: address, Link: uint32(link.Attrs().Index), Ttl: 64, PMtuDisc: 1}
		if err := handle.LinkAdd(tunnel); err != nil {
			return fmt.Errorf("creating ingress IP-in-IP interface: %w", err)
		}
	} else if err != nil {
		return err
	} else if existing, ok := tunnel.(*netlink.Iptun); !ok || !existing.Local.Equal(address) || existing.Attrs().ParentIndex != link.Attrs().Index {
		return fmt.Errorf("interface %s is already used by another configuration", ingressSourceTunnel)
	}
	if err := handle.LinkSetUp(tunnel); err != nil {
		return err
	}
	// Gateway addresses are also local VIPs; accept their outer packets and
	// keep loopback VIPs from answering ARP on the real ingress interface.
	for key, value := range map[string]string{"accept_local": "1\n", "arp_ignore": "1\n", "arp_announce": "2\n"} {
		if err := os.WriteFile("/proc/sys/net/ipv4/conf/"+iface+"/"+key, []byte(value), 0o644); err != nil {
			return err
		}
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/conf/"+ingressSourceTunnel+"/rp_filter", []byte("0\n"), 0o644); err != nil {
		return err
	}
	loopback, err := handle.LinkByName("lo")
	if err != nil {
		return err
	}
	current, err := handle.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	desired := make(map[int]struct{}, len(gateways))
	desiredAddresses := make(map[string]struct{}, len(gateways))
	for _, gateway := range gateways {
		rule := ingressReturnRule(pool, gateway.id)
		desired[rule.Table] = struct{}{}
		desiredAddresses[gateway.address.String()] = struct{}{}
		alias := &netlink.Addr{IPNet: &net.IPNet{IP: gateway.address, Mask: net.CIDRMask(32, 32)}, Label: ingressSourceAlias}
		if err := handle.AddrReplace(loopback, alias); err != nil {
			return fmt.Errorf("adding ingress gateway VIP: %w", err)
		}
		// A gateway VIP is local in this namespace, so it cannot also be an
		// IPv4 route's next hop. A private, permanent neighbor supplies the
		// gateway's Ethernet destination without changing the reply's IPs.
		nextHop := ingressReturnNextHop(gateway.id)
		if err := handle.NeighSet(&netlink.Neigh{LinkIndex: link.Attrs().Index, Family: netlink.FAMILY_V4,
			IP: nextHop, HardwareAddr: gateway.mac, State: netlink.NUD_PERMANENT}); err != nil {
			return fmt.Errorf("adding ingress return neighbor: %w", err)
		}
		route := &netlink.Route{LinkIndex: link.Attrs().Index, Table: rule.Table, Gw: nextHop,
			Src: address, Family: netlink.FAMILY_V4, Flags: syscall.RTNH_F_ONLINK}
		if err := handle.RouteReplace(route); err != nil {
			return fmt.Errorf("adding ingress return route: %w", err)
		}
		if !slices.ContainsFunc(current, func(existing netlink.Rule) bool {
			return ownedIngressReturnRule(existing, pool) && existing.Table == rule.Table
		}) {
			if err := handle.RuleAdd(rule); err != nil {
				return fmt.Errorf("adding ingress return policy: %w", err)
			}
		}
	}
	if err := replaceIngressGatewayRules(rules); err != nil {
		return err
	}
	iptable := iptables.GetIptable(iptables.IPv4)
	for location, args := range ingressReturnHooks(iface, address, pool) {
		parts := strings.Split(location, "/")
		table, chain := iptables.Table(parts[0]), parts[1]
		if !iptable.ExistsNative(table, chain, args...) {
			if err := iptable.RawCombinedOutputNative(append([]string{"-t", parts[0], "-I", chain}, args...)...); err != nil {
				return err
			}
		}
	}
	var errs []error
	for _, rule := range current {
		if ownedIngressReturnRule(rule, pool) {
			if _, keep := desired[rule.Table]; !keep {
				if err := removeIngressReturnRoute(handle, rule, link.Attrs().Index); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	aliases, err := handle.AddrList(loopback, netlink.FAMILY_V4)
	if err != nil {
		errs = append(errs, err)
	}
	for _, alias := range aliases {
		if alias.Label == ingressSourceAlias {
			if _, keep := desiredAddresses[alias.IP.String()]; !keep {
				if err := handle.AddrDel(loopback, &alias); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

func clearIngressSourceIPRouting(ns *osl.Namespace, ep *Endpoint) error {
	if ep.network.loadBalancerMode != loadBalancerModeSourceIP || ep.loadBalancer || ep.iface == nil || ep.iface.addr == nil {
		return nil
	}
	pool, err := ep.network.ingressSourceIPPool()
	if err != nil {
		return err
	}
	var iface string
	for _, current := range ns.Interfaces() {
		if current.SrcName() == ep.iface.srcName {
			iface = current.DstName()
			break
		}
	}
	if iface == "" {
		return nil
	}
	address := ep.iface.addr.IP.To4()
	var innerErr error
	if err := ns.InvokeFunc(func() {
		var errs []error
		iptable := iptables.GetIptable(iptables.IPv4)
		for location, args := range ingressReturnHooks(iface, address, pool) {
			parts := strings.Split(location, "/")
			if iptable.ExistsNative(iptables.Table(parts[0]), parts[1], args...) {
				if err := iptable.RawCombinedOutputNative(append([]string{"-t", parts[0], "-D", parts[1]}, args...)...); err != nil {
					errs = append(errs, err)
				}
			}
		}
		for _, entry := range [][2]string{{"mangle", ingressReturnChain}, {"mangle", ingressReplyChain}, {"nat", ingressReturnChain}, {"filter", ingressReturnChain}} {
			if err := exec.CommandContext(context.TODO(), "iptables", "--wait", "-t", entry[0], "-S", entry[1]).Run(); err == nil {
				for _, op := range []string{"-F", "-X"} {
					if err := iptable.RawCombinedOutputNative("-t", entry[0], op, entry[1]); err != nil {
						errs = append(errs, err)
					}
				}
			}
		}
		handle, err := nlwrap.NewHandle(syscall.NETLINK_ROUTE)
		if err != nil {
			innerErr = errors.Join(append(errs, err)...)
			return
		}
		defer handle.Close()
		link, err := handle.LinkByName(iface)
		if err != nil {
			innerErr = errors.Join(append(errs, err)...)
			return
		}
		rules, err := handle.RuleList(netlink.FAMILY_V4)
		if err != nil {
			errs = append(errs, err)
		}
		for _, rule := range rules {
			if ownedIngressReturnRule(rule, pool) {
				if err := removeIngressReturnRoute(handle, rule, link.Attrs().Index); err != nil {
					errs = append(errs, err)
				}
			}
		}
		loopback, err := handle.LinkByName("lo")
		if err == nil {
			aliases, err := handle.AddrList(loopback, netlink.FAMILY_V4)
			if err != nil {
				errs = append(errs, err)
			}
			for _, alias := range aliases {
				if alias.Label == ingressSourceAlias {
					if err := handle.AddrDel(loopback, &alias); err != nil {
						errs = append(errs, err)
					}
				}
			}
		} else {
			errs = append(errs, err)
		}
		if tunnel, err := handle.LinkByName(ingressSourceTunnel); err == nil {
			if existing, ok := tunnel.(*netlink.Iptun); ok && existing.Local.Equal(address) {
				if err := handle.LinkDel(tunnel); err != nil {
					errs = append(errs, err)
				}
			}
		}
		innerErr = errors.Join(errs...)
	}); err != nil {
		return err
	}
	return innerErr
}
