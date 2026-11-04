/*
Package libnetwork provides the basic functionality and extension points to
create network namespaces and allocate interfaces for containers to use.

	networkType := "bridge"

	// Create a new controller instance
	driverOptions := options.Generic{}
	genericOption := make(map[string]interface{})
	genericOption[netlabel.GenericData] = driverOptions
	controller, err := libnetwork.New(config.OptionDriverConfig(networkType, genericOption))
	if err != nil {
		return
	}

	// Create a network for containers to join.
	// NewNetwork accepts Variadic optional arguments that libnetwork and Drivers can make use of
	network, err := controller.NewNetwork(networkType, "network1", "")
	if err != nil {
		return
	}

	// For each new container: allocate IP and interfaces. The returned network
	// settings will be used for container infos (inspect and such), as well as
	// iptables rules for port publishing. This info is contained or accessible
	// from the returned endpoint.
	ep, err := network.CreateEndpoint(context.TODO(), "Endpoint1")
	if err != nil {
		return
	}

	// Create the sandbox for the container.
	// NewSandbox accepts Variadic optional arguments which libnetwork can use.
	sbx, err := controller.NewSandbox("container1",
		libnetwork.OptionHostname("test"),
		libnetwork.OptionDomainname("example.com"))

	// A sandbox can join the endpoint via the join api.
	err = ep.Join(sbx)
	if err != nil {
		return
	}
*/
package libnetwork

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/containerd/log"
	"github.com/moby/locker"
	"github.com/moby/moby/v2/daemon/internal/otelutil"
	"github.com/moby/moby/v2/daemon/internal/stringid"
	"github.com/moby/moby/v2/daemon/libnetwork/cluster"
	"github.com/moby/moby/v2/daemon/libnetwork/config"
	"github.com/moby/moby/v2/daemon/libnetwork/datastore"
	"github.com/moby/moby/v2/daemon/libnetwork/diagnostic"
	"github.com/moby/moby/v2/daemon/libnetwork/discoverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/driverapi"
	remotedriver "github.com/moby/moby/v2/daemon/libnetwork/drivers/remote"
	"github.com/moby/moby/v2/daemon/libnetwork/drvregistry"
	"github.com/moby/moby/v2/daemon/libnetwork/ipamapi"
	"github.com/moby/moby/v2/daemon/libnetwork/ipams"
	"github.com/moby/moby/v2/daemon/libnetwork/kvstore/memstore"
	"github.com/moby/moby/v2/daemon/libnetwork/netlabel"
	"github.com/moby/moby/v2/daemon/libnetwork/osl"
	"github.com/moby/moby/v2/daemon/libnetwork/scope"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/moby/moby/v2/errdefs"
	"github.com/moby/moby/v2/pkg/plugingetter"
	"github.com/moby/moby/v2/pkg/plugins"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel"
)

// NetworkWalker is a client provided function which will be used to walk the Networks.
// When the function returns true, the walk will stop.
type NetworkWalker func(nw *Network) bool

// Controller manages networks.
type Controller struct {
	drvRegistry      drvregistry.Networks
	ipamRegistry     drvregistry.IPAMs
	pmRegistry       drvregistry.PortMappers
	sandboxes        map[string]*Sandbox
	cfg              *config.Config
	store            *datastore.Store
	svcRecords       map[string]*svcInfo
	serviceBindings  map[serviceKey]*service
	ingressSandbox   *Sandbox
	agent            *nwAgent
	networkLocker    *locker.Locker
	agentInitDone    chan struct{}
	agentStopDone    chan struct{}
	keys             []*types.EncryptionKey
	diagnosticServer *diagnostic.Server
	mu               sync.Mutex

	// networks is an in-memory cache of Network. Do not use this map unless
	// you're sure your code is thread-safe.
	//
	// The data persistence layer is instantiating new Network objects every
	// time it loads an object from its store or in-memory cache. This leads to
	// multiple instances representing the same network to concurrently live in
	// memory. As such, the Network mutex might be ineffective and not
	// correctly protect against data races.
	//
	// If you want to use this map for new or existing code, you need to make
	// sure: 1. the Network object is correctly locked; 2. the lock order
	// between Sandbox, Network and Endpoint is the same as the rest of the
	// code (in order to avoid deadlocks).
	networks map[string]*Network
	// networksMu protects the networks map.
	networksMu sync.Mutex

	// endpoints is an in-memory cache of Endpoint. Do not use this map unless
	// you're sure your code is thread-safe.
	//
	// The data persistence layer is instantiating new Endpoint objects every
	// time it loads an object from its store or in-memory cache. This leads to
	// multiple instances representing the same endpoint to concurrently live
	// in memory. As such, the Endpoint mutex might be ineffective and not
	// correctly protect against data races.
	//
	// If you want to use this map for new or existing code, you need to make
	// sure: 1. the Endpoint object is correctly locked; 2. the lock order
	// between Sandbox, Network and Endpoint is the same as the rest of the
	// code (in order to avoid deadlocks).
	endpoints map[string]*Endpoint
	// endpointsMu protects the endpoints map.
	endpointsMu sync.Mutex

	// FIXME(thaJeztah): defOsSbox is always nil on non-Linux: move these fields to Linux-only files.
	defOsSboxOnce sync.Once
	defOsSbox     *osl.Namespace
}

