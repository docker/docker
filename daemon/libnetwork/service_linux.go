package libnetwork

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/containerd/log"
	"github.com/moby/ipvs"
	"github.com/moby/moby/v2/daemon/libnetwork/iptables"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/vishvananda/netlink/nl"
)

// Populate all loadbalancers on the network that the passed endpoint
// belongs to, into this sandbox.
func (sb *Sandbox) populateLoadBalancers(ep *Endpoint) {
	// This is an interface less endpoint. Nothing to do.
	if ep.Iface() == nil {
		return
	}

	n := ep.getNetwork()
	eIP := ep.Iface().Address()

	if n.ingress {
		if err := sb.addRedirectRules(eIP, ep.ingressPorts); err != nil {
			log.G(context.TODO()).Errorf("Failed to add redirect rules for ep %s (%.7s): %v", ep.Name(), ep.ID(), err)
		}
	}
}

func (n *Network) findLBEndpointSandbox() (*Endpoint, *Sandbox, error) {
	// TODO: get endpoint from store?  See EndpointInfo()
	var ep *Endpoint
	// Find this node's LB sandbox endpoint:  there should be exactly one
	for _, e := range n.Endpoints() {
		epi := e.Info()
		if epi != nil && epi.LoadBalancer() {
			ep = e
			break
		}
	}
	if ep == nil {
		return nil, nil, fmt.Errorf("Unable to find load balancing endpoint for network %s", n.ID())
	}
	// Get the load balancer sandbox itself as well
	sb, ok := ep.getSandbox()
	if !ok {
		return nil, nil, fmt.Errorf("Unable to get sandbox for %s(%s) in for %s", ep.Name(), ep.ID(), n.ID())
	}
	sep := sb.GetEndpoint(ep.ID())
	if sep == nil {
		return nil, nil, fmt.Errorf("Load balancing endpoint %s(%s) removed from %s", ep.Name(), ep.ID(), n.ID())
	}
	return sep, sb, nil
}

// Searches the OS sandbox for the name of the endpoint interface
// within the sandbox.   This is required for adding/removing IP
// aliases to the interface.
func findIfaceDstName(sb *Sandbox, ep *Endpoint) string {
	srcName := ep.Iface().SrcName()
	for _, i := range sb.osSbox.Interfaces() {
		if i.SrcName() == srcName {
			return i.DstName()
		}
	}
	return ""
}

