package libnetwork

//go:generate protoc -I=. -I=../../vendor/ --gogofaster_out=import_path=github.com/moby/moby/v2/daemon/libnetwork:. agent.proto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sort"
	"sync"

	"github.com/containerd/log"
	"github.com/docker/go-events"
	"github.com/gogo/protobuf/proto"
	"github.com/moby/moby/v2/daemon/libnetwork/cluster"
	"github.com/moby/moby/v2/daemon/libnetwork/discoverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/driverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/networkdb"
	"github.com/moby/moby/v2/daemon/libnetwork/scope"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/moby/moby/v2/internal/iterutil"
)

const (
	subsysGossip = "networking:gossip"
	subsysIPSec  = "networking:ipsec"
	keyringSize  = 3
)

// ByTime implements sort.Interface for []*types.EncryptionKey based on
// the LamportTime field.
type ByTime []*types.EncryptionKey

func (b ByTime) Len() int           { return len(b) }
func (b ByTime) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b ByTime) Less(i, j int) bool { return b[i].LamportTime < b[j].LamportTime }

type nwAgent struct {
	networkDB         *networkdb.NetworkDB
	bindAddr          net.IP
	advertiseAddr     string
	dataPathAddr      string
	coreCancelFuncs   []func()
	driverCancelFuncs map[string][]func()
	mu                sync.Mutex
}

func (a *nwAgent) dataPathAddress() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dataPathAddr != "" {
		return a.dataPathAddr
	}
	return a.advertiseAddr
}

const libnetworkEPTable = "endpoint_table"

func getBindAddr(ifaceName string) (net.IP, error) {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("failed to find interface %s: %v", ifaceName, err)
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("failed to get interface addresses: %v", err)
	}

	for _, a := range addrs {
		addr, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		addrIP := addr.IP

		if addrIP.IsLinkLocalUnicast() {
			continue
		}

		return addrIP, nil
	}

	return nil, errors.New("failed to get bind address")
}

// resolveAddr resolves the given address, which can be one of, and
// parsed in the following order or priority:
//
// - a well-formed IP-address
// - a hostname
// - an interface-name
func resolveAddr(addrOrInterface string) (net.IP, error) {
	// Try and see if this is a valid IP address
	if ip := net.ParseIP(addrOrInterface); ip != nil {
		return ip, nil
	}

	// If not a valid IP address, it could be a hostname.
	addr, err := net.ResolveIPAddr("ip", addrOrInterface)
	if err != nil {
		// If hostname lookup failed, try to look for an interface with the given name.
		return getBindAddr(addrOrInterface)
	}
	return addr.IP, nil
}

func (c *Controller) handleKeyChange(encryptionKeys []*types.EncryptionKey) error {
	drvEnc := discoverapi.DriverEncryptionUpdate{}

	agent := c.getAgent()
	if agent == nil {
		log.G(context.TODO()).Debug("Skipping key change as agent is nil")
		return nil
	}

	// Find the deleted key. If the deleted key was the primary key,
	// a new primary key should be set before removing if from keyring.
	c.mu.Lock()
	added := []byte{}
	deleted := []byte{}
	j := len(c.keys)
	for i := 0; i < j; {
		same := false
		for _, key := range encryptionKeys {
			if same = key.LamportTime == c.keys[i].LamportTime; same {
				break
			}
		}
		if !same {
			cKey := c.keys[i]
			if cKey.Subsystem == subsysGossip {
				deleted = cKey.Key
			}

			if cKey.Subsystem == subsysIPSec {
				drvEnc.Prune = cKey.Key
				drvEnc.PruneTag = cKey.LamportTime
			}
			c.keys[i], c.keys[j-1] = c.keys[j-1], c.keys[i]
			c.keys[j-1] = nil
			j--
		}
		i++
	}
	c.keys = c.keys[:j]

	// Find the new key and add it to the key ring
	for _, key := range encryptionKeys {
		same := false
		for _, cKey := range c.keys {
			if same = cKey.LamportTime == key.LamportTime; same {
				break
			}
		}
		if !same {
			c.keys = append(c.keys, key)
			if key.Subsystem == subsysGossip {
				added = key.Key
			}

			if key.Subsystem == subsysIPSec {
				drvEnc.Key = key.Key
				drvEnc.Tag = key.LamportTime
			}
		}
	}
	c.mu.Unlock()

	if len(added) > 0 {
		agent.networkDB.SetKey(added)
	}

	key, _, err := c.getPrimaryKeyTag(subsysGossip)
	if err != nil {
		return err
	}
	agent.networkDB.SetPrimaryKey(key)

	key, tag, err := c.getPrimaryKeyTag(subsysIPSec)
	if err != nil {
		return err
	}
	drvEnc.Primary = key
	drvEnc.PrimaryTag = tag

	if len(deleted) > 0 {
		agent.networkDB.RemoveKey(deleted)
	}

	c.drvRegistry.WalkDrivers(func(name string, driver driverapi.Driver, capability driverapi.Capability) bool {
		dr, ok := driver.(discoverapi.Discover)
		if !ok {
			return false
		}
		if err := dr.DiscoverNew(discoverapi.EncryptionKeysUpdate, drvEnc); err != nil {
			log.G(context.TODO()).WithFields(log.Fields{
				"error":  err,
				"driver": name,
			}).Warn("Failed to update datapath keys; resetting datapath keys")
			// Attempt to reconfigure keys in case of a update failure
			// which can arise due to a mismatch of keys
			// if worker nodes get temporarily disconnected
			keys, tags := c.getKeys(subsysIPSec)
			err = dr.DiscoverNew(discoverapi.EncryptionKeysConfig, discoverapi.DriverEncryptionConfig{
				Keys: keys,
				Tags: tags,
			})
			if err != nil {
				log.G(context.TODO()).WithFields(log.Fields{
					"error":  err,
					"driver": name,
				}).Warn("Failed to reset datapath keys")
			}
		}
		return false
	})

	return nil
}

