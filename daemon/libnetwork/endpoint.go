package libnetwork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/internal/stringid"
	"github.com/moby/moby/v2/daemon/libnetwork/datastore"
	"github.com/moby/moby/v2/daemon/libnetwork/driverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/ipamapi"
	"github.com/moby/moby/v2/daemon/libnetwork/netlabel"
	"github.com/moby/moby/v2/daemon/libnetwork/scope"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"go.opentelemetry.io/otel"
)

// ByNetworkType sorts a [Endpoint] slice based on the network-type
// they're attached to. It implements [sort.Interface] and can be used
// with [sort.Stable] or [sort.Sort]. It is used by [Sandbox.ResolveName]
// when resolving names in swarm mode. In swarm mode, services with exposed
// ports are connected to user overlay network, ingress network, and local
// ("docker_gwbridge") networks. Name resolution should prioritize returning
// the VIP/IPs on user overlay network over ingress and local networks.
//
// ByNetworkType re-orders the endpoints based on the network-type they
// are attached to:
//
//  1. dynamic networks (user overlay networks)
//  2. ingress network(s)
//  3. local networks ("docker_gwbridge")
type ByNetworkType []*Endpoint

func (ep ByNetworkType) Len() int      { return len(ep) }
func (ep ByNetworkType) Swap(i, j int) { ep[i], ep[j] = ep[j], ep[i] }
func (ep ByNetworkType) Less(i, j int) bool {
	return getNetworkType(ep[i].getNetwork()) < getNetworkType(ep[j].getNetwork())
}

// Define the order in which resolution should happen if an endpoint is
// attached to multiple network-types. It is used by [ByNetworkType].
const (
	typeDynamic = iota
	typeIngress
	typeLocal
)

func getNetworkType(nw *Network) int {
	switch {
	case nw.ingress:
		return typeIngress
	case nw.dynamic:
		return typeDynamic
	default:
		return typeLocal
	}
}

// EndpointOption is an option setter function type used to pass various options to Network
// and Endpoint interfaces methods. The various setter functions of type EndpointOption are
// provided by libnetwork, they look like <Create|Join|Leave>Option[...](...)
type EndpointOption func(ep *Endpoint)

// Endpoint represents a logical connection between a network and a sandbox.
type Endpoint struct {
	name         string
	id           string
	network      *Network
	iface        *EndpointInterface
	joinInfo     *endpointJoinInfo
	sandboxID    string
	exposedPorts []types.TransportPort
	// dnsNames holds all the non-fully qualified DNS names associated to this endpoint. Order matters: first entry
	// will be used for the PTR records associated to the endpoint's IPv4 and IPv6 addresses.
	dnsNames          []string
	disableResolution bool
	disableIPv6       bool
	generic           map[string]any
	prefAddress       net.IP
	prefAddressV6     net.IP
	ipamOptions       map[string]string
	aliases           map[string]string
	svcID             string
	svcName           string
	virtualIP         net.IP
	svcAliases        []string
	ingressPorts      []*PortConfig
	dbIndex           uint64
	dbExists          bool
	serviceEnabled    bool
	loadBalancer      bool
	mu                sync.Mutex
}

func (ep *Endpoint) MarshalJSON() ([]byte, error) {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	epMap := make(map[string]any)
	epMap["name"] = ep.name
	epMap["id"] = ep.id
	epMap["ep_iface"] = ep.iface
	epMap["joinInfo"] = ep.joinInfo
	epMap["exposed_ports"] = ep.exposedPorts
	if ep.generic != nil {
		epMap["generic"] = ep.generic
	}
	epMap["sandbox"] = ep.sandboxID
	epMap["dnsNames"] = ep.dnsNames
	epMap["disableResolution"] = ep.disableResolution
	epMap["disableIPv6"] = ep.disableIPv6
	epMap["svcName"] = ep.svcName
	epMap["svcID"] = ep.svcID
	epMap["virtualIP"] = ep.virtualIP.String()
	epMap["ingressPorts"] = ep.ingressPorts
	epMap["svcAliases"] = ep.svcAliases
	epMap["loadBalancer"] = ep.loadBalancer

	return json.Marshal(epMap)
}

