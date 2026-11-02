package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/containerd/log"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	networktypes "github.com/moby/moby/api/types/network"
	clustertypes "github.com/moby/moby/v2/daemon/cluster/provider"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/multierror"
	"github.com/moby/moby/v2/daemon/internal/netipstringer"
	"github.com/moby/moby/v2/daemon/internal/netiputil"
	"github.com/moby/moby/v2/daemon/internal/ocihook"
	"github.com/moby/moby/v2/daemon/internal/otelutil"
	"github.com/moby/moby/v2/daemon/libnetwork"
	lncluster "github.com/moby/moby/v2/daemon/libnetwork/cluster"
	"github.com/moby/moby/v2/daemon/libnetwork/driverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/ipamapi"
	"github.com/moby/moby/v2/daemon/libnetwork/netlabel"
	"github.com/moby/moby/v2/daemon/libnetwork/networkdb"
	"github.com/moby/moby/v2/daemon/libnetwork/options"
	lntypes "github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/moby/moby/v2/daemon/network"
	"github.com/moby/moby/v2/daemon/pkg/opts"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/errdefs"
	"github.com/moby/moby/v2/internal/iterutil"
	"github.com/moby/moby/v2/internal/sliceutil"
	"github.com/moby/moby/v2/pkg/plugingetter"
	"go.opentelemetry.io/otel/baggage"
)

// PredefinedNetworkError is returned when user tries to create predefined network that already exists.
type PredefinedNetworkError string

func (pnr PredefinedNetworkError) Error() string {
	return fmt.Sprintf("operation is not permitted on predefined %s network ", string(pnr))
}

// Forbidden denotes the type of this error
func (pnr PredefinedNetworkError) Forbidden() {}

// NetworkController returns the network controller created by the daemon.
func (daemon *Daemon) NetworkController() *libnetwork.Controller {
	return daemon.netController
}

// OCIHooks returns the server for OCI hooks the daemon installs in
// containers it does not start itself. It is nil on Windows.
func (daemon *Daemon) OCIHooks() *ocihook.Server {
	return daemon.ociHooks
}

// FindNetwork returns a network based on:
// 1. Full ID
// 2. Full Name
// 3. Partial ID
// as long as there is no ambiguity
func (daemon *Daemon) FindNetwork(term string) (*libnetwork.Network, error) {
	var listByFullName, listByPartialID []*libnetwork.Network
	for _, nw := range daemon.getAllNetworks() {
		nwID := nw.ID()
		if nwID == term {
			return nw, nil
		}
		if strings.HasPrefix(nw.ID(), term) {
			listByPartialID = append(listByPartialID, nw)
		}
		if nw.Name() == term {
			listByFullName = append(listByFullName, nw)
		}
	}
	switch {
	case len(listByFullName) == 1:
		return listByFullName[0], nil
	case len(listByFullName) > 1:
		return nil, errdefs.InvalidParameter(fmt.Errorf("network %s is ambiguous (%d matches found on name)", term, len(listByFullName)))
	case len(listByPartialID) == 1:
		return listByPartialID[0], nil
	case len(listByPartialID) > 1:
		return nil, errdefs.InvalidParameter(fmt.Errorf("network %s is ambiguous (%d matches found based on ID prefix)", term, len(listByPartialID)))
	}

	// Be very careful to change the error type here, the
	// libnetwork.ErrNoSuchNetwork error is used by the controller
	// to retry the creation of the network as managed through the swarm manager
	return nil, errdefs.NotFound(libnetwork.ErrNoSuchNetwork(term))
}

// GetNetworkByID function returns a network whose ID matches the given ID.
// It fails with an error if no matching network is found.
func (daemon *Daemon) GetNetworkByID(id string) (*libnetwork.Network, error) {
	c := daemon.netController
	if c == nil {
		return nil, fmt.Errorf("netcontroller is nil: %w", libnetwork.ErrNoSuchNetwork(id))
	}
	return c.NetworkByID(id)
}

// getAllNetworks returns a list containing all networks
func (daemon *Daemon) getAllNetworks() []*libnetwork.Network {
	c := daemon.netController
	if c == nil {
		return nil
	}
	ctx := context.TODO()
	return c.Networks(ctx)
}

type ingressJob struct {
	create  *clustertypes.NetworkCreateRequest
	ip      net.IP
	jobDone chan struct{}
}

var (
	ingressWorkerOnce  sync.Once
	ingressJobsChannel chan *ingressJob
	ingressID          string
)

func (daemon *Daemon) startIngressWorker() {
	ingressJobsChannel = make(chan *ingressJob, 100)
	go func() {
		for r := range ingressJobsChannel {
			if r.create != nil {
				daemon.setupIngress(&daemon.config().Config, r.create, r.ip, ingressID)
				ingressID = r.create.ID
			} else {
				daemon.releaseIngress(ingressID)
				ingressID = ""
			}
			close(r.jobDone)
		}
	}()
}

// enqueueIngressJob adds a ingress add/rm request to the worker queue.
// It guarantees the worker is started.
func (daemon *Daemon) enqueueIngressJob(job *ingressJob) {
	ingressWorkerOnce.Do(daemon.startIngressWorker)
	ingressJobsChannel <- job
}

// SetupIngress setups ingress networking.
// The function returns a channel which will signal the caller when the programming is completed.
func (daemon *Daemon) SetupIngress(create clustertypes.NetworkCreateRequest, nodeIP string) (<-chan struct{}, error) {
	ip, _, err := net.ParseCIDR(nodeIP)
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	daemon.enqueueIngressJob(&ingressJob{&create, ip, done})
	return done, nil
}

