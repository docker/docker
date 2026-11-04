package libnetwork

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/libnetwork/osl"
	"github.com/moby/moby/v2/daemon/libnetwork/scope"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// SandboxOption is an option setter function type used to pass various options to
// NewNetContainer method. The various setter functions of type SandboxOption are
// provided by libnetwork, they look like ContainerOptionXXXX(...)
type SandboxOption func(sb *Sandbox)

func (sb *Sandbox) processOptions(options ...SandboxOption) {
	for _, opt := range options {
		if opt != nil {
			opt(sb)
		}
	}
}

// Sandbox provides the control over the network container entity.
// It is a one to one mapping with the container.
type Sandbox struct {
	id              string
	containerID     string
	config          containerConfig
	extDNS          []extDNSEntry
	osSbox          *osl.Namespace
	controller      *Controller
	resolver        *Resolver
	resolverOnce    sync.Once
	dbIndex         uint64
	dbExists        bool
	isStub          bool
	inDelete        bool
	ingress         bool
	ndots           int
	oslTypes        []osl.SandboxType // slice of properties of this sandbox
	loadBalancerNID string            // NID that this SB is a load balancer for
	mu              sync.Mutex

	// joinLeaveMu is required as well as mu to modify the following fields,
	// acquire joinLeaveMu first, and keep it at-least until gateway changes
	// have been applied following updates to endpoints.
	//
	// mu is required to access these fields.
	joinLeaveMu        sync.Mutex
	endpoints          []*Endpoint
	epPriority         map[string]int
	populatedEndpoints map[string]struct{}

	// This mutex is used to serialize service related operation for an endpoint
	// The lock is here because the endpoint is saved into the store so is not unique
	service sync.Mutex
}

// These are the container configs used to customize container /etc/hosts file.
type hostsPathConfig struct {
	hostName        string
	domainName      string
	hostsPath       string
	originHostsPath string
	extraHosts      []extraHost
}

type extraHost struct {
	name string
	IP   netip.Addr
}

// These are the container configs used to customize container /etc/resolv.conf file.
type resolvConfPathConfig struct {
	resolvConfPath       string
	originResolvConfPath string
	resolvConfHashFile   string
	dnsList              []netip.Addr
	dnsSearchList        []string
	dnsOptionsList       []string
}

type containerConfig struct {
	hostsPathConfig
	resolvConfPathConfig
	generic           map[string]any
	useDefaultSandBox bool
	useExternalKey    bool
	exposedPorts      []types.TransportPort
}

// ID returns the ID of the sandbox.
func (sb *Sandbox) ID() string {
	return sb.id
}

// ContainerID returns the container id associated to this sandbox.
func (sb *Sandbox) ContainerID() string {
	return sb.containerID
}

// Key returns the sandbox's key.
func (sb *Sandbox) Key() string {
	if sb.config.useDefaultSandBox {
		return sb.controller.sandboxKey("default")
	}
	return sb.controller.sandboxKey(sb.id)
}

// Labels returns the sandbox's labels.
func (sb *Sandbox) Labels() map[string]any {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	opts := make(map[string]any, len(sb.config.generic))
	maps.Copy(opts, sb.config.generic)
	return opts
}

// Delete destroys this container after detaching it from all connected endpoints.
func (sb *Sandbox) Delete(ctx context.Context) error {
	return sb.delete(ctx, false)
}