func (ep *Endpoint) UnmarshalJSON(b []byte) (err error) {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	var epMap map[string]any
	if err := json.Unmarshal(b, &epMap); err != nil {
		return err
	}
	ep.name = epMap["name"].(string)
	ep.id = epMap["id"].(string)

	// TODO(cpuguy83): So yeah, this isn't checking any errors anywhere.
	// Seems like we should be checking errors even because of memory related issues that can arise.
	// Alas it seems like given the nature of this data we could introduce problems if we start checking these errors.
	//
	// If anyone ever comes here and figures out one way or another if we can/should be checking these errors and it turns out we can't... then please document *why*

	ib, _ := json.Marshal(epMap["ep_iface"]) //nolint:errchkjson // FIXME: handle json (Un)Marshal errors (see above)
	_ = json.Unmarshal(ib, &ep.iface)        //nolint:errcheck

	jb, _ := json.Marshal(epMap["joinInfo"]) //nolint:errchkjson // FIXME: handle json (Un)Marshal errors (see above)
	_ = json.Unmarshal(jb, &ep.joinInfo)     //nolint:errcheck

	tb, _ := json.Marshal(epMap["exposed_ports"]) //nolint:errchkjson // FIXME: handle json (Un)Marshal errors (see above)
	var tPorts []types.TransportPort
	_ = json.Unmarshal(tb, &tPorts) //nolint:errcheck
	ep.exposedPorts = tPorts

	cb, _ := json.Marshal(epMap["sandbox"]) //nolint:errchkjson // FIXME: handle json (Un)Marshal errors (see above)
	_ = json.Unmarshal(cb, &ep.sandboxID)   //nolint:errcheck

	if v, ok := epMap["generic"]; ok {
		ep.generic = v.(map[string]any)

		if opt, ok := ep.generic[netlabel.PortMap]; ok {
			pblist := []types.PortBinding{}

			for i := 0; i < len(opt.([]any)); i++ {
				pb := types.PortBinding{}
				tmp := opt.([]any)[i].(map[string]any)

				bytes, err := json.Marshal(tmp)
				if err != nil {
					log.G(context.TODO()).Error(err)
					break
				}
				err = json.Unmarshal(bytes, &pb)
				if err != nil {
					log.G(context.TODO()).Error(err)
					break
				}
				pblist = append(pblist, pb)
			}
			ep.generic[netlabel.PortMap] = pblist
		}

		if opt, ok := ep.generic[netlabel.ExposedPorts]; ok {
			tplist := []types.TransportPort{}

			for i := 0; i < len(opt.([]any)); i++ {
				tp := types.TransportPort{}
				tmp := opt.([]any)[i].(map[string]any)

				bytes, err := json.Marshal(tmp)
				if err != nil {
					log.G(context.TODO()).Error(err)
					break
				}
				err = json.Unmarshal(bytes, &tp)
				if err != nil {
					log.G(context.TODO()).Error(err)
					break
				}
				tplist = append(tplist, tp)
			}
			ep.generic[netlabel.ExposedPorts] = tplist
		}
	}

	if v, ok := epMap["disableResolution"]; ok {
		ep.disableResolution = v.(bool)
	}
	if v, ok := epMap["disableIPv6"]; ok {
		ep.disableIPv6 = v.(bool)
	}

	if sn, ok := epMap["svcName"]; ok {
		ep.svcName = sn.(string)
	}

	if si, ok := epMap["svcID"]; ok {
		ep.svcID = si.(string)
	}

	if vip, ok := epMap["virtualIP"]; ok {
		ep.virtualIP = net.ParseIP(vip.(string))
	}

	if v, ok := epMap["loadBalancer"]; ok {
		ep.loadBalancer = v.(bool)
	}

	sal, _ := json.Marshal(epMap["svcAliases"]) //nolint:errchkjson // FIXME: handle json (Un)Marshal errors (see above)
	var svcAliases []string
	_ = json.Unmarshal(sal, &svcAliases) //nolint:errcheck
	ep.svcAliases = svcAliases

	pc, _ := json.Marshal(epMap["ingressPorts"]) //nolint:errchkjson // FIXME: handle json (Un)Marshal errors (see above)
	var ingressPorts []*PortConfig
	_ = json.Unmarshal(pc, &ingressPorts) //nolint:errcheck
	ep.ingressPorts = ingressPorts

	ma, _ := json.Marshal(epMap["myAliases"]) //nolint:errchkjson // FIXME: handle json (Un)Marshal errors (see above)
	var myAliases []string
	_ = json.Unmarshal(ma, &myAliases) //nolint:errcheck

	dn, _ := json.Marshal(epMap["dnsNames"]) //nolint:errchkjson // FIXME: handle json (Un)Marshal errors (see above)
	var dnsNames []string
	_ = json.Unmarshal(dn, &dnsNames) //nolint:errcheck
	ep.dnsNames = dnsNames

	return nil
}

func (ep *Endpoint) New() datastore.KVObject {
	return &Endpoint{network: ep.getNetwork()}
}

var _ datastore.KVObject = (*Endpoint)(nil)

func (ep *Endpoint) CopyTo(o datastore.KVObject) error {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	// TODO(thaJeztah): should dstEp be locked during this copy? (ideally we'd not have a mutex at all in this struct).
	dstEp := o.(*Endpoint)
	dstEp.name = ep.name
	dstEp.id = ep.id
	dstEp.sandboxID = ep.sandboxID
	dstEp.dbIndex = ep.dbIndex
	dstEp.dbExists = ep.dbExists
	dstEp.disableResolution = ep.disableResolution
	dstEp.disableIPv6 = ep.disableIPv6
	dstEp.svcName = ep.svcName
	dstEp.svcID = ep.svcID
	dstEp.virtualIP = ep.virtualIP // TODO(thaJeztah): should this be cloned? (net.IP)?
	dstEp.loadBalancer = ep.loadBalancer
	dstEp.svcAliases = slices.Clone(ep.svcAliases)
	dstEp.ingressPorts = slices.Clone(ep.ingressPorts) // TODO(thaJeztah): should this be copied in depth? ([]*PortConfig)
	dstEp.iface = ep.iface.Copy()
	dstEp.joinInfo = ep.joinInfo.Copy()
	dstEp.exposedPorts = slices.Clone(ep.exposedPorts)
	dstEp.dnsNames = slices.Clone(ep.dnsNames)
	dstEp.generic = maps.Clone(ep.generic)
	return nil
}

// ID returns the system-generated id for this endpoint.
func (ep *Endpoint) ID() string {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	return ep.id
}

// Name returns the name of this endpoint.
func (ep *Endpoint) Name() string {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	return ep.name
}

// Network returns the name of the network to which this endpoint is attached.
func (ep *Endpoint) Network() string {
	if ep.network == nil {
		return ""
	}

	return ep.network.name
}

// getDNSNames returns a copy of the DNS names associated to this endpoint. The first entry is the one used for PTR
// records.
func (ep *Endpoint) getDNSNames() []string {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	dnsNames := make([]string, len(ep.dnsNames))
	copy(dnsNames, ep.dnsNames)
	return dnsNames
}

// isServiceEnabled check if service is enabled on the endpoint
func (ep *Endpoint) isServiceEnabled() bool {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	return ep.serviceEnabled
}

// enableService sets service enabled on the endpoint
func (ep *Endpoint) enableService() {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	ep.serviceEnabled = true
}

// disableService disables service on the endpoint
func (ep *Endpoint) disableService() {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	ep.serviceEnabled = false
}

func (ep *Endpoint) needResolver() bool {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	return !ep.disableResolution
}

// Key returns the endpoint's key.
//
// Key structure: endpoint/network-id/endpoint-id
func (ep *Endpoint) Key() []string {
	if ep.network == nil {
		return nil
	}

	return []string{datastore.EndpointKeyPrefix, ep.network.id, ep.id}
}

func (ep *Endpoint) KeyPrefix() []string {
	if ep.network == nil {
		return nil
	}

	return []string{datastore.EndpointKeyPrefix, ep.network.id}
}

func (ep *Endpoint) Value() []byte {
	b, err := json.Marshal(ep)
	if err != nil {
		return nil
	}
	return b
}

