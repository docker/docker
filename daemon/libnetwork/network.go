package libnetwork

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/v2/daemon/internal/netiputil"
	"github.com/moby/moby/v2/daemon/internal/stringid"
	"github.com/moby/moby/v2/daemon/libnetwork/datastore"
	"github.com/moby/moby/v2/daemon/libnetwork/driverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/setmatrix"
	"github.com/moby/moby/v2/daemon/libnetwork/ipamapi"
	"github.com/moby/moby/v2/daemon/libnetwork/ipams/defaultipam"
	"github.com/moby/moby/v2/daemon/libnetwork/netlabel"
	"github.com/moby/moby/v2/daemon/libnetwork/netutils"
	"github.com/moby/moby/v2/daemon/libnetwork/networkdb"
	"github.com/moby/moby/v2/daemon/libnetwork/options"
	"github.com/moby/moby/v2/daemon/libnetwork/scope"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/moby/moby/v2/errdefs"
	"github.com/moby/moby/v2/internal/iterutil"
	"github.com/moby/moby/v2/internal/sliceutil"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// EndpointWalker is a client provided function which will be used to walk the Endpoints.
// When the function returns true, the walk will stop.
type EndpointWalker func(ep *Endpoint) bool

// ipInfo is the reverse mapping from IP to service name to serve the PTR query.
// extResolver is set if an external server resolves a service name to this IP.
// It's an indication to defer PTR queries also to that external server.
type ipInfo struct {
	name        string
	serviceID   string
	extResolver bool
}

// svcMapEntry is the body of the element into the svcMap
// The ip is a string because the SetMatrix does not accept non hashable values
type svcMapEntry struct {
	ip        string
	serviceID string
}

type svcInfo struct {
	svcMap     setmatrix.SetMatrix[string, svcMapEntry]
	svcIPv6Map setmatrix.SetMatrix[string, svcMapEntry]
	ipMap      setmatrix.SetMatrix[string, ipInfo]
	service    map[string][]servicePorts
}

// backing container or host's info
type serviceTarget struct {
	name string
	ip   net.IP
	port uint16
}

type servicePorts struct {
	portName string
	proto    string
	target   []serviceTarget
}

type networkDBTable struct {
	name    string
	objType driverapi.ObjectType
}

// IpamConf contains all the ipam related configurations for a network
//
// TODO(aker): use proper net/* structs instead of string literals.
type IpamConf struct {
	// PreferredPool is the master address pool for containers and network interfaces.
	PreferredPool string
	// SubPool is a subset of the master pool. If specified,
	// this becomes the container pool for automatic address allocations.
	SubPool string
	// Gateway is the preferred Network Gateway address (optional).
	Gateway string
	// AuxAddresses contains auxiliary addresses for network driver. Must be within the master pool.
	// libnetwork will reserve them if they fall into the container pool.
	AuxAddresses map[string]string
}

// Validate checks whether the configuration is valid
func (c *IpamConf) Validate() error {
	if c.Gateway != "" && net.ParseIP(c.Gateway) == nil {
		return types.InvalidParameterErrorf("invalid gateway address %s in Ipam configuration", c.Gateway)
	}
	return nil
}

// Contains checks whether the ipam master address pool contains [addr].
func (c *IpamConf) Contains(addr netip.Addr) bool {
	if c == nil {
		return false
	}
	if c.PreferredPool == "" {
		return false
	}

	allowedRange, _ := netiputil.ParseCIDR(c.PreferredPool)

	return allowedRange.Contains(addr)
}

// IsStatic checks whether the subnet was statically allocated (ie. user-defined).
func (c *IpamConf) IsStatic() bool {
	return c != nil && c.PreferredPool != ""
}

func (c *IpamConf) IPAMConfig() network.IPAMConfig {
	if c == nil {
		return network.IPAMConfig{}
	}

	subnet, _ := netiputil.ParseCIDR(c.PreferredPool)
	ipr, _ := netiputil.ParseCIDR(c.SubPool)
	gw, _ := netip.ParseAddr(c.Gateway)

	conf := network.IPAMConfig{
		Subnet:  subnet.Masked(),
		IPRange: ipr.Masked(),
		Gateway: gw.Unmap(),
	}

	if c.AuxAddresses != nil {
		conf.AuxAddress = maps.Collect(iterutil.Map2(maps.All(c.AuxAddresses), func(k, v string) (string, netip.Addr) {
			a, _ := netip.ParseAddr(v)
			return k, a.Unmap()
		}))
	}
	return conf
}

// IpamInfo contains all the ipam related operational info for a network
type IpamInfo struct {
	PoolID string
	Meta   map[string]string
	driverapi.IPAMData
}

// MarshalJSON encodes IpamInfo into json message
func (i *IpamInfo) MarshalJSON() ([]byte, error) {
	m := map[string]any{
		"PoolID": i.PoolID,
	}
	v, err := json.Marshal(&i.IPAMData)
	if err != nil {
		return nil, err
	}
	m["IPAMData"] = string(v)

	if i.Meta != nil {
		m["Meta"] = i.Meta
	}
	return json.Marshal(m)
}

// UnmarshalJSON decodes json message into PoolData
func (i *IpamInfo) UnmarshalJSON(data []byte) error {
	var (
		m   map[string]any
		err error
	)
	if err = json.Unmarshal(data, &m); err != nil {
		return err
	}
	i.PoolID = m["PoolID"].(string)
	if v, ok := m["Meta"]; ok {
		b, _ := json.Marshal(v) //nolint:errchkjson // FIXME: handle json (Un)Marshal errors
		if err = json.Unmarshal(b, &i.Meta); err != nil {
			return err
		}
	}
	if v, ok := m["IPAMData"]; ok {
		if err = json.Unmarshal([]byte(v.(string)), &i.IPAMData); err != nil {
			return err
		}
	}
	return nil
}

// Network represents a logical connectivity zone that containers may
// join using the Link method. A network is managed by a specific driver.
type Network struct {
	ctrlr            *Controller
	name             string
	networkType      string // networkType is the name of the netdriver used by this network
	id               string
	created          time.Time
	scope            string // network data scope
	labels           map[string]string
	ipamType         string // ipamType is the name of the IPAM driver
	ipamOptions      map[string]string
	addrSpace        string
	ipamV4Config     []*IpamConf
	ipamV6Config     []*IpamConf
	ipamV4Info       []*IpamInfo
	ipamV6Info       []*IpamInfo
	enableIPv4       bool
	enableIPv6       bool
	generic          options.Generic
	dbIndex          uint64
	dbExists         bool
	persist          bool
	drvOnce          *sync.Once
	resolver         []*Resolver
	internal         bool
	attachable       bool
	inDelete         bool
	ingress          bool
	driverTables     []networkDBTable
	dynamic          bool
	configOnly       bool
	configFrom       string
	loadBalancerIP   net.IP
	loadBalancerMode string
	skipGwAllocIPv4  bool
	skipGwAllocIPv6  bool
	platformNetwork  //nolint:nolintlint,unused // only populated on windows
	mu               sync.Mutex
}

const (
	loadBalancerModeNAT     = "NAT"
	loadBalancerModeDSR     = "DSR"
	loadBalancerModeDefault = loadBalancerModeNAT
)

// Name returns a user chosen name for this network.
func (n *Network) Name() string {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.name
}

// ID returns a system generated id for this network.
func (n *Network) ID() string {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.id
}

func (n *Network) Created() time.Time {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.created
}

// Type returns the type of network, which corresponds to its managing driver.
func (n *Network) Type() string {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.networkType
}

// Driver is an alias for [Network.Type].
func (n *Network) Driver() string {
	return n.Type()
}

func (n *Network) Resolvers() []*Resolver {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.resolver
}

func (n *Network) Key() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return []string{datastore.NetworkKeyPrefix, n.id}
}

func (n *Network) KeyPrefix() []string {
	return []string{datastore.NetworkKeyPrefix}
}

func (n *Network) Value() []byte {
	n.mu.Lock()
	defer n.mu.Unlock()
	b, err := json.Marshal(n)
	if err != nil {
		return nil
	}
	return b
}

func (n *Network) SetValue(value []byte) error {
	return json.Unmarshal(value, n)
}

func (n *Network) Index() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.dbIndex
}

func (n *Network) SetIndex(index uint64) {
	n.mu.Lock()
	n.dbIndex = index
	n.dbExists = true
	n.mu.Unlock()
}

func (n *Network) Exists() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.dbExists
}

func (n *Network) Skip() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return !n.persist
}