// New creates a new instance of network controller.
func New(ctx context.Context, cfgOptions ...config.Option) (_ *Controller, retErr error) {
	ctx, span := otel.Tracer("").Start(ctx, "libnetwork.New")
	defer func() {
		otelutil.RecordStatus(span, retErr)
		span.End()
	}()

	cfg := config.New(cfgOptions...)
	kv := cfg.KVStore
	if kv == nil {
		kv = memstore.New()
	}

	c := &Controller{
		cfg:              cfg,
		store:            datastore.New(kv),
		sandboxes:        map[string]*Sandbox{},
		networks:         map[string]*Network{},
		endpoints:        map[string]*Endpoint{},
		svcRecords:       make(map[string]*svcInfo),
		serviceBindings:  make(map[serviceKey]*service),
		agentInitDone:    make(chan struct{}),
		networkLocker:    locker.New(),
		diagnosticServer: diagnostic.New(),
	}

	if err := c.createNetnsDir(); err != nil {
		return nil, err
	}

	if err := c.selectFirewallBackend(); err != nil {
		return nil, err
	}
	c.drvRegistry.Notify = c

	// Register portmappers before network drivers to make sure they can
	// restore existing sandboxes (with port mappings) during their
	// initialization, if the daemon is started in live restore mode.
	if err := registerPortMappers(ctx, &c.pmRegistry, c.cfg); err != nil {
		return nil, err
	}

	// External plugins don't need config passed through daemon. They can
	// bootstrap themselves.
	if err := remotedriver.Register(&c.drvRegistry, c.cfg.PluginGetter); err != nil {
		return nil, err
	}

	if err := registerNetworkDrivers(&c.drvRegistry, c.cfg, c.store, &c.pmRegistry); err != nil {
		return nil, err
	}

	if err := ipams.Register(&c.ipamRegistry, c.cfg.PluginGetter, c.cfg.DefaultAddressPool, nil); err != nil {
		return nil, err
	}

	c.WalkNetworks(func(nw *Network) bool {
		if n := nw; n.hasSpecialDriver() && !n.ConfigOnly() {
			if err := n.getController().addNetwork(ctx, n); err != nil {
				log.G(ctx).Warnf("Failed to populate network %q with driver %q", nw.Name(), nw.Type())
			}
		}
		return false
	})

	// Reserve pools first before doing cleanup. Otherwise the
	// cleanups of endpoint/network and sandbox below will
	// generate many unnecessary warnings
	c.reservePools()

	if err := c.sandboxRestore(c.cfg.ActiveSandboxes); err != nil {
		log.G(ctx).WithError(err).Error("error during sandbox cleanup")
	}

	// Cleanup resources
	if err := c.cleanupLocalEndpoints(); err != nil {
		log.G(ctx).WithError(err).Warnf("error during endpoint cleanup")
	}
	c.networkCleanup()

	c.setupPlatformFirewall()
	return c, nil
}

// SetClusterProvider sets the cluster provider.
func (c *Controller) SetClusterProvider(provider cluster.Provider) {
	var sameProvider bool
	c.mu.Lock()
	// Avoids to spawn multiple goroutine for the same cluster provider
	if c.cfg.ClusterProvider == provider {
		// If the cluster provider is already set, there is already a go routine spawned
		// that is listening for events, so nothing to do here
		sameProvider = true
	} else {
		c.cfg.ClusterProvider = provider
	}
	c.mu.Unlock()

	if provider == nil || sameProvider {
		return
	}
	// We don't want to spawn a new go routine if the previous one did not exit yet
	c.AgentStopWait()
	go c.clusterAgentInit()
}