func (ep *Endpoint) getSysctls() []string {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	if s, ok := ep.generic[netlabel.EndpointSysctls]; ok {
		if ss, ok := s.(string); ok {
			return strings.Split(ss, ",")
		}
	}
	return nil
}

func (ep *Endpoint) SetValue(value []byte) error {
	return json.Unmarshal(value, ep)
}

func (ep *Endpoint) Index() uint64 {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	return ep.dbIndex
}

func (ep *Endpoint) SetIndex(index uint64) {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	ep.dbIndex = index
	ep.dbExists = true
}

func (ep *Endpoint) Exists() bool {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	return ep.dbExists
}

func (ep *Endpoint) Skip() bool {
	return ep.getNetwork().Skip()
}

func (ep *Endpoint) processOptions(options ...EndpointOption) {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	for _, opt := range options {
		if opt != nil {
			opt(ep)
		}
	}
}

func (ep *Endpoint) getNetwork() *Network {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	return ep.network
}

func (ep *Endpoint) getNetworkFromStore() (*Network, error) {
	if ep.network == nil {
		return nil, fmt.Errorf("invalid network object in endpoint %s", ep.Name())
	}

	return ep.network.controller.getNetworkFromStore(ep.network.id)
}

// Join joins the sandbox to the endpoint and populates into the sandbox
// the network resources allocated for the endpoint.
func (ep *Endpoint) Join(ctx context.Context, sb *Sandbox, options ...EndpointOption) error {
	if sb == nil || sb.ID() == "" || sb.Key() == "" {
		return types.InvalidParameterErrorf("invalid Sandbox passed to endpoint join: %v", sb)
	}

	sb.joinLeaveMu.Lock()
	defer sb.joinLeaveMu.Unlock()

	return ep.sbJoin(ctx, sb, options...)
}

func epId(ep *Endpoint) string {
	if ep == nil {
		return ""
	}
	return ep.id
}

func epShortId(ep *Endpoint) string {
	return stringid.TruncateID(epId(ep))
}

func (ep *Endpoint) sbJoin(ctx context.Context, sb *Sandbox, options ...EndpointOption) (retErr error) {
	ctx, span := otel.Tracer("").Start(ctx, "libnetwork.sbJoin")
	defer span.End()

	n, err := ep.getNetworkFromStore()
	if err != nil {
		return fmt.Errorf("failed to get network from store during join: %v", err)
	}

	ep, err = n.getEndpointFromStore(ep.ID())
	if err != nil {
		return fmt.Errorf("failed to get endpoint from store during join: %v", err)
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(log.Fields{
		"nid": stringid.TruncateID(n.ID()),
		"net": n.Name(),
		"eid": stringid.TruncateID(ep.ID()),
		"ep":  ep.Name(),
	}))

	ep.mu.Lock()
	if ep.sandboxID != "" {
		ep.mu.Unlock()
		return types.ForbiddenErrorf("another container is attached to the same network endpoint")
	}
	ep.network = n
	ep.sandboxID = sb.ID()
	ep.joinInfo = &endpointJoinInfo{}
	epid := ep.id
	ep.mu.Unlock()
	defer func() {
		if retErr != nil {
			if err := ep.sbLeave(ctx, sb, n, true); err != nil {
				log.G(ctx).WithFields(log.Fields{
					"error":  err,
					"retErr": retErr,
				}).Warn("Failed to remove endpoint after join error")
			}
		}
	}()

	nid := n.ID()

	ep.processOptions(options...)

	d, err := n.driver(true)
	if err != nil {
		return fmt.Errorf("failed to get driver during join: %v", err)
	}

	// Tell the driver about the new endpoint. The driver populates ep.joinInfo using
	// the Endpoint's JoinInfo interface.
	if err := d.Join(ctx, nid, epid, sb.Key(), ep, ep.generic, sb.Labels()); err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			if err := d.Leave(nid, epid); err != nil {
				log.G(ctx).WithError(err).Warnf("driver leave failed while rolling back join")
			}
		}
	}()

	// Discard the IPv6 gateway if the endpoint has no IPv6 address (because IPv6
	// is disabled in the container).
	if ep.iface.addrv6 == nil {
		ep.joinInfo.gw6 = nil
	}

	// Current endpoint(s) providing external connectivity for the Sandbox.
	// If ep is selected as a gateway endpoint once it's been added to the Sandbox,
	// these are the endpoints that need to be un-gateway'd.
	gwepBefore4, gwepBefore6 := sb.getGatewayEndpoint()

	sb.addEndpoint(ep)

	// For Linux, at this point, in most cases, the container task has been created
	// and the container's network namespace (sb.osSbox) is ready to be configured
	// with addresses, routes and so on. The exception is when the SetKey re-exec is
	// used by a build container. In that case, the osSbox doesn't exist yet. So,
	// stop here and SetKey will finish off the configuration when it's ready.
	// For Windows, canPopulateNetworkResources() is always true.
	if sb.canPopulateNetworkResources() {
		if err := sb.populateNetworkResources(ctx, ep); err != nil {
			return err
		}

		// If the old gateway was in the docker_gwbridge network, it's already been removed if
		// the new endpoint provides a gateway. Don't try to remove it again.
		if gwepBefore4 != nil && sb.GetEndpoint(gwepBefore4.ID()) == nil {
			gwepBefore4 = nil
		}
		if gwepBefore6 != nil && sb.GetEndpoint(gwepBefore6.ID()) == nil {
			gwepBefore6 = nil
		}

		if err := ep.updateExternalConnectivity(ctx, sb, gwepBefore4, gwepBefore6); err != nil {
			return err
		}
	}

	if err := n.controller.storeEndpoint(ctx, ep); err != nil {
		return err
	}
	return nil
}