func (sb *Sandbox) delete(ctx context.Context, force bool) error {
	sb.mu.Lock()
	if sb.inDelete {
		sb.mu.Unlock()
		return types.ForbiddenErrorf("another sandbox delete in progress")
	}
	// Set the inDelete flag. This will ensure that we don't
	// update the store until we have completed all the endpoint
	// leaves and deletes. And when endpoint leaves and deletes
	// are completed then we can finally delete the sandbox object
	// altogether from the data store. If the daemon exits
	// ungracefully in the middle of a sandbox delete this way we
	// will have all the references to the endpoints in the
	// sandbox so that we can clean them up when we restart
	sb.inDelete = true
	sb.mu.Unlock()

	c := sb.controller

	// Detach from all endpoints
	retain := false
	for _, ep := range sb.Endpoints() {
		// gw network endpoint detach and removal are automatic
		if ep.endpointInGWNetwork() && !force {
			continue
		}
		// Retain the sandbox if we can't obtain the network from store.
		if _, err := c.getNetworkFromStore(ep.getNetwork().ID()); err != nil {
			if !c.isSwarmNode() {
				retain = true
			}
			log.G(ctx).Warnf("Failed getting network for ep %s during sandbox %s delete: %v", ep.ID(), sb.ID(), err)
			continue
		}

		if !force {
			if err := ep.Leave(context.WithoutCancel(ctx), sb); err != nil {
				log.G(ctx).Warnf("Failed detaching sandbox %s from endpoint %s: %v\n", sb.ID(), ep.ID(), err)
			}
		}

		if err := ep.Delete(context.WithoutCancel(ctx), force); err != nil {
			log.G(ctx).Warnf("Failed deleting endpoint %s: %v\n", ep.ID(), err)
		}
	}

	if retain {
		sb.mu.Lock()
		sb.inDelete = false
		sb.mu.Unlock()
		return fmt.Errorf("could not cleanup all the endpoints in container %s / sandbox %s", sb.containerID, sb.id)
	}
	if sb.resolver != nil {
		sb.resolver.Stop()
	}

	if sb.osSbox != nil && !sb.config.useDefaultSandBox {
		if err := sb.osSbox.Destroy(); err != nil {
			log.G(ctx).WithError(err).Warn("error destroying network sandbox")
		}
	}

	if err := sb.storeDelete(); err != nil {
		log.G(ctx).Warnf("Failed to delete sandbox %s from store: %v", sb.ID(), err)
	}

	c.mu.Lock()
	if sb.ingress {
		c.ingressSandbox = nil
	}
	delete(c.sandboxes, sb.ID())
	c.mu.Unlock()

	return nil
}

// Rename changes the name of all attached Endpoints.
func (sb *Sandbox) Rename(name string) error {
	var err error

	for _, ep := range sb.Endpoints() {
		if ep.endpointInGWNetwork() {
			continue
		}

		oldName := ep.Name()
		lEp := ep
		if err = ep.rename(name); err != nil {
			break
		}

		defer func() {
			if err != nil {
				if err2 := lEp.rename(oldName); err2 != nil {
					log.G(context.TODO()).WithField("old", oldName).WithField("origError", err).WithError(err2).Error("error renaming sandbox")
				}
			}
		}()
	}

	return err
}

// Refresh leaves all the endpoints, resets and re-applies the options,
// re-joins all the endpoints without destroying the osl sandbox
func (sb *Sandbox) Refresh(ctx context.Context, options ...SandboxOption) error {
	// Store connected endpoints
	epList := sb.Endpoints()

	// Detach from all endpoints
	for _, ep := range epList {
		if err := ep.Leave(context.WithoutCancel(ctx), sb); err != nil {
			log.G(ctx).Warnf("Failed detaching sandbox %s from endpoint %s: %v\n", sb.ID(), ep.ID(), err)
		}
	}

	// Re-apply options
	sb.config = containerConfig{}
	sb.processOptions(options...)

	// Setup discovery files
	if err := sb.setupResolutionFiles(ctx); err != nil {
		return err
	}

	// Re-connect to all endpoints
	for _, ep := range epList {
		if err := ep.Join(context.WithoutCancel(ctx), sb); err != nil {
			log.G(ctx).Warnf("Failed attach sandbox %s to endpoint %s: %v\n", sb.ID(), ep.ID(), err)
		}
	}

	return nil
}

func (sb *Sandbox) UpdateLabels(labels map[string]any) {
	if sb.config.generic == nil {
		sb.config.generic = make(map[string]any, len(labels))
	}
	maps.Copy(sb.config.generic, labels)
}

func (sb *Sandbox) MarshalJSON() ([]byte, error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	// We are just interested in the container ID. This can be expanded to include all of containerInfo if there is a need
	return json.Marshal(sb.id)
}