func (c *Controller) agentSetup(clusterProvider cluster.Provider) error {
	agent := c.getAgent()
	if agent != nil {
		// agent is already present, so there is no need initialize it again.
		return nil
	}

	bindAddr := clusterProvider.GetLocalAddress()
	advAddr := clusterProvider.GetAdvertiseAddress()
	dataAddr := clusterProvider.GetDataPathAddress()
	remoteAddrList := remoteAddrs(clusterProvider)

	listen := clusterProvider.GetListenAddress()
	listenAddr, _, _ := net.SplitHostPort(listen)

	log.G(context.TODO()).WithFields(log.Fields{
		"listen-addr":               listenAddr,
		"local-addr":                bindAddr,
		"advertise-addr":            advAddr,
		"data-path-addr":            dataAddr,
		"remote-addr-list":          remoteAddrList,
		"network-control-plane-mtu": c.Config().NetworkControlPlaneMTU,
	}).Info("Initializing Libnetwork Agent")
	if advAddr != "" {
		if err := c.agentInit(listenAddr, bindAddr, advAddr, dataAddr); err != nil {
			log.G(context.TODO()).WithError(err).Errorf("Error in agentInit")
			return err
		}
		c.drvRegistry.WalkDrivers(func(name string, driver driverapi.Driver, capability driverapi.Capability) bool {
			if capability.ConnectivityScope == scope.Global {
				if d, ok := driver.(discoverapi.Discover); ok {
					c.agentDriverNotify(d)
				}
			}
			return false
		})
	}

	if len(remoteAddrList) > 0 {
		if err := c.agentJoin(remoteAddrList); err != nil {
			log.G(context.TODO()).WithError(err).Error("Error in joining gossip cluster: join will be retried in background")
		}
	}

	return nil
}

// For a given subsystem getKeys sorts the keys by lamport time and returns
// slice of keys and lamport time which can used as a unique tag for the keys
func (c *Controller) getKeys(subsystem string) (keys [][]byte, tags []uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	sort.Sort(ByTime(c.keys))

	keys = make([][]byte, 0, len(c.keys))
	tags = make([]uint64, 0, len(c.keys))
	for _, key := range c.keys {
		if key.Subsystem == subsystem {
			keys = append(keys, key.Key)
			tags = append(tags, key.LamportTime)
		}
	}

	if len(keys) > 1 {
		// TODO(thaJeztah): why are we swapping order here? This code was added in https://github.com/moby/libnetwork/commit/e83d68b7d1fd9c479120914024242238f791b4dc
		keys[0], keys[1] = keys[1], keys[0]
		tags[0], tags[1] = tags[1], tags[0]
	}
	return keys, tags
}

// getPrimaryKeyTag returns the primary key for a given subsystem from the
// list of sorted key and the associated tag
func (c *Controller) getPrimaryKeyTag(subsystem string) (key []byte, lamportTime uint64, _ error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sort.Sort(ByTime(c.keys))
	keys := make([]*types.EncryptionKey, 0, len(c.keys))
	for _, k := range c.keys {
		if k.Subsystem == subsystem {
			keys = append(keys, k)
		}
	}
	if len(keys) < 2 {
		return nil, 0, fmt.Errorf("no primary key found for %s subsystem: %d keys found on controller, expected at least 2", subsystem, len(keys))
	}
	return keys[1].Key, keys[1].LamportTime, nil
}

