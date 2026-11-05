//go:build windows

// Shim for the Host Network Service (HNS) to manage networking for
// Windows Server containers and Hyper-V containers. This module
// is a basic libnetwork driver that passes all the calls to HNS
// It implements the 4 networking modes supported by HNS L2Bridge,
// L2Tunnel, NAT and Transparent(DHCP)
//
// The network are stored in memory and docker daemon ensures discovering
// and loading these networks on startup

package windows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/Microsoft/hcsshim"
	"github.com/Microsoft/hcsshim/hcn"
	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/libnetwork/datastore"
	"github.com/moby/moby/v2/daemon/libnetwork/driverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/netlabel"
	"github.com/moby/moby/v2/daemon/libnetwork/portallocator"
	"github.com/moby/moby/v2/daemon/libnetwork/scope"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// networkConfiguration for network specific configuration
type networkConfiguration struct {
	ID                    string
	Type                  string
	Name                  string
	HnsID                 string
	RDID                  string
	VLAN                  uint
	VSID                  uint
	DNSServers            string
	MacPools              []hcsshim.MacPool
	DNSSuffix             string
	SourceMac             string
	NetworkAdapterName    string
	dbIndex               uint64
	dbExists              bool
	DisableGatewayDNS     bool
	EnableOutboundNat     bool
	OutboundNatExceptions []string
}

// endpointConfiguration represents the user-specified configuration for the sandbox endpoint
type endpointOption struct {
	MacAddress  net.HardwareAddr
	QosPolicies []types.QosPolicy
	DNSServers  []string
	DisableDNS  bool
	DisableICC  bool
}

// EndpointConnectivity stores the port bindings and exposed ports that the user has specified in epOptions.
type EndpointConnectivity struct {
	PortBindings []types.PortBinding
	ExposedPorts []types.TransportPort
}

type hnsEndpoint struct {
	id        string
	nid       string
	profileID string
	Type      string
	// Note: Currently, the sandboxID is the same as the containerID since windows does
	// not expose the sandboxID.
	// In the future, windows will support a proper sandboxID that is different
	// than the containerID.
	// Therefore, we are using sandboxID now, so that we won't have to change this code
	// when windows properly supports a sandboxID.
	sandboxID      string
	macAddress     net.HardwareAddr
	epOption       *endpointOption       // User-specified parameters
	epConnectivity *EndpointConnectivity // User-specified parameters
	portMapping    []types.PortBinding   // Operation port bindings
	addr           *net.IPNet
	gateway        net.IP
	dbIndex        uint64
	dbExists       bool
}

type hnsNetwork struct {
	id        string
	created   bool
	config    *networkConfiguration
	endpoints map[string]*hnsEndpoint // key: endpoint id
	driver    *driver                 // The network's driver
	pa        *portallocator.OSAllocator
	sync.Mutex
}

type containerNamespaceCache struct {
	namespaces map[string]string
	sync.Mutex
}

func newContainerNamespaceCache() *containerNamespaceCache {
	return &containerNamespaceCache{namespaces: map[string]string{}}
}

func (c *containerNamespaceCache) get(containerID string) string {
	c.Lock()
	defer c.Unlock()
	return c.namespaces[containerID]
}

func (c *containerNamespaceCache) set(containerID, namespaceID string) {
	c.Lock()
	defer c.Unlock()
	c.namespaces[containerID] = namespaceID
}

func (c *containerNamespaceCache) deleteIfEqual(containerID, namespaceID string) {
	c.Lock()
	defer c.Unlock()
	if c.namespaces[containerID] == namespaceID {
		delete(c.namespaces, containerID)
	}
}

type driver struct {
	name                string
	networks            map[string]*hnsNetwork
	containerNamespaces *containerNamespaceCache
	store               *datastore.Store
	sync.Mutex
}

const (
	errNotFound = "HNS failed with error : The object identifier does not represent a valid object. "
)

var builtinLocalDrivers = map[string]struct{}{
	"transparent": {},
	"l2bridge":    {},
	"l2tunnel":    {},
	"nat":         {},
	"internal":    {},
	"private":     {},
	"ics":         {},
}

// IsBuiltinLocalDriver validates if network-type is a builtin local-scoped driver
func IsBuiltinLocalDriver(networkType string) bool {
	_, ok := builtinLocalDrivers[networkType]
	return ok
}