func (n *Network) New() datastore.KVObject {
	n.mu.Lock()
	defer n.mu.Unlock()

	return &Network{
		ctrlr:   n.ctrlr,
		drvOnce: &sync.Once{},
		scope:   n.scope,
	}
}

// Copy returns a deep copy of the [IpamConf]. If the receiver is nil,
// Copy returns nil.
func (c *IpamConf) Copy() *IpamConf {
	if c == nil {
		return nil
	}
	return &IpamConf{
		PreferredPool: c.PreferredPool,
		SubPool:       c.SubPool,
		Gateway:       c.Gateway,
		AuxAddresses:  maps.Clone(c.AuxAddresses),
	}
}

// Copy returns a deep copy of [IpamInfo]. If the receiver is nil,
// Copy returns nil.
func (i *IpamInfo) Copy() *IpamInfo {
	if i == nil {
		return nil
	}

	var aux map[string]*net.IPNet
	if i.IPAMData.AuxAddresses != nil {
		aux = make(map[string]*net.IPNet, len(i.IPAMData.AuxAddresses))
		for k, v := range i.AuxAddresses {
			aux[k] = types.GetIPNetCopy(v)
		}
	}

	return &IpamInfo{
		PoolID: i.PoolID,
		Meta:   maps.Clone(i.Meta),
		IPAMData: driverapi.IPAMData{
			AddressSpace: i.AddressSpace,
			Pool:         types.GetIPNetCopy(i.IPAMData.Pool),
			Gateway:      types.GetIPNetCopy(i.IPAMData.Gateway),
			AuxAddresses: aux,
		},
	}
}

func (n *Network) validateConfiguration() error {
	if n.configOnly {
		// Only supports network specific configurations.
		// Network operator configurations are not supported.
		if n.ingress || n.internal || n.attachable || n.scope != "" {
			return types.ForbiddenErrorf("configuration network can only contain network " +
				"specific fields. Network operator fields like " +
				"[ ingress | internal | attachable | scope ] are not supported.")
		}
	}
	if n.configFrom == "" {
		if err := n.validateAdvertiseAddrConfig(); err != nil {
			return err
		}
	} else {
		if n.configOnly {
			return types.ForbiddenErrorf("a configuration network cannot depend on another configuration network")
		}
		// Check that no config has been set for this --config-from network.
		// (Note that the default for enableIPv4 is 'true', ipamType has its own default,
		// and other settings are zero valued by default.)
		if n.ipamType != "" &&
			n.ipamType != defaultIpamForNetworkType(n.networkType) ||
			!n.enableIPv4 || n.enableIPv6 ||
			len(n.labels) > 0 || len(n.ipamOptions) > 0 ||
			len(n.ipamV4Config) > 0 || len(n.ipamV6Config) > 0 {
			return types.ForbiddenErrorf("user-specified configurations are not supported if the network depends on a configuration network")
		}
		if len(n.generic) > 0 {
			if data, ok := n.generic[netlabel.GenericData]; ok {
				var (
					driverOptions map[string]string
					opts          any
				)
				switch t := data.(type) {
				case map[string]any, map[string]string:
					opts = t
				}
				ba, err := json.Marshal(opts)
				if err != nil {
					return fmt.Errorf("failed to validate network configuration: %v", err)
				}
				if err := json.Unmarshal(ba, &driverOptions); err != nil {
					return fmt.Errorf("failed to validate network configuration: %v", err)
				}
				if len(driverOptions) > 0 {
					return types.ForbiddenErrorf("network driver options are not supported if the network depends on a configuration network")
				}
			}
		}
	}
	return nil
}

// applyConfigurationTo applies network specific configurations.
func (n *Network) applyConfigurationTo(to *Network) error {
	to.enableIPv4 = n.enableIPv4
	to.enableIPv6 = n.enableIPv6
	if len(n.labels) > 0 {
		to.labels = make(map[string]string, len(n.labels))
		for k, v := range n.labels {
			if _, ok := to.labels[k]; !ok {
				to.labels[k] = v
			}
		}
	}
	if n.ipamType != "" {
		to.ipamType = n.ipamType
	}
	if len(n.ipamOptions) > 0 {
		to.ipamOptions = make(map[string]string, len(n.ipamOptions))
		for k, v := range n.ipamOptions {
			if _, ok := to.ipamOptions[k]; !ok {
				to.ipamOptions[k] = v
			}
		}
	}
	if len(n.ipamV4Config) > 0 {
		to.ipamV4Config = make([]*IpamConf, 0, len(n.ipamV4Config))
		to.ipamV4Config = append(to.ipamV4Config, n.ipamV4Config...)
	}
	if len(n.ipamV6Config) > 0 {
		to.ipamV6Config = make([]*IpamConf, 0, len(n.ipamV6Config))
		to.ipamV6Config = append(to.ipamV6Config, n.ipamV6Config...)
	}
	if len(n.generic) > 0 {
		to.generic = options.Generic{}
		maps.Copy(to.generic, n.generic)
	}

	// Network drivers only see generic flags. So, make sure they match.
	if to.generic == nil {
		to.generic = options.Generic{}
	}
	to.generic[netlabel.Internal] = to.internal
	to.generic[netlabel.EnableIPv4] = to.enableIPv4
	to.generic[netlabel.EnableIPv6] = to.enableIPv6

	return nil
}

func (n *Network) CopyTo(o datastore.KVObject) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	dstN := o.(*Network)
	dstN.name = n.name
	dstN.id = n.id
	dstN.created = n.created
	dstN.networkType = n.networkType
	dstN.scope = n.scope
	dstN.dynamic = n.dynamic
	dstN.ipamType = n.ipamType
	dstN.enableIPv4 = n.enableIPv4
	dstN.enableIPv6 = n.enableIPv6
	dstN.persist = n.persist
	dstN.dbIndex = n.dbIndex
	dstN.dbExists = n.dbExists
	dstN.drvOnce = n.drvOnce
	dstN.internal = n.internal
	dstN.attachable = n.attachable
	dstN.inDelete = n.inDelete
	dstN.ingress = n.ingress
	dstN.configOnly = n.configOnly
	dstN.configFrom = n.configFrom
	dstN.loadBalancerIP = n.loadBalancerIP
	dstN.loadBalancerMode = n.loadBalancerMode
	dstN.skipGwAllocIPv4 = n.skipGwAllocIPv4
	dstN.skipGwAllocIPv6 = n.skipGwAllocIPv6

	// copy labels
	if dstN.labels == nil {
		dstN.labels = make(map[string]string, len(n.labels))
	}
	maps.Copy(dstN.labels, n.labels)

	if n.ipamOptions != nil {
		dstN.ipamOptions = make(map[string]string, len(n.ipamOptions))
		maps.Copy(dstN.ipamOptions, n.ipamOptions)
	}

	for _, c := range n.ipamV4Config {
		dstN.ipamV4Config = append(dstN.ipamV4Config, c.Copy())
	}

	for _, inf := range n.ipamV4Info {
		dstN.ipamV4Info = append(dstN.ipamV4Info, inf.Copy())
	}

	for _, c := range n.ipamV6Config {
		dstN.ipamV6Config = append(dstN.ipamV6Config, c.Copy())
	}

	for _, inf := range n.ipamV6Info {
		dstN.ipamV6Info = append(dstN.ipamV6Info, inf.Copy())
	}

	dstN.generic = options.Generic{}
	maps.Copy(dstN.generic, n.generic)

	return nil
}

func (n *Network) validateAdvertiseAddrConfig() error {
	var errs []error
	_, err := n.validatedAdvertiseAddrNMsgs()
	errs = append(errs, err)
	_, err = n.validatedAdvertiseAddrInterval()
	errs = append(errs, err)
	return errors.Join(errs...)
}

func (n *Network) advertiseAddrNMsgs() (int, bool) {
	v, err := n.validatedAdvertiseAddrNMsgs()
	if err != nil || v == nil {
		// On Linux, config was validated before network creation. This
		// path is for un-set values and unsupported platforms.
		return 0, false
	}
	return *v, true
}

func (n *Network) advertiseAddrInterval() (time.Duration, bool) {
	v, err := n.validatedAdvertiseAddrInterval()
	if err != nil || v == nil {
		// On Linux, config was validated before network creation. This
		// path is for un-set values and unsupported platforms.
		return 0, false
	}
	return *v, true
}