// SetKeys configures the encryption key for gossip and overlay data path.
func (c *Controller) SetKeys(keys []*types.EncryptionKey) error {
	// libnetwork side of agent depends on the keys. On the first receipt of
	// keys setup the agent. For subsequent key set handle the key change
	subsysKeys := make(map[string]int)
	for _, key := range keys {
		if key.Subsystem != subsysGossip &&
			key.Subsystem != subsysIPSec {
			return errors.New("key received for unrecognized subsystem")
		}
		subsysKeys[key.Subsystem]++
	}
	for s, count := range subsysKeys {
		if count != keyringSize {
			return fmt.Errorf("incorrect number of keys for subsystem %v", s)
		}
	}

	if c.getAgent() == nil {
		c.mu.Lock()
		c.keys = keys
		c.mu.Unlock()
		return nil
	}
	return c.handleKeyChange(keys)
}

func (c *Controller) getAgent() *nwAgent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.agent
}

func (c *Controller) clusterAgentInit() {
	clusterProvider := c.cfg.ClusterProvider
	var keysAvailable bool
	for {
		eventType := <-clusterProvider.ListenClusterEvents()
		// The events: EventSocketChange, EventNodeReady and EventNetworkKeysAvailable are not ordered
		// when all the condition for the agent initialization are met then proceed with it
		switch eventType {
		case cluster.EventNetworkKeysAvailable:
			// Validates that the keys are actually available before starting the initialization
			// This will handle old spurious messages left on the channel
			c.mu.Lock()
			keysAvailable = c.keys != nil
			c.mu.Unlock()
			fallthrough
		case cluster.EventSocketChange, cluster.EventNodeReady:
			if keysAvailable && c.isSwarmNode() {
				c.agentOperationStart()
				if err := c.agentSetup(clusterProvider); err != nil {
					c.agentStopComplete()
				} else {
					c.agentInitComplete()
				}
			}
		case cluster.EventNodeLeave:
			c.agentOperationStart()
			c.mu.Lock()
			c.keys = nil
			c.mu.Unlock()

			// We are leaving the cluster. Make sure we
			// close the gossip so that we stop all
			// incoming gossip updates before cleaning up
			// any remaining service bindings. But before
			// deleting the networks since the networks
			// should still be present when cleaning up
			// service bindings
			c.agentClose()
			c.cleanupServiceDiscovery("")
			c.cleanupServiceBindings("")

			c.agentStopComplete()

			return
		}
	}
}

// AgentInitWait waits for agent initialization to be completed in the controller.
func (c *Controller) AgentInitWait() {
	c.mu.Lock()
	agentInitDone := c.agentInitDone
	c.mu.Unlock()

	if agentInitDone != nil {
		<-agentInitDone
	}
}

// AgentStopWait waits for the Agent stop to be completed in the controller.
func (c *Controller) AgentStopWait() {
	c.mu.Lock()
	agentStopDone := c.agentStopDone
	c.mu.Unlock()
	if agentStopDone != nil {
		<-agentStopDone
	}
}

// agentOperationStart marks the start of an Agent Init or Agent Stop
func (c *Controller) agentOperationStart() {
	c.mu.Lock()
	if c.agentInitDone == nil {
		c.agentInitDone = make(chan struct{})
	}
	if c.agentStopDone == nil {
		c.agentStopDone = make(chan struct{})
	}
	c.mu.Unlock()
}

// agentInitComplete notifies the successful completion of the Agent initialization
func (c *Controller) agentInitComplete() {
	c.mu.Lock()
	if c.agentInitDone != nil {
		close(c.agentInitDone)
		c.agentInitDone = nil
	}
	c.mu.Unlock()
}

// agentStopComplete notifies the successful completion of the Agent stop
func (c *Controller) agentStopComplete() {
	c.mu.Lock()
	if c.agentStopDone != nil {
		close(c.agentStopDone)
		c.agentStopDone = nil
	}
	c.mu.Unlock()
}

// BuiltinDrivers returns the list of builtin network drivers.
func (c *Controller) BuiltinDrivers() []string {
	drivers := []string{}
	c.drvRegistry.WalkDrivers(func(name string, driver driverapi.Driver, capability driverapi.Capability) bool {
		if driver.IsBuiltIn() {
			drivers = append(drivers, name)
		}
		return false
	})
	return drivers
}

// BuiltinIPAMDrivers returns the list of builtin ipam drivers.
func (c *Controller) BuiltinIPAMDrivers() []string {
	drivers := []string{}
	c.ipamRegistry.WalkIPAMs(func(name string, driver ipamapi.Ipam, _ *ipamapi.Capability) bool {
		if driver.IsBuiltIn() {
			drivers = append(drivers, name)
		}
		return false
	})
	return drivers
}

func (c *Controller) processNodeDiscovery(nodes []net.IP, add bool) {
	c.drvRegistry.WalkDrivers(func(name string, driver driverapi.Driver, capability driverapi.Capability) bool {
		if d, ok := driver.(discoverapi.Discover); ok {
			c.pushNodeDiscovery(d, capability, nodes, add)
		}
		return false
	})
}