var unadoptableNetworkTypes = map[string]struct{}{
	// "internal" and "private" are included here to preserve the workarounds added
	// in commits b91fd26 ("Ignore HNS networks with type Private") and 6a1a4f9 ("Fix
	// long startup on windows, with non-hns governed Hyper-V networks").
	//
	// That long delay was caused by trying to load network driver plugins named
	// "internal" and "private". The workaround doesn't seem necessary now, because
	// those network types are both associated with the windows [driver] by the
	// entries in [builtinLocalDrivers]. (The code has been refactored, but those
	// network types were included at the time the workaround was added. Something
	// else must have changed in the meantime.)
	//
	// TODO(robmry) - remove internal/private from this map? ...
	//
	// On startup the daemon tries to adopt all existing HNS networks by creating
	// corresponding Docker networks. (HNS is the source of truth for Windows, not
	// Docker's network store.)
	//
	// So, removing internal/private from this map would have two consequences
	// (removing restrictions that were probably introduced unintentionally by the
	// workarounds mentioned above):
	// - internal/private networks created on the host would appear in Docker's list
	//   of networks, and it'd be possible to create containers in those networks.
	//   That would match the behaviour for other network types (including the other
	//   currently-undocumented type "ics").
	// - On startup, if an internal/private network originally created by Docker has
	//   been reconfigured outside Docker, the network definition in Docker's store
	//   will be updated to match the current HNS configuration (as it is for other
	//   network types).
	"internal": {},
	"private":  {},

	// "mirrored" is not currently in the list of HNS network types understood by
	// Docker [builtinLocalDrivers]. So, it's not possible for Docker to create a
	// network of this type. And, if the host has "mirrored" networks, Docker
	// should not spend time on startup trying to load a plugin called "mirrored".
	"mirrored": {},
}

// IsAdoptableNetworkType returns true if HNS networks of this type can be
// adopted on startup when searching for networks created outside Docker
// (these networks can be added to the network store).
func IsAdoptableNetworkType(networkType string) bool {
	_, ok := unadoptableNetworkTypes[strings.ToLower(networkType)]
	return !ok
}

func newDriver(networkType string, store *datastore.Store, containerNamespaces *containerNamespaceCache) (*driver, error) {
	d := &driver{
		name:                networkType,
		store:               store,
		networks:            map[string]*hnsNetwork{},
		containerNamespaces: containerNamespaces,
	}
	err := d.initStore()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize %q driver: %w", networkType, err)
	}
	return d, nil
}

// RegisterBuiltinLocalDrivers registers the builtin local drivers.
func RegisterBuiltinLocalDrivers(r driverapi.Registerer, store *datastore.Store) error {
	containerNamespaces := newContainerNamespaceCache()
	for networkType := range builtinLocalDrivers {
		d, err := newDriver(networkType, store, containerNamespaces)
		if err != nil {
			return err
		}
		err = r.RegisterDriver(networkType, d, driverapi.Capability{
			DataScope:         scope.Local,
			ConnectivityScope: scope.Local,
		})
		if err != nil {
			return fmt.Errorf("failed to register %q driver: %w", networkType, err)
		}
	}
	return nil
}

func (d *driver) getNetwork(id string) (*hnsNetwork, error) {
	d.Lock()
	defer d.Unlock()

	if nw, ok := d.networks[id]; ok {
		return nw, nil
	}

	return nil, types.NotFoundErrorf("network not found: %s", id)
}

func (n *hnsNetwork) getEndpoint(eid string) (*hnsEndpoint, error) {
	n.Lock()
	defer n.Unlock()

	if ep, ok := n.endpoints[eid]; ok {
		return ep, nil
	}

	return nil, types.NotFoundErrorf("Endpoint not found: %s", eid)
}