// ReleaseIngress releases the ingress networking.
// The function returns a channel which will signal the caller when the programming is completed.
func (daemon *Daemon) ReleaseIngress() (<-chan struct{}, error) {
	done := make(chan struct{})
	daemon.enqueueIngressJob(&ingressJob{nil, nil, done})
	return done, nil
}

func (daemon *Daemon) setupIngress(cfg *config.Config, create *clustertypes.NetworkCreateRequest, ip net.IP, staleID string) {
	controller := daemon.netController
	controller.AgentInitWait()

	if staleID != "" && staleID != create.ID {
		daemon.releaseIngress(staleID)
	}

	ctx := baggage.ContextWithBaggage(context.TODO(), otelutil.MustNewBaggage(
		otelutil.MustNewMemberRaw(otelutil.TriggerKey, "daemon.setupIngress"),
	))
	if _, err := daemon.createNetwork(ctx, cfg, create.CreateRequest, create.ID, true); err != nil {
		// If it is any other error other than already
		// exists error log error and return.
		if _, ok := err.(libnetwork.NetworkNameError); !ok {
			log.G(ctx).Errorf("Failed creating ingress network: %v", err)
			return
		}
		// Otherwise continue down the call to create or recreate sandbox.
	}

	_, err := daemon.GetNetworkByID(create.ID)
	if err != nil {
		log.G(context.TODO()).Errorf("Failed getting ingress network by id after creating: %v", err)
	}
}

func (daemon *Daemon) releaseIngress(id string) {
	controller := daemon.netController

	if id == "" {
		return
	}

	n, err := controller.NetworkByID(id)
	if err != nil {
		log.G(context.TODO()).Errorf("failed to retrieve ingress network %s: %v", id, err)
		return
	}

	if err := n.Delete(libnetwork.NetworkDeleteOptionRemoveLB); err != nil {
		log.G(context.TODO()).Errorf("Failed to delete ingress network %s: %v", n.ID(), err)
		return
	}
}

// SetNetworkBootstrapKeys sets the bootstrap keys.
func (daemon *Daemon) SetNetworkBootstrapKeys(keys []*lntypes.EncryptionKey) error {
	if err := daemon.netController.SetKeys(keys); err != nil {
		return err
	}
	// Upon successful key setting dispatch the keys available event
	daemon.cluster.SendClusterEvent(lncluster.EventNetworkKeysAvailable)
	return nil
}

// UpdateAttachment notifies the attacher about the attachment config.
func (daemon *Daemon) UpdateAttachment(networkName, networkID, containerID string, config *networktypes.NetworkingConfig) error {
	if daemon.clusterProvider == nil {
		return errors.New("cluster provider is not initialized")
	}

	if err := daemon.clusterProvider.UpdateAttachment(networkName, containerID, config); err != nil {
		return daemon.clusterProvider.UpdateAttachment(networkID, containerID, config)
	}

	return nil
}

// WaitForDetachment makes the cluster manager wait for detachment of
// the container from the network.
func (daemon *Daemon) WaitForDetachment(ctx context.Context, networkName, networkID, taskID, containerID string) error {
	if daemon.clusterProvider == nil {
		return errors.New("cluster provider is not initialized")
	}

	return daemon.clusterProvider.WaitForDetachment(ctx, networkName, networkID, taskID, containerID)
}

// CreateManagedNetwork creates an agent network.
func (daemon *Daemon) CreateManagedNetwork(create clustertypes.NetworkCreateRequest) error {
	return daemon.runInNetNS(func() error {
		_, err := daemon.createNetwork(context.TODO(), &daemon.config().Config, create.CreateRequest, create.ID, true)
		return err
	})
}

// CreateNetwork creates a network with the given name, driver and other optional parameters
func (daemon *Daemon) CreateNetwork(ctx context.Context, create networktypes.CreateRequest) (*networktypes.CreateResponse, error) {
	var resp *networktypes.CreateResponse
	err := daemon.runInNetNS(func() error {
		var err error
		resp, err = daemon.createNetwork(ctx, &daemon.config().Config, create, "", false)
		return err
	})
	return resp, err
}

