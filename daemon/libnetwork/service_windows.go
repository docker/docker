package libnetwork

import (
	"context"
	"net"

	"github.com/Microsoft/hcsshim"
	"github.com/containerd/log"
)

// osSandbox is the OS-specific state a [Sandbox] carries. There is none on this
// platform.
type osSandbox struct{}

type osLoadBalancer struct{}

type policyLists struct {
	ilb *hcsshim.PolicyList
	elb *hcsshim.PolicyList
}

var lbPolicylistMap = make(map[*loadBalancer]*policyLists)

func (n *Network) addLBBackend(ip net.IP, lb *loadBalancer) {
	if len(lb.vip) == 0 {
		return
	}

	vip := lb.vip
	ingressPorts := lb.service.ingressPorts

	lb.Lock()
	defer lb.Unlock()
	// find the load balancer IP for the network.
	var sourceVIP string
	for _, e := range n.Endpoints() {
		epInfo := e.Info()
		if epInfo == nil {
			continue
		}
		if epInfo.LoadBalancer() {
			sourceVIP = epInfo.Iface().Address().IP.String()
			break
		}
	}

	if sourceVIP == "" {
		log.G(context.TODO()).Errorf("Failed to find load balancer IP for network %s", n.Name())
		return
	}

	var endpoints []hcsshim.HNSEndpoint

	for eid, be := range lb.backEnds {
		if be.disabled {
			continue
		}
		// Call HNS to get back ID (GUID) corresponding to the endpoint.
		hnsEndpoint, err := hcsshim.GetHNSEndpointByName(eid)
		if err != nil {
			log.G(context.TODO()).Errorf("Failed to find HNS ID for endpoint %v: %v", eid, err)
			return
		}

		endpoints = append(endpoints, *hnsEndpoint)
	}

	if policies, ok := lbPolicylistMap[lb]; ok {

		if policies.ilb != nil {
			policies.ilb.Delete()
			policies.ilb = nil
		}

		if policies.elb != nil {
			policies.elb.Delete()
			policies.elb = nil
		}
		delete(lbPolicylistMap, lb)
	}

	ilbPolicy, err := hcsshim.AddLoadBalancer(endpoints, true, sourceVIP, vip.String(), 0, 0, 0)
	if err != nil {
		log.G(context.TODO()).Errorf("Failed to add ILB policy for service %s (%s) with endpoints %v using load balancer IP %s on network %s: %v",
			lb.service.name, vip.String(), endpoints, sourceVIP, n.Name(), err)
		return
	}

	lbPolicylistMap[lb] = &policyLists{
		ilb: ilbPolicy,
	}

	publishedPorts := make(map[uint32]uint32)

	for i, port := range ingressPorts {
		protocol := uint16(6)

		// Skip already published port
		if publishedPorts[port.PublishedPort] == port.TargetPort {
			continue
		}

		if port.Protocol == ProtocolUDP {
			protocol = 17
		}

		// check if already has udp matching to add wild card publishing
		for j := i + 1; j < len(ingressPorts); j++ {
			if ingressPorts[j].TargetPort == port.TargetPort &&
				ingressPorts[j].PublishedPort == port.PublishedPort {
				protocol = 0
			}
		}

		publishedPorts[port.PublishedPort] = port.TargetPort

		lbPolicylistMap[lb].elb, err = hcsshim.AddLoadBalancer(endpoints, false, sourceVIP, "", protocol, uint16(port.TargetPort), uint16(port.PublishedPort))
		if err != nil {
			log.G(context.TODO()).Errorf("Failed to add ELB policy for service %s (ip:%s target port:%v published port:%v) with endpoints %v using load balancer IP %s on network %s: %v",
				lb.service.name, vip.String(), uint16(port.TargetPort), uint16(port.PublishedPort), endpoints, sourceVIP, n.Name(), err)
			return
		}
	}
}

func (n *Network) rmLBBackend(ip net.IP, lb *loadBalancer, rmService bool, fullRemove bool) {
	if len(lb.vip) == 0 {
		return
	}

	if numEnabledBackends(lb) > 0 {
		// Reprogram HNS (actually VFP) with the existing backends.
		n.addLBBackend(ip, lb)
	} else {
		lb.Lock()
		defer lb.Unlock()
		log.G(context.TODO()).Debugf("No more backends for service %s (ip:%s).  Removing all policies", lb.service.name, lb.vip.String())

		if policyLists, ok := lbPolicylistMap[lb]; ok {
			if policyLists.ilb != nil {
				if _, err := policyLists.ilb.Delete(); err != nil {
					log.G(context.TODO()).Errorf("Failed to remove HNS ILB policylist %s: %s", policyLists.ilb.ID, err)
				}
				policyLists.ilb = nil
			}

			if policyLists.elb != nil {
				if _, err := policyLists.elb.Delete(); err != nil {
					log.G(context.TODO()).Errorf("Failed to remove HNS ELB policylist %s: %s", policyLists.elb.ID, err)
				}
				policyLists.elb = nil
			}
			delete(lbPolicylistMap, lb)

		} else {
			log.G(context.TODO()).Errorf("Failed to find policies for service %s (%s)", lb.service.name, lb.vip.String())
		}
	}
}

func numEnabledBackends(lb *loadBalancer) int {
	nEnabled := 0
	for _, be := range lb.backEnds {
		if !be.disabled {
			nEnabled++
		}
	}
	return nEnabled
}

func (sb *Sandbox) populateLoadBalancers(ep *Endpoint) {
}

func arrangeIngressFilterRule() {
}