// Add loadbalancer backend to the loadbalancer sandbox for the network.
// If needed add the service as well.
func (n *Network) addLBBackend(ip net.IP, lb *loadBalancer) {
	if len(lb.vip) == 0 {
		return
	}
	ep, sb, err := n.findLBEndpointSandbox()
	if err != nil {
		log.G(context.TODO()).Errorf("addLBBackend %s/%s: %v", n.ID(), n.Name(), err)
		return
	}
	if sb.osSbox == nil {
		return
	}

	eIP := ep.Iface().Address()

	i, err := ipvs.New(sb.Key())
	if err != nil {
		log.G(context.TODO()).Errorf("Failed to create an ipvs handle for sbox %.7s (%.7s,%s) for lb addition: %v", sb.ID(), sb.ContainerID(), sb.Key(), err)
		return
	}
	defer i.Close()

	s := &ipvs.Service{
		AddressFamily: nl.FAMILY_V4,
		FWMark:        lb.fwMark,
		SchedName:     ipvs.RoundRobin,
	}

	if !i.IsServicePresent(s) {
		// Add IP alias for the VIP to the endpoint
		ifName := findIfaceDstName(sb, ep)
		if ifName == "" {
			log.G(context.TODO()).Errorf("Failed find interface name for endpoint %s(%s) to create LB alias", ep.ID(), ep.Name())
			return
		}
		err := sb.osSbox.AddAliasIP(ifName, &net.IPNet{IP: lb.vip, Mask: net.CIDRMask(32, 32)})
		if err != nil {
			if errors.Is(err, syscall.EEXIST) {
				log.G(context.TODO()).Debugf("IP alias %s already exists on network %s LB endpoint interface %s", lb.vip, n.ID(), ifName)
			} else {
				log.G(context.TODO()).Errorf("Failed add IP alias %s to network %s LB endpoint interface %s: %v", lb.vip, n.ID(), ifName, err)
				return
			}
		}

		if sb.ingress {
			gwEP, _ := sb.getGatewayEndpoint()
			if gwEP == nil {
				log.G(context.TODO()).Errorf("Failed to add ingress ports: no gateway endpoint for sandbox %.7s", sb.ID())
				return
			}
			if err := addIngressPorts(gwEP, lb.service.ingressPorts); err != nil {
				log.G(context.TODO()).Errorf("Failed to add ingress: %v", err)
				return
			}
		}

		log.G(context.TODO()).Debugf("Creating service for vip %s fwMark %d ingressPorts %#v in sbox %.7s (%.7s)", lb.vip, lb.fwMark, lb.service.ingressPorts, sb.ID(), sb.ContainerID())
		if err := sb.configureFWMark(lb.vip, lb.fwMark, lb.service.ingressPorts, eIP, false, n.loadBalancerMode); err != nil {
			log.G(context.TODO()).Errorf("Failed to add firewall mark rule in sbox %.7s (%.7s): %v", sb.ID(), sb.ContainerID(), err)
			return
		}

		if err := i.NewService(s); err != nil && !errors.Is(err, syscall.EEXIST) {
			log.G(context.TODO()).Errorf("Failed to create a new service for vip %s fwmark %d in sbox %.7s (%.7s): %v", lb.vip, lb.fwMark, sb.ID(), sb.ContainerID(), err)
			return
		}
	}

	// Remove the sched name before using the service to add
	// destination.
	s.SchedName = ""

	var flags uint32
	if n.loadBalancerMode == loadBalancerModeDSR {
		flags = ipvs.ConnFwdDirectRoute
	}
	err = i.NewDestination(s, &ipvs.Destination{
		AddressFamily:   nl.FAMILY_V4,
		Address:         ip,
		Weight:          1,
		ConnectionFlags: flags,
	})
	if err != nil && !errors.Is(err, syscall.EEXIST) {
		log.G(context.TODO()).Errorf("Failed to create real server %s for vip %s fwmark %d in sbox %.7s (%.7s): %v", ip, lb.vip, lb.fwMark, sb.ID(), sb.ContainerID(), err)
	}

	// Ensure that kernel tweaks are applied in case this is the first time
	// we've initialized ip_vs
	sb.osSbox.ApplyOSTweaks(sb.oslTypes)
}

// Remove loadbalancer backend the load balancing endpoint for this
// network. If 'rmService' is true, then remove the service entry as well.
// If 'fullRemove' is true then completely remove the entry, otherwise
// just deweight it for now.
func (n *Network) rmLBBackend(ip net.IP, lb *loadBalancer, rmService bool, fullRemove bool) {
	if len(lb.vip) == 0 {
		return
	}
	ep, sb, err := n.findLBEndpointSandbox()
	if err != nil {
		log.G(context.TODO()).Debugf("rmLBBackend for %s/%s: %v -- probably transient state", n.ID(), n.Name(), err)
		return
	}
	if sb.osSbox == nil {
		return
	}

	eIP := ep.Iface().Address()

	i, err := ipvs.New(sb.Key())
	if err != nil {
		log.G(context.TODO()).Errorf("Failed to create an ipvs handle for sbox %.7s (%.7s,%s) for lb removal: %v", sb.ID(), sb.ContainerID(), sb.Key(), err)
		return
	}
	defer i.Close()

	s := &ipvs.Service{
		AddressFamily: nl.FAMILY_V4,
		FWMark:        lb.fwMark,
	}

	d := &ipvs.Destination{
		AddressFamily: nl.FAMILY_V4,
		Address:       ip,
		Weight:        1,
	}
	if n.loadBalancerMode == loadBalancerModeDSR {
		d.ConnectionFlags = ipvs.ConnFwdDirectRoute
	}

	if fullRemove {
		if err := i.DelDestination(s, d); err != nil && !errors.Is(err, syscall.ENOENT) {
			log.G(context.TODO()).Errorf("Failed to delete real server %s for vip %s fwmark %d in sbox %.7s (%.7s): %v", ip, lb.vip, lb.fwMark, sb.ID(), sb.ContainerID(), err)
		}
	} else {
		d.Weight = 0
		if err := i.UpdateDestination(s, d); err != nil && !errors.Is(err, syscall.ENOENT) {
			log.G(context.TODO()).Errorf("Failed to set LB weight of real server %s to 0 for vip %s fwmark %d in sbox %.7s (%.7s): %v", ip, lb.vip, lb.fwMark, sb.ID(), sb.ContainerID(), err)
		}
	}

	if rmService {
		s.SchedName = ipvs.RoundRobin
		if err := i.DelService(s); err != nil && !errors.Is(err, syscall.ENOENT) {
			log.G(context.TODO()).Errorf("Failed to delete service for vip %s fwmark %d in sbox %.7s (%.7s): %v", lb.vip, lb.fwMark, sb.ID(), sb.ContainerID(), err)
		}

		if sb.ingress {
			// This is teardown: if the gateway endpoint is already gone, or
			// unpublishing the ingress ports fails, log and carry on so the fwmark
			// rules and VIP alias below are still cleaned up rather than left
			// behind. Only the ingress-port unpublishing is skipped.
			if gwEP, _ := sb.getGatewayEndpoint(); gwEP == nil {
				log.G(context.TODO()).Errorf("Failed to remove ingress ports: no gateway endpoint for sandbox %.7s", sb.ID())
			} else if err := removeIngressPorts(gwEP, lb.service.ingressPorts); err != nil {
				log.G(context.TODO()).Errorf("Failed to remove ingress: %v", err)
			}
		}

		if err := sb.configureFWMark(lb.vip, lb.fwMark, lb.service.ingressPorts, eIP, true, n.loadBalancerMode); err != nil {
			log.G(context.TODO()).Errorf("Failed to delete firewall mark rule in sbox %.7s (%.7s): %v", sb.ID(), sb.ContainerID(), err)
		}

		// Remove IP alias from the VIP to the endpoint
		ifName := findIfaceDstName(sb, ep)
		if ifName == "" {
			log.G(context.TODO()).Errorf("Failed find interface name for endpoint %s(%s) to create LB alias", ep.ID(), ep.Name())
			return
		}
		err := sb.osSbox.RemoveAliasIP(ifName, &net.IPNet{IP: lb.vip, Mask: net.CIDRMask(32, 32)})
		if err != nil {
			log.G(context.TODO()).Errorf("Failed to remove IP alias %s from network %s LB endpoint interface %s: %v", lb.vip, n.ID(), ifName, err)
		}
	}
}