func (c *Controller) pushNodeDiscovery(d discoverapi.Discover, capability driverapi.Capability, nodes []net.IP, add bool) {
	var self net.IP
	// try swarm-mode config
	if agent := c.getAgent(); agent != nil {
		self = net.ParseIP(agent.advertiseAddr)
	}

	if d == nil || capability.ConnectivityScope != scope.Global || nodes == nil {
		return
	}

	for _, node := range nodes {
		nodeData := discoverapi.NodeDiscoveryData{Address: node.String(), Self: node.Equal(self)}
		var err error
		if add {
			err = d.DiscoverNew(discoverapi.NodeDiscovery, nodeData)
		} else {
			err = d.DiscoverDelete(discoverapi.NodeDiscovery, nodeData)
		}
		if err != nil {
			log.G(context.TODO()).Debugf("discovery notification error: %v", err)
		}
	}
}

// Config returns the bootup configuration for the controller.
func (c *Controller) Config() config.Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cfg == nil {
		return config.Config{}
	}
	return *c.cfg
}

func (c *Controller) isManager() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cfg == nil || c.cfg.ClusterProvider == nil {
		return false
	}
	return c.cfg.ClusterProvider.IsManager()
}

func (c *Controller) isAgent() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cfg == nil || c.cfg.ClusterProvider == nil {
		return false
	}
	return c.cfg.ClusterProvider.IsAgent()
}

func (c *Controller) isSwarmNode() bool {
	return c.isManager() || c.isAgent()
}

func (c *Controller) GetPluginGetter() plugingetter.PluginGetter {
	return c.cfg.PluginGetter
}

func (c *Controller) RegisterDriver(_ string, driver driverapi.Driver, _ driverapi.Capability) error {
	if d, ok := driver.(discoverapi.Discover); ok {
		c.agentDriverNotify(d)
	}
	return nil
}

func (c *Controller) RegisterNetworkAllocator(_ string, _ driverapi.NetworkAllocator) error {
	return nil
}

// XXX  This should be made driver agnostic.  See comment below.
const overlayDSROptionString = "dsr"