func (sb *Sandbox) UnmarshalJSON(b []byte) (err error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	var id string
	if err := json.Unmarshal(b, &id); err != nil {
		return err
	}
	sb.id = id
	return nil
}

// Endpoints returns all the endpoints connected to the sandbox.
func (sb *Sandbox) Endpoints() []*Endpoint {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	eps := make([]*Endpoint, len(sb.endpoints))
	copy(eps, sb.endpoints)

	return eps
}

func (sb *Sandbox) addEndpoint(ep *Endpoint) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	i := sort.Search(len(sb.endpoints), func(j int) bool {
		return ep.Less(sb.endpoints[j])
	})
	sb.endpoints = slices.Insert(sb.endpoints, i, ep)
}

func (sb *Sandbox) updateGwPriorityOrdering(ep *Endpoint) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	sb.endpoints = slices.DeleteFunc(sb.endpoints, func(other *Endpoint) bool { return other.id == ep.id })
	i := sort.Search(len(sb.endpoints), func(j int) bool {
		return ep.Less(sb.endpoints[j])
	})
	sb.endpoints = slices.Insert(sb.endpoints, i, ep)
}

func (sb *Sandbox) populateNetworkResources(ctx context.Context, ep *Endpoint) (retErr error) {
	ctx, span := otel.Tracer("").Start(ctx, "libnetwork.Sandbox.populateNetworkResources", trace.WithAttributes(
		attribute.String("endpoint.Name", ep.Name())))
	defer span.End()

	if err := sb.populateNetworkResourcesOS(ctx, ep); err != nil {
		return err
	}

	// Populate DNS records.
	n := ep.getNetwork()
	if !n.getController().isSwarmNode() || n.Scope() != scope.Swarm || !n.driverIsMultihost() {
		n.updateSvcRecord(context.WithoutCancel(ctx), ep, true)
	}

	if err := ep.addDriverInfoToCluster(); err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			if e := ep.deleteDriverInfoFromCluster(); e != nil {
				log.G(ctx).WithError(e).Error("Could not delete endpoint state from cluster on join failure")
			}
		}
	}()

	// Load balancing endpoints should never have a default gateway nor
	// should they alter the status of a network's default gateway
	if !ep.loadBalancer || sb.ingress {
		if sb.needDefaultGW() {
			if sb.getEndpointInGWNetwork() == nil {
				// sb.populateNetworkResources() will be called recursively for the new
				// gateway endpoint. So, it'll set the resolver's forwarding policy.
				return sb.setupDefaultGW()
			}
		} else if err := sb.clearDefaultGW(); err != nil {
			log.G(ctx).WithFields(log.Fields{
				"error": err,
				"sid":   sb.ID(),
				"cid":   sb.ContainerID(),
			}).Warn("Failure while disconnecting sandbox from gateway network")
		}

		// Enable upstream forwarding if the sandbox gained external connectivity.
		if sb.resolver != nil {
			sb.resolver.SetForwardingPolicy(sb.hasExternalAccess())
		}
	}

	return nil
}

func (sb *Sandbox) GetEndpoint(id string) *Endpoint {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	for _, ep := range sb.endpoints {
		if ep.id == id {
			return ep
		}
	}

	return nil
}

func (sb *Sandbox) HandleQueryResp(name string, ip net.IP) {
	for _, ep := range sb.Endpoints() {
		n := ep.getNetwork()
		n.HandleQueryResp(name, ip)
	}
}

func (sb *Sandbox) ResolveIP(ctx context.Context, ip string) string {
	var svc string
	log.G(ctx).Debugf("IP To resolve %v", ip)

	for _, ep := range sb.Endpoints() {
		n := ep.getNetwork()
		svc = n.ResolveIP(ctx, ip)
		if svc != "" {
			return svc
		}
	}

	return svc
}