func (c *Controller) agentInit(listenAddr, bindAddrOrInterface, advertiseAddr, dataPathAddr string) error {
	bindAddr, err := resolveAddr(bindAddrOrInterface)
	if err != nil {
		return err
	}

	keys, _ := c.getKeys(subsysGossip)

	netDBConf := networkdb.DefaultConfig()
	netDBConf.BindAddr = listenAddr
	netDBConf.AdvertiseAddr = advertiseAddr
	netDBConf.Keys = keys
	// Answered afresh every time a rejoin is considered, so that a node which
	// has to bootstrap its way back into the gossip cluster aims at the
	// managers the cluster has now rather than the ones it had at startup.
	netDBConf.BootstrapPeers = c.remoteAddrList
	if c.Config().NetworkControlPlaneMTU != 0 {
		// Consider the MTU remove the IP hdr (IPv4 or IPv6) and the TCP/UDP hdr.
		// To be on the safe side let's cut 100 bytes
		netDBConf.PacketBufferSize = (c.Config().NetworkControlPlaneMTU - 100)
		log.G(context.TODO()).Debugf("Control plane MTU: %d will initialize NetworkDB with: %d",
			c.Config().NetworkControlPlaneMTU, netDBConf.PacketBufferSize)
	}
	nDB, err := networkdb.New(netDBConf)
	if err != nil {
		return err
	}

	// Register the diagnostic handlers
	nDB.RegisterDiagnosticHandlers(c.diagnosticServer)

	var cancelList []func()
	ch, cancel := nDB.Watch(libnetworkEPTable, "")
	cancelList = append(cancelList, cancel)
	nodeCh, cancel := nDB.Watch(networkdb.NodeTable, "")
	cancelList = append(cancelList, cancel)
	gatewayCh, cancel := nDB.Watch(ingressGatewayTable, "")
	cancelList = append(cancelList, cancel)

	c.mu.Lock()
	c.agent = &nwAgent{
		networkDB:         nDB,
		bindAddr:          bindAddr,
		advertiseAddr:     advertiseAddr,
		dataPathAddr:      dataPathAddr,
		coreCancelFuncs:   cancelList,
		driverCancelFuncs: make(map[string][]func()),
	}
	c.mu.Unlock()

	go c.handleTableEvents(ch, func(ev events.Event) { handleEpTableEvent(c, ev) })
	go c.handleTableEvents(nodeCh, c.handleNodeTableEvent)
	go c.handleTableEvents(gatewayCh, c.handleIngressGatewayTableEvent)

	keys, tags := c.getKeys(subsysIPSec)
	c.drvRegistry.WalkDrivers(func(name string, driver driverapi.Driver, capability driverapi.Capability) bool {
		if dr, ok := driver.(discoverapi.Discover); ok {
			if err := dr.DiscoverNew(discoverapi.EncryptionKeysConfig, discoverapi.DriverEncryptionConfig{
				Keys: keys,
				Tags: tags,
			}); err != nil {
				log.G(context.TODO()).Warnf("Failed to set datapath keys in driver %s: %v", name, err)
			}
		}
		return false
	})

	c.WalkNetworks(joinCluster)
	c.WalkNetworks(func(n *Network) bool {
		n.restoreIngressSourceIPRouting()
		return false
	})

	return nil
}

// remoteAddrList is [remoteAddrs] for whichever cluster provider is current,
// so that a NetworkDB holding on to it as a callback asks the cluster the node
// is in now rather than the one it was in when the agent came up.
//
// It reports nil once the node has left the cluster and the provider has gone,
// which NetworkDB reads as "no bootstrap nodes known" and skips its rejoin.
func (c *Controller) remoteAddrList() []string {
	c.mu.Lock()
	cp := c.cfg.ClusterProvider
	c.mu.Unlock()
	if cp == nil {
		return nil
	}
	// Deliberately outside the lock above: this calls out into the cluster,
	// which takes locks of its own.
	return remoteAddrs(cp)
}

// remoteAddrs is the addresses cp reports for the cluster's other managers, as
// NetworkDB wants them: bare IPs, with the port the cluster provider reports
// dropped, because a peer is assumed to gossip on the same port we do.
func remoteAddrs(cp cluster.Provider) []string {
	remoteList := cp.GetRemoteAddressList()
	remoteAddrList := make([]string, 0, len(remoteList))
	for _, remote := range remoteList {
		addr, _, err := net.SplitHostPort(remote)
		if err != nil {
			// Nothing to drop: the provider named an address without a port.
			// Pass it through rather than the empty string SplitHostPort
			// leaves behind, which is not an address at all.
			addr = remote
		}
		remoteAddrList = append(remoteAddrList, addr)
	}
	return remoteAddrList
}

func (c *Controller) agentJoin(remoteAddrList []string) error {
	agent := c.getAgent()
	if agent == nil {
		return nil
	}
	return agent.networkDB.Join(remoteAddrList)
}