// NewNetwork creates a new network of the specified network type. The options
// are network specific and modeled in a generic way.
func (c *Controller) NewNetwork(ctx context.Context, networkType, name string, id string, options ...NetworkOption) (_ *Network, retErr error) {
	if id != "" {
		c.networkLocker.Lock(id)
		defer c.networkLocker.Unlock(id) //nolint:errcheck

		if _, err := c.NetworkByID(id); err == nil {
			return nil, NetworkNameError(id)
		}
	}

	if strings.TrimSpace(name) == "" {
		return nil, types.InvalidParameterErrorf("invalid name: name is empty")
	}

	// Make sure two concurrent calls to this method won't create conflicting
	// networks, otherwise libnetwork will end up in an invalid state.
	if name != "" {
		c.networkLocker.Lock(name)
		defer c.networkLocker.Unlock(name)

		if _, err := c.NetworkByName(name); err == nil {
			return nil, NetworkNameError(name)
		}
	}

	if id == "" {
		id = stringid.GenerateRandomID()
	}

	defaultIpam := defaultIpamForNetworkType(networkType)
	// Construct the network object
	nw := &Network{
		name:             name,
		networkType:      networkType,
		generic:          map[string]any{netlabel.GenericData: make(map[string]string)},
		ipamType:         defaultIpam,
		enableIPv4:       true,
		id:               id,
		created:          time.Now(),
		ctrlr:            c,
		persist:          true,
		drvOnce:          &sync.Once{},
		loadBalancerMode: loadBalancerModeDefault,
	}

	nw.processOptions(options...)
	if err := nw.validateConfiguration(); err != nil {
		return nil, err
	}

	// These variables must be defined here, as declaration would otherwise
	// be skipped by the "goto addToStore"
	var (
		caps driverapi.Capability
		err  error
	)

	// Reset network types, force local scope and skip allocation and
	// plumbing for configuration networks. Reset of the config-only
	// network drivers is needed so that this special network is not
	// usable by old engine versions.
	if nw.configOnly {
		nw.scope = scope.Local
		nw.networkType = "null"
		goto addToStore
	}

	_, caps, err = c.resolveDriver(nw.networkType, true)
	if err != nil {
		return nil, err
	}

	if nw.scope == scope.Local && caps.DataScope == scope.Global {
		return nil, types.ForbiddenErrorf("cannot downgrade network scope for %s networks", networkType)
	}
	if nw.ingress && caps.DataScope != scope.Global {
		return nil, types.ForbiddenErrorf("Ingress network can only be global scope network")
	}

	// From this point on, we need the network specific configuration,
	// which may come from a configuration-only network
	if nw.configFrom != "" {
		configNetwork, err := c.getConfigNetwork(nw.configFrom)
		if err != nil {
			return nil, types.NotFoundErrorf("configuration network %q does not exist", nw.configFrom)
		}
		if err := configNetwork.applyConfigurationTo(nw); err != nil {
			return nil, types.InternalErrorf("Failed to apply configuration: %v", err)
		}
	}

	// At this point the network scope is still unknown if not set by user
	if (caps.DataScope == scope.Global || nw.scope == scope.Swarm) &&
		c.isSwarmNode() && !nw.dynamic {
		if c.isManager() {
			if !nw.enableIPv4 {
				return nil, types.InvalidParameterErrorf("IPv4 cannot be disabled in a Swarm scoped network")
			}
			// For non-distributed controlled environment, globalscoped non-dynamic networks are redirected to Manager
			return nil, ManagerRedirectError(name)
		}
		return nil, types.ForbiddenErrorf("Cannot create a multi-host network from a worker node. Please create the network from a manager node.")
	}

	if nw.scope == scope.Swarm && !c.isSwarmNode() {
		return nil, types.ForbiddenErrorf("cannot create a swarm scoped network when swarm is not active")
	}

	// Make sure we have a driver available for this network type
	// before we allocate anything.
	if d, err := nw.driver(true); err != nil {
		return nil, err
	} else if gac, ok := d.(driverapi.GwAllocChecker); ok {
		// Give the driver a chance to say it doesn't need a gateway IP address.
		nw.skipGwAllocIPv4, nw.skipGwAllocIPv6, err = gac.GetSkipGwAlloc(nw.generic)
		if err != nil {
			return nil, err
		}
	}

	if err := nw.ipamAllocate(); err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			nw.ipamRelease()
		}
	}()
	if err := nw.validateIngressSourceIPPool(); err != nil {
		return nil, err
	}

	// Note from thaJeztah to future code visitors, or "future self".
	//
	// This code was previously assigning the error to the global "err"
	// variable (before it was renamed to "retErr"), but in case of a
	// "MaskableError" did not *return* the error:
	// https://github.com/moby/moby/blob/b325dcbff60a04cedbe40eb627465fc7379d05bf/libnetwork/controller.go#L566-L573
	//
	// Depending on code paths further down, that meant that this error
	// was either overwritten by other errors (and thus not handled in
	// defer statements) or handled (if no other code was overwriting it.
	//
	// I suspect this was a bug (but possible without effect), but it could
	// have been intentional. This logic is confusing at least, and even
	// more so combined with the handling in defer statements that check for
	// both the "err" return AND "skipCfgEpCount":
	// https://github.com/moby/moby/blob/b325dcbff60a04cedbe40eb627465fc7379d05bf/libnetwork/controller.go#L586-L602
	//
	// To save future visitors some time to dig up history:
	//
	// - config-only networks were added in 25082206df465d1c11dd1276a65b4a1dc701bd43
	// - the special error-handling and "skipCfgEpcoung" was added in ddd22a819867faa0cd7d12b0c3fad1099ac3eb26
	// - and updated in 87b082f3659f9ec245ab15d781e6bfffced0af83 to don't use string-matching
	//
	// To cut a long story short: if this broke anything, you know who to blame :)
	if err := c.addNetwork(ctx, nw); err != nil {
		if _, ok := err.(types.MaskableError); !ok {
			return nil, err
		}
	}
	defer func() {
		if retErr != nil {
			if err := nw.deleteNetwork(); err != nil {
				log.G(ctx).Warnf("couldn't roll back driver network on network %s creation failure: %v", nw.name, retErr)
			}
		}
	}()

	// XXX If the driver type is "overlay" check the options for DSR
	// being set.  If so, set the network's load balancing mode to DSR.
	// This should really be done in a network option, but due to
	// time pressure to get this in without adding changes to moby,
	// swarm and CLI, it is being implemented as a driver-specific
	// option.  Unfortunately, drivers can't influence the core
	// "libnetwork.Network" data type.  Hence we need this hack code
	// to implement in this manner.
	if gval, ok := nw.generic[netlabel.GenericData]; ok && nw.networkType == "overlay" {
		optMap := gval.(map[string]string)
		if _, ok := optMap[overlayDSROptionString]; ok {
			nw.loadBalancerMode = loadBalancerModeDSR
		}
	}