func (daemon *Daemon) createNetwork(ctx context.Context, cfg *config.Config, create networktypes.CreateRequest, id string, agent bool) (*networktypes.CreateResponse, error) {
	if network.IsReserved(create.Name) {
		return nil, errdefs.Forbidden(fmt.Errorf("network name %q is reserved and cannot be created", create.Name))
	}

	if network.IsPredefined(create.Name) {
		return nil, PredefinedNetworkError(create.Name)
	}

	c := daemon.netController
	driver := create.Driver
	if driver == "" {
		driver = c.Config().DefaultDriver
	}

	if driver == "overlay" && !daemon.cluster.IsManager() && !agent {
		return nil, errdefs.Forbidden(errors.New(`This node is not a swarm manager. Use "docker swarm init" or "docker swarm join" to connect this node to swarm and try again.`))
	}

	networkOptions := make(map[string]string)
	maps.Copy(networkOptions, create.Options)
	if defaultOpts, ok := cfg.DefaultNetworkOpts[driver]; create.ConfigFrom == nil && ok {
		for k, v := range defaultOpts {
			if _, ok := networkOptions[k]; !ok {
				log.G(ctx).WithFields(log.Fields{"driver": driver, "network": id, k: v}).Debug("Applying network default option")
				networkOptions[k] = v
			}
		}
	}

	enableIPv4 := true
	if create.EnableIPv4 != nil {
		enableIPv4 = *create.EnableIPv4
		// Make sure there's no conflicting DefaultNetworkOpts value (it'd be ignored but
		// would look wrong in inspect output).
		delete(networkOptions, netlabel.EnableIPv4)
	} else if v, ok := networkOptions[netlabel.EnableIPv4]; ok {
		var err error
		if enableIPv4, err = strconv.ParseBool(v); err != nil {
			return nil, errdefs.InvalidParameter(fmt.Errorf("driver-opt %q is not a valid bool", netlabel.EnableIPv4))
		}
	}

	var enableIPv6 bool
	if create.EnableIPv6 != nil {
		enableIPv6 = *create.EnableIPv6
		// Make sure there's no conflicting DefaultNetworkOpts value (it'd be ignored but
		// would look wrong in inspect output).
		delete(networkOptions, netlabel.EnableIPv6)
	} else if v, ok := networkOptions[netlabel.EnableIPv6]; ok {
		var err error
		if enableIPv6, err = strconv.ParseBool(v); err != nil {
			return nil, errdefs.InvalidParameter(fmt.Errorf("driver-opt %q is not a valid bool", netlabel.EnableIPv6))
		}
	}

	nwOptions := []libnetwork.NetworkOption{
		libnetwork.NetworkOptionEnableIPv4(enableIPv4),
		libnetwork.NetworkOptionEnableIPv6(enableIPv6),
		libnetwork.NetworkOptionDriverOpts(networkOptions),
		libnetwork.NetworkOptionLabels(create.Labels),
		libnetwork.NetworkOptionAttachable(create.Attachable),
		libnetwork.NetworkOptionIngress(create.Ingress),
		libnetwork.NetworkOptionScope(create.Scope),
	}

	if create.ConfigOnly {
		nwOptions = append(nwOptions, libnetwork.NetworkOptionConfigOnly())
	}

	if create.IPAM != nil {
		ipam := create.IPAM
		if err := validateIpamConfig(ipam.Config, enableIPv6); err != nil {
			if agent {
				// This function is called with agent=false for all networks. For swarm-scoped
				// networks, the configuration is validated but ManagerRedirectError is returned
				// and the network is not created. Then, each time a swarm-scoped network is
				// needed, this function is called again with agent=true.
				//
				// Non-swarm networks created before ValidateIPAM was introduced continue to work
				// as they did before-upgrade, even if they would fail the new checks on creation
				// (for example, by having host-bits set in their subnet). Those networks are not
				// seen again here.
				//
				// By dropping errors for agent networks, existing swarm-scoped networks also
				// continue to behave as they did before upgrade - but new networks are still
				// validated.
				log.G(ctx).WithFields(log.Fields{
					"error":   err,
					"network": create.Name,
				}).Warn("Continuing with validation errors in agent IPAM")
			} else {
				return nil, errdefs.InvalidParameter(err)
			}
		}
		v4Conf, v6Conf, err := getIpamConfig(ipam.Config)
		if err != nil {
			return nil, err
		}
		nwOptions = append(nwOptions, libnetwork.NetworkOptionIpam(ipam.Driver, "", v4Conf, v6Conf, ipam.Options))
	}

	if create.Internal {
		nwOptions = append(nwOptions, libnetwork.NetworkOptionInternalNetwork())
	}
	if agent {
		nwOptions = append(nwOptions, libnetwork.NetworkOptionDynamic())
		nwOptions = append(nwOptions, libnetwork.NetworkOptionPersist(false))
	}

	if create.ConfigFrom != nil {
		nwOptions = append(nwOptions, libnetwork.NetworkOptionConfigFrom(create.ConfigFrom.Network))
	}

	if agent && driver == "overlay" {
		nodeIP, exists := daemon.GetAttachmentStore().GetIPForNetwork(id)
		if !exists {
			return nil, fmt.Errorf("failed to find a load balancer IP to use for network: %v", id)
		}

		nwOptions = append(nwOptions, libnetwork.NetworkOptionLBEndpoint(nodeIP))
	}

	n, err := c.NewNetwork(ctx, driver, create.Name, id, nwOptions...)
	if err != nil {
		return nil, err
	}

	daemon.pluginRefCount(driver, driverapi.NetworkPluginEndpointType, plugingetter.Acquire)
	if create.IPAM != nil {
		daemon.pluginRefCount(create.IPAM.Driver, ipamapi.PluginEndpointType, plugingetter.Acquire)
	}
	daemon.LogNetworkEvent(n, events.ActionCreate)

	return &networktypes.CreateResponse{ID: n.ID()}, nil
}

func (daemon *Daemon) pluginRefCount(driver, capability string, mode int) {
	var builtinDrivers []string

	switch capability {
	case driverapi.NetworkPluginEndpointType:
		builtinDrivers = daemon.netController.BuiltinDrivers()
	case ipamapi.PluginEndpointType:
		builtinDrivers = daemon.netController.BuiltinIPAMDrivers()
	default:
		// other capabilities can be ignored for now
	}

	if slices.Contains(builtinDrivers, driver) {
		return
	}

	if daemon.PluginStore != nil {
		_, err := daemon.PluginStore.Get(driver, capability, mode)
		if err != nil {
			log.G(context.TODO()).WithError(err).WithFields(log.Fields{"mode": mode, "driver": driver}).Error("Error handling plugin refcount operation")
		}
	}
}