func (c *Controller) agentDriverNotify(d discoverapi.Discover) {
	agent := c.getAgent()
	if agent == nil {
		return
	}

	if err := d.DiscoverNew(discoverapi.NodeDiscovery, discoverapi.NodeDiscoveryData{
		Address:     agent.dataPathAddress(),
		BindAddress: agent.bindAddr.String(),
		Self:        true,
	}); err != nil {
		log.G(context.TODO()).Warnf("Failed the node discovery in driver: %v", err)
	}

	keys, tags := c.getKeys(subsysIPSec)
	if err := d.DiscoverNew(discoverapi.EncryptionKeysConfig, discoverapi.DriverEncryptionConfig{
		Keys: keys,
		Tags: tags,
	}); err != nil {
		log.G(context.TODO()).Warnf("Failed to set datapath keys in driver: %v", err)
	}
}

func (c *Controller) agentClose() {
	// Acquire current agent instance and reset its pointer
	// then run closing functions
	c.mu.Lock()
	agent := c.agent
	c.agent = nil
	c.mu.Unlock()

	// when the agent is closed the cluster provider should be cleaned up
	c.SetClusterProvider(nil)

	if agent == nil {
		return
	}

	var cancelList []func()

	agent.mu.Lock()
	for _, cancelFuncs := range agent.driverCancelFuncs {
		cancelList = append(cancelList, cancelFuncs...)
	}

	// Add also the cancel functions for the network db
	cancelList = append(cancelList, agent.coreCancelFuncs...)
	agent.mu.Unlock()

	for _, cancel := range cancelList {
		cancel()
	}

	agent.networkDB.Close()
}

// Task has the backend container details
type Task struct {
	Name       string
	EndpointID string
	EndpointIP string
	Info       map[string]string
}

// ServiceInfo has service specific details along with the list of backend tasks
type ServiceInfo struct {
	VIP          string
	LocalLBIndex int
	Tasks        []Task
	Ports        []string
}

type epRecord struct {
	ep      EndpointRecord
	info    map[string]string
	lbIndex int
}

// Services returns a map of services keyed by the service name with the details
// of all the tasks that belong to the service. Applicable only in swarm mode.
func (n *Network) Services() map[string]ServiceInfo {
	agent, ok := n.clusterAgent()
	if !ok {
		return nil
	}
	nwID := n.ID()
	d, err := n.driver(true)
	if err != nil {
		log.G(context.TODO()).Errorf("Could not resolve driver for network %s/%s while fetching services: %v", n.networkType, nwID, err)
		return nil
	}

	// Walk through libnetworkEPTable and fetch the driver agnostic endpoint info
	eps := make(map[string]epRecord)
	c := n.getController()
	for eid, value := range agent.networkDB.GetTableByNetwork(libnetworkEPTable, nwID) {
		var epRec EndpointRecord
		if err := proto.Unmarshal(value.Value, &epRec); err != nil {
			log.G(context.TODO()).Errorf("Unmarshal of libnetworkEPTable failed for endpoint %s in network %s, %v", eid, nwID, err)
			continue
		}
		eps[eid] = epRecord{
			ep:      epRec,
			lbIndex: c.getLBIndex(epRec.ServiceID, nwID, epRec.IngressPorts),
		}
	}

	// Walk through the driver's tables, have the driver decode the entries
	// and return the tuple {ep ID, value}. value is a string that coveys
	// relevant info about the endpoint.
	if d, ok := d.(driverapi.TableWatcher); ok {
		for _, table := range n.driverTables {
			if table.objType != driverapi.EndpointObject {
				continue
			}
			for key, value := range agent.networkDB.GetTableByNetwork(table.name, nwID) {
				epID, info := d.DecodeTableEntry(table.name, key, value.Value)
				if ep, ok := eps[epID]; !ok {
					log.G(context.TODO()).Errorf("Inconsistent driver and libnetwork state for endpoint %s", epID)
				} else {
					ep.info = info
					eps[epID] = ep
				}
			}
		}
	}

	// group the endpoints into a map keyed by the service name
	sinfo := make(map[string]ServiceInfo)
	for ep, epr := range eps {
		s, ok := sinfo[epr.ep.ServiceName]
		if !ok {
			s = ServiceInfo{
				VIP:          epr.ep.VirtualIP,
				LocalLBIndex: epr.lbIndex,
			}
		}
		if s.Ports == nil {
			ports := make([]string, 0, len(epr.ep.IngressPorts))
			for _, port := range epr.ep.IngressPorts {
				ports = append(ports, fmt.Sprintf("Target: %d, Publish: %d", port.TargetPort, port.PublishedPort))
			}
			s.Ports = ports
		}
		s.Tasks = append(s.Tasks, Task{
			Name:       epr.ep.Name,
			EndpointID: ep,
			EndpointIP: epr.ep.EndpointIP,
			Info:       epr.info,
		})
		sinfo[epr.ep.ServiceName] = s
	}
	return sinfo
}

