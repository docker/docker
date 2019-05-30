package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/container"
	clustertypes "github.com/docker/docker/daemon/cluster/provider"
	internalnetwork "github.com/docker/docker/daemon/network"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/opts"
	"github.com/docker/docker/pkg/plugingetter"
	"github.com/docker/docker/runconfig"
	"github.com/docker/go-connections/nat"
	"github.com/docker/libnetwork"
	lncluster "github.com/docker/libnetwork/cluster"
	"github.com/docker/libnetwork/driverapi"
	"github.com/docker/libnetwork/ipamapi"
	"github.com/docker/libnetwork/netlabel"
	"github.com/docker/libnetwork/networkdb"
	"github.com/docker/libnetwork/options"
	networktypes "github.com/docker/libnetwork/types"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// PredefinedNetworkError is returned when user tries to create predefined network that already exists.
type PredefinedNetworkError string

func (pnr PredefinedNetworkError) Error() string {
	return fmt.Sprintf("operation is not permitted on predefined %s network ", string(pnr))
}

// Forbidden denotes the type of this error
func (pnr PredefinedNetworkError) Forbidden() {}

// NetworkControllerEnabled checks if the networking stack is enabled.
// This feature depends on OS primitives and it's disabled in systems like Windows.
func (daemon *Daemon) NetworkControllerEnabled() bool {
	return daemon.netController != nil
}

// NetworkController returns the network controller created by the daemon.
func (daemon *Daemon) NetworkController() libnetwork.NetworkController {
	return daemon.netController
}

// FindNetwork returns a network based on:
// 1. Full ID
// 2. Full Name
// 3. Partial ID
// as long as there is no ambiguity
func (daemon *Daemon) FindNetwork(term string) (libnetwork.Network, error) {
	listByFullName := []libnetwork.Network{}
	listByPartialID := []libnetwork.Network{}
	for _, nw := range daemon.getAllNetworks() {
		if nw.ID() == term {
			return nw, nil
		}
		if nw.Name() == term {
			listByFullName = append(listByFullName, nw)
		}
		if strings.HasPrefix(nw.ID(), term) {
			listByPartialID = append(listByPartialID, nw)
		}
	}
	switch {
	case len(listByFullName) == 1:
		return listByFullName[0], nil
	case len(listByFullName) > 1:
		return nil, errdefs.InvalidParameter(errors.Errorf("network %s is ambiguous (%d matches found on name)", term, len(listByFullName)))
	case len(listByPartialID) == 1:
		return listByPartialID[0], nil
	case len(listByPartialID) > 1:
		return nil, errdefs.InvalidParameter(errors.Errorf("network %s is ambiguous (%d matches found based on ID prefix)", term, len(listByPartialID)))
	}

	// Be very careful to change the error type here, the
	// libnetwork.ErrNoSuchNetwork error is used by the controller
	// to retry the creation of the network as managed through the swarm manager
	return nil, errdefs.NotFound(libnetwork.ErrNoSuchNetwork(term))
}

// GetNetworkByID function returns a network whose ID matches the given ID.
// It fails with an error if no matching network is found.
func (daemon *Daemon) GetNetworkByID(id string) (libnetwork.Network, error) {
	c := daemon.netController
	if c == nil {
		return nil, errors.Wrap(libnetwork.ErrNoSuchNetwork(id), "netcontroller is nil")
	}
	return c.NetworkByID(id)
}

// GetNetworkByName function returns a network for a given network name.
// If no network name is given, the default network is returned.
func (daemon *Daemon) GetNetworkByName(name string) (libnetwork.Network, error) {
	c := daemon.netController
	if c == nil {
		return nil, libnetwork.ErrNoSuchNetwork(name)
	}
	if name == "" {
		name = c.Config().Daemon.DefaultNetwork
	}
	return c.NetworkByName(name)
}

// GetNetworksByIDPrefix returns a list of networks whose ID partially matches zero or more networks
func (daemon *Daemon) GetNetworksByIDPrefix(partialID string) []libnetwork.Network {
	c := daemon.netController
	if c == nil {
		return nil
	}
	list := []libnetwork.Network{}
	l := func(nw libnetwork.Network) bool {
		if strings.HasPrefix(nw.ID(), partialID) {
			list = append(list, nw)
		}
		return false
	}
	c.WalkNetworks(l)

	return list
}

