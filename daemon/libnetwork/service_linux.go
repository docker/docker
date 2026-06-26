package libnetwork

import (
	"context"
	"fmt"
	"math"
	"net"
	"strings"
	"sync"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
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
	newService := n.addLBBackendIPTables(ip, lb)

	if newService && n.ingress {
		_, sb, err := n.findLBEndpointSandbox()
		if err != nil {
			log.G(context.TODO()).Errorf("Failed to find load balancer endpoint sandbox: %v", err)
			return
		}
		gwEP, _ := sb.getGatewayEndpoint()
		if gwEP == nil {
			log.G(context.TODO()).Errorf("Failed to add ingress ports: no gateway endpoint for sandbox %.7s", sb.ID())
			return
		}
		if err := addIngressPorts(gwEP, lb.service.ingressPorts); err != nil {
			log.G(context.TODO()).Errorf("Failed to add ingress: %v", err)
		}
	}
}

// Remove loadbalancer backend the load balancing endpoint for this
// network. If 'rmService' is true, then remove the service entry as well.
// If 'fullRemove' is true then completely remove the entry, otherwise
// just deweight it for now.
func (n *Network) rmLBBackend(ip net.IP, lb *loadBalancer, rmService bool, fullRemove bool) {
	if len(lb.vip) == 0 {
		return
	}
	n.rmLBBackendIPTables(ip, lb, rmService, fullRemove)

	if rmService && n.ingress {
		_, sb, err := n.findLBEndpointSandbox()
		if err != nil {
			log.G(context.TODO()).Errorf("Failed to find load balancer endpoint sandbox: %v", err)
			return
		}
		if gwEP, _ := sb.getGatewayEndpoint(); gwEP == nil {
			log.G(context.TODO()).Errorf("Failed to remove ingress ports: no gateway endpoint for sandbox %.7s", sb.ID())
		} else if err := removeIngressPorts(gwEP, lb.service.ingressPorts); err != nil {
			log.G(context.TODO()).Errorf("Failed to remove ingress: %v", err)
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

func (sb *Sandbox) addRedirectRules(eIP *net.IPNet, ingressPorts []*PortConfig) error {
	return sb.addRedirectRulesIPTables(eIP, ingressPorts)
}