// updateExternalConnectivity configures an Endpoint when it becomes the gateway
// endpoint for a network, revoking external connectivity from the previous gateway
// endpoints, if necessary. (It does not update the Sandbox's default gateway, the
// Sandbox takes care of that. This is just about network driver config.)
func (ep *Endpoint) updateExternalConnectivity(ctx context.Context, sb *Sandbox, gwepBefore4, gwepBefore6 *Endpoint) (retErr error) {
	gwepAfter4, gwepAfter6 := sb.getGatewayEndpoint()

	log.G(ctx).Infof("sbJoin: gwep4 '%s'->'%s', gwep6 '%s'->'%s'",
		epShortId(gwepBefore4), epShortId(gwepAfter4),
		epShortId(gwepBefore6), epShortId(gwepAfter6))

	// If ep has taken over as a gateway and there were gateways before, update them.
	if ep == gwepAfter4 || ep == gwepAfter6 {
		if gwepBefore4 != nil {
			if err := gwepBefore4.programExternalConnectivity(ctx, gwepAfter4, gwepAfter6); err != nil {
				return fmt.Errorf("updating external connectivity for IPv4 endpoint %s: %v", epShortId(gwepBefore4), err)
			}
			defer func() {
				if retErr != nil {
					if err := gwepBefore4.programExternalConnectivity(ctx, gwepBefore4, gwepBefore6); err != nil {
						log.G(ctx).WithFields(log.Fields{
							"error":     err,
							"restoreEp": epShortId(gwepBefore4),
						}).Errorf("Failed to restore external IPv4 connectivity")
					}
				}
			}()
		}
		if gwepBefore6 != nil {
			if err := gwepBefore6.programExternalConnectivity(ctx, gwepAfter4, gwepAfter6); err != nil {
				return fmt.Errorf("updating external connectivity for IPv6 endpoint %s: %v", epShortId(gwepBefore6), err)
			}
			defer func() {
				if retErr != nil {
					if err := gwepBefore6.programExternalConnectivity(ctx, gwepBefore4, gwepBefore6); err != nil {
						log.G(ctx).WithFields(log.Fields{
							"error":     err,
							"restoreEp": epShortId(gwepBefore6),
						}).Errorf("Failed to restore external IPv6 connectivity")
					}
				}
			}()
		}
	}

	// Tell the new endpoint whether it's a gateway.
	if err := ep.programExternalConnectivity(ctx, gwepAfter4, gwepAfter6); err != nil {
		return err
	}

	return nil
}

func (ep *Endpoint) programExternalConnectivity(ctx context.Context, gwep4, gwep6 *Endpoint) error {
	n, err := ep.getNetworkFromStore()
	if err != nil {
		return types.InternalErrorf("failed to get network from store for programming external connectivity: %v", err)
	}
	d, err := n.driver(true)
	if err != nil {
		return types.InternalErrorf("failed to get driver for programming external connectivity: %v", err)
	}
	if ecd, ok := d.(driverapi.ExtConner); ok {
		log.G(ctx).WithFields(log.Fields{
			"ep":   ep.Name(),
			"epid": epShortId(ep),
			"gw4":  epShortId(gwep4),
			"gw6":  epShortId(gwep6),
		}).Debug("Programming external connectivity on endpoint")
		if err := ecd.ProgramExternalConnectivity(context.WithoutCancel(ctx), n.ID(), ep.ID(), epId(gwep4), epId(gwep6)); err != nil {
			return types.InternalErrorf("driver failed programming external connectivity on endpoint %s (%s): %v",
				ep.Name(), ep.ID(), err)
		}
	}
	return nil
}

// AddEphemeralPorts publishes additional ports on this endpoint without
// recreating it. The endpoint must be joined to a sandbox. It's only supported
// by drivers that implement [driverapi.PortManager] (currently the bridge
// driver); other drivers return a not-implemented error.
//
// The added ports are not persisted; they do not survive a daemon restart.
// Because they aren't persisted, the driver also can't reconstruct them if the
// endpoint's gateway modes change (e.g. it gains or loses an IPv6 gateway):
// there's no stored configuration to re-derive them from, so they aren't
// re-applied for a newly-active mode. The caller therefore owns re-applying
// them, and this is intended for endpoints whose gateway modes are stable for
// their lifetime - such as the Swarm ingress load balancer's docker_gwbridge
// gateway endpoint.
//
// Each [types.PublishedPort] maps one host port to the endpoint port that
// receives it. That mapping is the caller's to declare: this API has no opinion
// on how the two relate, and callers with a port model of their own are
// responsible for translating it (see addIngressPorts for the Swarm ingress
// mapping). A host-port range can't be expressed, and wouldn't have any
// allocation semantics on this path.
func (ep *Endpoint) AddEphemeralPorts(ctx context.Context, ports []types.PublishedPort) error {
	return ep.modifyPorts(context.WithoutCancel(ctx), ports, driverapi.PortManager.AddEphemeralPorts)
}

// DelEphemeralPorts unpublishes ports previously published on this endpoint via
// [Endpoint.AddEphemeralPorts]. The endpoint must be joined to a sandbox. Ports
// that aren't currently published are ignored.
func (ep *Endpoint) DelEphemeralPorts(ctx context.Context, ports []types.PublishedPort) error {
	return ep.modifyPorts(context.WithoutCancel(ctx), ports, driverapi.PortManager.DelEphemeralPorts)
}

func (ep *Endpoint) modifyPorts(ctx context.Context, ports []types.PublishedPort, fn func(driverapi.PortManager, context.Context, string, string, []types.PublishedPort) error) error {
	if len(ports) == 0 {
		return nil
	}
	if _, ok := ep.getSandbox(); !ok {
		return types.InvalidParameterErrorf("endpoint %s (%.7s) is not attached to a sandbox", ep.Name(), ep.ID())
	}
	n, err := ep.getNetworkFromStore()
	if err != nil {
		return types.InternalErrorf("failed to get network from store for changing published ports: %v", err)
	}
	d, err := n.driver(true)
	if err != nil {
		return types.InternalErrorf("failed to get driver for changing published ports: %v", err)
	}
	pm, ok := d.(driverapi.PortManager)
	if !ok {
		return types.NotImplementedErrorf("network driver %q does not support changing published ports on a running endpoint", n.Type())
	}

	return fn(pm, ctx, n.ID(), ep.ID(), ports)
}

func (ep *Endpoint) rename(name string) error {
	ep.mu.Lock()
	ep.name = name
	ep.mu.Unlock()

	// Update the store with the updated name
	if err := ep.getNetwork().controller.storeEndpoint(context.TODO(), ep); err != nil {
		return err
	}

	return nil
}

