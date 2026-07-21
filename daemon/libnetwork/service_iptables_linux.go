package libnetwork

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/containerd/log"
	"github.com/moby/ipvs"
	"github.com/moby/moby/v2/daemon/libnetwork/iptables"
	"github.com/vishvananda/netlink/nl"
)

// addLBBackendIPTables adds a loadbalancer backend to the loadbalancer sandbox
// for the network.  If needed add the service as well.
func (n *Network) addLBBackendIPTables(ip net.IP, lb *loadBalancer) {
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
		if errors.Is(err, syscall.EEXIST) {
			log.G(context.TODO()).Debugf("IP alias %s already exists on network %s LB endpoint interface %s", lb.vip, n.ID(), ifName)
		} else if err != nil {
			log.G(context.TODO()).Errorf("Failed add IP alias %s to network %s LB endpoint interface %s: %v", lb.vip, n.ID(), ifName, err)
			return
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
		if err := sb.configureFWMarkIPTables(lb.vip, lb.fwMark, lb.service.ingressPorts, eIP, false, n.loadBalancerMode); err != nil {
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

// rmLBBackendIPTables removes a loadbalancer backend the load balancing
// endpoint for this network. If 'rmService' is true, then remove the service
// entry as well. If 'fullRemove' is true then completely remove the entry,
// otherwise just deweight it for now.
func (n *Network) rmLBBackendIPTables(ip net.IP, lb *loadBalancer, rmService bool, fullRemove bool) {
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

		if err := sb.configureFWMarkIPTables(lb.vip, lb.fwMark, lb.service.ingressPorts, eIP, true, n.loadBalancerMode); err != nil {
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

// configureFWMarkIPTables configures the sandbox firewall to mark vip destined packets
// with the firewall mark fwMark.
func (sb *Sandbox) configureFWMarkIPTables(vip net.IP, fwMark uint32, ingressPorts []*PortConfig, eIP *net.IPNet, isDelete bool, lbMode string) error {
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

func (sb *Sandbox) addRedirectRulesIPTables(eIP *net.IPNet, ingressPorts []*PortConfig) error {
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