func (n *Network) MarshalJSON() ([]byte, error) {
	// TODO: Can be made much more generic with the help of reflection (but has some golang limitations)
	netMap := make(map[string]any)
	netMap["name"] = n.name
	netMap["id"] = n.id
	netMap["created"] = n.created
	netMap["networkType"] = n.networkType
	netMap["scope"] = n.scope
	netMap["labels"] = n.labels
	netMap["ipamType"] = n.ipamType
	netMap["ipamOptions"] = n.ipamOptions
	netMap["addrSpace"] = n.addrSpace
	netMap["enableIPv4"] = n.enableIPv4
	netMap["enableIPv6"] = n.enableIPv6
	if n.generic != nil {
		netMap["generic"] = n.generic
	}
	netMap["persist"] = n.persist
	if len(n.ipamV4Config) > 0 {
		ics, err := json.Marshal(n.ipamV4Config)
		if err != nil {
			return nil, err
		}
		netMap["ipamV4Config"] = string(ics)
	}
	if len(n.ipamV4Info) > 0 {
		iis, err := json.Marshal(n.ipamV4Info)
		if err != nil {
			return nil, err
		}
		netMap["ipamV4Info"] = string(iis)
	}
	if len(n.ipamV6Config) > 0 {
		ics, err := json.Marshal(n.ipamV6Config)
		if err != nil {
			return nil, err
		}
		netMap["ipamV6Config"] = string(ics)
	}
	if len(n.ipamV6Info) > 0 {
		iis, err := json.Marshal(n.ipamV6Info)
		if err != nil {
			return nil, err
		}
		netMap["ipamV6Info"] = string(iis)
	}
	netMap["internal"] = n.internal
	netMap["attachable"] = n.attachable
	netMap["inDelete"] = n.inDelete
	netMap["ingress"] = n.ingress
	netMap["configOnly"] = n.configOnly
	netMap["configFrom"] = n.configFrom
	netMap["loadBalancerIP"] = n.loadBalancerIP
	netMap["loadBalancerMode"] = n.loadBalancerMode
	netMap["skipGwAllocIPv4"] = n.skipGwAllocIPv4
	netMap["skipGwAllocIPv6"] = n.skipGwAllocIPv6
	return json.Marshal(netMap)
}

func (n *Network) UnmarshalJSON(b []byte) (err error) {
	// TODO: Can be made much more generic with the help of reflection (but has some golang limitations)
	var netMap map[string]any
	if err := json.Unmarshal(b, &netMap); err != nil {
		return err
	}
	n.name = netMap["name"].(string)
	n.id = netMap["id"].(string)
	// "created" is not available in older versions
	if v, ok := netMap["created"]; ok {
		// n.created is time.Time but marshalled as string
		if err = n.created.UnmarshalText([]byte(v.(string))); err != nil {
			log.G(context.TODO()).Warnf("failed to unmarshal creation time %v: %v", v, err)
			n.created = time.Time{}
		}
	}
	n.networkType = netMap["networkType"].(string)
	n.enableIPv4 = true // Default for networks created before the option to disable IPv4 was added.
	if v, ok := netMap["enableIPv4"]; ok {
		n.enableIPv4 = v.(bool)
	}
	n.enableIPv6 = netMap["enableIPv6"].(bool)

	// if we weren't unmarshaling to netMap we could simply set n.labels
	// unfortunately, we can't because map[string]interface{} != map[string]string
	if labels, ok := netMap["labels"].(map[string]any); ok {
		n.labels = make(map[string]string, len(labels))
		for label, value := range labels {
			n.labels[label] = value.(string)
		}
	}

	if v, ok := netMap["ipamOptions"]; ok {
		if iOpts, ok := v.(map[string]any); ok {
			n.ipamOptions = make(map[string]string, len(iOpts))
			for k, v := range iOpts {
				n.ipamOptions[k] = v.(string)
			}
		}
	}

	if v, ok := netMap["generic"]; ok {
		n.generic = v.(map[string]any)
		// Restore opts in their map[string]string form
		if gv, ok := n.generic[netlabel.GenericData]; ok {
			var lmap map[string]string
			ba, err := json.Marshal(gv)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(ba, &lmap); err != nil {
				return err
			}
			n.generic[netlabel.GenericData] = lmap
		}
	}
	if v, ok := netMap["persist"]; ok {
		n.persist = v.(bool)
	}
	if v, ok := netMap["ipamType"]; ok {
		n.ipamType = v.(string)
	} else {
		n.ipamType = defaultipam.DriverName
	}
	if v, ok := netMap["addrSpace"]; ok {
		n.addrSpace = v.(string)
	}
	if v, ok := netMap["ipamV4Config"]; ok {
		if err := json.Unmarshal([]byte(v.(string)), &n.ipamV4Config); err != nil {
			return err
		}
	}
	if v, ok := netMap["ipamV4Info"]; ok {
		if err := json.Unmarshal([]byte(v.(string)), &n.ipamV4Info); err != nil {
			return err
		}
	}
	if v, ok := netMap["ipamV6Config"]; ok {
		if err := json.Unmarshal([]byte(v.(string)), &n.ipamV6Config); err != nil {
			return err
		}
	}
	if v, ok := netMap["ipamV6Info"]; ok {
		if err := json.Unmarshal([]byte(v.(string)), &n.ipamV6Info); err != nil {
			return err
		}
	}
	if v, ok := netMap["internal"]; ok {
		n.internal = v.(bool)
	}
	if v, ok := netMap["attachable"]; ok {
		n.attachable = v.(bool)
	}
	if s, ok := netMap["scope"]; ok {
		n.scope = s.(string)
	}
	if v, ok := netMap["inDelete"]; ok {
		n.inDelete = v.(bool)
	}
	if v, ok := netMap["ingress"]; ok {
		n.ingress = v.(bool)
	}
	if v, ok := netMap["configOnly"]; ok {
		n.configOnly = v.(bool)
	}
	if v, ok := netMap["configFrom"]; ok {
		n.configFrom = v.(string)
	}
	if v, ok := netMap["loadBalancerIP"]; ok {
		n.loadBalancerIP = net.ParseIP(v.(string))
	}
	n.loadBalancerMode = loadBalancerModeDefault
	if v, ok := netMap["loadBalancerMode"]; ok {
		n.loadBalancerMode = v.(string)
	}
	if v, ok := netMap["skipGwAllocIPv4"]; ok {
		n.skipGwAllocIPv4 = v.(bool)
	}
	if v, ok := netMap["skipGwAllocIPv6"]; ok {
		n.skipGwAllocIPv6 = v.(bool)
	}
	return nil
}

// NetworkOption is an option setter function type used to pass various options to
// NewNetwork method. The various setter functions of type NetworkOption are
// provided by libnetwork, they look like NetworkOptionXXXX(...)
type NetworkOption func(n *Network)

// NetworkOptionGeneric function returns an option setter for a Generic option defined
// in a Dictionary of Key-Value pair
func NetworkOptionGeneric(generic map[string]any) NetworkOption {
	return func(n *Network) {
		if n.generic == nil {
			n.generic = make(map[string]any)
		}
		if val, ok := generic[netlabel.EnableIPv4]; ok {
			n.enableIPv4 = val.(bool)
		}
		if val, ok := generic[netlabel.EnableIPv6]; ok {
			n.enableIPv6 = val.(bool)
		}
		if val, ok := generic[netlabel.Internal]; ok {
			n.internal = val.(bool)
		}
		maps.Copy(n.generic, generic)
	}
}

// NetworkOptionIngress returns an option setter to indicate if a network is
// an ingress network.
func NetworkOptionIngress(ingress bool) NetworkOption {
	return func(n *Network) {
		n.ingress = ingress
	}
}

// NetworkOptionPersist returns an option setter to set persistence policy for a network
func NetworkOptionPersist(persist bool) NetworkOption {
	return func(n *Network) {
		n.persist = persist
	}
}

// NetworkOptionEnableIPv4 returns an option setter to explicitly configure IPv4
func NetworkOptionEnableIPv4(enableIPv4 bool) NetworkOption {
	return func(n *Network) {
		if n.generic == nil {
			n.generic = make(map[string]any)
		}
		n.enableIPv4 = enableIPv4
		n.generic[netlabel.EnableIPv4] = enableIPv4
	}
}

// NetworkOptionEnableIPv6 returns an option setter to explicitly configure IPv6
func NetworkOptionEnableIPv6(enableIPv6 bool) NetworkOption {
	return func(n *Network) {
		if n.generic == nil {
			n.generic = make(map[string]any)
		}
		n.enableIPv6 = enableIPv6
		n.generic[netlabel.EnableIPv6] = enableIPv6
	}
}

// NetworkOptionInternalNetwork returns an option setter to config the network
// to be internal which disables default gateway service
func NetworkOptionInternalNetwork() NetworkOption {
	return func(n *Network) {
		if n.generic == nil {
			n.generic = make(map[string]any)
		}
		n.internal = true
		n.generic[netlabel.Internal] = true
	}
}