var (
	ingressMu    sync.Mutex // lock for operations on ingress
	portConfigMu sync.Mutex
	// portConfigTbl counts how many services reference each host-port
	// reservation. It's keyed by the reservation itself rather than by the
	// PortConfig that asked for it, because that reservation is the resource
	// being shared: two services - or, more usually, two generations of one
	// service - whose port configs differ only in TargetPort want the same host
	// port, and must share one reference to it rather than each taking a first
	// one and then both trying to reserve it.
	portConfigTbl = make(map[types.PublishedPort]int)
)

// publishedPort is the host-port reservation ingress port config pc implies.
//
// Both sides of the mapping are pc's PublishedPort: the host publishes
// PublishedPort and DNATs it to the gateway endpoint on that same port. It's the
// backend task's own sandbox that redirects PublishedPort -> TargetPort once the
// packet is on the ingress network, so TargetPort plays no part here - and must
// not, because what gets reserved is only the protocol and the host port.
func publishedPort(pc *PortConfig) (types.PublishedPort, error) {
	if pc.PublishedPort > math.MaxUint16 {
		return types.PublishedPort{}, types.InvalidParameterErrorf("ingress published port %d is out of range", pc.PublishedPort)
	}
	return types.PublishedPort{
		Proto:    types.ParseProtocol(strings.ToLower(pc.Protocol.String())),
		Port:     uint16(pc.PublishedPort),
		HostPort: uint16(pc.PublishedPort),
		// The ports are published on the load-balancer sandbox's gateway
		// endpoint. The sandbox has no container, so nothing behind the
		// endpoint connects to them.
		NoSelfConnect: true,
	}, nil
}

// publishedPorts converts each of ingressPorts to the reservation it implies,
// returning nothing unless every one of them is valid. Narrowing the whole set
// once, here at the boundary, is what lets the reference counting below be
// infallible - so a caller undoing it can't be left holding an error.
func publishedPorts(ingressPorts []*PortConfig) ([]types.PublishedPort, error) {
	pps := make([]types.PublishedPort, 0, len(ingressPorts))
	for _, pc := range ingressPorts {
		pp, err := publishedPort(pc)
		if err != nil {
			return nil, err
		}
		pps = append(pps, pp)
	}
	return pps, nil
}