func (d *driver) parseNetworkOptions(id string, genericOptions map[string]string) (*networkConfiguration, error) {
	config := &networkConfiguration{Type: d.name}

	for label, value := range genericOptions {
		switch label {
		case NetworkName:
			config.Name = value
		case HNSID:
			config.HnsID = value
		case RoutingDomain:
			config.RDID = value
		case Interface:
			config.NetworkAdapterName = value
		case DNSSuffix:
			config.DNSSuffix = value
		case DNSServers:
			config.DNSServers = value
		case DisableGatewayDNS:
			b, err := strconv.ParseBool(value)
			if err != nil {
				return nil, err
			}
			config.DisableGatewayDNS = b
		case MacPool:
			config.MacPools = make([]hcsshim.MacPool, 0)
			s := strings.Split(value, ",")
			if len(s)%2 != 0 {
				return nil, types.InvalidParameterErrorf("invalid mac pool. You must specify both a start range and an end range")
			}
			for i := 0; i < len(s)-1; i += 2 {
				config.MacPools = append(config.MacPools, hcsshim.MacPool{
					StartMacAddress: s[i],
					EndMacAddress:   s[i+1],
				})
			}
		case VLAN:
			vlan, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				return nil, err
			}
			config.VLAN = uint(vlan)
		case VSID:
			vsid, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				return nil, err
			}
			config.VSID = uint(vsid)
		case EnableOutboundNat:
			b, err := strconv.ParseBool(value)
			if err != nil {
				return nil, err
			}
			config.EnableOutboundNat = b
		case OutboundNatExceptions:
			s := strings.Split(value, ",")
			config.OutboundNatExceptions = s
		}
	}

	config.ID = id
	config.Type = d.name
	return config, nil
}

func (ncfg *networkConfiguration) processIPAM(id string, ipamV4Data, ipamV6Data []driverapi.IPAMData) error {
	if len(ipamV6Data) > 0 {
		return types.ForbiddenErrorf("windowsshim driver doesn't support v6 subnets")
	}

	if len(ipamV4Data) == 0 {
		return types.InvalidParameterErrorf("network %s requires ipv4 configuration", id)
	}

	return nil
}

func (d *driver) createNetwork(config *networkConfiguration) *hnsNetwork {
	network := &hnsNetwork{
		id:        config.ID,
		endpoints: make(map[string]*hnsEndpoint),
		config:    config,
		driver:    d,
		pa:        portallocator.New(),
	}

	d.Lock()
	d.networks[config.ID] = network
	d.Unlock()

	return network
}

// Create a new network
func (d *driver) CreateNetwork(ctx context.Context, id string, option map[string]any, nInfo driverapi.NetworkInfo, ipV4Data, ipV6Data []driverapi.IPAMData) error {
	if _, err := d.getNetwork(id); err == nil {
		return types.ForbiddenErrorf("network %s exists", id)
	}

	genData, ok := option[netlabel.GenericData].(map[string]string)
	if !ok {
		return fmt.Errorf("Unknown generic data option")
	}

	if v, ok := option[netlabel.EnableIPv4]; ok {
		if enable_IPv4, ok := v.(bool); ok && !enable_IPv4 {
			return types.InvalidParameterErrorf("IPv4 cannot be disabled on Windows")
		}
	}

	// Parse and validate the config. It should not conflict with existing networks' config
	config, err := d.parseNetworkOptions(id, genData)
	if err != nil {
		return err
	}

	err = config.processIPAM(id, ipV4Data, ipV6Data)
	if err != nil {
		return err
	}

	n := d.createNetwork(config)

	// A non blank hnsid indicates that the network was discovered
	// from HNS. No need to call HNS if this network was discovered
	// from HNS
	if config.HnsID == "" {
		subnets := []hcsshim.Subnet{}

		for _, ipData := range ipV4Data {
			subnet := hcsshim.Subnet{
				AddressPrefix: ipData.Pool.String(),
			}

			if ipData.Gateway != nil {
				subnet.GatewayAddress = ipData.Gateway.IP.String()
			}

			subnets = append(subnets, subnet)
		}

		network := &hcsshim.HNSNetwork{
			Name:               config.Name,
			Type:               d.name,
			Subnets:            subnets,
			DNSServerList:      config.DNSServers,
			DNSSuffix:          config.DNSSuffix,
			MacPools:           config.MacPools,
			SourceMac:          config.SourceMac,
			NetworkAdapterName: config.NetworkAdapterName,
		}

		if config.VLAN != 0 {
			vlanPolicy, err := json.Marshal(hcsshim.VlanPolicy{
				Type: "VLAN",
				VLAN: config.VLAN,
			})
			if err != nil {
				return err
			}
			network.Policies = append(network.Policies, vlanPolicy)
		}

		if config.VSID != 0 {
			vsidPolicy, err := json.Marshal(hcsshim.VsidPolicy{
				Type: "VSID",
				VSID: config.VSID,
			})
			if err != nil {
				return err
			}
			network.Policies = append(network.Policies, vsidPolicy)
		}

		if network.Name == "" {
			network.Name = id
		}

		configurationb, err := json.Marshal(network)
		if err != nil {
			return err
		}

		configuration := string(configurationb)
		log.G(ctx).Debugf("HNSNetwork Request =%v Address Space=%v", configuration, subnets)

		hnsresponse, err := hcsshim.HNSNetworkRequest(http.MethodPost, "", configuration)
		if err != nil {
			return err
		}

		config.HnsID = hnsresponse.Id
		genData[HNSID] = config.HnsID
		genData[NetworkName] = network.Name
		n.created = true

		defer func() {
			if err != nil {
				d.DeleteNetwork(n.id)
			}
		}()

		hnsIPv4Data := make([]driverapi.IPAMData, len(hnsresponse.Subnets))

		for i, subnet := range hnsresponse.Subnets {
			var gwIP, subnetIP *net.IPNet

			// The gateway returned from HNS is an IPAddress.
			// We need to convert it to an IPNet to use as the Gateway of driverapi.IPAMData struct
			gwCIDR := subnet.GatewayAddress + "/32"
			_, gwIP, err = net.ParseCIDR(gwCIDR)
			if err != nil {
				return err
			}

			hnsIPv4Data[i].Gateway = gwIP
			_, subnetIP, err = net.ParseCIDR(subnet.AddressPrefix)
			if err != nil {
				return err
			}
			hnsIPv4Data[i].Pool = subnetIP
		}

		nInfo.UpdateIpamConfig(hnsIPv4Data)

	} else {
		// Delete any stale HNS endpoints for this network.
		if endpoints, err := hcsshim.HNSListEndpointRequest(); err == nil {
			for _, ep := range endpoints {
				if ep.VirtualNetwork == config.HnsID {
					log.G(ctx).Infof("Removing stale HNS endpoint %s", ep.Id)
					_, err = hcsshim.HNSEndpointRequest(http.MethodDelete, ep.Id, "")
					if err != nil {
						log.G(ctx).Warnf("Error removing HNS endpoint %s", ep.Id)
					}
				}
			}
		} else {
			log.G(ctx).Warnf("Error listing HNS endpoints for network %s", config.HnsID)
		}

		n.created = true
	}

	return d.storeUpdate(config)
}