func (ep *Endpoint) UpdateDNSNames(dnsNames []string) error {
	nw := ep.getNetwork()
	c := nw.controller
	sb, ok := ep.getSandbox()
	if !ok {
		log.G(context.TODO()).WithFields(log.Fields{
			"sandboxID":  ep.sandboxID,
			"endpointID": ep.ID(),
		}).Warn("DNSNames update aborted, sandbox is not present anymore")
		return nil
	}

	if c.isAgent() {
		if err := ep.deleteServiceInfoFromCluster(sb, true, "UpdateDNSNames"); err != nil {
			return types.InternalErrorf("could not delete service state for endpoint %s from cluster on UpdateDNSNames: %v", ep.Name(), err)
		}

		ep.dnsNames = dnsNames
		if err := ep.addServiceInfoToCluster(sb); err != nil {
			return types.InternalErrorf("could not add service state for endpoint %s to cluster on UpdateDNSNames: %v", ep.Name(), err)
		}
	} else {
		nw.updateSvcRecord(context.WithoutCancel(context.TODO()), ep, false)

		ep.dnsNames = dnsNames
		nw.updateSvcRecord(context.WithoutCancel(context.TODO()), ep, true)
	}

	// Update the store with the updated name
	if err := c.storeEndpoint(context.TODO(), ep); err != nil {
		return err
	}

	return nil
}

func (ep *Endpoint) hasInterface(iName string) bool {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	return ep.iface != nil && ep.iface.srcName == iName
}

// Leave detaches the network resources populated in the sandbox.
func (ep *Endpoint) Leave(ctx context.Context, sb *Sandbox) error {
	if sb == nil || sb.ID() == "" || sb.Key() == "" {
		return types.InvalidParameterErrorf("invalid Sandbox passed to endpoint leave: %v", sb)
	}

	sb.joinLeaveMu.Lock()
	defer sb.joinLeaveMu.Unlock()

	n, err := ep.getNetworkFromStore()
	if err != nil {
		return fmt.Errorf("failed to get network from store during leave: %v", err)
	}

	storedEp, err := n.getEndpointFromStore(ep.ID())
	if err != nil {
		return fmt.Errorf("failed to get endpoint from store during leave: %v", err)
	}

	return storedEp.sbLeave(ctx, sb, n, false)
}

func (ep *Endpoint) sbLeave(ctx context.Context, sb *Sandbox, n *Network, force bool) error {
	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(log.Fields{
		"nid": stringid.TruncateID(n.ID()),
		"net": n.Name(),
		"eid": epShortId(ep),
		"ep":  ep.Name(),
		"sid": stringid.TruncateID(sb.ID()),
		"cid": stringid.TruncateID(sb.ContainerID()),
	}))

	sb.mu.Lock()
	sbInDelete := sb.inDelete
	sb.mu.Unlock()
	ep.mu.Lock()
	sid := ep.sandboxID
	ep.mu.Unlock()

	if sid == "" {
		return types.ForbiddenErrorf("cannot leave endpoint with no attached sandbox")
	}
	if sid != sb.ID() {
		return types.ForbiddenErrorf("unexpected sandbox ID in leave request. Expected %s. Got %s", ep.sandboxID, sb.ID())
	}

	d, err := n.driver(!force)
	if err != nil {
		return fmt.Errorf("failed to get driver during endpoint leave: %v", err)
	}

	ep.mu.Lock()
	ep.sandboxID = ""
	ep.network = n
	ep.mu.Unlock()

	if d != nil {
		if ecd, ok := d.(driverapi.ExtConner); ok {
			if err := ecd.ProgramExternalConnectivity(context.WithoutCancel(ctx), n.ID(), ep.ID(), "", ""); err != nil {
				log.G(ctx).WithError(err).Warn("driver failed revoking external connectivity on endpoint")
			}
		}

		if err := d.Leave(n.id, ep.id); err != nil {
			if _, ok := err.(types.MaskableError); !ok {
				log.G(ctx).WithError(err).Warn("driver error disconnecting container")
			}
		}
	}

	if err := ep.deleteServiceInfoFromCluster(sb, true, "sbLeave"); err != nil {
		log.G(ctx).WithError(err).Warn("Failed to clean up service info on container disconnect")
	}

	if err := deleteEpFromResolver(ep.Name(), ep.iface, n.Resolvers()); err != nil {
		log.G(ctx).WithError(err).Warn("Failed to clean up resolver info on container disconnect")
	}

	// Capture the addresses that were added to the container's /etc/hosts here,
	// before the endpoint is deleted, so that they can be removed from /etc/hosts.
	etcHostsAddrs := ep.getEtcHostsAddrs()
	// Before removing the Endpoint from the Sandbox's list of endpoints, check whether
	// it's acting as a gateway so that new gateways can be selected if it is. If the
	// sandbox is being deleted, skip the check as no new gateway will be needed.
	needNewGwEp := !sbInDelete && sb.isGatewayEndpoint(ep.id)

	// Remove the sb's references to ep.
	sb.mu.Lock()
	osSbox := sb.osSbox
	delete(sb.populatedEndpoints, ep.id)
	delete(sb.epPriority, ep.id)
	sb.endpoints = slices.DeleteFunc(sb.endpoints, func(other *Endpoint) bool { return other.id == ep.id })
	sb.mu.Unlock()

	// Delete interfaces, routes etc. from the OS.
	if osSbox != nil {
		releaseOSSboxResources(osSbox, ep)

		// Even if the interface was initially created in the container's namespace, it's
		// now been moved out. When a legacy link is deleted, the Endpoint is removed and
		// then re-added to the Sandbox. So, to make sure the re-add works, note that the
		// interface is now outside the container's netns.
		ep.iface.createdInContainer = false
	}

	// Update gateway / static routes if the ep was the gateway.
	var gwepAfter4, gwepAfter6 *Endpoint
	if needNewGwEp {
		gwepAfter4, gwepAfter6 = sb.getGatewayEndpoint()
		if err := sb.updateGateway(gwepAfter4, gwepAfter6); err != nil {
			// Don't return an error here without adding proper rollback of the work done above.
			// See https://github.com/moby/moby/issues/51578
			log.G(ctx).WithFields(log.Fields{
				"gw4":   epShortId(gwepAfter4),
				"gw6":   epShortId(gwepAfter6),
				"error": err,
			}).Warn("Configuring gateway after network disconnect")
		}
	}

	var errs []error

	// Update the store about the sandbox detach only after we
	// have completed sb.clearNetworkResources above to avoid
	// spurious logs when cleaning up the sandbox when the daemon
	// ungracefully exits and restarts before completing sandbox
	// detach but after store has been updated.
	if err := n.controller.storeEndpoint(ctx, ep); err != nil {
		errs = append(errs, fmt.Errorf("failed to store endpoint: %w", err))
		// Do not return early and continue with deleting driver info from the
		// cluster (NetworkDB) so that remote nodes learn about the endpoint
		// deletion and don't accumulate stale PERMANENT ARP/FDB entries in
		// overlay network namespaces.
	}

	if err := ep.deleteDriverInfoFromCluster(); err != nil {
		log.G(ctx).WithError(err).Error("Failed to delete endpoint state for endpoint from cluster")
	}

	// When a container is connected to a network, it gets /etc/hosts
	// entries for its addresses on that network. So, when it's connected
	// to two networks, it has a hosts entry for each. For example, if
	// the hostname is the default short-id, and it's connected to two
	// networks (172.19.0.0/16 and 172.20.0.0/17, plus IPv6 address for
	// each), the hosts file might include:
	//
	//   172.19.0.2	4b92a573912d
	//   fd8c:c894:d68::2	4b92a573912d
	//   172.20.0.2	4b92a573912d
	//   fd8c:c894:d68:1::2	4b92a573912d
	//
	// If the container is disconnected from 172.19.0.2, only remove
	// the hosts entries with addresses on that network.
	sb.deleteHostsEntries(etcHostsAddrs)

	if !sbInDelete && sb.needDefaultGW() && sb.getEndpointInGWNetwork() == nil {
		if err := sb.setupDefaultGW(); err != nil {
			errs = append(errs, fmt.Errorf("failed to set default gateway: %w", err))
		}
	}

	// Disable upstream forwarding if the sandbox lost external connectivity.
	if sb.resolver != nil {
		sb.resolver.SetForwardingPolicy(sb.hasExternalAccess())
	}

	// Configure the endpoints that now provide external connectivity for the sandbox
	// if endpoints have been selected.
	if gwepAfter4 != nil {
		if err := gwepAfter4.programExternalConnectivity(ctx, gwepAfter4, gwepAfter6); err != nil {
			log.G(ctx).WithError(err).Error("Failed to set IPv4 gateway")
		}
	}
	if gwepAfter6 != nil && gwepAfter6 != gwepAfter4 {
		if err := gwepAfter6.programExternalConnectivity(ctx, gwepAfter4, gwepAfter6); err != nil {
			log.G(ctx).WithError(err).Error("Failed to set IPv6 gateway")
		}
	}

	if !sb.needDefaultGW() {
		if err := sb.clearDefaultGW(); err != nil {
			log.G(ctx).WithFields(log.Fields{
				"error": err,
				"sid":   sb.ID(),
				"cid":   sb.ContainerID(),
			}).Warn("Failure while disconnecting sandbox from gateway network")
		}
	}

	return errors.Join(errs...)
}