// NetworkOptionAttachable returns an option setter to set attachable for a network
func NetworkOptionAttachable(attachable bool) NetworkOption {
	return func(n *Network) {
		n.attachable = attachable
	}
}

// NetworkOptionScope returns an option setter to overwrite the network's scope.
// By default the network's scope is set to the network driver's datascope.
func NetworkOptionScope(scope string) NetworkOption {
	return func(n *Network) {
		n.scope = scope
	}
}

// NetworkOptionIpam function returns an option setter for the ipam configuration for this network
func NetworkOptionIpam(ipamDriver string, addrSpace string, ipV4 []*IpamConf, ipV6 []*IpamConf, opts map[string]string) NetworkOption {
	return func(n *Network) {
		if ipamDriver != "" {
			n.ipamType = ipamDriver
			if ipamDriver == defaultipam.DriverName {
				n.ipamType = defaultIpamForNetworkType(n.Type())
			}
		}
		n.ipamOptions = opts
		n.addrSpace = addrSpace
		n.ipamV4Config = ipV4
		n.ipamV6Config = ipV6
	}
}

// NetworkOptionLBEndpoint function returns an option setter for the configuration of the load balancer endpoint for this network
func NetworkOptionLBEndpoint(ip net.IP) NetworkOption {
	return func(n *Network) {
		n.loadBalancerIP = ip
	}
}

// NetworkOptionDriverOpts function returns an option setter for any driver parameter described by a map
func NetworkOptionDriverOpts(opts map[string]string) NetworkOption {
	return func(n *Network) {
		if n.generic == nil {
			n.generic = make(map[string]any)
		}
		if opts == nil {
			opts = make(map[string]string)
		}
		// Store the options
		n.generic[netlabel.GenericData] = opts
	}
}

// NetworkOptionLabels function returns an option setter for labels specific to a network
func NetworkOptionLabels(labels map[string]string) NetworkOption {
	return func(n *Network) {
		n.labels = labels
	}
}

// NetworkOptionDynamic function returns an option setter for dynamic option for a network
func NetworkOptionDynamic() NetworkOption {
	return func(n *Network) {
		n.dynamic = true
	}
}

// NetworkOptionConfigOnly tells controller this network is
// a configuration only network. It serves as a configuration
// for other networks.
func NetworkOptionConfigOnly() NetworkOption {
	return func(n *Network) {
		n.configOnly = true
	}
}

// NetworkOptionConfigFrom tells controller to pick the
// network configuration from a configuration only network
func NetworkOptionConfigFrom(name string) NetworkOption {
	return func(n *Network) {
		n.configFrom = name
	}
}

func (n *Network) processOptions(options ...NetworkOption) {
	for _, opt := range options {
		if opt != nil {
			opt(n)
		}
	}
}

type networkDeleteParams struct {
	rmLBEndpoint bool
}

// NetworkDeleteOption is a type for optional parameters to pass to the
// Network.Delete() function.
type NetworkDeleteOption func(p *networkDeleteParams)

// NetworkDeleteOptionRemoveLB informs a Network.Delete() operation that should
// remove the load balancer endpoint for this network.  Note that the Delete()
// method will automatically remove a load balancing endpoint for most networks
// when the network is otherwise empty.  However, this does not occur for some
// networks.  In particular, networks marked as ingress (which are supposed to
// be more permanent than other overlay networks) won't automatically remove
// the LB endpoint on Delete().  This method allows for explicit removal of
// such networks provided there are no other endpoints present in the network.
// If the network still has non-LB endpoints present, Delete() will not
// remove the LB endpoint and will return an error.
func NetworkDeleteOptionRemoveLB(p *networkDeleteParams) {
	p.rmLBEndpoint = true
}

func (n *Network) driverIsMultihost() bool {
	_, capabilities, err := n.getController().resolveDriver(n.networkType, true)
	if err != nil {
		return false
	}
	return capabilities.ConnectivityScope == scope.Global
}

func (n *Network) driver(load bool) (driverapi.Driver, error) {
	d, capabilities, err := n.getController().resolveDriver(n.networkType, load)
	if err != nil {
		return nil, err
	}

	n.mu.Lock()
	// If load is not required, driver, cap and err may all be nil
	if n.scope == "" {
		n.scope = capabilities.DataScope
	}
	if n.dynamic {
		// If the network is dynamic, then it is swarm
		// scoped regardless of the backing driver.
		n.scope = scope.Swarm
	}
	n.mu.Unlock()
	return d, nil
}

// Delete the network.
func (n *Network) Delete(options ...NetworkDeleteOption) error {
	var params networkDeleteParams
	for _, opt := range options {
		opt(&params)
	}
	return n.delete(false, params.rmLBEndpoint)
}

// This function gets called in 3 ways:
//   - Delete() -- (false, false)
//     remove if endpoint count == 0 or endpoint count == 1 and
//     there is a load balancer IP
//   - Delete(libnetwork.NetworkDeleteOptionRemoveLB) -- (false, true)
//     remove load balancer and network if endpoint count == 1
//   - controller.networkCleanup() -- (true, true)
//     remove the network no matter what
func (n *Network) delete(force bool, rmLBEndpoint bool) error {
	n.mu.Lock()
	c := n.ctrlr
	name := n.name
	id := n.id
	n.mu.Unlock()

	c.networkLocker.Lock(id)
	defer c.networkLocker.Unlock(id) //nolint:errcheck

	n, err := c.getNetworkFromStore(id)
	if err != nil {
		return errdefs.NotFound(fmt.Errorf("unknown network %s id %s", name, id))
	}

	// Only remove ingress on force removal or explicit LB endpoint removal
	if n.ingress && !force && !rmLBEndpoint {
		return &ActiveEndpointsError{name: n.name, id: n.id}
	}

	if !force && n.configOnly {
		refNws := c.findNetworks(filterNetworkByConfigFrom(n.name))
		if len(refNws) > 0 {
			return types.ForbiddenErrorf("configuration network %q is in use", n.Name())
		}
	}

	// Check that the network is empty
	var emptyCount int
	if n.hasLoadBalancerEndpoint() {
		emptyCount = 1
	}
	eps := c.findEndpoints(filterEndpointByNetworkId(n.id))
	if !force && len(eps) > emptyCount {
		return &ActiveEndpointsError{
			name: n.name,
			id:   n.id,
			endpoints: sliceutil.Map(eps, func(ep *Endpoint) string {
				return fmt.Sprintf(`name:%q id:%q`, ep.name, stringid.TruncateID(ep.id))
			}),
		}
	}

	if n.hasLoadBalancerEndpoint() {
		// If we got to this point, then the following must hold:
		//  * force is true OR endpoint count == 1
		if err := n.deleteLoadBalancerSandbox(); err != nil {
			if !force {
				return err
			}
			// continue deletion when force is true even on error
			log.G(context.TODO()).Warnf("Error deleting load balancer sandbox: %v", err)
		}
	}

	// Up to this point, errors that we returned were recoverable.
	// From here on, any errors leave us in an inconsistent state.
	// This is unfortunate, but there isn't a safe way to
	// reconstitute a load-balancer endpoint after removing it.

	// Mark the network for deletion
	n.inDelete = true
	if err = c.storeNetwork(context.TODO(), n); err != nil {
		return fmt.Errorf("error marking network %s (%s) for deletion: %v", n.Name(), n.ID(), err)
	}

	if n.configOnly {
		goto removeFromStore
	}

	n.ipamRelease()

	// We are about to delete the network. Leave the gossip
	// cluster for the network to stop all incoming network
	// specific gossip updates before cleaning up all the service
	// bindings for the network. But cleanup service binding
	// before deleting the network from the store since service
	// bindings cleanup requires the network in the store.
	n.cancelDriverWatches()
	if err = n.leaveCluster(); err != nil {
		log.G(context.TODO()).Errorf("Failed leaving network %s from the agent cluster: %v", n.Name(), err)
	}

	// Cleanup the service discovery for this network
	c.cleanupServiceDiscovery(n.ID())

	// Cleanup the load balancer. On Windows this call is required
	// to remove remote loadbalancers in VFP, and must be performed before
	// dataplane network deletion.
	if runtime.GOOS == "windows" {
		c.cleanupServiceBindings(n.ID())
	}

	// Delete the network from the dataplane
	if err = n.deleteNetwork(); err != nil {
		if !force {
			return err
		}
		log.G(context.TODO()).Debugf("driver failed to delete stale network %s (%s): %v", n.Name(), n.ID(), err)
	}

removeFromStore:
	// TODO(robmry) - remove this once downgrade past 28.1.0 is no longer supported.
	// The endpoint count is no longer used, it's created in the store to make
	// downgrade work, versions older than 28.1.0 expect to read it and error if they
	// can't.
	if err := c.store.DeleteObject(&endpointCnt{n: n}); err != nil {
		if !errors.Is(err, datastore.ErrKeyNotFound) {
			log.G(context.TODO()).WithFields(log.Fields{
				"network": n.name,
				"error":   err,
			}).Debug("Error deleting network endpoint count from store")
		}
	}

	if err = c.deleteStoredNetwork(n); err != nil {
		return fmt.Errorf("error deleting network from store: %v", err)
	}

	return nil
}