func (d *driver) DeleteNetwork(nid string) error {
	n, err := d.getNetwork(nid)
	if err != nil {
		return types.InternalMaskableErrorf("%v", err)
	}

	n.Lock()
	config := n.config
	n.Unlock()

	if n.created {
		_, err = hcsshim.HNSNetworkRequest(http.MethodDelete, config.HnsID, "")
		if err != nil && !strings.EqualFold(err.Error(), errNotFound) {
			return types.ForbiddenErrorf("%v", err)
		}
	}

	d.Lock()
	delete(d.networks, nid)
	d.Unlock()

	// delete endpoints belong to this network
	for _, ep := range n.endpoints {
		if err := d.storeDelete(ep); err != nil {
			log.G(context.TODO()).Warnf("Failed to remove bridge endpoint %.7s from store: %v", ep.id, err)
		}
	}

	return d.storeDelete(config)
}

func convertQosPolicies(qosPolicies []types.QosPolicy) ([]json.RawMessage, error) {
	var qps []json.RawMessage

	// Enumerate through the qos policies specified by the user and convert
	// them into the internal structure matching the JSON blob that can be
	// understood by the HCS.
	for _, elem := range qosPolicies {
		encodedPolicy, err := json.Marshal(hcsshim.QosPolicy{
			Type:                            "QOS",
			MaximumOutgoingBandwidthInBytes: elem.MaxEgressBandwidth,
		})
		if err != nil {
			return nil, err
		}
		qps = append(qps, encodedPolicy)
	}
	return qps, nil
}