// ResolveService returns all the backend details about the containers or hosts
// backing a service. Its purpose is to satisfy an SRV query.
func (sb *Sandbox) ResolveService(ctx context.Context, name string) ([]*net.SRV, []net.IP) {
	log.G(ctx).Debugf("Service name To resolve: %v", name)

	// There are DNS implementations that allow SRV queries for names not in
	// the format defined by RFC 2782. Hence specific validations checks are
	// not done
	if parts := strings.SplitN(name, ".", 3); len(parts) < 3 {
		return nil, nil
	}

	for _, ep := range sb.Endpoints() {
		n := ep.getNetwork()

		srv, ip := n.ResolveService(ctx, name)
		if len(srv) > 0 {
			return srv, ip
		}
	}
	return nil, nil
}

func (sb *Sandbox) ResolveName(ctx context.Context, name string, ipType types.IPFamily) ([]net.IP, bool) {
	// Embedded server owns the docker network domain. Resolution should work
	// for both container_name and container_name.network_name
	// We allow '.' in service name and network name. For a name a.b.c.d the
	// following have to tried;
	// {a.b.c.d in the networks container is connected to}
	// {a.b.c in network d},
	// {a.b in network c.d},
	// {a in network b.c.d},

	log.G(ctx).Debugf("Name To resolve: %v", name)
	name = strings.TrimSuffix(name, ".")
	reqName := []string{name}
	networkName := []string{""}

	if strings.Contains(name, ".") {
		var i int
		dup := name
		for {
			if i = strings.LastIndex(dup, "."); i == -1 {
				break
			}
			networkName = append(networkName, name[i+1:])
			reqName = append(reqName, name[:i])

			dup = dup[:i]
		}
	}

	epList := sb.Endpoints()

	// In swarm mode, services with exposed ports are connected to user overlay
	// network, ingress network and docker_gwbridge networks. Name resolution
	// should prioritize returning the VIP/IPs on user overlay network.
	//
	// Re-order the endpoints based on the network-type they're attached to;
	//
	//  1. dynamic networks (user overlay networks)
	//  2. ingress network(s)
	//  3. local networks ("docker_gwbridge")
	if sb.controller.isSwarmNode() {
		sort.Sort(ByNetworkType(epList))
	}

	for i := 0; i < len(reqName); i++ {
		// First check for local container alias
		if ip, ok := sb.resolveName(ctx, reqName[i], networkName[i], epList, true, ipType); ok {
			return ip, true
		}
		// Resolve the actual container name
		if ip, ok := sb.resolveName(ctx, reqName[i], networkName[i], epList, false, ipType); ok {
			return ip, true
		}
	}
	return nil, false
}

func (sb *Sandbox) resolveName(ctx context.Context, nameOrAlias string, networkName string, epList []*Endpoint, lookupAlias bool, ipType types.IPFamily) ([]net.IP, bool) {
	ctx, span := otel.Tracer("").Start(ctx, "Sandbox.resolveName", trace.WithAttributes(
		attribute.String("libnet.resolver.name-or-alias", nameOrAlias),
		attribute.String("libnet.network.name", networkName),
		attribute.Bool("libnet.resolver.alias-lookup", lookupAlias),
		attribute.Int("libnet.resolver.ip-family", int(ipType))))
	defer span.End()

	for _, ep := range epList {
		if lookupAlias && len(ep.aliases) == 0 {
			continue
		}

		nw := ep.getNetwork()
		if networkName != "" && networkName != nw.Name() {
			continue
		}

		name := nameOrAlias
		if lookupAlias {
			ep.mu.Lock()
			alias, ok := ep.aliases[nameOrAlias]
			ep.mu.Unlock()
			if !ok {
				continue
			}
			name = alias
		} else {
			// If it is a regular lookup and if the requested name is an alias
			// don't perform a svc lookup for this endpoint.
			ep.mu.Lock()
			_, ok := ep.aliases[nameOrAlias]
			ep.mu.Unlock()
			if ok {
				continue
			}
		}

		ip, ok := nw.ResolveName(ctx, name, ipType)
		if ok {
			return ip, true
		}
	}
	return nil, false
}