// Delete deletes and detaches this endpoint from the network.
func (ep *Endpoint) Delete(ctx context.Context, force bool) error {
	var err error
	n, err := ep.getNetworkFromStore()
	if err != nil {
		return fmt.Errorf("failed to get network during Delete: %v", err)
	}

	ep, err = n.getEndpointFromStore(ep.ID())
	if err != nil {
		return fmt.Errorf("failed to get endpoint from store during Delete: %v", err)
	}

	ep.mu.Lock()
	epid := ep.id
	name := ep.name
	sbid := ep.sandboxID
	ep.mu.Unlock()

	if sb, _ := n.controller.SandboxByID(sbid); sb != nil {
		if !force {
			return &ActiveContainerError{name: name, id: epid}
		}
		func() {
			// Make sure this Delete isn't racing a Join/Leave/Delete that might also be
			// updating the Sandbox's selection of gateway endpoints.
			sb.joinLeaveMu.Lock()
			defer sb.joinLeaveMu.Unlock()

			if e := ep.sbLeave(context.WithoutCancel(ctx), sb, n, force); e != nil {
				log.G(ctx).Warnf("failed to leave sandbox for endpoint %s : %v", name, e)
			}
		}()
	}

	if err = n.controller.deleteStoredEndpoint(ep); err != nil {
		return err
	}

	defer func() {
		if err != nil && !force {
			ep.dbExists = false
			if e := n.controller.storeEndpoint(context.WithoutCancel(ctx), ep); e != nil {
				log.G(ctx).Warnf("failed to recreate endpoint in store %s : %v", name, e)
			}
		}
	}()

	if !n.controller.isSwarmNode() || n.Scope() != scope.Swarm || !n.driverIsMultihost() {
		n.updateSvcRecord(context.WithoutCancel(ctx), ep, false)
	}

	if err = ep.deleteEndpoint(force); err != nil && !force {
		return err
	}

	ep.releaseIPAddresses()

	return nil
}

func (ep *Endpoint) deleteEndpoint(force bool) error {
	ep.mu.Lock()
	n := ep.network
	name := ep.name
	epid := ep.id
	ep.mu.Unlock()

	driver, err := n.driver(!force)
	if err != nil {
		return fmt.Errorf("failed to delete endpoint: %v", err)
	}

	if driver == nil {
		return nil
	}

	if err := driver.DeleteEndpoint(n.id, epid); err != nil {
		if cerrdefs.IsPermissionDenied(err) {
			return err
		}

		if _, ok := err.(types.MaskableError); !ok {
			log.G(context.TODO()).Warnf("driver error deleting endpoint %s : %v", name, err)
		}
	}

	return nil
}