// refIngressPorts takes a reference to each of pps on behalf of a service, and
// returns those that are newly referenced - the ones that still need publishing
// on the load-balancer sandbox's gateway endpoint. Reservations already held for
// another service, or for another generation of the same one, are ref-counted and
// left out of the result.
//
// Undo a successful call with [unrefIngressPorts].
func refIngressPorts(pps []types.PublishedPort) []types.PublishedPort {
	portConfigMu.Lock()
	defer portConfigMu.Unlock()

	publish := make([]types.PublishedPort, 0, len(pps))
	for _, pp := range pps {
		if cnt, ok := portConfigTbl[pp]; ok {
			portConfigTbl[pp] = cnt + 1
			continue
		}

		// We are adding it for the first time, so it needs plumbing.
		portConfigTbl[pp] = 1
		publish = append(publish, pp)
	}

	return publish
}

// unrefIngressPorts drops a service's reference to each of pps, and returns those
// whose last reference is gone - the ones that should now be unpublished from the
// load-balancer sandbox's gateway endpoint. Reservations still referenced
// elsewhere are left out of the result, and ones with no outstanding reference
// are ignored.
func unrefIngressPorts(pps []types.PublishedPort) []types.PublishedPort {
	portConfigMu.Lock()
	defer portConfigMu.Unlock()

	unpublish := make([]types.PublishedPort, 0, len(pps))
	for _, pp := range pps {
		cnt, ok := portConfigTbl[pp]
		if !ok {
			continue
		}
		if cnt > 1 {
			portConfigTbl[pp] = cnt - 1
			continue
		}

		// This is the last reference to this reservation, so it needs unplumbing.
		delete(portConfigTbl, pp)
		unpublish = append(unpublish, pp)
	}

	return unpublish
}

// removeIngressPorts unpublishes the ingress ports whose last reference is being
// removed from the load-balancer sandbox's gateway endpoint.
func removeIngressPorts(gwEP *Endpoint, ingressPorts []*PortConfig) error {
	pps, err := publishedPorts(ingressPorts)
	if err != nil {
		return err
	}

	ingressMu.Lock()
	defer ingressMu.Unlock()

	unpublish := unrefIngressPorts(pps)
	if len(unpublish) == 0 {
		return nil
	}
	// The references stay dropped even if this fails. DelEphemeralPorts tears the
	// bindings down best-effort and commits that whether or not every step
	// succeeded, so an error does not mean the ports are still published - it
	// means they were unpublished with complaints. Re-taking the references would
	// pin reservations that no longer exist, and because rmServiceBinding has
	// already removed this loadBalancer from the service, nothing would ever
	// release them: the next service to publish the same port would find a
	// non-zero count, be told there was nothing to plumb, and never come up.
	if err := gwEP.DelEphemeralPorts(context.TODO(), unpublish); err != nil {
		return fmt.Errorf("failed to remove ingress ports: %v", err)
	}
	return nil
}

// addIngressPorts publishes the ingress ports being referenced for the first
// time on the load-balancer sandbox's gateway endpoint, reusing the bridge
// driver's port-publishing machinery (DNAT + forwarding rules + host-port
// reservation).
func addIngressPorts(gwEP *Endpoint, ingressPorts []*PortConfig) error {
	pps, err := publishedPorts(ingressPorts)
	if err != nil {
		return err
	}

	ingressMu.Lock()
	defer ingressMu.Unlock()

	publish := refIngressPorts(pps)
	if len(publish) == 0 {
		return nil
	}
	// Ingress ports are re-derived from the cluster and re-added on
	// restart, so they must not be saved to the bridge driver's store (else
	// they'd be restored and then published a second time).
	if err := gwEP.AddEphemeralPorts(context.TODO(), publish); err != nil {
		// Drop the references just taken - all of them, not just the
		// newly-referenced ones, so the counts stay balanced.
		unrefIngressPorts(pps)
		return fmt.Errorf("failed to program ingress ports: %v", err)
	}
	return nil
}