// ConvertPortBindings converts PortBindings to JSON for HNS request
func ConvertPortBindings(portBindings []types.PortBinding) ([]json.RawMessage, error) {
	var pbs []json.RawMessage

	// Enumerate through the port bindings specified by the user and convert
	// them into the internal structure matching the JSON blob that can be
	// understood by the HCS.
	for _, elem := range portBindings {
		proto := strings.ToUpper(elem.Proto.String())
		if proto != "TCP" && proto != "UDP" {
			return nil, fmt.Errorf("invalid protocol %s", elem.Proto.String())
		}

		if elem.HostPort != elem.HostPortEnd {
			return nil, fmt.Errorf("Windows does not support more than one host port in NAT settings")
		}

		if len(elem.HostIP) != 0 && !elem.HostIP.IsUnspecified() {
			return nil, fmt.Errorf("Windows does not support host IP addresses in NAT settings")
		}

		encodedPolicy, err := json.Marshal(hcsshim.NatPolicy{
			Type:                 "NAT",
			ExternalPort:         elem.HostPort,
			InternalPort:         elem.Port,
			Protocol:             elem.Proto.String(),
			ExternalPortReserved: true,
		})
		if err != nil {
			return nil, err
		}
		pbs = append(pbs, encodedPolicy)
	}
	return pbs, nil
}

// ParsePortBindingPolicies parses HNS endpoint response message to PortBindings
func ParsePortBindingPolicies(policies []json.RawMessage) ([]types.PortBinding, error) {
	var bindings []types.PortBinding
	hcsPolicy := &hcsshim.NatPolicy{}

	for _, elem := range policies {

		if err := json.Unmarshal([]byte(elem), &hcsPolicy); err != nil || hcsPolicy.Type != "NAT" {
			continue
		}

		binding := types.PortBinding{
			HostPort:    hcsPolicy.ExternalPort,
			HostPortEnd: hcsPolicy.ExternalPort,
			Port:        hcsPolicy.InternalPort,
			Proto:       types.ParseProtocol(hcsPolicy.Protocol),
			HostIP:      net.IPv4(0, 0, 0, 0),
		}

		bindings = append(bindings, binding)
	}

	return bindings, nil
}

func parseEndpointOptions(epOptions map[string]any) (*endpointOption, error) {
	if epOptions == nil {
		return nil, nil
	}

	ec := &endpointOption{}

	if opt, ok := epOptions[netlabel.MacAddress]; ok {
		if mac, ok := opt.(net.HardwareAddr); ok {
			ec.MacAddress = mac
		} else {
			return nil, fmt.Errorf("Invalid endpoint configuration")
		}
	}

	if opt, ok := epOptions[QosPolicies]; ok {
		if policies, ok := opt.([]types.QosPolicy); ok {
			ec.QosPolicies = policies
		} else {
			return nil, fmt.Errorf("Invalid endpoint configuration")
		}
	}

	if opt, ok := epOptions[DisableICC]; ok {
		if disableICC, ok := opt.(bool); ok {
			ec.DisableICC = disableICC
		} else {
			return nil, fmt.Errorf("Invalid endpoint configuration")
		}
	}

	if opt, ok := epOptions[DisableDNS]; ok {
		if disableDNS, ok := opt.(bool); ok {
			ec.DisableDNS = disableDNS
		} else {
			return nil, fmt.Errorf("Invalid endpoint configuration")
		}
	}

	return ec, nil
}

func ParseDNSServers(epOptions map[string]any) (string, error) {
	if opt, ok := epOptions[netlabel.DNSServers]; ok {
		if dns, ok := opt.([]string); ok {
			return strings.Join(dns, ","), nil
		} else {
			return "", fmt.Errorf("Invalid endpoint configuration")
		}
	}
	return "", nil
}

// ParseEndpointConnectivity parses options passed to CreateEndpoint, specifically port bindings, and store in a endpointConnectivity object.
func ParseEndpointConnectivity(epOptions map[string]any) (*EndpointConnectivity, error) {
	if epOptions == nil {
		return nil, nil
	}

	ec := &EndpointConnectivity{}

	if opt, ok := epOptions[netlabel.PortMap]; ok {
		if bs, ok := opt.([]types.PortBinding); ok {
			ec.PortBindings = bs
		} else {
			return nil, fmt.Errorf("Invalid endpoint configuration")
		}
	}

	if opt, ok := epOptions[netlabel.ExposedPorts]; ok {
		if ports, ok := opt.([]types.TransportPort); ok {
			ec.ExposedPorts = ports
		} else {
			return nil, fmt.Errorf("Invalid endpoint configuration")
		}
	}
	return ec, nil
}