// getAllNetworks returns a list containing all networks
func (daemon *Daemon) getAllNetworks() []libnetwork.Network {
	c := daemon.netController
	if c == nil {
		return nil
	}
	return c.Networks()
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
		// nolint: gosimple
		for {
			select {
			case r := <-ingressJobsChannel:
				if r.create != nil {
					daemon.setupIngress(r.create, r.ip, ingressID)
					ingressID = r.create.ID
				} else {
					daemon.releaseIngress(ingressID)
					ingressID = ""
				}
				close(r.jobDone)
			}
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

func (daemon *Daemon) setupIngress(create *clustertypes.NetworkCreateRequest, ip net.IP, staleID string) {
	controller := daemon.netController
	controller.AgentInitWait()

	if staleID != "" && staleID != create.ID {
		daemon.releaseIngress(staleID)
	}

	if _, err := daemon.createNetwork(create.NetworkCreateRequest, create.ID, true); err != nil {
		// If it is any other error other than already
		// exists error log error and return.
		if _, ok := err.(libnetwork.NetworkNameError); !ok {
			logrus.Errorf("Failed creating ingress network: %v", err)
			return
		}
		// Otherwise continue down the call to create or recreate sandbox.
	}

	_, err := daemon.GetNetworkByID(create.ID)
	if err != nil {
		logrus.Errorf("Failed getting ingress network by id after creating: %v", err)
	}
}

func (daemon *Daemon) releaseIngress(id string) {
	controller := daemon.netController

	if id == "" {
		return
	}

	n, err := controller.NetworkByID(id)
	if err != nil {
		logrus.Errorf("failed to retrieve ingress network %s: %v", id, err)
		return
	}

	if err := n.Delete(libnetwork.NetworkDeleteOptionRemoveLB); err != nil {
		logrus.Errorf("Failed to delete ingress network %s: %v", n.ID(), err)
		return
	}
}

// SetNetworkBootstrapKeys sets the bootstrap keys.
func (daemon *Daemon) SetNetworkBootstrapKeys(keys []*networktypes.EncryptionKey) error {
	err := daemon.netController.SetKeys(keys)
	if err == nil {
		// Upon successful key setting dispatch the keys available event
		daemon.cluster.SendClusterEvent(lncluster.EventNetworkKeysAvailable)
	}
	return err
}

// UpdateAttachment notifies the attacher about the attachment config.
func (daemon *Daemon) UpdateAttachment(networkName, networkID, containerID string, config *network.NetworkingConfig) error {
	if daemon.clusterProvider == nil {
		return fmt.Errorf("cluster provider is not initialized")
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
		return fmt.Errorf("cluster provider is not initialized")
	}

	return daemon.clusterProvider.WaitForDetachment(ctx, networkName, networkID, taskID, containerID)
}

// CreateManagedNetwork creates an agent network.
func (daemon *Daemon) CreateManagedNetwork(create clustertypes.NetworkCreateRequest) error {
	_, err := daemon.createNetwork(create.NetworkCreateRequest, create.ID, true)
	return err
}

// CreateNetwork creates a network with the given name, driver and other optional parameters
func (daemon *Daemon) CreateNetwork(create types.NetworkCreateRequest) (*types.NetworkCreateResponse, error) {
	resp, err := daemon.createNetwork(create, "", false)
	if err != nil {
		return nil, err
	}
	return resp, err
}

func (daemon *Daemon) createNetwork(create types.NetworkCreateRequest, id string, agent bool) (*types.NetworkCreateResponse, error) {
	if runconfig.IsPreDefinedNetwork(create.Name) {
		return nil, PredefinedNetworkError(create.Name)
	}

	var warning string
	nw, err := daemon.GetNetworkByName(create.Name)
	if err != nil {
		if _, ok := err.(libnetwork.ErrNoSuchNetwork); !ok {
			return nil, err
		}
	}
	if nw != nil {
		// check if user defined CheckDuplicate, if set true, return err
		// otherwise prepare a warning message
		if create.CheckDuplicate {
			if !agent || nw.Info().Dynamic() {
				return nil, libnetwork.NetworkNameError(create.Name)
			}
		}
		warning = fmt.Sprintf("Network with name %s (id : %s) already exists", nw.Name(), nw.ID())
	}

	c := daemon.netController
	driver := create.Driver
	if driver == "" {
		driver = c.Config().Daemon.DefaultDriver
	}

	nwOptions := []libnetwork.NetworkOption{
		libnetwork.NetworkOptionEnableIPv6(create.EnableIPv6),
		libnetwork.NetworkOptionDriverOpts(create.Options),
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
			return nil, fmt.Errorf("Failed to find a load balancer IP to use for network: %v", id)
		}

		nwOptions = append(nwOptions, libnetwork.NetworkOptionLBEndpoint(nodeIP))
	}

	n, err := c.NewNetwork(driver, create.Name, id, nwOptions...)
	if err != nil {
		if _, ok := err.(libnetwork.ErrDataStoreNotInitialized); ok {
			// nolint: golint
			return nil, errors.New("This node is not a swarm manager. Use \"docker swarm init\" or \"docker swarm join\" to connect this node to swarm and try again.")
		}
		return nil, err
	}

	daemon.pluginRefCount(driver, driverapi.NetworkPluginEndpointType, plugingetter.Acquire)
	if create.IPAM != nil {
		daemon.pluginRefCount(create.IPAM.Driver, ipamapi.PluginEndpointType, plugingetter.Acquire)
	}
	daemon.LogNetworkEvent(n, "create")

	return &types.NetworkCreateResponse{
		ID:      n.ID(),
		Warning: warning,
	}, nil
}

func (daemon *Daemon) pluginRefCount(driver, capability string, mode int) {
	var builtinDrivers []string

	if capability == driverapi.NetworkPluginEndpointType {
		builtinDrivers = daemon.netController.BuiltinDrivers()
	} else if capability == ipamapi.PluginEndpointType {
		builtinDrivers = daemon.netController.BuiltinIPAMDrivers()
	}

	for _, d := range builtinDrivers {
		if d == driver {
			return
		}
	}

	if daemon.PluginStore != nil {
		_, err := daemon.PluginStore.Get(driver, capability, mode)
		if err != nil {
			logrus.WithError(err).WithFields(logrus.Fields{"mode": mode, "driver": driver}).Error("Error handling plugin refcount operation")
		}
	}
}

func getIpamConfig(data []network.IPAMConfig) ([]*libnetwork.IpamConf, []*libnetwork.IpamConf, error) {
	ipamV4Cfg := []*libnetwork.IpamConf{}
	ipamV6Cfg := []*libnetwork.IpamConf{}
	for _, d := range data {
		iCfg := libnetwork.IpamConf{}
		iCfg.PreferredPool = d.Subnet
		iCfg.SubPool = d.IPRange
		iCfg.Gateway = d.Gateway
		iCfg.AuxAddresses = d.AuxAddress
		ip, _, err := net.ParseCIDR(d.Subnet)
		if err != nil {
			return nil, nil, fmt.Errorf("Invalid subnet %s : %v", d.Subnet, err)
		}
		if ip.To4() != nil {
			ipamV4Cfg = append(ipamV4Cfg, &iCfg)
		} else {
			ipamV6Cfg = append(ipamV6Cfg, &iCfg)
		}
	}
	return ipamV4Cfg, ipamV6Cfg, nil
}

// UpdateContainerServiceConfig updates a service configuration.
func (daemon *Daemon) UpdateContainerServiceConfig(containerName string, serviceConfig *clustertypes.ServiceConfig) error {
	container, err := daemon.GetContainer(containerName)
	if err != nil {
		return err
	}

	container.NetworkSettings.Service = serviceConfig
	return nil
}

// ConnectContainerToNetwork connects the given container to the given
// network. If either cannot be found, an err is returned. If the
// network cannot be set up, an err is returned.
func (daemon *Daemon) ConnectContainerToNetwork(containerName, networkName string, endpointConfig *network.EndpointSettings) error {
	container, err := daemon.GetContainer(containerName)
	if err != nil {
		return err
	}
	return daemon.ConnectToNetwork(container, networkName, endpointConfig)
}

// DisconnectContainerFromNetwork disconnects the given container from
// the given network. If either cannot be found, an err is returned.
func (daemon *Daemon) DisconnectContainerFromNetwork(containerName string, networkName string, force bool) error {
	container, err := daemon.GetContainer(containerName)
	if err != nil {
		if force {
			return daemon.ForceEndpointDelete(containerName, networkName)
		}
		return err
	}
	return daemon.DisconnectFromNetwork(container, networkName, force)
}

// GetNetworkDriverList returns the list of plugins drivers
// registered for network.
func (daemon *Daemon) GetNetworkDriverList() []string {
	if !daemon.NetworkControllerEnabled() {
		return nil
	}

	pluginList := daemon.netController.BuiltinDrivers()

	managedPlugins := daemon.PluginStore.GetAllManagedPluginsByCap(driverapi.NetworkPluginEndpointType)

	for _, plugin := range managedPlugins {
		pluginList = append(pluginList, plugin.Name())
	}

	pluginMap := make(map[string]bool)
	for _, plugin := range pluginList {
		pluginMap[plugin] = true
	}

	networks := daemon.netController.Networks()

	for _, network := range networks {
		if !pluginMap[network.Type()] {
			pluginList = append(pluginList, network.Type())
			pluginMap[network.Type()] = true
		}
	}

	sort.Strings(pluginList)

	return pluginList
}

// DeleteManagedNetwork deletes an agent network.
// The requirement of networkID is enforced.
func (daemon *Daemon) DeleteManagedNetwork(networkID string) error {
	n, err := daemon.GetNetworkByID(networkID)
	if err != nil {
		return err
	}
	return daemon.deleteNetwork(n, true)
}

// DeleteNetwork destroys a network unless it's one of docker's predefined networks.
func (daemon *Daemon) DeleteNetwork(networkID string) error {
	n, err := daemon.GetNetworkByID(networkID)
	if err != nil {
		return errors.Wrap(err, "could not find network by ID")
	}
	return daemon.deleteNetwork(n, false)
}

func (daemon *Daemon) deleteNetwork(nw libnetwork.Network, dynamic bool) error {
	if runconfig.IsPreDefinedNetwork(nw.Name()) && !dynamic {
		err := fmt.Errorf("%s is a pre-defined network and cannot be removed", nw.Name())
		return errdefs.Forbidden(err)
	}

	if dynamic && !nw.Info().Dynamic() {
		if runconfig.IsPreDefinedNetwork(nw.Name()) {
			// Predefined networks now support swarm services. Make this
			// a no-op when cluster requests to remove the predefined network.
			return nil
		}
		err := fmt.Errorf("%s is not a dynamic network", nw.Name())
		return errdefs.Forbidden(err)
	}

	if err := nw.Delete(); err != nil {
		return errors.Wrap(err, "error while removing network")
	}

	// If this is not a configuration only network, we need to
	// update the corresponding remote drivers' reference counts
	if !nw.Info().ConfigOnly() {
		daemon.pluginRefCount(nw.Type(), driverapi.NetworkPluginEndpointType, plugingetter.Release)
		ipamType, _, _, _ := nw.Info().IpamConfig()
		daemon.pluginRefCount(ipamType, ipamapi.PluginEndpointType, plugingetter.Release)
		daemon.LogNetworkEvent(nw, "destroy")
	}

	return nil
}

// GetNetworks returns a list of all networks
func (daemon *Daemon) GetNetworks(filter filters.Args, config types.NetworkListConfig) ([]types.NetworkResource, error) {
	networks := daemon.getAllNetworks()

	list := make([]types.NetworkResource, 0, len(networks))
	var idx map[string]libnetwork.Network
	if config.Detailed {
		idx = make(map[string]libnetwork.Network)
	}

	for _, n := range networks {
		nr := buildNetworkResource(n)
		list = append(list, nr)
		if config.Detailed {
			idx[nr.ID] = n
		}
	}

	var err error
	list, err = internalnetwork.FilterNetworks(list, filter)
	if err != nil {
		return nil, err
	}

	if config.Detailed {
		for i, n := range list {
			np := &n
			buildDetailedNetworkResources(np, idx[n.ID], config.Verbose)
			list[i] = *np
		}
	}

	return list, nil
}

func buildNetworkResource(nw libnetwork.Network) types.NetworkResource {
	r := types.NetworkResource{}
	if nw == nil {
		return r
	}

	info := nw.Info()
	r.Name = nw.Name()
	r.ID = nw.ID()
	r.Created = info.Created()
	r.Scope = info.Scope()
	r.Driver = nw.Type()
	r.EnableIPv6 = info.IPv6Enabled()
	r.Internal = info.Internal()
	r.Attachable = info.Attachable()
	r.Ingress = info.Ingress()
	r.Options = info.DriverOptions()
	r.Containers = make(map[string]types.EndpointResource)
	buildIpamResources(&r, info)
	r.Labels = info.Labels()
	r.ConfigOnly = info.ConfigOnly()

	if cn := info.ConfigFrom(); cn != "" {
		r.ConfigFrom = network.ConfigReference{Network: cn}
	}

	peers := info.Peers()
	if len(peers) != 0 {
		r.Peers = buildPeerInfoResources(peers)
	}

	return r
}

func buildDetailedNetworkResources(r *types.NetworkResource, nw libnetwork.Network, verbose bool) {
	if nw == nil {
		return
	}

	epl := nw.Endpoints()
	for _, e := range epl {
		ei := e.Info()
		if ei == nil {
			continue
		}
		sb := ei.Sandbox()
		tmpID := e.ID()
		key := "ep-" + tmpID
		if sb != nil {
			key = sb.ContainerID()
		}

		r.Containers[key] = buildEndpointResource(tmpID, e.Name(), ei)
	}
	if !verbose {
		return
	}
	services := nw.Info().Services()
	r.Services = make(map[string]network.ServiceInfo)
	for name, service := range services {
		tasks := []network.Task{}
		for _, t := range service.Tasks {
			tasks = append(tasks, network.Task{
				Name:       t.Name,
				EndpointID: t.EndpointID,
				EndpointIP: t.EndpointIP,
				Info:       t.Info,
			})
		}
		r.Services[name] = network.ServiceInfo{
			VIP:          service.VIP,
			Ports:        service.Ports,
			Tasks:        tasks,
			LocalLBIndex: service.LocalLBIndex,
		}
	}
}

func buildPeerInfoResources(peers []networkdb.PeerInfo) []network.PeerInfo {
	peerInfo := make([]network.PeerInfo, 0, len(peers))
	for _, peer := range peers {
		peerInfo = append(peerInfo, network.PeerInfo{
			Name: peer.Name,
			IP:   peer.IP,
		})
	}
	return peerInfo
}

func buildIpamResources(r *types.NetworkResource, nwInfo libnetwork.NetworkInfo) {
	id, opts, ipv4conf, ipv6conf := nwInfo.IpamConfig()

	ipv4Info, ipv6Info := nwInfo.IpamInfo()

	r.IPAM.Driver = id

	r.IPAM.Options = opts

	r.IPAM.Config = []network.IPAMConfig{}
	for _, ip4 := range ipv4conf {
		if ip4.PreferredPool == "" {
			continue
		}
		iData := network.IPAMConfig{}
		iData.Subnet = ip4.PreferredPool
		iData.IPRange = ip4.SubPool
		iData.Gateway = ip4.Gateway
		iData.AuxAddress = ip4.AuxAddresses
		r.IPAM.Config = append(r.IPAM.Config, iData)
	}

	if len(r.IPAM.Config) == 0 {
		for _, ip4Info := range ipv4Info {
			iData := network.IPAMConfig{}
			iData.Subnet = ip4Info.IPAMData.Pool.String()
			if ip4Info.IPAMData.Gateway != nil {
				iData.Gateway = ip4Info.IPAMData.Gateway.IP.String()
			}
			r.IPAM.Config = append(r.IPAM.Config, iData)
		}
	}

	hasIpv6Conf := false
	for _, ip6 := range ipv6conf {
		if ip6.PreferredPool == "" {
			continue
		}
		hasIpv6Conf = true
		iData := network.IPAMConfig{}
		iData.Subnet = ip6.PreferredPool
		iData.IPRange = ip6.SubPool
		iData.Gateway = ip6.Gateway
		iData.AuxAddress = ip6.AuxAddresses
		r.IPAM.Config = append(r.IPAM.Config, iData)
	}

	if !hasIpv6Conf {
		for _, ip6Info := range ipv6Info {
			if ip6Info.IPAMData.Pool == nil {
				continue
			}
			iData := network.IPAMConfig{}
			iData.Subnet = ip6Info.IPAMData.Pool.String()
			iData.Gateway = ip6Info.IPAMData.Gateway.String()
			r.IPAM.Config = append(r.IPAM.Config, iData)
		}
	}
}

func buildEndpointResource(id string, name string, info libnetwork.EndpointInfo) types.EndpointResource {
	er := types.EndpointResource{}

	er.EndpointID = id
	er.Name = name
	ei := info
	if ei == nil {
		return er
	}

	if iface := ei.Iface(); iface != nil {
		if mac := iface.MacAddress(); mac != nil {
			er.MacAddress = mac.String()
		}
		if ip := iface.Address(); ip != nil && len(ip.IP) > 0 {
			er.IPv4Address = ip.String()
		}

		if ipv6 := iface.AddressIPv6(); ipv6 != nil && len(ipv6.IP) > 0 {
			er.IPv6Address = ipv6.String()
		}
	}
	return er
}

// clearAttachableNetworks removes the attachable networks
// after disconnecting any connected container
func (daemon *Daemon) clearAttachableNetworks() {
	for _, n := range daemon.getAllNetworks() {
		if !n.Info().Attachable() {
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
				logrus.Warnf("Failed to disconnect container %s from swarm network %s on cluster leave: %v",
					containerID, n.Name(), err)
			}
		}
		if err := daemon.DeleteManagedNetwork(n.ID()); err != nil {
			logrus.Warnf("Failed to remove swarm network %s on cluster leave: %v", n.Name(), err)
		}
	}
}