func validateIpamConfig(data []networktypes.IPAMConfig, enableIPv6 bool) error {
	var errs []error
	for _, cfg := range data {
		subnetFamily := 4
		if cfg.Subnet.Addr().Is6() {
			subnetFamily = 6
		}

		if !enableIPv6 && subnetFamily == 6 {
			continue
		}

		if cfg.Subnet != cfg.Subnet.Masked() {
			errs = append(errs, fmt.Errorf("invalid subnet %s: it should be %s", cfg.Subnet, cfg.Subnet.Masked()))
		}

		if ipRangeErrs := validateIPRange(cfg.IPRange, cfg.Subnet, subnetFamily); len(ipRangeErrs) > 0 {
			errs = append(errs, ipRangeErrs...)
		}

		if err := validateAddress(cfg.Gateway, cfg.Subnet, subnetFamily); err != nil {
			errs = append(errs, fmt.Errorf("invalid gateway %s: %w", cfg.Gateway, err))
		}

		for auxName, aux := range cfg.AuxAddress {
			if err := validateAddress(aux, cfg.Subnet, subnetFamily); err != nil {
				errs = append(errs, fmt.Errorf("invalid auxiliary address %s: %w", auxName, err))
			}
		}
	}

	if err := multierror.Join(errs...); err != nil {
		return fmt.Errorf("invalid network config:\n%w", err)
	}

	return nil
}

func validateIPRange(ipRange, subnet netip.Prefix, subnetFamily int) []error {
	if !ipRange.IsValid() {
		return nil
	}
	family := 4
	if ipRange.Addr().Is6() {
		family = 6
	}

	if family != subnetFamily {
		return []error{fmt.Errorf("invalid ip-range %s: parent subnet is an IPv%d block", ipRange, subnetFamily)}
	}

	var errs []error
	if ipRange.Bits() < subnet.Bits() {
		errs = append(errs, fmt.Errorf("invalid ip-range %s: CIDR block is bigger than its parent subnet %s", ipRange, subnet))
	}
	if ipRange != ipRange.Masked() {
		errs = append(errs, fmt.Errorf("invalid ip-range %s: it should be %s", ipRange, ipRange.Masked()))
	}
	if !subnet.Overlaps(ipRange) {
		errs = append(errs, fmt.Errorf("invalid ip-range %s: parent subnet %s doesn't contain ip-range", ipRange, subnet))
	}

	return errs
}

func validateAddress(addr netip.Addr, subnet netip.Prefix, subnetFamily int) error {
	if !addr.IsValid() {
		return nil
	}
	family := 4
	if addr.Is6() {
		family = 6
	}

	if family != subnetFamily {
		return fmt.Errorf("parent subnet is an IPv%d block", subnetFamily)
	}
	if !subnet.Contains(addr) {
		return fmt.Errorf("parent subnet %s doesn't contain this address", subnet)
	}

	return nil
}

func getIpamConfig(data []networktypes.IPAMConfig) ([]*libnetwork.IpamConf, []*libnetwork.IpamConf, error) {
	ipamV4Cfg := []*libnetwork.IpamConf{}
	ipamV6Cfg := []*libnetwork.IpamConf{}
	for _, d := range data {
		iCfg := libnetwork.IpamConf{
			PreferredPool: netipstringer.Prefix(netiputil.Unmap(d.Subnet).Masked()),
			SubPool:       netipstringer.Prefix(netiputil.Unmap(d.IPRange).Masked()),
			Gateway:       netipstringer.Addr(d.Gateway.Unmap()),
			AuxAddresses: maps.Collect(iterutil.Map2(maps.All(d.AuxAddress), func(k string, v netip.Addr) (string, string) {
				return k, v.Unmap().String()
			})),
		}
		if d.Subnet.Addr().Unmap().Is4() {
			ipamV4Cfg = append(ipamV4Cfg, &iCfg)
		} else {
			ipamV6Cfg = append(ipamV6Cfg, &iCfg)
		}
	}
	return ipamV4Cfg, ipamV6Cfg, nil
}

// UpdateContainerServiceConfig updates a service configuration.
func (daemon *Daemon) UpdateContainerServiceConfig(containerName string, serviceConfig *clustertypes.ServiceConfig) error {
	ctr, err := daemon.GetContainer(containerName)
	if err != nil {
		return err
	}

	ctr.NetworkSettings.Service = serviceConfig
	return nil
}

// ConnectContainerToNetwork connects the given container to the given
// network. If either cannot be found, an err is returned. If the
// network cannot be set up, an err is returned.
func (daemon *Daemon) ConnectContainerToNetwork(ctx context.Context, containerName, networkName string, endpointConfig *networktypes.EndpointSettings) error {
	ctr, err := daemon.GetContainer(containerName)
	if err != nil {
		return err
	}
	return daemon.ConnectToNetwork(ctx, ctr, networkName, endpointConfig)
}

// DisconnectContainerFromNetwork disconnects the given container from
// the given network. If either cannot be found, an err is returned.
func (daemon *Daemon) DisconnectContainerFromNetwork(containerName string, networkName string, force bool) error {
	ctr, err := daemon.GetContainer(containerName)
	if err != nil {
		if force {
			return daemon.ForceEndpointDelete(containerName, networkName)
		}
		return err
	}
	return daemon.DisconnectFromNetwork(context.TODO(), ctr, networkName, force)
}

// GetNetworkDriverList returns the list of plugins drivers
// registered for network.
func (daemon *Daemon) GetNetworkDriverList(ctx context.Context) []string {
	if daemon.netController == nil {
		return nil
	}

	pluginList := daemon.netController.BuiltinDrivers()

	for _, plugin := range daemon.PluginStore.GetAllManagedPluginsByCap(driverapi.NetworkPluginEndpointType) {
		pluginList = append(pluginList, plugin.Name())
	}

	pluginList = sliceutil.Dedup(pluginList)
	for _, nw := range daemon.netController.Networks(ctx) {
		if typ := nw.Type(); !slices.Contains(pluginList, typ) {
			pluginList = append(pluginList, typ)
		}
	}

	slices.Sort(pluginList)
	return pluginList
}

// DeleteManagedNetwork deletes an agent network.
// The requirement of networkID is enforced.
func (daemon *Daemon) DeleteManagedNetwork(networkID string) error {
	return daemon.runInNetNS(func() error {
		n, err := daemon.GetNetworkByID(networkID)
		if err != nil {
			return err
		}
		return daemon.deleteNetwork(n, true)
	})
}