func (n *Network) deleteNetwork() error {
	d, err := n.driver(true)
	if err != nil {
		return fmt.Errorf("failed deleting Network: %v", err)
	}

	if err := d.DeleteNetwork(n.ID()); err != nil {
		// Forbidden Errors should be honored
		if cerrdefs.IsPermissionDenied(err) {
			return err
		}

		if _, ok := err.(types.MaskableError); !ok {
			log.G(context.TODO()).Warnf("driver error deleting network %s : %v", n.name, err)
		}
	}

	for _, resolver := range n.resolver {
		resolver.Stop()
	}
	return nil
}

func (n *Network) addEndpoint(ctx context.Context, ep *Endpoint) error {
	d, err := n.driver(true)
	if err != nil {
		return fmt.Errorf("failed to add endpoint: %v", err)
	}

	err = d.CreateEndpoint(ctx, n.id, ep.id, ep.Iface(), ep.generic)
	if err != nil {
		return types.InternalErrorf("failed to create endpoint %s on network %s: %v",
			ep.Name(), n.Name(), err)
	}

	return nil
}

// CreateEndpoint creates a new endpoint to this network symbolically identified by the
// specified unique name. The options parameter carries driver specific options.
func (n *Network) CreateEndpoint(ctx context.Context, name string, options ...EndpointOption) (*Endpoint, error) {
	var err error
	if strings.TrimSpace(name) == "" {
		return nil, types.InvalidParameterErrorf("invalid name: name is empty")
	}

	if n.ConfigOnly() {
		return nil, types.ForbiddenErrorf("cannot create endpoint on configuration-only network")
	}

	if _, err = n.EndpointByName(name); err == nil {
		return nil, types.ForbiddenErrorf("endpoint with name %s already exists in network %s", name, n.Name())
	}

	n.ctrlr.networkLocker.Lock(n.id)
	defer n.ctrlr.networkLocker.Unlock(n.id) //nolint:errcheck

	return n.createEndpoint(ctx, name, options...)
}

func (n *Network) createEndpoint(ctx context.Context, name string, options ...EndpointOption) (*Endpoint, error) {
	var err error

	ep := &Endpoint{name: name, generic: make(map[string]any), iface: &EndpointInterface{}}
	ep.id = stringid.GenerateRandomID()

	// Initialize ep.network with a possibly stale copy of n. We need this to get network from
	// store. But once we get it from store we will have the most uptodate copy possibly.
	ep.network = n
	ep.network, err = ep.getNetworkFromStore()
	if err != nil {
		log.G(ctx).Errorf("failed to get network during CreateEndpoint: %v", err)
		return nil, err
	}
	n = ep.network

	ep.processOptions(options...)

	for _, llIPNet := range ep.Iface().LinkLocalAddresses() {
		if !llIPNet.IP.IsLinkLocalUnicast() {
			return nil, types.InvalidParameterErrorf("invalid link local IP address: %v", llIPNet.IP)
		}
	}

	if opt, ok := ep.generic[netlabel.MacAddress]; ok {
		if mac, ok := opt.(net.HardwareAddr); ok {
			ep.iface.mac = mac
		}
	}

	ipam, capability, err := n.getController().getIPAMDriver(n.ipamType)
	if err != nil {
		return nil, err
	}

	ep.ipamOptions = map[string]string{netlabel.EndpointName: name}
	if capability.RequiresMACAddress {
		if ep.iface.mac == nil {
			ep.iface.mac = netutils.GenerateRandomMAC()
		}
		ep.ipamOptions[netlabel.MacAddress] = ep.iface.mac.String()
	}

	wantIPv6 := n.enableIPv6 && !ep.disableIPv6

	if err = ep.assignAddress(ipam, n.enableIPv4, wantIPv6); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			ep.releaseIPAddresses()
		}
	}()

	if err = n.addEndpoint(ctx, ep); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			if e := ep.deleteEndpoint(false); e != nil {
				log.G(ctx).Warnf("cleaning up endpoint failed %s : %v", name, e)
			}
		}
	}()

	// We should perform storeEndpoint call right after addEndpoint
	// in order to have iface properly configured
	if err = n.getController().storeEndpoint(ctx, ep); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			if e := n.getController().deleteStoredEndpoint(ep); e != nil {
				log.G(ctx).Warnf("error rolling back endpoint %s from store: %v", name, e)
			}
		}
	}()

	return ep, nil
}

// Endpoints returns the list of Endpoint(s) in this network.
func (n *Network) Endpoints() []*Endpoint {
	endpoints, err := n.getEndpointsFromStore()
	if err != nil {
		log.G(context.TODO()).Error(err)
	}
	return endpoints
}

// HasContainerAttachments returns true when len(n.Endpoints()) > 0.
func (n *Network) HasContainerAttachments() bool {
	return len(n.Endpoints()) > 0
}

// WalkEndpoints uses the provided function to walk the Endpoints.
func (n *Network) WalkEndpoints(walker EndpointWalker) {
	if slices.ContainsFunc(n.Endpoints(), walker) {
		return
	}
}

// EndpointByName returns the Endpoint which has the passed name. If not found,
// an [errdefs.ErrNotFound] is returned.
func (n *Network) EndpointByName(name string) (*Endpoint, error) {
	if name == "" {
		return nil, types.InvalidParameterErrorf("invalid name: name is empty")
	}
	var e *Endpoint

	s := func(current *Endpoint) bool {
		if current.Name() == name {
			e = current
			return true
		}
		return false
	}

	n.WalkEndpoints(s)

	if e == nil {
		return nil, errdefs.NotFound(fmt.Errorf("endpoint %s not found", name))
	}

	return e, nil
}

// updateSvcRecord adds or deletes local DNS records for a given Endpoint.
func (n *Network) updateSvcRecord(ctx context.Context, ep *Endpoint, isAdd bool) {
	ctx, span := otel.Tracer("").Start(ctx, "libnetwork.updateSvcRecord", trace.WithAttributes(
		attribute.String("ep.name", ep.name),
		attribute.Bool("isAdd", isAdd)))
	defer span.End()

	iface := ep.Iface()
	if iface == nil {
		return
	}

	var ipv4, ipv6 net.IP
	if iface.Address() != nil {
		ipv4 = iface.Address().IP
	}
	if iface.AddressIPv6() != nil {
		ipv6 = iface.AddressIPv6().IP
	}

	serviceID := ep.svcID
	if serviceID == "" {
		serviceID = ep.ID()
	}

	dnsNames := ep.getDNSNames()
	if isAdd {
		for i, dnsName := range dnsNames {
			ipMapUpdate := i == 0 // ipMapUpdate indicates whether PTR records should be updated.
			n.addSvcRecords(ep.ID(), dnsName, serviceID, ipv4, ipv6, ipMapUpdate, "updateSvcRecord")
		}
	} else {
		for i, dnsName := range dnsNames {
			ipMapUpdate := i == 0 // ipMapUpdate indicates whether PTR records should be updated.
			n.deleteSvcRecords(ep.ID(), dnsName, serviceID, ipv4, ipv6, ipMapUpdate, "updateSvcRecord")
		}
	}
}

func addIPToName(ipMap *setmatrix.SetMatrix[string, ipInfo], name, serviceID string, ip net.IP) {
	reverseIP := netutils.ReverseIP(ip.String())
	ipMap.Insert(reverseIP, ipInfo{
		name:      name,
		serviceID: serviceID,
	})
}

func delIPToName(ipMap *setmatrix.SetMatrix[string, ipInfo], name, serviceID string, ip net.IP) {
	reverseIP := netutils.ReverseIP(ip.String())
	ipMap.Remove(reverseIP, ipInfo{
		name:      name,
		serviceID: serviceID,
	})
}

func addNameToIP(svcMap *setmatrix.SetMatrix[string, svcMapEntry], name, serviceID string, epIP net.IP) {
	// Since DNS name resolution is case-insensitive, Use the lower-case form
	// of the name as the key into svcMap
	lowerCaseName := strings.ToLower(name)
	svcMap.Insert(lowerCaseName, svcMapEntry{
		ip:        epIP.String(),
		serviceID: serviceID,
	})
}