func (d *driver) CreateEndpoint(ctx context.Context, nid, eid string, ifInfo driverapi.InterfaceInfo, epOptions map[string]any) error {
	ctx, span := otel.Tracer("").Start(ctx, fmt.Sprintf("libnetwork.drivers.windows_%s.CreateEndpoint", d.name), trace.WithAttributes(
		attribute.String("nid", nid),
		attribute.String("eid", eid)))
	defer span.End()

	n, err := d.getNetwork(nid)
	if err != nil {
		return err
	}

	// Check if endpoint id is good and retrieve corresponding endpoint
	ep, err := n.getEndpoint(eid)
	if err == nil && ep != nil {
		return driverapi.ErrEndpointExists(eid)
	}

	endpointStruct := &hcsshim.HNSEndpoint{
		VirtualNetwork: n.config.HnsID,
	}

	epOption, err := parseEndpointOptions(epOptions)
	if err != nil {
		return err
	}
	epConnectivity, err := ParseEndpointConnectivity(epOptions)
	if err != nil {
		return err
	}

	macAddress := ifInfo.MacAddress()
	// Use the macaddress if it was provided
	if macAddress != nil {
		endpointStruct.MacAddress = strings.ReplaceAll(macAddress.String(), ":", "-")
	}

	portMapping := epConnectivity.PortBindings

	if n.config.Type == "l2bridge" || n.config.Type == "l2tunnel" {
		portMapping, err = AllocatePorts(n.pa, portMapping)
		if err != nil {
			return err
		}

		defer func() {
			if err != nil {
				ReleasePorts(n.pa, portMapping)
			}
		}()
	}

	endpointStruct.Policies, err = ConvertPortBindings(portMapping)
	if err != nil {
		return err
	}

	qosPolicies, err := convertQosPolicies(epOption.QosPolicies)
	if err != nil {
		return err
	}
	endpointStruct.Policies = append(endpointStruct.Policies, qosPolicies...)

	if ifInfo.Address() != nil {
		endpointStruct.IPAddress = ifInfo.Address().IP
	}

	dnsServerList, err := ParseDNSServers(epOptions)
	if err != nil {
		return err
	}
	endpointStruct.DNSServerList = dnsServerList

	// overwrite the ep DisableDNS option if DisableGatewayDNS was set to true during the network creation option
	if n.config.DisableGatewayDNS {
		log.G(ctx).Debugf("n.config.DisableGatewayDNS[%v] overwrites epOption.DisableDNS[%v]", n.config.DisableGatewayDNS, epOption.DisableDNS)
		epOption.DisableDNS = n.config.DisableGatewayDNS
	}

	if n.driver.name == "nat" && !epOption.DisableDNS {
		endpointStruct.EnableInternalDNS = true
		log.G(ctx).Debugf("endpointStruct.EnableInternalDNS =[%v]", endpointStruct.EnableInternalDNS)
	}

	endpointStruct.DisableICC = epOption.DisableICC

	// Inherit OutboundNat policy from the network
	if n.config.EnableOutboundNat {
		outboundNatPolicy, err := json.Marshal(hcsshim.OutboundNatPolicy{
			Policy:     hcsshim.Policy{Type: hcsshim.OutboundNat},
			Exceptions: n.config.OutboundNatExceptions,
		})
		if err != nil {
			return err
		}
		endpointStruct.Policies = append(endpointStruct.Policies, outboundNatPolicy)
	}

	configurationb, err := json.Marshal(endpointStruct)
	if err != nil {
		return err
	}

	hnsresponse, err := hcsshim.HNSEndpointRequest(http.MethodPost, "", string(configurationb))
	if err != nil {
		return err
	}

	mac, err := net.ParseMAC(hnsresponse.MacAddress)
	if err != nil {
		return err
	}

	// TODO For now the ip mask is not in the info generated by HNS
	endpoint := &hnsEndpoint{
		id:         eid,
		nid:        n.id,
		Type:       d.name,
		addr:       &net.IPNet{IP: hnsresponse.IPAddress, Mask: hnsresponse.IPAddress.DefaultMask()},
		macAddress: mac,
	}

	if hnsresponse.GatewayAddress != "" {
		endpoint.gateway = net.ParseIP(hnsresponse.GatewayAddress)
	}

	endpoint.profileID = hnsresponse.Id
	endpoint.epConnectivity = epConnectivity
	endpoint.epOption = epOption
	endpoint.portMapping, err = ParsePortBindingPolicies(hnsresponse.Policies)
	if err != nil {
		hcsshim.HNSEndpointRequest(http.MethodDelete, hnsresponse.Id, "")
		return err
	}

	n.Lock()
	n.endpoints[eid] = endpoint
	n.Unlock()

	if ifInfo.Address() == nil {
		ifInfo.SetIPAddress(endpoint.addr)
	}

	if macAddress == nil {
		ifInfo.SetMacAddress(endpoint.macAddress)
	}

	if err = d.storeUpdate(endpoint); err != nil {
		log.G(ctx).Errorf("Failed to save endpoint %.7s to store: %v", endpoint.id, err)
	}

	return nil
}