// buildCreateEndpointOptions builds endpoint options from a given network.
func buildCreateEndpointOptions(c *container.Container, n libnetwork.Network, epConfig *network.EndpointSettings, sb libnetwork.Sandbox, daemonDNS []string) ([]libnetwork.EndpointOption, error) {
	var (
		bindings      = make(nat.PortMap)
		pbList        []networktypes.PortBinding
		exposeList    []networktypes.TransportPort
		createOptions []libnetwork.EndpointOption
	)

	defaultNetName := runconfig.DefaultDaemonNetworkMode().NetworkName()

	if (!serviceDiscoveryOnDefaultNetwork() && n.Name() == defaultNetName) ||
		c.NetworkSettings.IsAnonymousEndpoint {
		createOptions = append(createOptions, libnetwork.CreateOptionAnonymous())
	}

	if epConfig != nil {
		ipam := epConfig.IPAMConfig

		if ipam != nil {
			var (
				ipList          []net.IP
				ip, ip6, linkip net.IP
			)

			for _, ips := range ipam.LinkLocalIPs {
				if linkip = net.ParseIP(ips); linkip == nil && ips != "" {
					return nil, errors.Errorf("Invalid link-local IP address: %s", ipam.LinkLocalIPs)
				}
				ipList = append(ipList, linkip)

			}

			if ip = net.ParseIP(ipam.IPv4Address); ip == nil && ipam.IPv4Address != "" {
				return nil, errors.Errorf("Invalid IPv4 address: %s)", ipam.IPv4Address)
			}

			if ip6 = net.ParseIP(ipam.IPv6Address); ip6 == nil && ipam.IPv6Address != "" {
				return nil, errors.Errorf("Invalid IPv6 address: %s)", ipam.IPv6Address)
			}

			createOptions = append(createOptions,
				libnetwork.CreateOptionIpam(ip, ip6, ipList, nil))

		}

		for _, alias := range epConfig.Aliases {
			createOptions = append(createOptions, libnetwork.CreateOptionMyAlias(alias))
		}
		for k, v := range epConfig.DriverOpts {
			createOptions = append(createOptions, libnetwork.EndpointOptionGeneric(options.Generic{k: v}))
		}
	}

	if c.NetworkSettings.Service != nil {
		svcCfg := c.NetworkSettings.Service

		var vip string
		if svcCfg.VirtualAddresses[n.ID()] != nil {
			vip = svcCfg.VirtualAddresses[n.ID()].IPv4
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

		createOptions = append(createOptions, libnetwork.CreateOptionService(svcCfg.Name, svcCfg.ID, net.ParseIP(vip), portConfigs, svcCfg.Aliases[n.ID()]))
	}

	if !containertypes.NetworkMode(n.Name()).IsUserDefined() {
		createOptions = append(createOptions, libnetwork.CreateOptionDisableResolution())
	}

	// configs that are applicable only for the endpoint in the network
	// to which container was connected to on docker run.
	// Ideally all these network-specific endpoint configurations must be moved under
	// container.NetworkSettings.Networks[n.Name()]
	if n.Name() == c.HostConfig.NetworkMode.NetworkName() ||
		(n.Name() == defaultNetName && c.HostConfig.NetworkMode.IsDefault()) {
		if c.Config.MacAddress != "" {
			mac, err := net.ParseMAC(c.Config.MacAddress)
			if err != nil {
				return nil, err
			}

			genericOption := options.Generic{
				netlabel.MacAddress: mac,
			}

			createOptions = append(createOptions, libnetwork.EndpointOptionGeneric(genericOption))
		}

	}

	// Port-mapping rules belong to the container & applicable only to non-internal networks
	portmaps := getSandboxPortMapInfo(sb)
	if n.Info().Internal() || len(portmaps) > 0 {
		return createOptions, nil
	}

	if c.HostConfig.PortBindings != nil {
		for p, b := range c.HostConfig.PortBindings {
			bindings[p] = []nat.PortBinding{}
			for _, bb := range b {
				bindings[p] = append(bindings[p], nat.PortBinding{
					HostIP:   bb.HostIP,
					HostPort: bb.HostPort,
				})
			}
		}
	}

	portSpecs := c.Config.ExposedPorts
	ports := make([]nat.Port, len(portSpecs))
	var i int
	for p := range portSpecs {
		ports[i] = p
		i++
	}
	nat.SortPortMap(ports, bindings)
	for _, port := range ports {
		expose := networktypes.TransportPort{}
		expose.Proto = networktypes.ParseProtocol(port.Proto())
		expose.Port = uint16(port.Int())
		exposeList = append(exposeList, expose)

		pb := networktypes.PortBinding{Port: expose.Port, Proto: expose.Proto}
		binding := bindings[port]
		for i := 0; i < len(binding); i++ {
			pbCopy := pb.GetCopy()
			newP, err := nat.NewPort(nat.SplitProtoPort(binding[i].HostPort))
			var portStart, portEnd int
			if err == nil {
				portStart, portEnd, err = newP.Range()
			}
			if err != nil {
				return nil, errors.Wrapf(err, "Error parsing HostPort value (%s)", binding[i].HostPort)
			}
			pbCopy.HostPort = uint16(portStart)
			pbCopy.HostPortEnd = uint16(portEnd)
			pbCopy.HostIP = net.ParseIP(binding[i].HostIP)
			pbList = append(pbList, pbCopy)
		}

		if c.HostConfig.PublishAllPorts && len(binding) == 0 {
			pbList = append(pbList, pb)
		}
	}

	var dns []string

	if len(c.HostConfig.DNS) > 0 {
		dns = c.HostConfig.DNS
	} else if len(daemonDNS) > 0 {
		dns = daemonDNS
	}

	if len(dns) > 0 {
		createOptions = append(createOptions,
			libnetwork.CreateOptionDNS(dns))
	}

	createOptions = append(createOptions,
		libnetwork.CreateOptionPortMapping(pbList),
		libnetwork.CreateOptionExposedPorts(exposeList))

	return createOptions, nil
}

// getEndpointInNetwork returns the container's endpoint to the provided network.
func getEndpointInNetwork(name string, n libnetwork.Network) (libnetwork.Endpoint, error) {
	endpointName := strings.TrimPrefix(name, "/")
	return n.EndpointByName(endpointName)
}

// getSandboxPortMapInfo retrieves the current port-mapping programmed for the given sandbox
func getSandboxPortMapInfo(sb libnetwork.Sandbox) nat.PortMap {
	pm := nat.PortMap{}
	if sb == nil {
		return pm
	}

	for _, ep := range sb.Endpoints() {
		pm, _ = getEndpointPortMapInfo(ep)
		if len(pm) > 0 {
			break
		}
	}
	return pm
}

func getEndpointPortMapInfo(ep libnetwork.Endpoint) (nat.PortMap, error) {
	pm := nat.PortMap{}
	driverInfo, err := ep.DriverInfo()
	if err != nil {
		return pm, err
	}

	if driverInfo == nil {
		// It is not an error for epInfo to be nil
		return pm, nil
	}

	if expData, ok := driverInfo[netlabel.ExposedPorts]; ok {
		if exposedPorts, ok := expData.([]networktypes.TransportPort); ok {
			for _, tp := range exposedPorts {
				natPort, err := nat.NewPort(tp.Proto.String(), strconv.Itoa(int(tp.Port)))
				if err != nil {
					return pm, fmt.Errorf("Error parsing Port value(%v):%v", tp.Port, err)
				}
				pm[natPort] = nil
			}
		}
	}

	mapData, ok := driverInfo[netlabel.PortMap]
	if !ok {
		return pm, nil
	}

	if portMapping, ok := mapData.([]networktypes.PortBinding); ok {
		for _, pp := range portMapping {
			natPort, err := nat.NewPort(pp.Proto.String(), strconv.Itoa(int(pp.Port)))
			if err != nil {
				return pm, err
			}
			natBndg := nat.PortBinding{HostIP: pp.HostIP.String(), HostPort: strconv.Itoa(int(pp.HostPort))}
			pm[natPort] = append(pm[natPort], natBndg)
		}
	}

	return pm, nil
}

// buildEndpointInfo sets endpoint-related fields on container.NetworkSettings based on the provided network and endpoint.
func buildEndpointInfo(networkSettings *internalnetwork.Settings, n libnetwork.Network, ep libnetwork.Endpoint) error {
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

	if _, ok := networkSettings.Networks[n.Name()]; !ok {
		networkSettings.Networks[n.Name()] = &internalnetwork.EndpointSettings{
			EndpointSettings: &network.EndpointSettings{},
		}
	}
	networkSettings.Networks[n.Name()].NetworkID = n.ID()
	networkSettings.Networks[n.Name()].EndpointID = ep.ID()

	iface := epInfo.Iface()
	if iface == nil {
		return nil
	}

	if iface.MacAddress() != nil {
		networkSettings.Networks[n.Name()].MacAddress = iface.MacAddress().String()
	}

	if iface.Address() != nil {
		ones, _ := iface.Address().Mask.Size()
		networkSettings.Networks[n.Name()].IPAddress = iface.Address().IP.String()
		networkSettings.Networks[n.Name()].IPPrefixLen = ones
	}

	if iface.AddressIPv6() != nil && iface.AddressIPv6().IP.To16() != nil {
		onesv6, _ := iface.AddressIPv6().Mask.Size()
		networkSettings.Networks[n.Name()].GlobalIPv6Address = iface.AddressIPv6().IP.String()
		networkSettings.Networks[n.Name()].GlobalIPv6PrefixLen = onesv6
	}

	return nil
}

// buildJoinOptions builds endpoint Join options from a given network.
func buildJoinOptions(networkSettings *internalnetwork.Settings, n interface {
	Name() string
}) ([]libnetwork.EndpointOption, error) {
	var joinOptions []libnetwork.EndpointOption
	if epConfig, ok := networkSettings.Networks[n.Name()]; ok {
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
	}

	return joinOptions, nil
}