addToStore:
	// First store the endpoint count, then the network. To avoid to
	// end up with a datastore containing a network and not an epCnt,
	// in case of an ungraceful shutdown during this function call.
	//
	// TODO(robmry) - remove this once downgrade past 28.1.0 is no longer supported.
	// The endpoint count is no longer used, it's created in the store to make
	// downgrade work, versions older than 28.1.0 expect to read it and error if they
	// can't. The stored count is not maintained, so the downgraded version will
	// always find it's zero (which is usually correct because the daemon had
	// stopped), but older daemons fix it on startup anyway.
	epCnt := &endpointCnt{n: nw}
	if err := c.updateToStore(ctx, epCnt); err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			if err := c.deleteFromStore(epCnt); err != nil {
				log.G(ctx).Warnf("could not rollback from store, epCnt %v on failure (%v): %v", epCnt, retErr, err)
			}
		}
	}()

	if err := c.storeNetwork(ctx, nw); err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			if err := c.deleteStoredNetwork(nw); err != nil {
				log.G(ctx).Warnf("could not rollback from store, network %v on failure (%v): %v", nw, retErr, err)
			}
		}
	}()

	if nw.configOnly {
		return nw, nil
	}

	joinCluster(nw)
	defer func() {
		if retErr != nil {
			nw.cancelDriverWatches()
			if err := nw.leaveCluster(); err != nil {
				log.G(ctx).Warnf("Failed to leave agent cluster on network %s on failure (%v): %v", nw.name, retErr, err)
			}
		}
	}()

	if nw.hasLoadBalancerEndpoint() {
		if err := nw.createLoadBalancerSandbox(); err != nil {
			return nil, err
		}
	}

	return nw, nil
}

var joinCluster NetworkWalker = func(nw *Network) bool {
	if nw.configOnly {
		return false
	}
	if err := nw.joinCluster(); err != nil {
		log.G(context.TODO()).Errorf("Failed to join network %s (%s) into agent cluster: %v", nw.Name(), nw.ID(), err)
	}
	nw.addDriverWatches()
	return false
}

func (c *Controller) reservePools() {
	networks, err := c.getNetworks()
	if err != nil {
		log.G(context.TODO()).Warnf("Could not retrieve networks from local store during ipam allocation for existing networks: %v", err)
		return
	}

	for _, n := range networks {
		if n.configOnly {
			continue
		}
		if !doReplayPoolReserve(n) {
			continue
		}
		// Reserve pools
		if err := n.ipamAllocate(); err != nil {
			log.G(context.TODO()).Warnf("Failed to allocate ipam pool(s) for network %q (%s): %v", n.Name(), n.ID(), err)
		}
		// Reserve existing endpoints' addresses
		ipam, _, err := n.getController().getIPAMDriver(n.ipamType)
		if err != nil {
			log.G(context.TODO()).Warnf("Failed to retrieve ipam driver for network %q (%s) during address reservation", n.Name(), n.ID())
			continue
		}
		epl, err := n.getEndpointsFromStore()
		if err != nil {
			log.G(context.TODO()).Warnf("Failed to retrieve list of current endpoints on network %q (%s)", n.Name(), n.ID())
			continue
		}
		for _, ep := range epl {
			if ep.Iface() == nil {
				log.G(context.TODO()).Warnf("endpoint interface is empty for %q (%s)", ep.Name(), ep.ID())
				continue
			}
			if err := ep.assignAddress(ipam, ep.Iface().Address() != nil, ep.Iface().AddressIPv6() != nil); err != nil {
				log.G(context.TODO()).Warnf("Failed to reserve current address for endpoint %q (%s) on network %q (%s)",
					ep.Name(), ep.ID(), n.Name(), n.ID())
			}
		}
	}
}

func doReplayPoolReserve(n *Network) bool {
	_, caps, err := n.getController().getIPAMDriver(n.ipamType)
	if err != nil {
		log.G(context.TODO()).Warnf("Failed to retrieve ipam driver for network %q (%s): %v", n.Name(), n.ID(), err)
		return false
	}
	return caps.RequiresRequestReplay
}

func (c *Controller) addNetwork(ctx context.Context, n *Network) error {
	d, err := n.driver(true)
	if err != nil {
		return err
	}

	// Create the network
	if err := d.CreateNetwork(ctx, n.id, n.generic, n, n.getIPData(4), n.getIPData(6)); err != nil {
		return err
	}

	n.startResolver()

	return nil
}

// Networks returns the list of Network(s) managed by this controller.
func (c *Controller) Networks(ctx context.Context) []*Network {
	var list []*Network

	for _, n := range c.getNetworksFromStore(ctx) {
		if n.inDelete {
			continue
		}
		list = append(list, n)
	}

	return list
}