// DeleteNetwork destroys a network unless it's one of docker's predefined networks.
func (daemon *Daemon) DeleteNetwork(networkID string) error {
	return daemon.runInNetNS(func() error {
		n, err := daemon.GetNetworkByID(networkID)
		if err != nil {
			return fmt.Errorf("could not find network by ID: %w", err)
		}
		return daemon.deleteNetwork(n, false)
	})
}

func (daemon *Daemon) deleteNetwork(nw *libnetwork.Network, dynamic bool) error {
	if network.IsPredefined(nw.Name()) && !dynamic {
		err := fmt.Errorf("%s is a pre-defined network and cannot be removed", nw.Name())
		return errdefs.Forbidden(err)
	}

	if dynamic && !nw.Dynamic() {
		if network.IsPredefined(nw.Name()) {
			// Predefined networks now support swarm services. Make this
			// a no-op when cluster requests to remove the predefined network.
			return nil
		}
		err := fmt.Errorf("%s is not a dynamic network", nw.Name())
		return errdefs.Forbidden(err)
	}

	if err := nw.Delete(); err != nil {
		return fmt.Errorf("error while removing network: %w", err)
	}

	// If this is not a configuration only network, we need to
	// update the corresponding remote drivers' reference counts
	if !nw.ConfigOnly() {
		daemon.pluginRefCount(nw.Type(), driverapi.NetworkPluginEndpointType, plugingetter.Release)
		ipamType, _, _, _ := nw.IpamConfig()
		daemon.pluginRefCount(ipamType, ipamapi.PluginEndpointType, plugingetter.Release)
		daemon.LogNetworkEvent(nw, events.ActionDestroy)
	}

	return nil
}

// GetNetworks returns a list of all networks
func (daemon *Daemon) GetNetworks(filter network.Filter, config backend.NetworkListConfig) ([]networktypes.Inspect, error) {
	allNetworks := daemon.getAllNetworks()
	networks := make([]networktypes.Inspect, 0, len(allNetworks))
	for _, n := range allNetworks {
		if filter.Matches(n) {
			nr := networktypes.Inspect{
				Network:    buildNetworkResource(n),
				Containers: buildContainerAttachments(n),
			}
			if config.WithServices {
				nr.Services = buildServiceAttachments(n)
			}
			if config.WithStatus {
				ipam, err := n.IPAMStatus(context.TODO())
				if err != nil {
					log.G(context.TODO()).WithFields(log.Fields{
						"network": n.Name(),
						"id":      n.ID(),
						"error":   err,
					}).Warning("Error encountered while gathering IPAM status for network")
				}
				nr.Status = &networktypes.Status{
					IPAM: ipam,
				}
			}
			networks = append(networks, nr)
		}
	}

	return networks, nil
}

func (daemon *Daemon) GetNetworkSummaries(filter network.Filter) ([]networktypes.Summary, error) {
	allNetworks := daemon.getAllNetworks()
	networks := make([]networktypes.Summary, 0, len(allNetworks))
	for _, n := range allNetworks {
		if filter.Matches(n) {
			nr := networktypes.Summary{Network: buildNetworkResource(n)}
			networks = append(networks, nr)
		}
	}

	return networks, nil
}

// buildNetworkResource builds a [types.NetworkResource] from the given
// [libnetwork.Network], to be returned by the API.
func buildNetworkResource(nw *libnetwork.Network) networktypes.Network {
	if nw == nil {
		return networktypes.Network{}
	}

	return networktypes.Network{
		Name:       nw.Name(),
		ID:         nw.ID(),
		Created:    nw.Created(),
		Scope:      nw.Scope(),
		Driver:     nw.Type(),
		EnableIPv4: nw.IPv4Enabled(),
		EnableIPv6: nw.IPv6Enabled(),
		IPAM:       buildIPAMResources(nw),
		Internal:   nw.Internal(),
		Attachable: nw.Attachable(),
		Ingress:    nw.Ingress(),
		ConfigFrom: networktypes.ConfigReference{Network: nw.ConfigFrom()},
		ConfigOnly: nw.ConfigOnly(),
		Options:    nw.DriverOptions(),
		Labels:     nw.Labels(),
		Peers:      buildPeerInfoResources(nw.Peers()),
	}
}

// buildContainerAttachments creates a [types.EndpointResource] map of all
// containers attached to the network. It is used when listing networks in
// detailed mode.
func buildContainerAttachments(nw *libnetwork.Network) map[string]networktypes.EndpointResource {
	containers := make(map[string]networktypes.EndpointResource)
	for _, e := range nw.Endpoints() {
		ei := e.Info()
		if ei == nil {
			continue
		}
		if sb := ei.Sandbox(); sb != nil {
			containers[sb.ContainerID()] = buildEndpointResource(e, ei)
		} else {
			containers["ep-"+e.ID()] = buildEndpointResource(e, ei)
		}
	}
	return containers
}

// buildServiceAttachments creates a [network.ServiceInfo] map of all services
// attached to the network. It is used when listing networks in "verbose" mode.
func buildServiceAttachments(nw *libnetwork.Network) map[string]networktypes.ServiceInfo {
	services := make(map[string]networktypes.ServiceInfo)
	for name, service := range nw.Services() {
		tasks := make([]networktypes.Task, 0, len(service.Tasks))
		for _, t := range service.Tasks {
			eip, _ := netip.ParseAddr(t.EndpointIP)
			tasks = append(tasks, networktypes.Task{
				Name:       t.Name,
				EndpointID: t.EndpointID,
				EndpointIP: eip.Unmap(),
				Info:       t.Info,
			})
		}
		vip, _ := netip.ParseAddr(service.VIP)
		services[name] = networktypes.ServiceInfo{
			VIP:          vip.Unmap(),
			Ports:        service.Ports,
			Tasks:        tasks,
			LocalLBIndex: service.LocalLBIndex,
		}
	}
	return services
}