// HasServiceAttachments returns true when len(n.Services()) > 0.
func (n *Network) HasServiceAttachments() bool {
	return len(n.Services()) > 0
}

// clusterAgent returns the cluster agent if the network is a swarm-scoped,
// multi-host network.
func (n *Network) clusterAgent() (agent *nwAgent, ok bool) {
	if n.scope != scope.Swarm || !n.driverIsMultihost() {
		return nil, false
	}
	a := n.getController().getAgent()
	return a, a != nil
}

func (n *Network) joinCluster() error {
	agent, ok := n.clusterAgent()
	if !ok {
		return nil
	}
	return agent.networkDB.JoinNetwork(n.ID())
}

func (n *Network) leaveCluster() error {
	agent, ok := n.clusterAgent()
	if !ok {
		return nil
	}
	return agent.networkDB.LeaveNetwork(n.ID())
}

func (ep *Endpoint) addDriverInfoToCluster() error {
	if ep.joinInfo == nil || len(ep.joinInfo.driverTableEntries) == 0 {
		return nil
	}
	n := ep.getNetwork()
	agent, ok := n.clusterAgent()
	if !ok {
		return nil
	}

	nwID := n.ID()
	for _, te := range ep.joinInfo.driverTableEntries {
		if err := agent.networkDB.CreateEntry(te.tableName, nwID, te.key, te.value); err != nil {
			return err
		}
	}
	return ep.addIngressGatewayInfoToCluster()
}

func (ep *Endpoint) deleteDriverInfoFromCluster() error {
	if ep.joinInfo == nil || len(ep.joinInfo.driverTableEntries) == 0 {
		return nil
	}
	n := ep.getNetwork()
	agent, ok := n.clusterAgent()
	if !ok {
		return nil
	}

	nwID := n.ID()
	if err := ep.deleteIngressGatewayInfoFromCluster(); err != nil {
		log.G(context.TODO()).WithError(err).Warn("Failed to remove ingress gateway information")
	}
	for _, te := range ep.joinInfo.driverTableEntries {
		if err := agent.networkDB.DeleteEntry(te.tableName, nwID, te.key); err != nil {
			return err
		}
	}
	return nil
}

func (ep *Endpoint) addServiceInfoToCluster(sb *Sandbox) error {
	if len(ep.dnsNames) == 0 || ep.Iface() == nil || ep.Iface().Address() == nil {
		return nil
	}

	n := ep.getNetwork()
	agent, ok := n.clusterAgent()
	if !ok {
		return nil
	}

	sb.service.Lock()
	defer sb.service.Unlock()
	log.G(context.TODO()).Debugf("addServiceInfoToCluster START for %s %s", ep.svcName, ep.ID())

	// Check that the endpoint is still present on the sandbox before adding it to the service discovery.
	// This is to handle a race between the EnableService and the sbLeave
	// It is possible that the EnableService starts, fetches the list of the endpoints and
	// by the time the addServiceInfoToCluster is called the endpoint got removed from the sandbox
	// The risk is that the deleteServiceInfoToCluster happens before the addServiceInfoToCluster.
	// This check under the Service lock of the sandbox ensure the correct behavior.
	// If the addServiceInfoToCluster arrives first may find or not the endpoint and will proceed or exit
	// but in any case the deleteServiceInfoToCluster will follow doing the cleanup if needed.
	// In case the deleteServiceInfoToCluster arrives first, this one is happening after the endpoint is
	// removed from the list, in this situation the delete will bail out not finding any data to cleanup
	// and the add will bail out not finding the endpoint on the sandbox.
	if err := sb.GetEndpoint(ep.ID()); err == nil {
		log.G(context.TODO()).Warnf("addServiceInfoToCluster suppressing service resolution ep is not anymore in the sandbox %s", ep.ID())
		return nil
	}

	dnsNames := ep.getDNSNames()
	primaryDNSName, dnsAliases := dnsNames[0], dnsNames[1:]

	var ingressPorts []*PortConfig
	if ep.svcID != "" {
		// This is a task part of a service
		// Gossip ingress ports only in ingress network.
		if n.ingress {
			ingressPorts = ep.ingressPorts
		}
		if err := n.getController().addServiceBinding(ep.svcName, ep.svcID, n.ID(), ep.ID(), primaryDNSName, ep.virtualIP, ingressPorts, ep.svcAliases, dnsAliases, ep.Iface().Address().IP, "addServiceInfoToCluster"); err != nil {
			return err
		}
	} else {
		// This is a container simply attached to an attachable network
		if err := n.getController().addContainerNameResolution(n.ID(), ep.ID(), primaryDNSName, dnsAliases, ep.Iface().Address().IP, "addServiceInfoToCluster"); err != nil {
			return err
		}
	}

	buf, err := proto.Marshal(&EndpointRecord{
		Name:            primaryDNSName,
		ServiceName:     ep.svcName,
		ServiceID:       ep.svcID,
		VirtualIP:       ep.virtualIP.String(),
		IngressPorts:    ingressPorts,
		Aliases:         ep.svcAliases,
		TaskAliases:     dnsAliases,
		EndpointIP:      ep.Iface().Address().IP.String(),
		ServiceDisabled: false,
	})
	if err != nil {
		return err
	}

	if err := agent.networkDB.CreateEntry(libnetworkEPTable, n.ID(), ep.ID(), buf); err != nil {
		log.G(context.TODO()).Warnf("addServiceInfoToCluster NetworkDB CreateEntry failed for %s %s err:%s", ep.id, n.id, err)
		return err
	}

	log.G(context.TODO()).Debugf("addServiceInfoToCluster END for %s %s", ep.svcName, ep.ID())

	return nil
}