// WalkNetworks uses the provided function to walk the Network(s) managed by this controller.
func (c *Controller) WalkNetworks(walker NetworkWalker) {
	if slices.ContainsFunc(c.Networks(context.TODO()), walker) {
		return
	}
}

// NetworkByName returns the Network which has the passed name.
// If not found, the error [ErrNoSuchNetwork] is returned.
func (c *Controller) NetworkByName(name string) (*Network, error) {
	if name == "" {
		return nil, types.InvalidParameterErrorf("invalid name: name is empty")
	}
	var n *Network

	c.WalkNetworks(func(current *Network) bool {
		if current.Name() == name {
			n = current
			return true
		}
		return false
	})

	if n == nil {
		return nil, ErrNoSuchNetwork(name)
	}

	return n, nil
}

// NetworkByID returns the Network which has the passed id.
// If not found, the error [ErrNoSuchNetwork] is returned.
func (c *Controller) NetworkByID(id string) (*Network, error) {
	if id == "" {
		return nil, types.InvalidParameterErrorf("invalid id: id is empty")
	}
	return c.getNetworkFromStore(id)
}

// NewSandbox creates a new sandbox for containerID.
func (c *Controller) NewSandbox(ctx context.Context, containerID string, options ...SandboxOption) (_ *Sandbox, retErr error) {
	if containerID == "" {
		return nil, types.InvalidParameterErrorf("invalid container ID")
	}
	if err := c.requireNetnsDir(); err != nil {
		return nil, err
	}

	ctx, span := otel.Tracer("").Start(ctx, "libnetwork.Controller.NewSandbox")
	defer span.End()

	var sb *Sandbox
	c.mu.Lock()
	for _, s := range c.sandboxes {
		if s.containerID == containerID {
			// If not a stub, then we already have a complete sandbox.
			if !s.isStub {
				sbID := s.ID()
				c.mu.Unlock()
				return nil, types.ForbiddenErrorf("container %s is already present in sandbox %s", containerID, sbID)
			}

			// We already have a stub sandbox from the
			// store. Make use of it so that we don't lose
			// the endpoints from store but reset the
			// isStub flag.
			sb = s
			sb.isStub = false
			break
		}
	}
	c.mu.Unlock()

	// Create sandbox and process options first. Key generation depends on an option
	if sb == nil {
		// TODO(thaJeztah): given that a "containerID" must be unique in the list of sandboxes, is there any reason we're not using containerID as sandbox ID on non-Windows?
		sandboxID := containerID
		if runtime.GOOS != "windows" {
			sandboxID = stringid.GenerateRandomID()
		}
		sb = &Sandbox{
			id:                 sandboxID,
			containerID:        containerID,
			endpoints:          []*Endpoint{},
			epPriority:         map[string]int{},
			populatedEndpoints: map[string]struct{}{},
			config:             containerConfig{},
			controller:         c,
			extDNS:             []extDNSEntry{},
		}
	}

	sb.processOptions(options...)

	c.mu.Lock()
	if sb.ingress && c.ingressSandbox != nil {
		c.mu.Unlock()
		return nil, types.ForbiddenErrorf("ingress sandbox already present")
	}

	if sb.ingress {
		c.ingressSandbox = sb
		sb.id = "ingress_sbox"
	} else if sb.loadBalancerNID != "" {
		sb.id = "lb_" + sb.loadBalancerNID
	}
	c.mu.Unlock()

	defer func() {
		if retErr != nil {
			c.mu.Lock()
			if sb.ingress {
				c.ingressSandbox = nil
			}
			c.mu.Unlock()
		}
	}()

	if err := sb.setupResolutionFiles(ctx); err != nil {
		return nil, err
	}
	if err := c.setupOSLSandbox(sb); err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.sandboxes[sb.id] = sb
	c.mu.Unlock()
	defer func() {
		if retErr != nil {
			c.mu.Lock()
			delete(c.sandboxes, sb.id)
			c.mu.Unlock()
		}
	}()

	if err := sb.storeUpdate(ctx); err != nil {
		return nil, fmt.Errorf("failed to update the store state of sandbox: %v", err)
	}

	return sb, nil
}