func delNameToIP(svcMap *setmatrix.SetMatrix[string, svcMapEntry], name, serviceID string, epIP net.IP) {
	lowerCaseName := strings.ToLower(name)
	svcMap.Remove(lowerCaseName, svcMapEntry{
		ip:        epIP.String(),
		serviceID: serviceID,
	})
}

// TODO(aker): remove ipMapUpdate param and add a proper method dedicated to update PTR records.
func (n *Network) addSvcRecords(eID, name, serviceID string, epIPv4, epIPv6 net.IP, ipMapUpdate bool, method string) {
	// Do not add service names for ingress network as this is a
	// routing only network
	if n.ingress {
		return
	}
	networkID := n.ID()
	log.G(context.TODO()).Debugf("%s (%.7s).addSvcRecords(%s, %s, %s, %t) %s sid:%s", eID, networkID, name, epIPv4, epIPv6, ipMapUpdate, method, serviceID)

	c := n.getController()
	c.mu.Lock()
	defer c.mu.Unlock()

	sr, ok := c.svcRecords[networkID]
	if !ok {
		sr = &svcInfo{}
		c.svcRecords[networkID] = sr
	}

	if ipMapUpdate {
		if epIPv4 != nil {
			addIPToName(&sr.ipMap, name, serviceID, epIPv4)
		}
		if epIPv6 != nil {
			addIPToName(&sr.ipMap, name, serviceID, epIPv6)
		}
	}

	if epIPv4 != nil {
		addNameToIP(&sr.svcMap, name, serviceID, epIPv4)
	}
	if epIPv6 != nil {
		addNameToIP(&sr.svcIPv6Map, name, serviceID, epIPv6)
	}
}

func (n *Network) deleteSvcRecords(eID, name, serviceID string, epIPv4, epIPv6 net.IP, ipMapUpdate bool, method string) {
	// Do not delete service names from ingress network as this is a
	// routing only network
	if n.ingress {
		return
	}
	networkID := n.ID()
	log.G(context.TODO()).Debugf("%s (%.7s).deleteSvcRecords(%s, %s, %s, %t) %s sid:%s ", eID, networkID, name, epIPv4, epIPv6, ipMapUpdate, method, serviceID)

	c := n.getController()
	c.mu.Lock()
	defer c.mu.Unlock()

	sr, ok := c.svcRecords[networkID]
	if !ok {
		return
	}

	if ipMapUpdate {
		if epIPv4 != nil {
			delIPToName(&sr.ipMap, name, serviceID, epIPv4)
		}
		if epIPv6 != nil {
			delIPToName(&sr.ipMap, name, serviceID, epIPv6)
		}
	}

	if epIPv4 != nil {
		delNameToIP(&sr.svcMap, name, serviceID, epIPv4)
	}
	if epIPv6 != nil {
		delNameToIP(&sr.svcIPv6Map, name, serviceID, epIPv6)
	}
}

func (n *Network) getController() *Controller {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ctrlr
}

func (n *Network) ipamAllocate() (retErr error) {
	if n.hasSpecialDriver() {
		return nil
	}

	ipam, _, err := n.getController().getIPAMDriver(n.ipamType)
	if err != nil {
		return err
	}

	if n.addrSpace == "" {
		if n.addrSpace, err = n.deriveAddressSpace(); err != nil {
			return err
		}
	}

	if n.enableIPv4 {
		if len(n.ipamV4Config) == 0 {
			n.ipamV4Config = []*IpamConf{{}}
		}
		info, err := n.ipamAllocateVersion(ipam, false, n.ipamV4Config, n.ipamV4Info, n.skipGwAllocIPv4)
		if err != nil {
			return err
		}
		n.ipamV4Info = info
		defer func() {
			if retErr != nil {
				n.ipamReleaseVersion(4, ipam)
			}
		}()
	}

	if n.enableIPv6 {
		if len(n.ipamV6Config) == 0 {
			n.ipamV6Config = []*IpamConf{{}}
		}
		info, err := n.ipamAllocateVersion(ipam, true, n.ipamV6Config, n.ipamV6Info, n.skipGwAllocIPv6)
		if err != nil {
			return err
		}
		n.ipamV6Info = info
	}

	return nil
}

func (n *Network) ipamAllocateVersion(ipam ipamapi.Ipam, v6 bool, ipamConf []*IpamConf, ipamInfo []*IpamInfo, skipGwAlloc bool) (_ []*IpamInfo, retErr error) {
	var newInfo []*IpamInfo

	log.G(context.TODO()).WithFields(log.Fields{
		"network": n.Name(),
		"nid":     stringid.TruncateID(n.ID()),
		"ipv6":    v6,
	}).Debug("Allocating pools for network")

	for i, cfg := range ipamConf {
		if err := cfg.Validate(); err != nil {
			return nil, err
		}

		var reserved []netip.Prefix
		if n.Scope() != scope.Global {
			reserved = netutils.InferReservedNetworks(v6)
		}

		// Determine if the preferred pool is unspecified (blank, or a 0.0.0.0 or :: address)
		prefPool := cfg.PreferredPool
		isDefaultPool := prefPool == ""
		if !isDefaultPool {
			if prefix, err := netip.ParsePrefix(prefPool); err != nil {
				// This should never happen
				return nil, types.InvalidParameterErrorf("invalid preferred address %q: %v", prefPool, err)
			} else if prefix.Addr().IsUnspecified() {
				isDefaultPool = true
			}
		}

		// During network restore, if no subnet was specified in the original network-create
		// request, use the previously allocated subnet.
		if isDefaultPool && len(ipamInfo) > i {
			prefPool = ipamInfo[i].Pool.String()
		}

		alloc, err := ipam.RequestPool(ipamapi.PoolRequest{
			AddressSpace: n.addrSpace,
			Pool:         prefPool,
			SubPool:      cfg.SubPool,
			Options:      n.ipamOptions,
			Exclude:      reserved,
			V6:           v6,
		})
		if err != nil {
			return nil, err
		}
		defer func() {
			if retErr != nil {
				if err := ipam.ReleasePool(alloc.PoolID); err != nil {
					log.G(context.TODO()).WithFields(log.Fields{
						"network": n.Name(),
						"nid":     stringid.TruncateID(n.ID()),
						"pool":    alloc.PoolID,
						"retErr":  retErr,
						"error":   err,
					}).Warn("Failed to release address pool after failure to create network")
				}
			}
		}()

		d := &IpamInfo{
			PoolID: alloc.PoolID,
			Meta:   alloc.Meta,
			IPAMData: driverapi.IPAMData{
				Pool:         netiputil.ToIPNet(alloc.Pool),
				AddressSpace: n.addrSpace,
			},
		}
		newInfo = append(newInfo, d)

		// During network restore, if the original network-create request did not specify a
		// gateway, use the previously allocated gateway.
		prefGateway := cfg.Gateway
		if prefGateway == "" && len(ipamInfo) > i && ipamInfo[i].Gateway != nil {
			prefGateway = ipamInfo[i].Gateway.IP.String()
		}

		// If there's no user-configured gateway address but the IPAM driver returned a gw when it
		// set up the pool, use it. (It doesn't need to be requested/reserved in IPAM.)
		if prefGateway == "" {
			if gws, ok := d.Meta[netlabel.Gateway]; ok {
				if d.Gateway, err = types.ParseCIDR(gws); err != nil {
					return nil, types.InvalidParameterErrorf("failed to parse gateway address (%v) returned by ipam driver: %v", gws, err)
				}
			}
		}

		// If there's still no gateway, reserve cfg.Gateway if the user specified it. Else,
		// if the driver wants a gateway, let the IPAM driver select an address.
		if d.Gateway == nil && (prefGateway != "" || !skipGwAlloc) {
			gatewayOpts := map[string]string{
				ipamapi.RequestAddressType: netlabel.Gateway,
			}
			if d.Gateway, _, err = ipam.RequestAddress(d.PoolID, net.ParseIP(prefGateway), gatewayOpts); err != nil {
				return nil, types.InternalErrorf("failed to allocate gateway (%v): %v", prefGateway, err)
			}
		}

		// Auxiliary addresses must be part of the master address pool.
		// If they fall into the container addressable pool, reserve them.
		if cfg.AuxAddresses != nil {
			var ip net.IP
			d.IPAMData.AuxAddresses = make(map[string]*net.IPNet, len(cfg.AuxAddresses))
			for k, v := range cfg.AuxAddresses {
				if ip = net.ParseIP(v); ip == nil {
					return nil, types.InvalidParameterErrorf("non parsable secondary ip address (%s:%s) passed for network %s", k, v, n.Name())
				}
				if !d.Pool.Contains(ip) {
					return nil, types.ForbiddenErrorf("auxiliary address: (%s:%s) must belong to the master pool: %s", k, v, d.Pool)
				}
				// Attempt reservation in the container addressable pool, silent the error if address does not belong to that pool
				if d.IPAMData.AuxAddresses[k], _, err = ipam.RequestAddress(d.PoolID, ip, nil); err != nil && !errors.Is(err, ipamapi.ErrIPOutOfRange) {
					return nil, types.InternalErrorf("failed to allocate secondary ip address (%s:%s): %v", k, v, err)
				}
			}
		}
	}

	return newInfo, nil
}