// hasExternalAccess returns true if any of sb's Endpoints appear to have external
// network access.
func (sb *Sandbox) hasExternalAccess() bool {
	for _, ep := range sb.Endpoints() {
		nw := ep.getNetwork()
		if nw.Internal() || nw.Type() == "null" || nw.Type() == "host" {
			continue
		}
		if v4, v6 := ep.hasGatewayOrDefaultRoute(); v4 || v6 {
			return true
		}
	}
	return false
}

// EnableService makes a managed container's service available by adding the
// endpoint to the service load balancer and service discovery.
func (sb *Sandbox) EnableService() (retErr error) {
	log.G(context.TODO()).WithField("container", sb.containerID).Debug("EnableService START")
	defer func() {
		if retErr != nil {
			if err := sb.DisableService(); err != nil {
				log.G(context.TODO()).WithFields(log.Fields{
					"error":     err,
					"origError": retErr,
					"container": sb.containerID,
				}).Error("Error while disabling service after original error")
			}
		}
	}()
	for _, ep := range sb.Endpoints() {
		if !ep.isServiceEnabled() {
			if err := ep.addServiceInfoToCluster(sb); err != nil {
				return fmt.Errorf("could not update state for endpoint %s into cluster: %v", ep.Name(), err)
			}
			ep.enableService()
		}
	}
	log.G(context.TODO()).WithField("container", sb.containerID).Debug("EnableService DONE")
	return nil
}

// DisableService removes a managed container's endpoints from the load balancer
// and service discovery.
func (sb *Sandbox) DisableService() error {
	log.G(context.TODO()).WithField("container", sb.containerID).Debug("DisableService START")
	var failedEps []string
	for _, ep := range sb.Endpoints() {
		if !ep.isServiceEnabled() {
			continue
		}
		if err := ep.deleteServiceInfoFromCluster(sb, false, "DisableService"); err != nil {
			failedEps = append(failedEps, ep.Name())
			log.G(context.TODO()).WithFields(log.Fields{
				"container": sb.containerID,
				"error":     err,
				"ep":        ep.Name(),
			}).Warn("failed to update state for endpoint into cluster")
		}
		ep.disableService()
	}
	log.G(context.TODO()).WithField("container", sb.containerID).Debug("DisableService DONE")
	if len(failedEps) > 0 {
		return fmt.Errorf("failed to disable service on sandbox:%s, for endpoints %s", sb.ID(), strings.Join(failedEps, ","))
	}
	return nil
}

// Less defines an ordering over endpoints, with better candidates for the default
// gateway sorted first.
//
//	<=> Returns true if a < b, false if a > b and advances to next level if a == b
//	ep.prio <=> epj.prio           # 2 < 1
//	ep.gw <=> epj.gw               # non-gw < gw
//	ep.internal <=> epj.internal   # non-internal < internal
//	ep.hasGw <=> epj.hasGw         # (gw4 and gw6) < (gw4 or gw6) < (no gw)
//	ep.name <=> epj.name           # bar < foo
func (ep *Endpoint) Less(epj *Endpoint) bool {
	sbi, _ := ep.getSandbox()
	sbj, _ := epj.getSandbox()

	// Prio defaults to 0
	var prioi, prioj int
	if sbi != nil {
		prioi = sbi.epPriority[ep.ID()]
	}
	if sbj != nil {
		prioj = sbj.epPriority[epj.ID()]
	}
	if prioi != prioj {
		return prioi > prioj
	}

	gwNeti := ep.endpointInGWNetwork()
	gwNetj := epj.endpointInGWNetwork()
	if gwNeti != gwNetj {
		return gwNetj
	}

	inti := ep.getNetwork().Internal()
	intj := epj.getNetwork().Internal()
	if inti != intj {
		return intj
	}

	gwCount := func(ep *Endpoint) int {
		gw4, gw6 := ep.hasGatewayOrDefaultRoute()
		if gw4 && gw6 {
			return 2
		}
		if gw4 || gw6 {
			return 1
		}
		return 0
	}
	gwCounti := gwCount(ep)
	gwCountj := gwCount(epj)
	if gwCounti != gwCountj {
		return gwCounti > gwCountj
	}

	return ep.network.Name() < epj.network.Name()
}

func (sb *Sandbox) Ndots() int {
	return sb.ndots
}