func (ep *Endpoint) deleteServiceInfoFromCluster(sb *Sandbox, fullRemove bool, method string) error {
	if len(ep.dnsNames) == 0 {
		return nil
	}

	n := ep.getNetwork()
	agent, ok := n.clusterAgent()
	if !ok {
		return nil
	}

	sb.service.Lock()
	defer sb.service.Unlock()
	log.G(context.TODO()).Debugf("deleteServiceInfoFromCluster from %s START for %s %s", method, ep.svcName, ep.ID())

	// Avoid a race w/ with a container that aborts preemptively.  This would
	// get caught in disableServiceInNetworkDB, but we check here to make the
	// nature of the condition more clear.
	// See comment in addServiceInfoToCluster()
	if err := sb.GetEndpoint(ep.ID()); err == nil {
		log.G(context.TODO()).Warnf("deleteServiceInfoFromCluster suppressing service resolution ep is not anymore in the sandbox %s", ep.ID())
		return nil
	}

	dnsNames := ep.getDNSNames()
	primaryDNSName, dnsAliases := dnsNames[0], dnsNames[1:]

	// First update the networkDB then locally
	if fullRemove {
		if err := agent.networkDB.DeleteEntry(libnetworkEPTable, n.ID(), ep.ID()); err != nil {
			log.G(context.TODO()).Warnf("deleteServiceInfoFromCluster NetworkDB DeleteEntry failed for %s %s err:%s", ep.id, n.id, err)
		}
	} else {
		disableServiceInNetworkDB(agent, n, ep)
	}

	if ep.Iface() != nil && ep.Iface().Address() != nil {
		if ep.svcID != "" {
			// This is a task part of a service
			var ingressPorts []*PortConfig
			if n.ingress {
				ingressPorts = ep.ingressPorts
			}
			if err := n.getController().rmServiceBinding(ep.svcName, ep.svcID, n.ID(), ep.ID(), primaryDNSName, ep.virtualIP, ingressPorts, ep.svcAliases, dnsAliases, ep.Iface().Address().IP, "deleteServiceInfoFromCluster", true, fullRemove); err != nil {
				return err
			}
		} else {
			// This is a container simply attached to an attachable network
			if err := n.getController().delContainerNameResolution(n.ID(), ep.ID(), primaryDNSName, dnsAliases, ep.Iface().Address().IP, "deleteServiceInfoFromCluster"); err != nil {
				return err
			}
		}
	}

	log.G(context.TODO()).Debugf("deleteServiceInfoFromCluster from %s END for %s %s", method, ep.svcName, ep.ID())

	return nil
}

func disableServiceInNetworkDB(a *nwAgent, n *Network, ep *Endpoint) {
	var epRec EndpointRecord

	log.G(context.TODO()).Debugf("disableServiceInNetworkDB for %s %s", ep.svcName, ep.ID())

	// Update existing record to indicate that the service is disabled
	inBuf, err := a.networkDB.GetEntry(libnetworkEPTable, n.ID(), ep.ID())
	if err != nil {
		log.G(context.TODO()).Warnf("disableServiceInNetworkDB GetEntry failed for %s %s err:%s", ep.id, n.id, err)
		return
	}
	// Should never fail
	if err := proto.Unmarshal(inBuf, &epRec); err != nil {
		log.G(context.TODO()).Errorf("disableServiceInNetworkDB unmarshal failed for %s %s err:%s", ep.id, n.id, err)
		return
	}
	epRec.ServiceDisabled = true
	// Should never fail
	outBuf, err := proto.Marshal(&epRec)
	if err != nil {
		log.G(context.TODO()).Errorf("disableServiceInNetworkDB marshalling failed for %s %s err:%s", ep.id, n.id, err)
		return
	}
	// Send update to the whole cluster
	if err := a.networkDB.UpdateEntry(libnetworkEPTable, n.ID(), ep.ID(), outBuf); err != nil {
		log.G(context.TODO()).Warnf("disableServiceInNetworkDB UpdateEntry failed for %s %s err:%s", ep.id, n.id, err)
	}
}