func (n *Network) ipamRelease() {
	if n.hasSpecialDriver() {
		return
	}
	ipam, _, err := n.getController().getIPAMDriver(n.ipamType)
	if err != nil {
		log.G(context.TODO()).Warnf("Failed to retrieve ipam driver to release address pool(s) on delete of network %s (%s): %v", n.Name(), n.ID(), err)
		return
	}
	n.ipamReleaseVersion(4, ipam)
	n.ipamReleaseVersion(6, ipam)
}

func (n *Network) ipamReleaseVersion(ipVer int, ipam ipamapi.Ipam) {
	var infoList *[]*IpamInfo

	switch ipVer {
	case 4:
		infoList = &n.ipamV4Info
	case 6:
		infoList = &n.ipamV6Info
	default:
		log.G(context.TODO()).Warnf("incorrect ip version passed to ipam release: %d", ipVer)
		return
	}

	if len(*infoList) == 0 {
		return
	}

	log.G(context.TODO()).Debugf("releasing IPv%d pools from network %s (%s)", ipVer, n.Name(), n.ID())

	for _, d := range *infoList {
		if d.Gateway != nil {
			// FIXME(robmry) - if an IPAM driver returned a gateway in Meta[netlabel.Gateway], and
			// no user config overrode that address, it wasn't explicitly allocated so it shouldn't
			// be released here?
			if err := ipam.ReleaseAddress(d.PoolID, d.Gateway.IP); err != nil {
				log.G(context.TODO()).Warnf("Failed to release gateway ip address %s on delete of network %s (%s): %v", d.Gateway.IP, n.Name(), n.ID(), err)
			}
		}
		if d.IPAMData.AuxAddresses != nil {
			for k, nw := range d.IPAMData.AuxAddresses {
				if d.Pool.Contains(nw.IP) {
					if err := ipam.ReleaseAddress(d.PoolID, nw.IP); err != nil && !errors.Is(err, ipamapi.ErrIPOutOfRange) {
						log.G(context.TODO()).Warnf("Failed to release secondary ip address %s (%v) on delete of network %s (%s): %v", k, nw.IP, n.Name(), n.ID(), err)
					}
				}
			}
		}
		if err := ipam.ReleasePool(d.PoolID); err != nil {
			log.G(context.TODO()).Warnf("Failed to release address pool %s on delete of network %s (%s): %v", d.PoolID, n.Name(), n.ID(), err)
		}
	}

	*infoList = nil
}

func (n *Network) getIPInfo(ipVer int) []*IpamInfo {
	var info []*IpamInfo
	switch ipVer {
	case 4:
		info = n.ipamV4Info
	case 6:
		info = n.ipamV6Info
	default:
		return nil
	}
	l := make([]*IpamInfo, 0, len(info))
	n.mu.Lock()
	l = append(l, info...)
	n.mu.Unlock()
	return l
}

func (n *Network) getIPData(ipVer int) []driverapi.IPAMData {
	var info []*IpamInfo
	switch ipVer {
	case 4:
		info = n.ipamV4Info
	case 6:
		info = n.ipamV6Info
	default:
		return nil
	}
	l := make([]driverapi.IPAMData, 0, len(info))
	n.mu.Lock()
	for _, d := range info {
		l = append(l, d.IPAMData)
	}
	n.mu.Unlock()
	return l
}

func (n *Network) deriveAddressSpace() (string, error) {
	ipam, _ := n.getController().ipamRegistry.IPAM(n.ipamType)
	if ipam == nil {
		return "", types.NotFoundErrorf("failed to get default address space: unknown ipam type %q", n.ipamType)
	}
	local, global, err := ipam.GetDefaultAddressSpaces()
	if err != nil {
		return "", types.NotFoundErrorf("failed to get default address space: %v", err)
	}
	if n.Scope() == scope.Global {
		return global, nil
	}
	return local, nil
}

// Peers returns a slice of PeerInfo structures which has the information about the peer
// nodes participating in the same overlay network. This is currently the per-network
// gossip cluster. For non-dynamic overlay networks and bridge networks it returns an
// empty slice
func (n *Network) Peers() []networkdb.PeerInfo {
	if !n.Dynamic() {
		return []networkdb.PeerInfo{}
	}

	a := n.getController().getAgent()
	if a == nil {
		return []networkdb.PeerInfo{}
	}

	return a.networkDB.Peers(n.ID())
}

func (n *Network) DriverOptions() map[string]string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.generic != nil {
		if m, ok := n.generic[netlabel.GenericData]; ok {
			return m.(map[string]string)
		}
	}
	return map[string]string{}
}

func (n *Network) Scope() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.scope
}

func (n *Network) IpamConfig() (ipamType string, ipamOptions map[string]string, ipamV4Config []*IpamConf, ipamV6Config []*IpamConf) {
	n.mu.Lock()
	defer n.mu.Unlock()

	ipamV4Config = make([]*IpamConf, len(n.ipamV4Config))
	for i, c := range n.ipamV4Config {
		ipamV4Config[i] = c.Copy()
	}

	ipamV6Config = make([]*IpamConf, len(n.ipamV6Config))
	for i, c := range n.ipamV6Config {
		ipamV6Config[i] = c.Copy()
	}

	return n.ipamType, n.ipamOptions, ipamV4Config, ipamV6Config
}

func (n *Network) IpamInfo() (ipamV4Info []*IpamInfo, ipamV6Info []*IpamInfo) {
	n.mu.Lock()
	defer n.mu.Unlock()

	ipamV4Info = make([]*IpamInfo, len(n.ipamV4Info))
	for i, info := range n.ipamV4Info {
		ipamV4Info[i] = info.Copy()
	}

	ipamV6Info = make([]*IpamInfo, len(n.ipamV6Info))
	for i, info := range n.ipamV6Info {
		ipamV6Info[i] = info.Copy()
	}

	return ipamV4Info, ipamV6Info
}

func (n *Network) Internal() bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.internal
}

func (n *Network) Attachable() bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.attachable
}

func (n *Network) Ingress() bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.ingress
}

func (n *Network) Dynamic() bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.dynamic
}

func (n *Network) IPv4Enabled() bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.enableIPv4
}

func (n *Network) IPv6Enabled() bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.enableIPv6
}

func (n *Network) ConfigFrom() string {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.configFrom
}

func (n *Network) ConfigOnly() bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	return n.configOnly
}

func (n *Network) Labels() map[string]string {
	n.mu.Lock()
	defer n.mu.Unlock()

	lbls := make(map[string]string, len(n.labels))
	maps.Copy(lbls, n.labels)

	return lbls
}

