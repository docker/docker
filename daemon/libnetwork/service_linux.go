package libnetwork

import (
	"context"
	"fmt"
	"math"
	"net"
	"strings"
	"sync"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/nftables"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
)

// osSandbox is the OS-specific state a [Sandbox] carries.
type osSandbox struct {
	nftLBMu sync.Mutex
	// nftLB is the load-balancer data plane this sandbox's nftables tables should
	// hold. Only a load-balancer sandbox has any; its zero value is empty and is
	// populated on first use. See [nftLBState].
	nftLB nftLBState
}

type osLoadBalancer struct {
	// serviceProgrammed records whether this service is fully plumbed: its data
	// plane and, on an ingress network, its published ports. It's the source of
	// truth for the once-per-service publish/unpublish of those ports, so it must
	// not be derived from the live backend count, which can transiently drop to
	// zero and recover without the service being added or removed (e.g. during a
	// rolling update).
	//
	// It's owned by programService/unprogramService and set only once every step
	// has succeeded, so a partial failure leaves it false and the next backend event
	// retries the whole setup.
	//
	// It does not gate the data plane itself. The IPVS backend asks IPVS whether
	// its service already exists, and the nftables backend states its whole
	// desired data plane on every sync and diffs it (see nftLBState), so
	// neither needs a latch to avoid adding or removing something twice.
	serviceProgrammed bool
}