func (n *Network) addDriverWatches() {
	if len(n.driverTables) == 0 {
		return
	}
	agent, ok := n.clusterAgent()
	if !ok {
		return
	}

	c := n.getController()
	for _, table := range n.driverTables {
		ch, cancel := agent.networkDB.Watch(table.name, n.ID())
		agent.mu.Lock()
		agent.driverCancelFuncs[n.ID()] = append(agent.driverCancelFuncs[n.ID()], cancel)
		agent.mu.Unlock()
		go c.handleTableEvents(ch, n.handleDriverTableEvent)
	}
}

func (n *Network) cancelDriverWatches() {
	agent, ok := n.clusterAgent()
	if !ok {
		return
	}

	agent.mu.Lock()
	cancelFuncs := agent.driverCancelFuncs[n.ID()]
	delete(agent.driverCancelFuncs, n.ID())
	agent.mu.Unlock()

	for _, cancel := range cancelFuncs {
		cancel()
	}
}

func (c *Controller) handleTableEvents(ch *events.Channel, fn func(events.Event)) {
	for {
		select {
		case ev := <-ch.C:
			fn(ev)
		case <-ch.Done():
			return
		}
	}
}

func (n *Network) handleDriverTableEvent(ev events.Event) {
	d, err := n.driver(false)
	if err != nil {
		log.G(context.TODO()).Errorf("Could not resolve driver %s while handling driver table event: %v", n.networkType, err)
		return
	}
	ed, ok := d.(driverapi.TableWatcher)
	if !ok {
		log.G(context.TODO()).Errorf("Could not notify driver %s about table event: driver does not implement TableWatcher interface", n.networkType)
		return
	}

	event := ev.(networkdb.WatchEvent)
	ed.EventNotify(n.ID(), event.Table, event.Key, event.Prev, event.Value)
}

func (c *Controller) handleNodeTableEvent(ev events.Event) {
	var (
		value    []byte
		isAdd    bool
		nodeAddr networkdb.NodeAddr
	)
	event := ev.(networkdb.WatchEvent)
	switch {
	case event.IsCreate():
		value = event.Value
		isAdd = true
	case event.IsDelete():
		value = event.Prev
	case event.IsUpdate():
		log.G(context.TODO()).Errorf("Unexpected update node table event = %#v", event)
	}

	err := json.Unmarshal(value, &nodeAddr)
	if err != nil {
		log.G(context.TODO()).Errorf("Error unmarshalling node table event %v", err)
		return
	}
	c.processNodeDiscovery([]net.IP{nodeAddr.Addr}, isAdd)
}

type endpointEvent struct {
	EndpointRecord
	// Virtual IP of the service to which this endpoint belongs.
	VirtualIP netip.Addr
	// IP assigned to this endpoint.
	EndpointIP netip.Addr
}

func unmarshalEndpointRecord(data []byte) (*endpointEvent, error) {
	var epRec EndpointRecord
	if err := proto.Unmarshal(data, &epRec); err != nil {
		return nil, fmt.Errorf("failed to unmarshal endpoint record: %w", err)
	}

	vip, _ := netip.ParseAddr(epRec.VirtualIP)
	eip, _ := netip.ParseAddr(epRec.EndpointIP)

	if epRec.Name == "" || !eip.IsValid() {
		return nil, fmt.Errorf("invalid endpoint name/ip in service table event %s", data)
	}

	return &endpointEvent{
		EndpointRecord: epRec,
		VirtualIP:      vip,
		EndpointIP:     eip,
	}, nil
}

// EquivalentTo returns true if ev is semantically equivalent to other,
// ignoring the ServiceDisabled field.
func (ev *endpointEvent) EquivalentTo(other *endpointEvent) bool {
	if ev == nil || other == nil {
		return (ev == nil) == (other == nil)
	}
	return ev.Name == other.Name &&
		ev.ServiceName == other.ServiceName &&
		ev.ServiceID == other.ServiceID &&
		ev.VirtualIP == other.VirtualIP &&
		ev.EndpointIP == other.EndpointIP &&
		iterutil.SameValues(
			iterutil.Deref(slices.Values(ev.IngressPorts)),
			iterutil.Deref(slices.Values(other.IngressPorts))) &&
		iterutil.SameValues(slices.Values(ev.Aliases), slices.Values(other.Aliases)) &&
		iterutil.SameValues(slices.Values(ev.TaskAliases), slices.Values(other.TaskAliases))
}