// buildPeerInfoResources converts a list of [networkdb.PeerInfo] to a
// [network.PeerInfo] for inclusion in API responses. It returns nil if
// the list of peers is empty.
func buildPeerInfoResources(peers []networkdb.PeerInfo) []networktypes.PeerInfo {
	if len(peers) == 0 {
		return nil
	}
	peerInfo := make([]networktypes.PeerInfo, 0, len(peers))
	for _, peer := range peers {
		peerInfo = append(peerInfo, networktypes.PeerInfo(peer))
	}
	return peerInfo
}

// buildIPAMResources constructs a [network.IPAM] from the network's
// IPAM information for inclusion in API responses.
func buildIPAMResources(nw *libnetwork.Network) networktypes.IPAM {
	var ipamConfig []networktypes.IPAMConfig

	ipamDriver, ipamOptions, ipv4Conf, ipv6Conf := nw.IpamConfig()
	ipv4Info, ipv6Info := nw.IpamInfo()

	hasIPv4Config := false
	if len(ipv4Info) > 0 {
		// Only check ipv4 networks if there were any allocated
		for i, cfg := range ipv4Conf {
			if cfg.PreferredPool == "" {
				continue
			}
			hasIPv4Config = true
			subnet := ipv4Info[i].IPAMData.Pool
			if subnet != nil {
				cfg.PreferredPool = subnet.String()
			}
			if ipv4Info[i].IPAMData.Gateway != nil && cfg.Gateway == "" {
				cfg.Gateway = ipv4Info[i].IPAMData.Gateway.IP.String()
			}

			ipamConfig = append(ipamConfig, cfg.IPAMConfig())
		}
	}

	hasIPv6Config := false
	if len(ipv6Info) > 0 {
		// Only check ipv6 networks if there were any allocated
		for i, cfg := range ipv6Conf {
			if cfg.PreferredPool == "" {
				continue
			}
			hasIPv6Config = true
			subnet := ipv6Info[i].IPAMData.Pool
			if subnet != nil {
				cfg.PreferredPool = subnet.String()
			}

			if ipv6Info[i].IPAMData.Gateway != nil && cfg.Gateway == "" {
				cfg.Gateway = ipv6Info[i].IPAMData.Gateway.IP.String()
			}
			ipamConfig = append(ipamConfig, cfg.IPAMConfig())
		}
	}

	if !hasIPv4Config || !hasIPv6Config {
		if !hasIPv4Config {
			for _, info := range ipv4Info {
				ipamConfig = append(ipamConfig, info.IPAMData.IPAMConfig())
			}
		}

		if !hasIPv6Config {
			for _, info := range ipv6Info {
				if info.IPAMData.Pool == nil {
					continue
				}
				ipamConfig = append(ipamConfig, info.IPAMData.IPAMConfig())
			}
		}
	}

	return networktypes.IPAM{
		Driver:  ipamDriver,
		Options: ipamOptions,
		Config:  ipamConfig,
	}
}

// buildEndpointResource combines information from the endpoint and additional
// endpoint-info into a [types.EndpointResource].
func buildEndpointResource(ep *libnetwork.Endpoint, info libnetwork.EndpointInfo) networktypes.EndpointResource {
	er := networktypes.EndpointResource{
		EndpointID: ep.ID(),
		Name:       ep.Name(),
	}
	if iface := info.Iface(); iface != nil {
		er.MacAddress = networktypes.HardwareAddr(iface.MacAddress())
		er.IPv4Address = netiputil.Unmap(iface.Addr())
		er.IPv6Address = iface.AddrIPv6()
	}
	return er
}

// clearAttachableNetworks removes the attachable networks
// after disconnecting any connected container
func (daemon *Daemon) clearAttachableNetworks() {
	for _, n := range daemon.getAllNetworks() {
		if !n.Attachable() {
			continue
		}
		for _, ep := range n.Endpoints() {
			epInfo := ep.Info()
			if epInfo == nil {
				continue
			}
			sb := epInfo.Sandbox()
			if sb == nil {
				continue
			}
			containerID := sb.ContainerID()
			if err := daemon.DisconnectContainerFromNetwork(containerID, n.ID(), true); err != nil {
				log.G(context.TODO()).Warnf("Failed to disconnect container %s from swarm network %s on cluster leave: %v",
					containerID, n.Name(), err)
			}
		}
		if err := daemon.DeleteManagedNetwork(n.ID()); err != nil {
			log.G(context.TODO()).Warnf("Failed to remove swarm network %s on cluster leave: %v", n.Name(), err)
		}
	}
}