func (d *driver) DeleteEndpoint(nid, eid string) error {
	n, err := d.getNetwork(nid)
	if err != nil {
		return types.InternalMaskableErrorf("%v", err)
	}

	ep, err := n.getEndpoint(eid)
	if err != nil {
		return err
	}

	if n.config.Type == "l2bridge" || n.config.Type == "l2tunnel" {
		ReleasePorts(n.pa, ep.portMapping)
	}

	n.Lock()
	delete(n.endpoints, eid)
	n.Unlock()

	_, err = hcsshim.HNSEndpointRequest(http.MethodDelete, ep.profileID, "")
	if err != nil && !strings.EqualFold(err.Error(), errNotFound) {
		return err
	}

	if err := d.storeDelete(ep); err != nil {
		log.G(context.TODO()).Warnf("Failed to remove bridge endpoint %.7s from store: %v", ep.id, err)
	}
	return nil
}

func (d *driver) EndpointOperInfo(nid, eid string) (map[string]any, error) {
	network, err := d.getNetwork(nid)
	if err != nil {
		return nil, err
	}

	ep, err := network.getEndpoint(eid)
	if err != nil {
		return nil, err
	}

	data := make(map[string]any, 1)
	if network.driver.name == "nat" {
		data["AllowUnqualifiedDNSQuery"] = true
	}

	data["hnsid"] = ep.profileID
	if ep.epConnectivity.ExposedPorts != nil {
		data[netlabel.ExposedPorts] = slices.Clone(ep.epConnectivity.ExposedPorts)
	}

	if ep.portMapping != nil {
		// Return a copy of the operational data
		pmc := make([]types.PortBinding, 0, len(ep.portMapping))
		for _, pm := range ep.portMapping {
			pmc = append(pmc, pm.Copy())
		}
		data[netlabel.PortMap] = pmc
	}

	if len(ep.macAddress) != 0 {
		data[netlabel.MacAddress] = ep.macAddress
	}
	return data, nil
}

// Join method is invoked when a Sandbox is attached to an endpoint.
func (d *driver) Join(ctx context.Context, nid, eid string, sboxKey string, jinfo driverapi.JoinInfo, _, options map[string]any) error {
	ctx, span := otel.Tracer("").Start(ctx, fmt.Sprintf("libnetwork.drivers.windows_%s.Join", d.name), trace.WithAttributes(
		attribute.String("nid", nid),
		attribute.String("eid", eid),
		attribute.String("sboxKey", sboxKey)))
	defer span.End()

	network, err := d.getNetwork(nid)
	if err != nil {
		return err
	}

	// Ensure that the endpoint exists
	endpoint, err := network.getEndpoint(eid)
	if err != nil {
		return err
	}

	err = jinfo.SetGateway(endpoint.gateway)
	if err != nil {
		return err
	}

	endpoint.sandboxID = sboxKey

	err = hcsshim.HotAttachEndpoint(endpoint.sandboxID, endpoint.profileID)
	if err == hcsshim.ErrPlatformNotSupported {
		namespaceID, namespaceErr := d.containerNamespaceID(endpoint.sandboxID)
		if namespaceErr != nil {
			return namespaceErr
		}

		if err = hcn.AddNamespaceEndpoint(namespaceID, endpoint.profileID); err != nil {
			return err
		}
		namespace, err := hcn.GetNamespaceByID(namespaceID)
		if err != nil {
			return errors.Join(err, hcn.RemoveNamespaceEndpoint(namespaceID, endpoint.profileID))
		}
		if err = namespace.Sync(); err != nil {
			return errors.Join(err, hcn.RemoveNamespaceEndpoint(namespaceID, endpoint.profileID))
		}
	}
	if err != nil {
		// If container doesn't exists in hcs, do not throw error for hot add/remove
		if err != hcsshim.ErrComputeSystemDoesNotExist {
			return err
		}
	}

	jinfo.DisableGatewayService()
	return nil
}