type serviceBinder interface {
	addContainerNameResolution(nID, eID, containerName string, taskAliases []string, ip net.IP, method string) error
	delContainerNameResolution(nID, eID, containerName string, taskAliases []string, ip net.IP, method string) error
	addServiceBinding(svcName, svcID, nID, eID, containerName string, vip net.IP, ingressPorts []*PortConfig, serviceAliases, taskAliases []string, ip net.IP, method string) error
	rmServiceBinding(svcName, svcID, nID, eID, containerName string, vip net.IP, ingressPorts []*PortConfig, serviceAliases []string, taskAliases []string, ip net.IP, method string, deleteSvcRecords bool, fullRemove bool) error
}

func handleEpTableEvent(c serviceBinder, ev events.Event) {
	event := ev.(networkdb.WatchEvent)
	nid := event.NetworkID
	eid := event.Key

	var prev, epRec *endpointEvent
	if event.Prev != nil {
		var err error
		prev, err = unmarshalEndpointRecord(event.Prev)
		if err != nil {
			log.G(context.TODO()).WithError(err).Error("error unmarshaling previous value from service table event")
			return
		}
	}
	if event.Value != nil {
		var err error
		epRec, err = unmarshalEndpointRecord(event.Value)
		if err != nil {
			log.G(context.TODO()).WithError(err).Error("error unmarshaling service table event")
			return
		}
	}

	logger := log.G(context.TODO()).WithFields(log.Fields{
		"evt":  event,
		"R":    epRec,
		"prev": prev,
	})
	logger.Debug("handleEpTableEvent")

	equivalent := prev.EquivalentTo(epRec)
	if prev != nil {
		if equivalent && prev.ServiceDisabled == epRec.ServiceDisabled {
			// Avoid flapping if we would otherwise remove a service
			// binding then immediately replace it with an equivalent one.
			return
		}

		if prev.ServiceID != "" {
			// This is a remote task part of a service
			if epRec == nil || !equivalent {
				// Either the endpoint is deleted from NetworkDB or has
				// been replaced with a different one. Remove the old
				// binding. The new binding, if any, will be added
				// below.
				err := c.rmServiceBinding(prev.ServiceName, prev.ServiceID, nid, eid,
					prev.Name, prev.VirtualIP.AsSlice(), prev.IngressPorts,
					prev.Aliases, prev.TaskAliases, prev.EndpointIP.AsSlice(),
					"handleEpTableEvent", true, true)
				if err != nil {
					logger.WithError(err).Error("failed removing service binding")
				}
			}
		} else if !prev.ServiceDisabled && (!equivalent || epRec == nil || epRec.ServiceDisabled) {
			// This is a remote container simply attached to an attachable network
			err := c.delContainerNameResolution(nid, eid, prev.Name, prev.TaskAliases,
				prev.EndpointIP.AsSlice(), "handleEpTableEvent")
			if err != nil {
				logger.WithError(err).Errorf("failed removing container name resolution")
			}
		}
	}

	if epRec != nil {
		if epRec.ServiceID != "" {
			// This is a remote task part of a service
			if equivalent && !prev.ServiceDisabled && epRec.ServiceDisabled {
				err := c.rmServiceBinding(epRec.ServiceName, epRec.ServiceID,
					nid, eid, epRec.Name, epRec.VirtualIP.AsSlice(),
					epRec.IngressPorts, epRec.Aliases, epRec.TaskAliases,
					epRec.EndpointIP.AsSlice(), "handleEpTableEvent", true, false)
				if err != nil {
					logger.WithError(err).Error("failed disabling service binding")
					return
				}
			} else if !epRec.ServiceDisabled {
				err := c.addServiceBinding(epRec.ServiceName, epRec.ServiceID, nid, eid,
					epRec.Name, epRec.VirtualIP.AsSlice(), epRec.IngressPorts,
					epRec.Aliases, epRec.TaskAliases, epRec.EndpointIP.AsSlice(),
					"handleEpTableEvent")
				if err != nil {
					logger.WithError(err).Error("failed adding service binding")
					return
				}
			}
		} else if !epRec.ServiceDisabled && (!equivalent || prev == nil || prev.ServiceDisabled) {
			// This is a remote container simply attached to an attachable network
			err := c.addContainerNameResolution(nid, eid, epRec.Name, epRec.TaskAliases,
				epRec.EndpointIP.AsSlice(), "handleEpTableEvent")
			if err != nil {
				logger.WithError(err).Errorf("failed adding container name resolution")
			}
		}
	}
}