// Populate all loadbalancers on the network that the passed endpoint
// belongs to, into this sandbox.
//
// On the ingress network that means the rules that redirect published ports to
// target ports and filter everything else addressed to the endpoint. Without
// them the task can't serve its published ports, and other endpoints on the
// ingress network can reach any port it listens on. So a failure is returned,
// to fail the join, rather than leaving the task running without them.
func (sb *Sandbox) populateLoadBalancers(ep *Endpoint) error {
	// This is an interface less endpoint. Nothing to do.
	if ep.Iface() == nil {
		return nil
	}

	n := ep.getNetwork()
	eIP := ep.Iface().Address()

	if n.ingress {
		if err := sb.addRedirectRules(eIP, ep.ingressPorts); err != nil {
			return fmt.Errorf("failed to add ingress redirect rules for endpoint %s (%.7s): %w", ep.Name(), ep.ID(), err)
		}
	}
	return nil
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

// hasEnabledBackends reports whether lb has at least one backend that isn't
// deweighted, i.e. whether it has anything to load-balance to.
func hasEnabledBackends(lb *loadBalancer) bool {
	for _, be := range lb.backEnds {
		if !be.disabled {
			return true
		}
	}
	return false
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

	lb.programService(n.ingress,
		func() bool {
			if nftables.Enabled() {
				return n.syncLBBackendsNFTables(context.TODO(), ep, sb, lb, false)
			}
			return n.addLBBackendIPTables(ip, ep, sb, lb)
		},
		func() error {
			gwEP, _ := sb.getGatewayEndpoint()
			if gwEP == nil {
				return fmt.Errorf("no gateway endpoint for sandbox %.7s", sb.ID())
			}
			return addIngressPorts(gwEP, lb.service.ingressPorts)
		})
}

// programService brings lb's data plane up and then, on an ingress network,
// publishes the service's host-published ports - once per service, however many
// backend events arrive.
//
// dataPlane states lb's whole desired data plane and reports whether it is fully
// in place. publishPorts publishes the service's ingress ports; it is only called
// on an ingress network, and only when the ports still need publishing. Both are
// parameters so that the ordering and the latching below can be exercised without
// a network namespace.
//
// Undo a call that latched with [loadBalancer.unprogramService].
func (lb *loadBalancer) programService(ingress bool, dataPlane func() bool, publishPorts func() error) {
	if !dataPlane() {
		// The data plane isn't fully in place. Leave serviceProgrammed false so the
		// next backend event retries the whole setup, including any ingress ports.
		return
	}

	if lb.serviceProgrammed || !hasEnabledBackends(lb) {
		// Already programmed, or there's nothing to program yet. The latch must only
		// be set for a service that actually has a data plane - setting it for one
		// with no backends would suppress the VIP alias when a backend arrives.
		return
	}

	// Publishing the ingress ports is the last step, and lb.serviceProgrammed is
	// only set once it has succeeded - a failure here must leave the service
	// un-programmed so the next backend event retries, otherwise the service's
	// published ports would stay closed for its whole lifetime.
	if ingress {
		if err := publishPorts(); err != nil {
			lb.reportIngressFailure(err)
			return
		}
	}

	lb.serviceProgrammed = true
}

// reportIngressFailure reports that this service's host-published ports aren't
// published on this node.
//
// The daemon log is the only channel available. The ports belong to the service
// rather than to any one task, so a task status would misattribute the failure -
// and every node attached to the ingress network publishes a service's ports
// whether or not it runs a task of that service, so there may be no task here to
// attribute it to at all.
//
// It repeats for every backend event while the cause persists - most often the
// host port already being bound by something outside Swarm. That's deliberate: a
// once-only report would leave anything watching the log unable to tell an
// ongoing failure from one that resolved. Log volume is a reason to add a metric,
// not to hide an unresolved problem.
func (lb *loadBalancer) reportIngressFailure(err error) {
	log.G(context.TODO()).WithError(err).WithFields(log.Fields{
		"service":      lb.service.name,
		"serviceID":    lb.service.id,
		"ingressPorts": lb.service.ingressPorts.String(),
	}).Error("Failed to publish this service's Swarm ingress ports on this node - they will not be reachable via this node, and will be retried on the service's next backend event")
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

	// The result is deliberately dropped: there is nothing useful to do with a
	// failed teardown here, because this loadBalancer has already been removed
	// from the service, so no later event can retry it. Each backend logs its own
	// failures. Whether the ingress ports need unpublishing depends only on
	// whether they were ever published, which lb.serviceProgrammed records.
	//
	// Residue differs by backend. The nftables sync states the whole desired data
	// plane for the network and applies it wholesale, so whatever this call failed
	// to write is corrected by the next sync from any service on the network. The
	// iptables path has no such convergence - rules it fails to delete stay until
	// the sandbox goes away.
	if nftables.Enabled() {
		n.syncLBBackendsNFTables(context.TODO(), ep, sb, lb, rmService)
	} else {
		n.rmLBBackendIPTables(ip, ep, sb, lb, rmService, fullRemove)
	}

	if !rmService {
		// Backends remain, so the service keeps its ingress ports.
		return
	}

	lb.unprogramService(n.ingress, func() error {
		gwEP, _ := sb.getGatewayEndpoint()
		if gwEP == nil {
			return fmt.Errorf("no gateway endpoint for sandbox %.7s", sb.ID())
		}
		return removeIngressPorts(gwEP, lb.service.ingressPorts)
	})
}

// unprogramService undoes a [loadBalancer.programService] call that latched: on an
// ingress network it unpublishes the service's host-published ports, then clears
// the latch.
//
// unpublishPorts is a parameter for the same reason as programService's, and is
// likewise only called on an ingress network.
func (lb *loadBalancer) unprogramService(ingress bool, unpublishPorts func() error) {
	if !lb.serviceProgrammed {
		// This service was never fully programmed, so its ingress ports were never
		// published - unpublishing them here would drop a reference it never took.
		return
	}

	if ingress {
		if err := unpublishPorts(); err != nil {
			log.G(context.TODO()).WithError(err).WithFields(log.Fields{
				"service":      lb.service.name,
				"serviceID":    lb.service.id,
				"ingressPorts": lb.service.ingressPorts.String(),
			}).Error("Failed to unpublish this service's Swarm ingress ports on this node")
			return
		}
	}

	// The ingress ports are down, so the service is no longer fully plumbed
	// whatever became of the data plane. Clear the latch so that a service which
	// comes back gets the complete setup again; the data-plane steps tolerate
	// anything a failed teardown left behind.
	lb.serviceProgrammed = false
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
	if nftables.Enabled() {
		return sb.addRedirectRulesNFTables(context.TODO(), eIP, ingressPorts)
	} else {
		return sb.addRedirectRulesIPTables(eIP, ingressPorts)
	}
}