// containerNamespaceID returns the HCN namespace for a container. Existing
// endpoints are authoritative; the cache bridges the period after the
// container's last endpoint is detached.
func (d *driver) containerNamespaceID(containerID string) (string, error) {
	endpoints, err := hcsshim.HNSListEndpointRequest()
	if err != nil {
		return "", err
	}
	namespaceID, err := d.liveContainerNamespaceID(endpoints, containerID)
	if err != nil {
		return "", err
	}
	if namespaceID != "" {
		return namespaceID, nil
	}

	return d.cachedContainerNamespaceID(containerID, hcn.GetNamespaceByID)
}

func (d *driver) cachedContainerNamespaceID(containerID string, getNamespace func(string) (*hcn.HostComputeNamespace, error)) (string, error) {
	namespaceID := d.containerNamespaces.get(containerID)
	if namespaceID == "" {
		return "", fmt.Errorf("failed to find network namespace for container %s", containerID)
	}
	if _, err := getNamespace(namespaceID); err != nil {
		if hcn.IsNotFoundError(err) {
			d.containerNamespaces.deleteIfEqual(containerID, namespaceID)
			return "", fmt.Errorf("cached network namespace %s for container %s is no longer valid: %w", namespaceID, containerID, err)
		}
		return "", fmt.Errorf("failed to query cached network namespace %s for container %s: %w", namespaceID, containerID, err)
	}
	return namespaceID, nil
}

func (d *driver) liveContainerNamespaceID(endpoints []hcsshim.HNSEndpoint, containerID string) (string, error) {
	namespaceID, err := findContainerNamespaceID(endpoints, containerID)
	if err != nil {
		return "", err
	}
	if namespaceID != "" {
		d.containerNamespaces.set(containerID, namespaceID)
	}
	return namespaceID, nil
}

func findContainerNamespaceID(endpoints []hcsshim.HNSEndpoint, containerID string) (string, error) {
	var namespaceID string
	for _, endpoint := range endpoints {
		if endpoint.Namespace != nil && endpoint.Namespace.ID != "" && slices.Contains(endpoint.SharedContainers, containerID) {
			if namespaceID != "" && namespaceID != endpoint.Namespace.ID {
				return "", fmt.Errorf("multiple network namespaces found for container %s", containerID)
			}
			namespaceID = endpoint.Namespace.ID
		}
	}
	return namespaceID, nil
}

// Leave method is invoked when a Sandbox detaches from an endpoint.
func (d *driver) Leave(nid, eid string) error {
	network, err := d.getNetwork(nid)
	if err != nil {
		return types.InternalMaskableErrorf("%v", err)
	}

	// Ensure that the endpoint exists
	endpoint, err := network.getEndpoint(eid)
	if err != nil {
		return err
	}

	err = hcsshim.HotDetachEndpoint(endpoint.sandboxID, endpoint.profileID)
	if err == hcsshim.ErrPlatformNotSupported {
		hnsEndpoint, getErr := hcsshim.GetHNSEndpointByID(endpoint.profileID)
		if getErr != nil {
			return getErr
		}
		if hnsEndpoint.Namespace == nil || hnsEndpoint.Namespace.ID == "" {
			return err
		}
		namespaceID := hnsEndpoint.Namespace.ID
		if err = d.cacheAndRemoveNamespaceEndpoint(endpoint.sandboxID, namespaceID, endpoint.profileID, hcn.RemoveNamespaceEndpoint); err != nil {
			return err
		}
		namespace, err := hcn.GetNamespaceByID(namespaceID)
		if err != nil {
			return err
		}
		if err = namespace.Sync(); err != nil {
			return err
		}
	}
	if err != nil {
		// If container doesn't exists in hcs, do not throw error for hot add/remove
		if err != hcsshim.ErrComputeSystemDoesNotExist {
			return err
		}
	}
	return nil
}

func (d *driver) cacheAndRemoveNamespaceEndpoint(containerID, namespaceID, endpointID string, removeEndpoint func(string, string) error) error {
	d.containerNamespaces.set(containerID, namespaceID)
	return removeEndpoint(namespaceID, endpointID)
}

func (d *driver) IsBuiltIn() bool {
	return true
}