func (n *Network) TableEventRegister(tableName string, objType driverapi.ObjectType) error {
	if !driverapi.IsValidType(objType) {
		return fmt.Errorf("invalid object type %v in registering table, %s", objType, tableName)
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	n.driverTables = append(n.driverTables, networkDBTable{
		name:    tableName,
		objType: objType,
	})
	return nil
}

func (n *Network) UpdateIpamConfig(ipV4Data []driverapi.IPAMData) {
	ipamV4Config := make([]*IpamConf, len(ipV4Data))
	for i, data := range ipV4Data {
		ipamV4Config[i] = &IpamConf{
			PreferredPool: data.Pool.String(),
			Gateway:       data.Gateway.IP.String(),
		}
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	n.ipamV4Config = ipamV4Config
}

// Special drivers are ones which do not need to perform any Network plumbing
func (n *Network) hasSpecialDriver() bool {
	return n.Type() == "host" || n.Type() == "null"
}

func (n *Network) hasLoadBalancerEndpoint() bool {
	return len(n.loadBalancerIP) != 0
}

// ResolveName looks up addresses of ipType for name req.
// Returns (addresses, true) if req is found, but len(addresses) may be 0 if
// there are no addresses of ipType. If the name is not found, the bool return
// will be false.
func (n *Network) ResolveName(ctx context.Context, req string, ipType types.IPFamily) ([]net.IP, bool) {
	c := n.getController()
	networkID := n.ID()

	_, span := otel.Tracer("").Start(ctx, "Network.ResolveName", trace.WithAttributes(
		attribute.String("libnet.network.name", n.Name()),
		attribute.String("libnet.network.id", networkID),
	))
	defer span.End()

	c.mu.Lock()
	// TODO(aker): release the lock earlier
	defer c.mu.Unlock()
	sr, ok := c.svcRecords[networkID]
	if !ok {
		return nil, false
	}

	req = strings.TrimSuffix(req, ".")
	req = strings.ToLower(req)

	ipSet, ok4 := sr.svcMap.Get(req)
	ipSet6, ok6 := sr.svcIPv6Map.Get(req)
	if !ok4 && !ok6 {
		// No result for v4 or v6, the name doesn't exist.
		return nil, false
	}
	if ipType == types.IPv6 {
		ipSet = ipSet6
	}

	// this map is to avoid IP duplicates, this can happen during a transition period where 2 services are using the same IP
	noDup := make(map[string]bool)
	var ipLocal []net.IP
	for _, ip := range ipSet {
		if _, dup := noDup[ip.ip]; !dup {
			noDup[ip.ip] = true
			ipLocal = append(ipLocal, net.ParseIP(ip.ip))
		}
	}
	return ipLocal, true
}

func (n *Network) HandleQueryResp(name string, ip net.IP) {
	networkID := n.ID()
	c := n.getController()
	c.mu.Lock()
	defer c.mu.Unlock()
	sr, ok := c.svcRecords[networkID]

	if !ok {
		return
	}

	ipStr := netutils.ReverseIP(ip.String())
	// If an object with extResolver == true is already in the set this call will fail
	// but anyway it means that has already been inserted before
	if ok, _ := sr.ipMap.Contains(ipStr, ipInfo{name: name}); ok {
		sr.ipMap.Remove(ipStr, ipInfo{name: name})
		sr.ipMap.Insert(ipStr, ipInfo{name: name, extResolver: true})
	}
}

func (n *Network) ResolveIP(_ context.Context, ip string) string {
	networkID := n.ID()
	c := n.getController()
	c.mu.Lock()
	defer c.mu.Unlock()
	sr, ok := c.svcRecords[networkID]

	if !ok {
		return ""
	}

	nwName := n.Name()

	elemSet, ok := sr.ipMap.Get(ip)
	if !ok || len(elemSet) == 0 {
		return ""
	}
	// NOTE it is possible to have more than one element in the Set, this will happen
	// because of interleave of different events from different sources (local container create vs
	// network db notifications)
	// In such cases the resolution will be based on the first element of the set, and can vary
	// during the system stabilization
	elem := elemSet[0]
	if elem.extResolver {
		return ""
	}

	return elem.name + "." + nwName
}

func (n *Network) ResolveService(ctx context.Context, name string) ([]*net.SRV, []net.IP) {
	c := n.getController()

	log.G(ctx).Debugf("Service name To resolve: %v", name)

	// There are DNS implementations that allow SRV queries for names not in
	// the format defined by RFC 2782. Hence specific validation checks are
	// not done.
	portName, protoService, ok := strings.Cut(name, ".")
	if !ok {
		return nil, nil
	}

	proto, svcName, ok := strings.Cut(protoService, ".")
	if !ok {
		return nil, nil
	}

	networkID := n.ID()
	c.mu.Lock()
	defer c.mu.Unlock()
	sr, ok := c.svcRecords[networkID]

	if !ok {
		return nil, nil
	}

	svcs, ok := sr.service[svcName]
	if !ok {
		return nil, nil
	}

	srv := []*net.SRV{}
	ip := []net.IP{}
	for _, svc := range svcs {
		if svc.portName != portName {
			continue
		}
		if svc.proto != proto {
			continue
		}
		for _, t := range svc.target {
			srv = append(srv, &net.SRV{
				Target: t.name,
				Port:   t.port,
			})

			ip = append(ip, t.ip)
		}
	}

	return srv, ip
}

func (n *Network) NdotsSet() bool {
	return false
}

func (n *Network) Ndots() int {
	return 0
}

// config-only network is looked up by name
func (c *Controller) getConfigNetwork(name string) (*Network, error) {
	var n *Network
	c.WalkNetworks(func(current *Network) bool {
		if current.ConfigOnly() && current.Name() == name {
			n = current
			return true
		}
		return false
	})

	if n == nil {
		return nil, types.NotFoundErrorf("configuration network %q not found", name)
	}

	return n, nil
}

func (n *Network) lbSandboxName() string {
	name := "lb-" + n.name
	if n.ingress {
		name = n.name + "-sbox"
	}
	return name
}

func (n *Network) lbEndpointName() string {
	return n.name + "-endpoint"
}

func (n *Network) createLoadBalancerSandbox() (retErr error) {
	sandboxName := n.lbSandboxName()
	// Mark the sandbox to be a load balancer
	sbOptions := []SandboxOption{OptionLoadBalancer(n.id)}
	if n.ingress {
		sbOptions = append(sbOptions, OptionIngress())
	}
	sb, err := n.ctrlr.NewSandbox(context.TODO(), sandboxName, sbOptions...)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			if e := n.ctrlr.SandboxDestroy(context.WithoutCancel(context.TODO()), sandboxName); e != nil {
				log.G(context.TODO()).Warnf("could not delete sandbox %s on failure on failure (%v): %v", sandboxName, retErr, e)
			}
		}
	}()

	endpointName := n.lbEndpointName()
	epOptions := []EndpointOption{
		CreateOptionIPAM(n.loadBalancerIP, nil, nil),
		CreateOptionLoadBalancer(),
		CreateOptionDisableResolution(),
	}
	ep, err := n.createEndpoint(context.TODO(), endpointName, epOptions...)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			if e := ep.Delete(context.WithoutCancel(context.TODO()), true); e != nil {
				log.G(context.TODO()).Warnf("could not delete endpoint %s on failure on failure (%v): %v", endpointName, retErr, e)
			}
		}
	}()

	if err := ep.Join(context.TODO(), sb, nil); err != nil {
		return err
	}

	return sb.EnableService()
}

func (n *Network) deleteLoadBalancerSandbox() error {
	n.mu.Lock()
	c := n.ctrlr
	name := n.name
	n.mu.Unlock()

	sandboxName := n.lbSandboxName()
	endpointName := n.lbEndpointName()

	endpoint, err := n.EndpointByName(endpointName)
	if err != nil {
		log.G(context.TODO()).Warnf("Failed to find load balancer endpoint %s on network %s: %v", endpointName, name, err)
	} else {
		info := endpoint.Info()
		if info != nil {
			sb := info.Sandbox()
			if sb != nil {
				if err := sb.DisableService(); err != nil {
					log.G(context.TODO()).Warnf("Failed to disable service on sandbox %s: %v", sandboxName, err)
					// Ignore error and attempt to delete the load balancer endpoint
				}
			}
		}

		if err := endpoint.Delete(context.TODO(), true); err != nil {
			log.G(context.TODO()).Warnf("Failed to delete endpoint %s (%s) in %s: %v", endpoint.Name(), endpoint.ID(), sandboxName, err)
			// Ignore error and attempt to delete the sandbox.
		}
	}

	if err := c.SandboxDestroy(context.TODO(), sandboxName); err != nil {
		return fmt.Errorf("Failed to delete %s sandbox: %v", sandboxName, err)
	}
	return nil
}

func (n *Network) IPAMStatus(ctx context.Context) (network.IPAMStatus, error) {
	status := network.IPAMStatus{
		Subnets: make(map[netip.Prefix]network.SubnetStatus),
	}

	if n.hasSpecialDriver() {
		// Special drivers do not assign addresses from IPAM
		return status, nil
	}

	ipamdriver, _, err := n.getController().getIPAMDriver(n.ipamType)
	if err != nil {
		return status, err
	}
	ipam, ok := ipamdriver.(ipamapi.PoolStatuser)
	if !ok {
		return status, nil
	}

	var errs []error
	info4, info6 := n.IpamInfo()
	for info := range iterutil.Chain(slices.Values(info4), slices.Values(info6)) {
		pstat, err := ipam.PoolStatus(info.PoolID)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to retrieve pool %s status: %w", info.PoolID, err))
			continue
		}
		prefix, _ := netiputil.ToPrefix(info.Pool)
		status.Subnets[prefix] = pstat
	}

	return status, errors.Join(errs...)
}