// buildCreateEndpointOptions builds endpoint options from a given network.
func buildCreateEndpointOptions(c *container.Container, n *libnetwork.Network, epConfig *network.EndpointSettings, sb *libnetwork.Sandbox, daemonDNS []netip.Addr) ([]libnetwork.EndpointOption, error) {
	var createOptions []libnetwork.EndpointOption
	genericOptions := make(options.Generic)

	nwName := n.Name()

	if epConfig != nil {
		if ipam := epConfig.IPAMConfig; ipam != nil {
			var ipList []net.IP
			for _, linkIP := range ipam.LinkLocalIPs {
				if !linkIP.IsValid() {
					return nil, fmt.Errorf("invalid link-local IP address: %s", ipam.LinkLocalIPs)
				}
				ipList = append(ipList, linkIP.AsSlice())
			}

			if ipam.IPv4Address.IsValid() && !ipam.IPv4Address.Is4() && !ipam.IPv4Address.Is4In6() {
				return nil, fmt.Errorf("invalid IPv4 address: %s", ipam.IPv4Address)
			}

			if ipam.IPv6Address.IsValid() && !ipam.IPv6Address.Is6() {
				return nil, fmt.Errorf("invalid IPv6 address: %s", ipam.IPv6Address)
			}

			createOptions = append(createOptions, libnetwork.CreateOptionIPAM(ipam.IPv4Address.AsSlice(), ipam.IPv6Address.AsSlice(), ipList))
		}

		createOptions = append(createOptions, libnetwork.CreateOptionDNSNames(epConfig.DNSNames))

		for k, v := range epConfig.DriverOpts {
			createOptions = append(createOptions, libnetwork.EndpointOptionGeneric(options.Generic{k: v}))
		}

		if len(epConfig.DesiredMacAddress) != 0 {
			genericOptions[netlabel.MacAddress] = net.HardwareAddr(epConfig.DesiredMacAddress)
		}
	}

	if svcCfg := c.NetworkSettings.Service; svcCfg != nil {
		nwID := n.ID()

		var vip net.IP
		if virtualAddress := svcCfg.VirtualAddresses[nwID]; virtualAddress != nil {
			vip = net.ParseIP(virtualAddress.IPv4)
		}

		var portConfigs []*libnetwork.PortConfig
		for _, portConfig := range svcCfg.ExposedPorts {
			portConfigs = append(portConfigs, &libnetwork.PortConfig{
				Name:          portConfig.Name,
				Protocol:      libnetwork.PortConfig_Protocol(portConfig.Protocol),
				TargetPort:    portConfig.TargetPort,
				PublishedPort: portConfig.PublishedPort,
			})
		}

		createOptions = append(createOptions, libnetwork.CreateOptionService(svcCfg.Name, svcCfg.ID, vip, portConfigs, svcCfg.Aliases[nwID]))
	}

	if !containertypes.NetworkMode(nwName).IsUserDefined() {
		createOptions = append(createOptions, libnetwork.CreateOptionDisableResolution())
	}

	epOpts, err := buildPortsRelatedCreateEndpointOptions(c, n, sb)
	if err != nil {
		return nil, err
	}
	createOptions = append(createOptions, epOpts...)

	// On Windows, DNS config is a per-adapter config option whereas on Linux, it's a sandbox-wide parameter; hence why
	// we're dealing with DNS config both here and in buildSandboxOptions. Following DNS options are only honored by
	// Windows netdrivers, whereas DNS options in buildSandboxOptions are only honored by Linux netdrivers.
	if !n.Internal() {
		var nameservers []netip.Addr
		if len(c.HostConfig.DNS) > 0 {
			nameservers = c.HostConfig.DNS
		} else if len(daemonDNS) > 0 {
			nameservers = daemonDNS
		}
		if len(nameservers) > 0 {
			createOptions = append(createOptions, libnetwork.CreateOptionDNS(sliceutil.Map(nameservers, (netip.Addr).String)))
		}
	}

	createOptions = append(createOptions, libnetwork.EndpointOptionGeneric(genericOptions))

	// IPv6 may be enabled on the network, but disabled in the container.
	if n.IPv6Enabled() {
		if sbIPv6, ok := sb.IPv6Enabled(); ok && !sbIPv6 {
			createOptions = append(createOptions, libnetwork.CreateOptionDisableIPv6())
		}
	}

	if path, ok := sb.NetnsPath(); ok {
		createOptions = append(createOptions, libnetwork.WithNetnsPath(path))
	}

	return createOptions, nil
}

// buildPortsRelatedCreateEndpointOptions returns the appropriate endpoint options to apply config related to port
// mapping and exposed ports.
func buildPortsRelatedCreateEndpointOptions(c *container.Container, n *libnetwork.Network, sb *libnetwork.Sandbox) ([]libnetwork.EndpointOption, error) {
	// Port-mapping rules belong to the container & applicable only to non-internal networks.
	//
	// TODO(thaJeztah): Look if we can provide a more minimal function for getPortMapInfo, as it does a lot, and we only need the "length".
	if n.Internal() || len(getPortMapInfo(sb)) > 0 {
		return nil, nil
	}

	var (
		exposedPorts   []lntypes.TransportPort
		publishedPorts []lntypes.PortBinding
	)

	ports := c.HostConfig.PortBindings
	if c.HostConfig.PublishAllPorts && len(c.Config.ExposedPorts) > 0 {
		// Add exposed ports to a copy of the map to make sure a "publishedPorts" entry is created
		// for each exposed port, even if there's no specific binding.
		ports = maps.Clone(c.HostConfig.PortBindings)
		if ports == nil {
			ports = networktypes.PortMap{}
		}
		for p := range c.Config.ExposedPorts {
			if _, exists := ports[p]; !exists {
				ports[p] = nil
			}
		}
	}

	for p, bindings := range ports {
		protocol := lntypes.ParseProtocol(string(p.Proto()))
		exposedPorts = append(exposedPorts, lntypes.TransportPort{
			Proto: protocol,
			Port:  p.Num(),
		})

		for _, binding := range bindings {
			var (
				portRange networktypes.PortRange
				err       error
			)

			// Empty HostPort means to map to an ephemeral port.
			if binding.HostPort != "" {
				portRange, err = networktypes.ParsePortRange(binding.HostPort)
				if err != nil {
					return nil, fmt.Errorf("error parsing HostPort value(%s):%v", binding.HostPort, err)
				}
			}

			publishedPorts = append(publishedPorts, lntypes.PortBinding{
				Proto:       protocol,
				Port:        p.Num(),
				HostIP:      binding.HostIP.AsSlice(),
				HostPort:    portRange.Start(),
				HostPortEnd: portRange.End(),
			})
		}

		if c.HostConfig.PublishAllPorts && len(bindings) == 0 {
			publishedPorts = append(publishedPorts, lntypes.PortBinding{
				Proto: protocol,
				Port:  p.Num(),
			})
		}
	}

	return []libnetwork.EndpointOption{
		libnetwork.CreateOptionPortMapping(publishedPorts),
		libnetwork.CreateOptionExposedPorts(exposedPorts),
	}, nil
}