// GetSandbox returns the Sandbox which has the passed id.
//
// It returns an [ErrInvalidID] when passing an invalid ID, or an
// [errdefs.ErrNotFound] if no Sandbox was found for the container.
func (c *Controller) GetSandbox(containerID string) (*Sandbox, error) {
	if containerID == "" {
		return nil, types.InvalidParameterErrorf("invalid id: id is empty")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if runtime.GOOS == "windows" {
		// fast-path for Windows, which uses the container ID as sandbox ID.
		if sb := c.sandboxes[containerID]; sb != nil && !sb.isStub {
			return sb, nil
		}
	} else {
		for _, sb := range c.sandboxes {
			if sb.containerID == containerID && !sb.isStub {
				return sb, nil
			}
		}
	}

	return nil, types.NotFoundErrorf("network sandbox for container %s not found", containerID)
}

// SandboxByID returns the Sandbox which has the passed id.
// If not found, a [errdefs.NotFoundError] is returned.
func (c *Controller) SandboxByID(id string) (*Sandbox, error) {
	if id == "" {
		return nil, types.InvalidParameterErrorf("invalid id: id is empty")
	}
	c.mu.Lock()
	s, ok := c.sandboxes[id]
	c.mu.Unlock()
	if !ok {
		return nil, types.NotFoundErrorf("sandbox %s not found", id)
	}
	return s, nil
}

// SandboxDestroy destroys a sandbox given a container ID.
func (c *Controller) SandboxDestroy(ctx context.Context, id string) error {
	var sb *Sandbox
	c.mu.Lock()
	for _, s := range c.sandboxes {
		if s.containerID == id {
			sb = s
			break
		}
	}
	c.mu.Unlock()

	// It is not an error if sandbox is not available
	if sb == nil {
		return nil
	}

	return sb.Delete(ctx)
}

// resolveDriver checks if a driver for the specified network type is available,
// optionally attempting to load the driver if it's not loaded.
func (c *Controller) resolveDriver(name string, load bool) (driverapi.Driver, driverapi.Capability, error) {
	d, capabilities := c.drvRegistry.Driver(name)
	if d != nil {
		return d, capabilities, nil
	}
	if !load {
		// don't fail if driver loading is not required
		//
		// TODO(thaJeztah): can we return a sentinel "not exists" error?
		return nil, driverapi.Capability{}, nil
	}

	err := c.loadDriver(name)
	if err != nil {
		return nil, driverapi.Capability{}, err
	}

	d, capabilities = c.drvRegistry.Driver(name)
	if d == nil {
		return nil, driverapi.Capability{}, fmt.Errorf("could not resolve driver %s in registry", name)
	}
	return d, capabilities, nil
}

func (c *Controller) loadDriver(networkType string) error {
	var err error

	// TODO(thaJeztah): plugingetter.Get ALSO has a fallback to plugins.Get if allowV1PluginsFallback (const) is true.
	if pg := c.GetPluginGetter(); pg != nil {
		_, err = pg.Get(networkType, driverapi.NetworkPluginEndpointType, plugingetter.Lookup)
	} else {
		_, err = plugins.Get(networkType, driverapi.NetworkPluginEndpointType)
	}

	if err != nil {
		if errors.Is(err, plugins.ErrNotFound) {
			return errdefs.NotFound(err)
		}
		return err
	}

	return nil
}

func (c *Controller) loadIPAMDriver(name string) error {
	var err error

	if pg := c.GetPluginGetter(); pg != nil {
		_, err = pg.Get(name, ipamapi.PluginEndpointType, plugingetter.Lookup)
	} else {
		_, err = plugins.Get(name, ipamapi.PluginEndpointType)
	}

	if err != nil {
		if errors.Is(err, plugins.ErrNotFound) {
			return types.NotFoundErrorf("%v", err)
		}
		return err
	}

	return nil
}

func (c *Controller) getIPAMDriver(name string) (ipamapi.Ipam, *ipamapi.Capability, error) {
	id, caps := c.ipamRegistry.IPAM(name)
	if id == nil {
		// Might be a plugin name. Try loading it
		if err := c.loadIPAMDriver(name); err != nil {
			return nil, nil, err
		}

		// Now that we resolved the plugin, try again looking up the registry
		id, caps = c.ipamRegistry.IPAM(name)
		if id == nil {
			return nil, nil, types.InvalidParameterErrorf("invalid ipam driver: %q", name)
		}
	}

	return id, caps, nil
}

// StartDiagnostic starts the network diagnostic server listening on port.
func (c *Controller) StartDiagnostic(port int) {
	c.diagnosticServer.Enable("127.0.0.1", port)
}

// StopDiagnostic stops the network diagnostic server.
func (c *Controller) StopDiagnostic() {
	c.diagnosticServer.Shutdown()
}

// IsDiagnosticEnabled returns true if the diagnostic server is running.
func (c *Controller) IsDiagnosticEnabled() bool {
	return c.diagnosticServer.Enabled()
}