func (ep *Endpoint) getSandbox() (*Sandbox, bool) {
	c := ep.network.controller
	ep.mu.Lock()
	sid := ep.sandboxID
	ep.mu.Unlock()

	c.mu.Lock()
	ps, ok := c.sandboxes[sid]
	c.mu.Unlock()

	return ps, ok
}

// Return a list of this endpoint's addresses to add to '/etc/hosts'.
func (ep *Endpoint) getEtcHostsAddrs() []netip.Addr {
	ep.mu.Lock()
	defer ep.mu.Unlock()

	// Do not update hosts file with internal network's endpoint IP
	if n := ep.network; n == nil || n.ingress || n.Name() == libnGWNetwork {
		return nil
	}

	var addresses []netip.Addr
	if ep.iface.addr != nil {
		if addr, ok := netip.AddrFromSlice(ep.iface.addr.IP); ok {
			addresses = append(addresses, addr.Unmap())
		}
	}
	if ep.iface.addrv6 != nil {
		if addr, ok := netip.AddrFromSlice(ep.iface.addrv6.IP); ok {
			addresses = append(addresses, addr)
		}
	}
	return addresses
}

// EndpointOptionGeneric function returns an option setter for a Generic option defined
// in a Dictionary of Key-Value pair
func EndpointOptionGeneric(generic map[string]any) EndpointOption {
	return func(ep *Endpoint) {
		maps.Copy(ep.generic, generic)
	}
}

var (
	linkLocalMask     = net.CIDRMask(16, 32)
	linkLocalMaskIPv6 = net.CIDRMask(64, 128)
)

// CreateOptionIPAM function returns an option setter for the ipam configuration for this endpoint
func CreateOptionIPAM(ipV4, ipV6 net.IP, llIPs []net.IP) EndpointOption {
	return func(ep *Endpoint) {
		ep.prefAddress = ipV4
		ep.prefAddressV6 = ipV6
		if len(llIPs) != 0 {
			for _, ip := range llIPs {
				nw := &net.IPNet{IP: ip, Mask: linkLocalMask}
				if ip.To4() == nil {
					nw.Mask = linkLocalMaskIPv6
				}
				ep.iface.llAddrs = append(ep.iface.llAddrs, nw)
			}
		}
	}
}

// CreateOptionExposedPorts function returns an option setter for the container exposed
// ports option to be passed to [Network.CreateEndpoint] method.
func CreateOptionExposedPorts(exposedPorts []types.TransportPort) EndpointOption {
	return func(ep *Endpoint) {
		// Defensive copy
		eps := make([]types.TransportPort, len(exposedPorts))
		copy(eps, exposedPorts)
		// Store endpoint label and in generic because driver needs it
		ep.exposedPorts = eps
		ep.generic[netlabel.ExposedPorts] = eps
	}
}

// CreateOptionPortMapping function returns an option setter for the mapping
// ports option to be passed to [Network.CreateEndpoint] method.
func CreateOptionPortMapping(portBindings []types.PortBinding) EndpointOption {
	return func(ep *Endpoint) {
		// Store a copy of the bindings as generic data to pass to the driver
		pbs := make([]types.PortBinding, len(portBindings))
		copy(pbs, portBindings)
		ep.generic[netlabel.PortMap] = pbs
	}
}

// CreateOptionDNS function returns an option setter for dns entry option to
// be passed to container Create method.
func CreateOptionDNS(dns []string) EndpointOption {
	return func(ep *Endpoint) {
		ep.generic[netlabel.DNSServers] = dns
	}
}

// CreateOptionDNSNames specifies the list of (non fully qualified) DNS names associated to an endpoint. These will be
// used to populate the embedded DNS server. Order matters: first name will be used to generate PTR records.
func CreateOptionDNSNames(names []string) EndpointOption {
	return func(ep *Endpoint) {
		ep.dnsNames = names
	}
}

// CreateOptionDisableResolution function returns an option setter to indicate
// this endpoint doesn't want embedded DNS server functionality
func CreateOptionDisableResolution() EndpointOption {
	return func(ep *Endpoint) {
		ep.disableResolution = true
	}
}

// CreateOptionDisableIPv6 prevents allocation of an IPv6 address/gateway, even
// if the container is connected to an IPv6-enabled network.
func CreateOptionDisableIPv6() EndpointOption {
	return func(ep *Endpoint) {
		ep.disableIPv6 = true
	}
}

// CreateOptionAlias function returns an option setter for setting endpoint alias
func CreateOptionAlias(name string, alias string) EndpointOption {
	return func(ep *Endpoint) {
		if ep.aliases == nil {
			ep.aliases = make(map[string]string)
		}
		ep.aliases[alias] = name
	}
}

// CreateOptionService function returns an option setter for setting service binding configuration
func CreateOptionService(name, id string, vip net.IP, ingressPorts []*PortConfig, aliases []string) EndpointOption {
	return func(ep *Endpoint) {
		ep.svcName = name
		ep.svcID = id
		ep.virtualIP = vip
		ep.ingressPorts = ingressPorts
		ep.svcAliases = aliases
	}
}

// CreateOptionLoadBalancer function returns an option setter for denoting the endpoint is a load balancer for a network
func CreateOptionLoadBalancer() EndpointOption {
	return func(ep *Endpoint) {
		ep.loadBalancer = true
	}
}

// JoinOptionPriority function returns an option setter for priority option to
// be passed to the endpoint.Join() method.
func JoinOptionPriority(prio int) EndpointOption {
	return func(ep *Endpoint) {
		// ep lock already acquired
		c := ep.network.controller
		c.mu.Lock()
		sb, ok := c.sandboxes[ep.sandboxID]
		c.mu.Unlock()
		if !ok {
			log.G(context.TODO()).Errorf("Could not set endpoint priority value during Join to endpoint %s: No sandbox id present in endpoint", ep.id)
			return
		}
		sb.epPriority[ep.id] = prio
	}
}

func WithNetnsPath(path string) EndpointOption {
	return func(ep *Endpoint) {
		ep.iface.netnsPath = path
	}
}