// getPortMapInfo retrieves the current port-mapping programmed for the given sandbox
func getPortMapInfo(sb *libnetwork.Sandbox) networktypes.PortMap {
	pm := networktypes.PortMap{}
	if sb == nil {
		return pm
	}

	for _, ep := range sb.Endpoints() {
		getEndpointPortMapInfo(pm, ep)
	}
	return pm
}

func getEndpointPortMapInfo(pm networktypes.PortMap, ep *libnetwork.Endpoint) {
	driverInfo, _ := ep.DriverInfo()
	if driverInfo == nil {
		// It is not an error for epInfo to be nil
		return
	}

	if expData, ok := driverInfo[netlabel.ExposedPorts]; ok {
		if exposedPorts, ok := expData.([]lntypes.TransportPort); ok {
			for _, tp := range exposedPorts {
				natPort, ok := networktypes.PortFrom(tp.Port, networktypes.IPProtocol(tp.Proto.String()))
				if !ok {
					log.G(context.TODO()).Errorf("Invalid exposed port: %s", tp.String())
					continue
				}
				if _, ok := pm[natPort]; !ok {
					pm[natPort] = nil
				}
			}
		}
	}

	mapData, ok := driverInfo[netlabel.PortMap]
	if !ok {
		return
	}

	if portMapping, ok := mapData.([]lntypes.PortBinding); ok {
		for _, pp := range portMapping {
			// Use an empty string for the host natPort if there's no natPort assigned.
			natPort, ok := networktypes.PortFrom(pp.Port, networktypes.IPProtocol(pp.Proto.String()))
			if !ok {
				log.G(context.TODO()).Errorf("Invalid port binding: %s", pp.String())
				continue
			}

			var hp string
			if pp.HostPort > 0 {
				hp = strconv.Itoa(int(pp.HostPort))
			}
			hip, _ := netip.AddrFromSlice(pp.HostIP)
			natBndg := networktypes.PortBinding{
				HostIP:   hip.Unmap(),
				HostPort: hp,
			}
			pm[natPort] = append(pm[natPort], natBndg)
		}
	}
}

// buildEndpointInfo sets endpoint-related fields on container.NetworkSettings based on the provided network and endpoint.
func buildEndpointInfo(networkSettings *network.Settings, n *libnetwork.Network, ep *libnetwork.Endpoint) error {
	if ep == nil {
		return errors.New("endpoint cannot be nil")
	}

	if networkSettings == nil {
		return errors.New("network cannot be nil")
	}

	epInfo := ep.Info()
	if epInfo == nil {
		// It is not an error to get an empty endpoint info
		return nil
	}

	nwName := n.Name()
	if _, ok := networkSettings.Networks[nwName]; !ok {
		networkSettings.Networks[nwName] = &network.EndpointSettings{
			EndpointSettings: &networktypes.EndpointSettings{},
		}
	}
	networkSettings.Networks[nwName].NetworkID = n.ID()
	networkSettings.Networks[nwName].EndpointID = ep.ID()

	iface := epInfo.Iface()
	if iface == nil {
		return nil
	}

	if mac := iface.MacAddress(); mac != nil {
		networkSettings.Networks[nwName].MacAddress = networktypes.HardwareAddr(mac)
	}

	if iface.Address() != nil {
		ones, _ := iface.Address().Mask.Size()
		addr, _ := netip.AddrFromSlice(iface.Address().IP)
		networkSettings.Networks[nwName].IPAddress = addr.Unmap()
		networkSettings.Networks[nwName].IPPrefixLen = ones
	}

	if iface.AddressIPv6() != nil && iface.AddressIPv6().IP.To16() != nil {
		onesv6, _ := iface.AddressIPv6().Mask.Size()
		networkSettings.Networks[nwName].GlobalIPv6Address, _ = netip.AddrFromSlice(iface.AddressIPv6().IP)
		networkSettings.Networks[nwName].GlobalIPv6PrefixLen = onesv6
	} else {
		// If IPv6 was disabled on the interface, and its address was removed, remove it here too.
		networkSettings.Networks[nwName].GlobalIPv6Address = netip.Addr{}
		networkSettings.Networks[nwName].GlobalIPv6PrefixLen = 0
	}

	return nil
}

// buildJoinOptions builds endpoint Join options from a given network.
func buildJoinOptions(settings *network.Settings, n interface{ Name() string }) ([]libnetwork.EndpointOption, error) {
	epConfig, ok := settings.Networks[n.Name()]
	if !ok {
		return []libnetwork.EndpointOption{}, nil
	}

	joinOptions := []libnetwork.EndpointOption{
		libnetwork.JoinOptionPriority(epConfig.GwPriority),
	}

	for _, str := range epConfig.Links {
		name, alias, err := opts.ParseLink(str)
		if err != nil {
			return nil, err
		}
		joinOptions = append(joinOptions, libnetwork.CreateOptionAlias(name, alias))
	}
	for k, v := range epConfig.DriverOpts {
		joinOptions = append(joinOptions, libnetwork.EndpointOptionGeneric(options.Generic{k: v}))
	}

	return joinOptions, nil
}