// configureFWMark configures the sandbox firewall to mark vip destined packets
// with the firewall mark fwMark.
func (sb *Sandbox) configureFWMark(vip net.IP, fwMark uint32, ingressPorts []*PortConfig, eIP *net.IPNet, isDelete bool, lbMode string) error {
	// TODO IPv6 support
	iptable := iptables.GetIptable(iptables.IPv4)

	fwMarkStr := strconv.FormatUint(uint64(fwMark), 10)
	addDelOpt := "-A"
	if isDelete {
		addDelOpt = "-D"
	}

	rules := make([][]string, 0, len(ingressPorts))
	for _, iPort := range ingressPorts {
		var (
			protocol      = strings.ToLower(PortConfig_Protocol_name[int32(iPort.Protocol)])
			publishedPort = strconv.FormatUint(uint64(iPort.PublishedPort), 10)
		)
		rule := []string{"-t", "mangle", addDelOpt, "PREROUTING", "-p", protocol, "--dport", publishedPort, "-j", "MARK", "--set-mark", fwMarkStr}
		rules = append(rules, rule)
	}

	var innerErr error
	err := sb.ExecFunc(func() {
		if !isDelete && lbMode == loadBalancerModeNAT {
			subnet := net.IPNet{IP: eIP.IP.Mask(eIP.Mask), Mask: eIP.Mask}
			ruleParams := []string{"-m", "ipvs", "--ipvs", "-d", subnet.String(), "-j", "SNAT", "--to-source", eIP.IP.String()}
			if !iptable.Exists("nat", "POSTROUTING", ruleParams...) {
				rule := append([]string{"-t", "nat", "-A", "POSTROUTING"}, ruleParams...)
				rules = append(rules, rule)

				err := os.WriteFile("/proc/sys/net/ipv4/vs/conntrack", []byte{'1', '\n'}, 0o644)
				if err != nil {
					innerErr = err
					return
				}
			}
		}

		rule := []string{"-t", "mangle", addDelOpt, "INPUT", "-d", vip.String() + "/32", "-j", "MARK", "--set-mark", fwMarkStr}
		rules = append(rules, rule)

		for _, rule := range rules {
			if err := iptable.RawCombinedOutputNative(rule...); err != nil {
				innerErr = fmt.Errorf("set up rule failed, %v: %w", rule, err)
				return
			}
		}
	})
	if err != nil {
		return err
	}
	return innerErr
}

func (sb *Sandbox) addRedirectRules(eIP *net.IPNet, ingressPorts []*PortConfig) error {
	// TODO IPv6 support
	iptable := iptables.GetIptable(iptables.IPv4)
	ipAddr := eIP.IP.String()

	rules := make([][]string, 0, len(ingressPorts)*3) // 3 rules per port
	for _, iPort := range ingressPorts {
		var (
			protocol      = strings.ToLower(PortConfig_Protocol_name[int32(iPort.Protocol)])
			publishedPort = strconv.FormatUint(uint64(iPort.PublishedPort), 10)
			targetPort    = strconv.FormatUint(uint64(iPort.TargetPort), 10)
		)

		rules = append(rules,
			[]string{"-t", "nat", "-A", "PREROUTING", "-d", ipAddr, "-p", protocol, "--dport", publishedPort, "-j", "REDIRECT", "--to-port", targetPort},

			// Allow only incoming connections to exposed ports
			[]string{"-I", "INPUT", "-d", ipAddr, "-p", protocol, "--dport", targetPort, "-m", "conntrack", "--ctstate", "NEW,ESTABLISHED", "-j", "ACCEPT"},

			// Allow only outgoing connections from exposed ports
			[]string{"-I", "OUTPUT", "-s", ipAddr, "-p", protocol, "--sport", targetPort, "-m", "conntrack", "--ctstate", "ESTABLISHED", "-j", "ACCEPT"},
		)
	}

	var innerErr error
	err := sb.ExecFunc(func() {
		for _, rule := range rules {
			if err := iptable.RawCombinedOutputNative(rule...); err != nil {
				innerErr = fmt.Errorf("set up rule failed, %v: %w", rule, err)
				return
			}
		}

		if len(ingressPorts) == 0 {
			return
		}

		// Ensure blocking rules for anything else in/to ingress network
		for _, rule := range [][]string{
			{"-d", ipAddr, "-p", "sctp", "-j", "DROP"},
			{"-d", ipAddr, "-p", "udp", "-j", "DROP"},
			{"-d", ipAddr, "-p", "tcp", "-j", "DROP"},
		} {
			if !iptable.ExistsNative(iptables.Filter, "INPUT", rule...) {
				if err := iptable.RawCombinedOutputNative(append([]string{"-A", "INPUT"}, rule...)...); err != nil {
					innerErr = fmt.Errorf("set up rule failed, %v: %w", rule, err)
					return
				}
			}
			rule[0] = "-s"
			if !iptable.ExistsNative(iptables.Filter, "OUTPUT", rule...) {
				if err := iptable.RawCombinedOutputNative(append([]string{"-A", "OUTPUT"}, rule...)...); err != nil {
					innerErr = fmt.Errorf("set up rule failed, %v: %w", rule, err)
					return
				}
			}
		}
	})
	if err != nil {
		return err
	}
	return innerErr
}