func (ep *Endpoint) assignAddress(ipam ipamapi.Ipam, assignIPv4, assignIPv6 bool) error {
	n := ep.getNetwork()
	if n.hasSpecialDriver() {
		return nil
	}

	log.G(context.TODO()).Debugf("Assigning addresses for endpoint %s's interface on network %s", ep.Name(), n.Name())

	if assignIPv4 {
		if err := ep.assignAddressVersion(4, ipam); err != nil {
			return err
		}
	}

	if assignIPv6 {
		if err := ep.assignAddressVersion(6, ipam); err != nil {
			return err
		}
	}

	return nil
}

func (ep *Endpoint) assignAddressVersion(ipVer int, ipam ipamapi.Ipam) error {
	var (
		poolID  *string
		address **net.IPNet
		prefAdd net.IP
		progAdd net.IP
	)

	n := ep.getNetwork()
	switch ipVer {
	case 4:
		poolID = &ep.iface.v4PoolID
		address = &ep.iface.addr
		prefAdd = ep.prefAddress
	case 6:
		poolID = &ep.iface.v6PoolID
		address = &ep.iface.addrv6
		prefAdd = ep.prefAddressV6
	default:
		return types.InternalErrorf("incorrect ip version number passed: %d", ipVer)
	}

	ipInfo := n.getIPInfo(ipVer)
	if len(ipInfo) == 0 {
		return fmt.Errorf("no IPv%d information available for endpoint %s", ipVer, ep.Name())
	}

	// The address to program may be chosen by the user or by the network driver in one specific
	// case to support backward compatibility with `docker daemon --fixed-cidrv6` use case
	if prefAdd != nil {
		progAdd = prefAdd
	} else if *address != nil {
		progAdd = (*address).IP
	}

	for _, d := range ipInfo {
		if progAdd != nil && !d.Pool.Contains(progAdd) {
			continue
		}
		addr, _, err := ipam.RequestAddress(d.PoolID, progAdd, ep.ipamOptions)
		if err == nil {
			ep.mu.Lock()
			*address = addr
			*poolID = d.PoolID
			ep.mu.Unlock()
			return nil
		}
		if !errors.Is(err, ipamapi.ErrNoAvailableIPs) || progAdd != nil {
			return err
		}
	}
	if progAdd != nil {
		return types.InvalidParameterErrorf("invalid address %s: It does not belong to any of this network's subnets", prefAdd)
	}
	return fmt.Errorf("no available IPv%d addresses on this network's address pools: %s (%s)", ipVer, n.Name(), n.ID())
}

func (ep *Endpoint) releaseIPAddresses() {
	n := ep.getNetwork()
	if n.hasSpecialDriver() {
		return
	}

	log.G(context.TODO()).Debugf("Releasing addresses for endpoint %s's interface on network %s", ep.Name(), n.Name())

	ipam, _, err := n.controller.getIPAMDriver(n.ipamType)
	if err != nil {
		log.G(context.TODO()).Warnf("Failed to retrieve ipam driver to release interface address on delete of endpoint %s (%s): %v", ep.Name(), ep.ID(), err)
		return
	}

	if ep.iface.addr != nil {
		if err := ipam.ReleaseAddress(ep.iface.v4PoolID, ep.iface.addr.IP); err != nil {
			log.G(context.TODO()).Warnf("Failed to release ip address %s on delete of endpoint %s (%s): %v", ep.iface.addr.IP, ep.Name(), ep.ID(), err)
		}
	}

	if ep.iface.addrv6 != nil {
		if err := ipam.ReleaseAddress(ep.iface.v6PoolID, ep.iface.addrv6.IP); err != nil {
			log.G(context.TODO()).Warnf("Failed to release ip address %s on delete of endpoint %s (%s): %v", ep.iface.addrv6.IP, ep.Name(), ep.ID(), err)
		}
	}
}

func (ep *Endpoint) releaseIPv6Address(ctx context.Context) error {
	n := ep.network
	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(log.Fields{
		"net": n.Name(),
		"ep":  ep.name,
		"ip":  ep.iface.addrv6,
	}))

	if ep.iface.addrv6 == nil || n.hasSpecialDriver() {
		return nil
	}

	log.G(ctx).Debug("Releasing IPv6 address for endpoint")

	ipam, _, err := n.controller.getIPAMDriver(n.ipamType)
	if err != nil {
		log.G(ctx).WithError(err).Warn("Failed to retrieve ipam driver to release IPv6 address")
		return err
	}

	if err := ipam.ReleaseAddress(ep.iface.v6PoolID, ep.iface.addrv6.IP); err != nil {
		log.G(ctx).WithError(err).Warn("Failed to release IPv6 address")
		return err
	}

	ep.iface.addrv6 = nil
	if ep.joinInfo != nil {
		ep.joinInfo.gw6 = nil
	}

	d, err := n.driver(true)
	if err != nil {
		return fmt.Errorf("fetching driver to release IPv6 address: %v", err)
	}
	if dr, ok := d.(driverapi.IPv6Releaser); ok {
		if err := dr.ReleaseIPv6(ctx, n.id, ep.id); err != nil {
			return fmt.Errorf("releasing IPv6 address: %v", err)
		}
	}

	if err := ep.network.controller.updateToStore(ctx, ep); err != nil {
		return err
	}

	return nil
}

func (c *Controller) cleanupLocalEndpoints() error {
	// Get used endpoints
	eps := make(map[string]any)
	for _, sb := range c.sandboxes {
		for _, ep := range sb.endpoints {
			eps[ep.id] = true
		}
	}
	nl, err := c.getNetworks()
	if err != nil {
		return fmt.Errorf("could not get list of networks: %v", err)
	}

	for _, n := range nl {
		if n.ConfigOnly() {
			continue
		}
		epl, err := n.getEndpointsFromStore()
		if err != nil {
			log.G(context.TODO()).Warnf("Could not get list of endpoints in network %s during endpoint cleanup: %v", n.name, err)
			continue
		}

		for _, ep := range epl {
			if _, ok := eps[ep.id]; ok {
				continue
			}
			log.G(context.TODO()).Infof("Removing stale endpoint %s (%s)", ep.name, ep.id)
			if err := ep.Delete(context.WithoutCancel(context.TODO()), true); err != nil {
				log.G(context.TODO()).Warnf("Could not delete local endpoint %s during endpoint cleanup: %v", ep.name, err)
			}
		}
	}

	return nil
}
