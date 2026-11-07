package bridge

import (
	"context"
	"fmt"
	"iter"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/internal/netiputil"
	"github.com/moby/moby/v2/daemon/internal/otelutil"
	"github.com/moby/moby/v2/daemon/internal/rootless"
	"github.com/moby/moby/v2/daemon/internal/stringid"
	"github.com/moby/moby/v2/daemon/libnetwork/datastore"
	"github.com/moby/moby/v2/daemon/libnetwork/driverapi"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge/internal/firewaller"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge/internal/iptabler"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge/internal/nftabler"
	"github.com/moby/moby/v2/daemon/libnetwork/drvregistry"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/nftables"
	"github.com/moby/moby/v2/daemon/libnetwork/iptables"
	"github.com/moby/moby/v2/daemon/libnetwork/netlabel"
	"github.com/moby/moby/v2/daemon/libnetwork/netutils"
	"github.com/moby/moby/v2/daemon/libnetwork/nlwrap"
	"github.com/moby/moby/v2/daemon/libnetwork/ns"
	"github.com/moby/moby/v2/daemon/libnetwork/options"
	"github.com/moby/moby/v2/daemon/libnetwork/portmapperapi"
	"github.com/moby/moby/v2/daemon/libnetwork/scope"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/moby/moby/v2/errdefs"
	"github.com/moby/moby/v2/internal/iterutil"
	"github.com/moby/moby/v2/internal/sliceutil"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	NetworkType                = "bridge"
	vethPrefix                 = "veth"
	vethLen                    = len(vethPrefix) + 7
	defaultContainerVethPrefix = "eth"
)

const (
	// DefaultGatewayV4AuxKey represents the default-gateway configured by the user
	DefaultGatewayV4AuxKey = "DefaultGatewayIPv4"
	// DefaultGatewayV6AuxKey represents the ipv6 default-gateway configured by the user
	DefaultGatewayV6AuxKey = "DefaultGatewayIPv6"
)

const spanPrefix = "libnetwork.drivers.bridge"

// Configuration info for the "bridge" driver.
type Configuration struct {
	EnableIPForwarding       bool
	DisableFilterForwardDrop bool
	EnableIPTables           bool
	EnableIP6Tables          bool
	// EnableProxy indicates whether the userland proxy should be used for NAT
	// port-mappings that can't be fulfilled with firewall rules alone. This
	// must not be true if ProxyPath is empty.
	EnableProxy        bool
	ProxyPath          string
	AllowDirectRouting bool
	AcceptFwMark       string
}

// networkConfiguration for network specific configuration
type networkConfiguration struct {
	ID                    string
	BridgeName            string
	EnableIPv4            bool
	EnableIPv6            bool
	EnableIPMasquerade    bool
	GwModeIPv4            gwMode
	GwModeIPv6            gwMode
	EnableICC             bool
	TrustedHostInterfaces []string // Interface names must not contain ':' characters
	InhibitIPv4           bool
	Mtu                   int
	DefaultBindingIP      net.IP
	DefaultBridge         bool
	HostIPv4              net.IP
	HostIPv6              net.IP
	ContainerIfacePrefix  string
	// Internal fields set after ipam data parsing
	AddressIPv4        *net.IPNet
	AddressIPv6        *net.IPNet
	DefaultGatewayIPv4 net.IP
	DefaultGatewayIPv6 net.IP
	dbIndex            uint64
	dbExists           bool
	Internal           bool

	BridgeIfaceCreator ifaceCreator
}

// ifaceCreator represents how the bridge interface was created
type ifaceCreator int8

const (
	ifaceCreatorUnknown ifaceCreator = iota
	ifaceCreatedByLibnetwork
	ifaceCreatedByUser
)

// containerConfiguration represents the user-specified configuration for a container
type containerConfiguration struct {
	ParentEndpoints []string
	ChildEndpoints  []string
}

// connectivityConfiguration represents the user-specified configuration regarding the external connectivity
type connectivityConfiguration struct {
	PortBindings []portmapperapi.PortBindingReq
	ExposedPorts []types.TransportPort
}

type bridgeEndpoint struct {
	id              string
	nid             string
	srcName         string
	addr            *net.IPNet
	addrv6          *net.IPNet
	macAddress      net.HardwareAddr
	containerConfig *containerConfiguration
	extConnConfig   *connectivityConfiguration
	portMapping     []portmapperapi.PortBinding // Operational port bindings, persisted for live-restore.
	// ephemeralPortMapping holds operational port bindings that must not be
	// persisted - they're re-created from another source of truth after a restart
	// (e.g. Swarm ingress ports, re-derived from the cluster). They're treated
	// exactly like portMapping for all live operations (firewalld reload, release
	// on leave, etc.), just not written to the store.
	ephemeralPortMapping []portmapperapi.PortBinding
	portBindingState     portBindingMode // Not persisted, even on live-restore port mappings are re-created.
	dbIndex              uint64
	dbExists             bool
}

// allPortBindings returns an iterator over the endpoint's persisted and
// ephemeral operational port bindings. Callers that only read the bindings (to
// reprogram or release them) should use this; callers that mutate the set must
// operate on portMapping and ephemeralPortMapping directly.
func (ep *bridgeEndpoint) allPortBindings() iter.Seq[portmapperapi.PortBinding] {
	return iterutil.Chain(slices.Values(ep.ephemeralPortMapping), slices.Values(ep.portMapping))
}

type bridgeNetwork struct {
	id                string
	bridge            *bridgeInterface // The bridge's L3 interface
	config            *networkConfiguration
	endpoints         map[string]*bridgeEndpoint // key: endpoint id
	driver            *driver                    // The network's driver
	firewallerNetwork firewaller.Network
	sync.Mutex
}

type driver struct {
	config        Configuration
	networks      map[string]*bridgeNetwork
	store         *datastore.Store
	nlh           nlwrap.Handle
	configNetwork sync.Mutex
	firewaller    firewaller.Firewaller
	portmappers   *drvregistry.PortMappers
	// mu is used to protect accesses to config and networks. Do not hold this lock while locking configNetwork.
	mu sync.Mutex
}

// Assert that the driver is a driverapi.IPv6Releaser.
var _ driverapi.IPv6Releaser = (*driver)(nil)

type gwMode string

const (
	gwModeDefault   gwMode = ""
	gwModeNAT       gwMode = "nat"
	gwModeNATUnprot gwMode = "nat-unprotected"
	gwModeRouted    gwMode = "routed"
	gwModeIsolated  gwMode = "isolated"
)

// New constructs a new bridge driver
func newDriver(store *datastore.Store, config Configuration, pms *drvregistry.PortMappers) (*driver, error) {
	fw, err := newFirewaller(context.Background(), firewaller.Config{
		IPv4:               config.EnableIPTables,
		IPv6:               config.EnableIP6Tables,
		Hairpin:            !config.EnableProxy,
		AllowDirectRouting: config.AllowDirectRouting,
		WSL2Mirrored:       isRunningUnderWSL2MirroredMode(context.Background()),
	})
	if err != nil {
		return nil, err
	}

	d := &driver{
		store:       store,
		config:      config,
		nlh:         ns.NlHandle(),
		networks:    map[string]*bridgeNetwork{},
		firewaller:  fw,
		portmappers: pms,
	}

	if err := d.initStore(); err != nil {
		return nil, err
	}

	iptables.OnReloaded(d.handleFirewalldReload)

	return d, nil
}

func runInNetNS(f func() error) error {
	if rootless.RunningWithRootlessKit() {
		if detachedNetNS, err := rootless.DetachedNetNS(); err != nil {
			return err
		} else if detachedNetNS != "" {
			return rootless.RunInNetNS(detachedNetNS, f)
		}
	}
	return f()
}

// Register registers a new instance of bridge driver.
func Register(r driverapi.Registerer, store *datastore.Store, pms *drvregistry.PortMappers, config Configuration) error {
	var d *driver
	if err := runInNetNS(func() error {
		var err error
		d, err = newDriver(store, config, pms)
		return err
	}); err != nil {
		return err
	}
	return r.RegisterDriver(NetworkType, d, driverapi.Capability{
		DataScope:         scope.Local,
		ConnectivityScope: scope.Local,
	})
}

// The behaviour of previous implementations of bridge subnet prefix assignment
// is preserved here...
//
// The LL prefix, 'fe80::/64' can be used as an IPAM pool. Linux always assigns
// link-local addresses with this prefix. But, pool-assigned addresses are very
// unlikely to conflict.
//
// Don't allow a nonstandard LL subnet to overlap with 'fe80::/64'. For example,
// if the config asked for subnet prefix 'fe80::/80', the bridge and its
// containers would each end up with two LL addresses, Linux's '/64' and one from
// the IPAM pool claiming '/80'. Although the specified prefix length must not
// affect the host's determination of whether the address is on-link and to be
// added to the interface's Prefix List (RFC-5942), differing prefix lengths
// would be confusing and have been disallowed by earlier implementations of
// bridge address assignment.
func validateIPv6Subnet(addr netip.Prefix) error {
	if !addr.Addr().Is6() || addr.Addr().Is4In6() {
		return fmt.Errorf("'%s' is not a valid IPv6 subnet", addr)
	}
	if addr.Addr().IsMulticast() {
		return fmt.Errorf("multicast subnet '%s' is not allowed", addr)
	}
	if addr.Masked() != linkLocalPrefix && linkLocalPrefix.Overlaps(addr) {
		return fmt.Errorf("'%s' clashes with the Link-Local prefix 'fe80::/64'", addr)
	}
	return nil
}

// ValidateFixedCIDRV6 checks that val is an IPv6 address and prefix length that
// does not overlap with the link local subnet prefix 'fe80::/64'.
func ValidateFixedCIDRV6(val string) error {
	if val == "" {
		return nil
	}
	prefix, err := netip.ParsePrefix(val)
	if err == nil {
		err = validateIPv6Subnet(prefix)
	}
	return errdefs.InvalidParameter(errors.Wrap(err, "invalid fixed-cidr-v6"))
}

// Validate performs a static validation on the network configuration parameters.
// Whatever can be assessed a priori before attempting any programming.
func (ncfg *networkConfiguration) Validate() error {
	if ncfg.Mtu < 0 {
		return errdefs.InvalidParameter(fmt.Errorf("invalid MTU number: %d", ncfg.Mtu))
	}

	if ncfg.EnableIPv4 {
		// If IPv4 is enabled, AddressIPv4 must have been configured.
		if ncfg.AddressIPv4 == nil {
			return errdefs.System(errors.New("no IPv4 address was allocated for the bridge"))
		}
		// If default gw is specified, it must be part of bridge subnet
		if ncfg.DefaultGatewayIPv4 != nil {
			if !ncfg.AddressIPv4.Contains(ncfg.DefaultGatewayIPv4) {
				return errInvalidGateway
			}
		}
	}

	if ncfg.EnableIPv6 {
		// If IPv6 is enabled, AddressIPv6 must have been configured.
		if ncfg.AddressIPv6 == nil {
			return errdefs.System(errors.New("no IPv6 address was allocated for the bridge"))
		}
		// AddressIPv6 must be IPv6, and not overlap with the LL subnet prefix.
		addr, ok := netiputil.ToPrefix(ncfg.AddressIPv6)
		if !ok {
			return errdefs.InvalidParameter(fmt.Errorf("invalid IPv6 address '%s'", ncfg.AddressIPv6))
		}
		if err := validateIPv6Subnet(addr); err != nil {
			return errdefs.InvalidParameter(err)
		}
		// If a default gw is specified, it must belong to AddressIPv6's subnet
		if ncfg.DefaultGatewayIPv6 != nil && !ncfg.AddressIPv6.Contains(ncfg.DefaultGatewayIPv6) {
			return errInvalidGateway
		}
	}

	return nil
}

// Conflicts check if two NetworkConfiguration objects overlap
func (ncfg *networkConfiguration) Conflicts(o *networkConfiguration) error {
	if o == nil {
		return errors.New("same configuration")
	}

	// Also empty, because only one network with empty name is allowed
	if ncfg.BridgeName == o.BridgeName {
		return errors.New("networks have same bridge name")
	}

	// They must be in different subnets
	if (ncfg.AddressIPv4 != nil && o.AddressIPv4 != nil) &&
		(ncfg.AddressIPv4.Contains(o.AddressIPv4.IP) || o.AddressIPv4.Contains(ncfg.AddressIPv4.IP)) {
		return errors.New("networks have overlapping IPv4")
	}

	// They must be in different v6 subnets
	if (ncfg.AddressIPv6 != nil && o.AddressIPv6 != nil) &&
		(ncfg.AddressIPv6.Contains(o.AddressIPv6.IP) || o.AddressIPv6.Contains(ncfg.AddressIPv6.IP)) {
		return errors.New("networks have overlapping IPv6")
	}

	return nil
}

func (ncfg *networkConfiguration) fromLabels(labels map[string]string) error {
	var err error
	for label, value := range labels {
		switch label {
		case BridgeName:
			ncfg.BridgeName = value
		case netlabel.DriverMTU:
			if ncfg.Mtu, err = strconv.Atoi(value); err != nil {
				return parseErr(label, value, err.Error())
			}
		case netlabel.EnableIPv4:
			if ncfg.EnableIPv4, err = strconv.ParseBool(value); err != nil {
				return parseErr(label, value, err.Error())
			}
		case netlabel.EnableIPv6:
			if ncfg.EnableIPv6, err = strconv.ParseBool(value); err != nil {
				return parseErr(label, value, err.Error())
			}
		case EnableIPMasquerade:
			if ncfg.EnableIPMasquerade, err = strconv.ParseBool(value); err != nil {
				return parseErr(label, value, err.Error())
			}
		case IPv4GatewayMode:
			if ncfg.GwModeIPv4, err = newGwMode(value); err != nil {
				return parseErr(label, value, err.Error())
			}
		case IPv6GatewayMode:
			if ncfg.GwModeIPv6, err = newGwMode(value); err != nil {
				return parseErr(label, value, err.Error())
			}
		case EnableICC:
			if ncfg.EnableICC, err = strconv.ParseBool(value); err != nil {
				return parseErr(label, value, err.Error())
			}
		case TrustedHostInterfaces:
			ncfg.TrustedHostInterfaces = strings.FieldsFunc(value, func(r rune) bool { return r == ':' })
		case InhibitIPv4:
			if ncfg.InhibitIPv4, err = strconv.ParseBool(value); err != nil {
				return parseErr(label, value, err.Error())
			}
		case DefaultBridge:
			if ncfg.DefaultBridge, err = strconv.ParseBool(value); err != nil {
				return parseErr(label, value, err.Error())
			}
		case DefaultBindingIP:
			if ncfg.DefaultBindingIP = net.ParseIP(value); ncfg.DefaultBindingIP == nil {
				return parseErr(label, value, "nil ip")
			}
		case netlabel.ContainerIfacePrefix:
			ncfg.ContainerIfacePrefix = value
		case netlabel.HostIPv4:
			if ncfg.HostIPv4 = net.ParseIP(value); ncfg.HostIPv4 == nil {
				return parseErr(label, value, "nil ip")
			}
		case netlabel.HostIPv6:
			if ncfg.HostIPv6 = net.ParseIP(value); ncfg.HostIPv6 == nil {
				return parseErr(label, value, "nil ip")
			}
		}
	}

	return nil
}

func newGwMode(gwMode string) (gwMode, error) {
	switch gwMode {
	case "nat":
		return gwModeNAT, nil
	case "nat-unprotected":
		return gwModeNATUnprot, nil
	case "routed":
		return gwModeRouted, nil
	case "isolated":
		return gwModeIsolated, nil
	}
	return gwModeDefault, fmt.Errorf("unknown gateway mode %s", gwMode)
}

func (m gwMode) routed() bool {
	return m == gwModeRouted
}

func (m gwMode) unprotected() bool {
	return m == gwModeNATUnprot
}

func (m gwMode) isolated() bool {
	return m == gwModeIsolated
}

func parseErr(label, value, errString string) error {
	return types.InvalidParameterErrorf("failed to parse %s value: %v (%s)", label, value, errString)
}

func (n *bridgeNetwork) newFirewallerNetwork(ctx context.Context) (_ firewaller.Network, retErr error) {
	config4, err := makeNetworkConfigFam(n.config.HostIPv4, n.bridge.bridgeIPv4, n.gwMode(firewaller.IPv4))
	if err != nil {
		return nil, err
	}
	config6, err := makeNetworkConfigFam(n.config.HostIPv6, n.bridge.bridgeIPv6, n.gwMode(firewaller.IPv6))
	if err != nil {
		return nil, err
	}

	// A firewalld zone applies to an interface, not to an address family. So
	// the bridge goes in the docker zone if the daemon manages either family's
	// firewall rules, and is left for the user to place if it manages neither.
	if n.driver.config.EnableIPTables || n.driver.config.EnableIP6Tables {
		if err := iptables.AddInterfaceFirewalld(n.config.BridgeName); err != nil {
			return nil, err
		}
		defer func() {
			if retErr != nil {
				if err := iptables.DelInterfaceFirewalld(n.config.BridgeName); err != nil {
					log.G(ctx).WithError(err).Errorf("failed to delete network level rules following error")
				}
			}
		}()
	}

	return n.driver.firewaller.NewNetwork(ctx, firewaller.NetworkConfig{
		IfName:                n.config.BridgeName,
		Internal:              n.config.Internal,
		ICC:                   n.config.EnableICC,
		Masquerade:            n.config.EnableIPMasquerade,
		TrustedHostInterfaces: n.config.TrustedHostInterfaces,
		AcceptFwMark:          n.driver.config.AcceptFwMark,
		Config4:               config4,
		Config6:               config6,
	})
}

func makeNetworkConfigFam(hostIP net.IP, bridgePrefix *net.IPNet, gwm gwMode) (firewaller.NetworkConfigFam, error) {
	c := firewaller.NetworkConfigFam{
		Routed:      gwm.routed(),
		Unprotected: gwm.unprotected(),
	}
	if hostIP != nil {
		var ok bool
		c.HostIP, ok = netip.AddrFromSlice(hostIP)
		if !ok {
			return firewaller.NetworkConfigFam{}, fmt.Errorf("invalid host address for pktFilter %q", hostIP)
		}
		c.HostIP = c.HostIP.Unmap()
	}
	if bridgePrefix != nil {
		p, ok := netiputil.ToPrefix(bridgePrefix)
		if !ok {
			return firewaller.NetworkConfigFam{}, fmt.Errorf("invalid bridge prefix for pktFilter %s", bridgePrefix)
		}
		c.Prefix = p.Masked()
	}
	return c, nil
}

func (n *bridgeNetwork) getNATDisabled() (ipv4, ipv6 bool) {
	n.Lock()
	defer n.Unlock()
	return n.config.GwModeIPv4.routed(), n.config.GwModeIPv6.routed()
}

func (n *bridgeNetwork) gwMode(v firewaller.IPVersion) gwMode {
	n.Lock()
	defer n.Unlock()
	if v == firewaller.IPv4 {
		return n.config.GwModeIPv4
	}
	return n.config.GwModeIPv6
}

func (n *bridgeNetwork) portMappers() *drvregistry.PortMappers {
	n.Lock()
	defer n.Unlock()
	if n.driver == nil {
		return nil
	}
	return n.driver.portmappers
}

func (n *bridgeNetwork) getEndpoint(eid string) (*bridgeEndpoint, error) {
	if eid == "" {
		return nil, invalidEndpointIDError(eid)
	}

	n.Lock()
	defer n.Unlock()
	if ep, ok := n.endpoints[eid]; ok {
		return ep, nil
	}

	return nil, nil
}

var newFirewaller = func(ctx context.Context, config firewaller.Config) (firewaller.Firewaller, error) {
	if nftables.Enabled() {
		fw, err := nftabler.NewNftabler(ctx, config)
		if err != nil {
			return nil, err
		}
		// Without seeing config (interface names, addresses, and so on), the iptabler's
		// cleaner can't clean up network or port-specific rules that may have been added
		// to iptables built-in chains. So, if cleanup is needed, give the cleaner to
		// the nftabler. Then, it'll use it to delete old rules as networks are restored.
		fw.SetFirewallCleaner(iptabler.NewCleaner(ctx, config))
		return fw, nil
	}

	// The nftabler can clean all of its rules in one go. So, even if there's cleanup
	// to do, there's no need to pass a cleaner to the iptabler.
	nftabler.Cleanup(ctx, config)
	return iptabler.NewIptabler(ctx, config)
}

func (d *driver) getNetwork(id string) (*bridgeNetwork, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if id == "" {
		return nil, types.InvalidParameterErrorf("invalid network id: %s", id)
	}

	if nw, ok := d.networks[id]; ok {
		return nw, nil
	}

	return nil, types.NotFoundErrorf("network not found: %s", id)
}

func parseNetworkGenericOptions(data any) (*networkConfiguration, error) {
	var (
		err    error
		config *networkConfiguration
	)

	switch opt := data.(type) {
	case *networkConfiguration:
		config = opt
	case map[string]string:
		config = &networkConfiguration{
			EnableICC:          true,
			EnableIPMasquerade: true,
		}
		err = config.fromLabels(opt)
	default:
		err = types.InvalidParameterErrorf("do not recognize network configuration format: %T", opt)
	}

	return config, err
}

func (ncfg *networkConfiguration) processIPAM(ipamV4Data, ipamV6Data []driverapi.IPAMData) error {
	if len(ipamV4Data) > 1 || len(ipamV6Data) > 1 {
		return types.ForbiddenErrorf("bridge driver doesn't support multiple subnets")
	}

	if len(ipamV4Data) > 0 {
		ncfg.AddressIPv4 = ipamV4Data[0].Pool

		if ipamV4Data[0].Gateway != nil {
			ncfg.AddressIPv4 = types.GetIPNetCopy(ipamV4Data[0].Gateway)
		}

		if gw, ok := ipamV4Data[0].AuxAddresses[DefaultGatewayV4AuxKey]; ok {
			ncfg.DefaultGatewayIPv4 = gw.IP
		}
	}

	if len(ipamV6Data) > 0 {
		ncfg.AddressIPv6 = ipamV6Data[0].Pool

		if ipamV6Data[0].Gateway != nil {
			ncfg.AddressIPv6 = types.GetIPNetCopy(ipamV6Data[0].Gateway)
		}

		if gw, ok := ipamV6Data[0].AuxAddresses[DefaultGatewayV6AuxKey]; ok {
			ncfg.DefaultGatewayIPv6 = gw.IP
		}
	}

	return nil
}

func parseNetworkOptions(id string, option options.Generic) (*networkConfiguration, error) {
	var (
		err    error
		config = &networkConfiguration{}
	)

	// Parse generic label first, config will be re-assigned
	if genData, ok := option[netlabel.GenericData]; ok && genData != nil {
		if config, err = parseNetworkGenericOptions(genData); err != nil {
			return nil, err
		}
	}

	// Process well-known labels next
	if val, ok := option[netlabel.EnableIPv4]; ok {
		config.EnableIPv4 = val.(bool)
	}
	if val, ok := option[netlabel.EnableIPv6]; ok {
		config.EnableIPv6 = val.(bool)
	}

	if val, ok := option[netlabel.Internal]; ok {
		if internal, ok := val.(bool); ok && internal {
			config.Internal = true
		}
	}

	if config.BridgeName == "" && !config.DefaultBridge {
		config.BridgeName = "br-" + id[:12]
	}

	exists, err := bridgeInterfaceExists(config.BridgeName)
	if err != nil {
		return nil, err
	}

	if (config.GwModeIPv4.isolated() || config.GwModeIPv6.isolated()) && !config.Internal {
		return nil, errors.New("gateway mode 'isolated' can only be used for an internal network")
	}

	if !exists {
		config.BridgeIfaceCreator = ifaceCreatedByLibnetwork
	} else {
		config.BridgeIfaceCreator = ifaceCreatedByUser
	}

	config.ID = id
	return config, nil
}

// Return a slice of networks over which caller can iterate safely
func (d *driver) getNetworks() []*bridgeNetwork {
	d.mu.Lock()
	defer d.mu.Unlock()

	ls := make([]*bridgeNetwork, 0, len(d.networks))
	for _, nw := range d.networks {
		ls = append(ls, nw)
	}
	return ls
}

func (d *driver) GetSkipGwAlloc(opts options.Generic) (ipv4, ipv6 bool, _ error) {
	// The network doesn't exist yet, so use a dummy id that's long enough to be
	// truncated to a short-id (12 characters) and used in the bridge device name.
	cfg, err := parseNetworkOptions("dummyNetworkId", opts)
	if err != nil {
		return false, false, err
	}
	// An isolated network should not have a gateway. Also, cfg.InhibitIPv4 means no
	// gateway address will be assigned to the bridge. So, if the network is also
	// cfg.Internal, there will not be a default route to use the gateway address.
	ipv4 = cfg.GwModeIPv4.isolated() || (cfg.InhibitIPv4 && cfg.Internal)
	ipv6 = cfg.GwModeIPv6.isolated()
	return ipv4, ipv6, nil
}

// CreateNetwork creates a new network using the bridge driver.
func (d *driver) CreateNetwork(ctx context.Context, id string, option map[string]any, nInfo driverapi.NetworkInfo, ipV4Data, ipV6Data []driverapi.IPAMData) error {
	// Sanity checks
	d.mu.Lock()
	if _, ok := d.networks[id]; ok {
		d.mu.Unlock()
		return types.ForbiddenErrorf("network %s exists", id)
	}
	d.mu.Unlock()

	// Parse the config.
	config, err := parseNetworkOptions(id, option)
	if err != nil {
		return err
	}

	if !config.EnableIPv4 && !config.EnableIPv6 {
		return types.InvalidParameterErrorf("IPv4 or IPv6 must be enabled")
	}
	if config.EnableIPv4 && (len(ipV4Data) == 0 || ipV4Data[0].Pool.String() == "0.0.0.0/0") {
		return types.InvalidParameterErrorf("ipv4 pool is empty")
	}
	if config.EnableIPv6 && (len(ipV6Data) == 0 || ipV6Data[0].Pool.String() == "::/0") {
		return types.InvalidParameterErrorf("ipv6 pool is empty")
	}

	// Add IP addresses/gateways to the configuration.
	if err = config.processIPAM(ipV4Data, ipV6Data); err != nil {
		return err
	}

	// Validate the configuration
	if err = config.Validate(); err != nil {
		return err
	}

	// start the critical section, from this point onward we are dealing with the list of networks
	// so to be consistent we cannot allow that the list changes
	d.configNetwork.Lock()
	defer d.configNetwork.Unlock()

	// check network conflicts
	if err = d.checkConflict(config); err != nil {
		return err
	}

	// there is no conflict, now create the network
	if err = d.createNetwork(ctx, config); err != nil {
		return err
	}

	return d.storeUpdate(ctx, config)
}

func (d *driver) checkConflict(config *networkConfiguration) error {
	networkList := d.getNetworks()
	for _, nw := range networkList {
		nw.Lock()
		nwConfig := nw.config
		nw.Unlock()
		if err := nwConfig.Conflicts(config); err != nil {
			return types.ForbiddenErrorf("cannot create network %s (%s): conflicts with network %s (%s): %s",
				config.ID, config.BridgeName, nwConfig.ID, nwConfig.BridgeName, err.Error())
		}
	}
	return nil
}

func (d *driver) createNetwork(ctx context.Context, config *networkConfiguration) (retErr error) {
	ctx, span := otel.Tracer("").Start(ctx, spanPrefix+".createNetwork", trace.WithAttributes(
		attribute.Bool("bridge.enable_ipv4", config.EnableIPv4),
		attribute.Bool("bridge.enable_ipv6", config.EnableIPv6),
		attribute.Bool("bridge.icc", config.EnableICC),
		attribute.Int("bridge.mtu", config.Mtu),
		attribute.Bool("bridge.internal", config.Internal)))
	defer func() {
		otelutil.RecordStatus(span, retErr)
		span.End()
	}()

	// Create or retrieve the bridge L3 interface
	bridgeIface, err := newInterface(d.nlh, config)
	if err != nil {
		return err
	}

	// Create and set network handler in driver
	network := &bridgeNetwork{
		id:        config.ID,
		endpoints: make(map[string]*bridgeEndpoint),
		config:    config,
		bridge:    bridgeIface,
		driver:    d,
	}

	d.mu.Lock()
	d.networks[config.ID] = network
	d.mu.Unlock()

	// On failure, clean up.
	defer func() {
		if retErr != nil {
			if err := d.deleteNetwork(config.ID); err != nil && !errors.Is(err, datastore.ErrKeyNotFound) {
				log.G(ctx).WithFields(log.Fields{
					"network": stringid.TruncateID(config.ID),
					"error":   err,
					"retErr":  retErr,
				}).Errorf("Error while cleaning up after network create error")
			}
		}
	}()

	// Prepare the bridge setup configuration
	bridgeSetup := newBridgeSetup(config, bridgeIface)

	// If the bridge interface doesn't exist, we need to start the setup steps
	// by creating a new device and assigning it an IPv4 address.
	bridgeAlreadyExists := bridgeIface.exists()
	if !bridgeAlreadyExists {
		bridgeSetup.queueStep("setupDevice", setupDevice)
		bridgeSetup.queueStep("setupDefaultSysctl", setupDefaultSysctl)
	}

	// For the default bridge, set expected sysctls
	if config.DefaultBridge {
		bridgeSetup.queueStep("setupDefaultSysctl", setupDefaultSysctl)
	}

	// Always set the bridge's MTU if specified. This is purely cosmetic; a bridge's
	// MTU is the min MTU of device connected to it, and MTU will be set on each
	// 'veth'. But, for a non-default MTU, the bridge's MTU will look wrong until a
	// container is attached.
	if config.Mtu > 0 {
		bridgeSetup.queueStep("setupMTU", setupMTU)
	}

	// Module br_netfilter needs to be loaded with net.bridge.bridge-nf-call-ip[6]tables
	// enabled to implement icc=false, or DNAT when the userland-proxy is disabled.
	enableBrNfCallIptables := !config.EnableICC || !d.config.EnableProxy

	// Conditionally queue setup steps depending on configuration values.
	for _, step := range []struct {
		Condition bool
		StepName  string
		StepFn    stepFn
	}{
		// Even if a bridge exists try to setup IPv4.
		{config.EnableIPv4, "setupBridgeIPv4", setupBridgeIPv4},

		// Enable IPv6 on the bridge if required. We do this even for a
		// previously  existing bridge, as it may be here from a previous
		// installation where IPv6 wasn't supported yet and needs to be
		// assigned an IPv6 link-local address.
		{config.EnableIPv6, "setupBridgeIPv6", setupBridgeIPv6},

		// Ensure the bridge has the expected IPv4 addresses in the case of a previously
		// existing device.
		{config.EnableIPv4 && bridgeAlreadyExists && !config.InhibitIPv4, "setupVerifyAndReconcileIPv4", setupVerifyAndReconcileIPv4},

		// Enable IP Forwarding
		{
			config.EnableIPv4 && d.config.EnableIPForwarding,
			"setupIPv4Forwarding",
			func(*networkConfiguration, *bridgeInterface) error {
				ffd, ok := d.firewaller.(filterForwardDropper)
				if !ok {
					// The firewaller can't drop non-Docker forwarding. It's up to the user to enable
					// forwarding on their host, and configure their firewall appropriately.
					return checkIPv4Forwarding()
				}
				// Enable forwarding and set a default-drop forwarding policy if necessary.
				return setupIPv4Forwarding(ffd, d.config.EnableIPTables && !d.config.DisableFilterForwardDrop)
			},
		},
		{
			config.EnableIPv6 && d.config.EnableIPForwarding,
			"setupIPv6Forwarding",
			func(*networkConfiguration, *bridgeInterface) error {
				ffd, ok := d.firewaller.(filterForwardDropper)
				if !ok {
					// The firewaller can't drop non-Docker forwarding. It's up to the user to enable
					// forwarding on their host, and configure their firewall appropriately.
					return checkIPv6Forwarding()
				}
				// Enable forwarding and set a default-drop forwarding policy if necessary.
				return setupIPv6Forwarding(ffd, d.config.EnableIP6Tables && !d.config.DisableFilterForwardDrop)
			},
		},

		// Setup Loopback Addresses Routing
		{!d.config.EnableProxy, "setupLoopbackAddressesRouting", setupLoopbackAddressesRouting},

		// Setup DefaultGatewayIPv4
		{config.DefaultGatewayIPv4 != nil, "setupGatewayIPv4", setupGatewayIPv4},

		// Setup DefaultGatewayIPv6
		{config.DefaultGatewayIPv6 != nil, "setupGatewayIPv6", setupGatewayIPv6},

		// Configure bridge networking filtering if needed and IP tables are enabled
		{enableBrNfCallIptables && d.config.EnableIPTables, "setupIPv4BridgeNetFiltering", setupIPv4BridgeNetFiltering},
		{enableBrNfCallIptables && d.config.EnableIP6Tables, "setupIPv6BridgeNetFiltering", setupIPv6BridgeNetFiltering},
	} {
		if step.Condition {
			bridgeSetup.queueStep(step.StepName, step.StepFn)
		}
	}

	bridgeSetup.queueStep("addfirewallerNetwork", func(*networkConfiguration, *bridgeInterface) error {
		n, err := network.newFirewallerNetwork(ctx)
		if err != nil {
			return err
		}
		network.firewallerNetwork = n
		return nil
	})

	// Apply the prepared list of steps, and abort at the first error.
	bridgeSetup.queueStep("setupDeviceUp", setupDeviceUp)

	if v := os.Getenv("DOCKER_TEST_BRIDGE_INIT_ERROR"); v == config.BridgeName {
		bridgeSetup.queueStep("fakeError", func(n *networkConfiguration, b *bridgeInterface) error {
			return fmt.Errorf("DOCKER_TEST_BRIDGE_INIT_ERROR is %q", v)
		})
	}

	return bridgeSetup.apply(ctx)
}

func (d *driver) DeleteNetwork(nid string) error {
	d.configNetwork.Lock()
	defer d.configNetwork.Unlock()

	return d.deleteNetwork(nid)
}

func (d *driver) deleteNetwork(nid string) error {
	var err error

	// Get network handler and remove it from driver
	d.mu.Lock()
	n, ok := d.networks[nid]
	d.mu.Unlock()

	if !ok {
		// If the network was successfully created by an earlier incarnation of the daemon,
		// but it failed to initialise this time, the network is still in the store (in
		// case whatever caused the failure can be fixed for a future daemon restart). But,
		// it's not in d.networks. To prevent the driver's state from getting out of step
		// with its parent, make sure it's not in the store before reporting that it does
		// not exist.
		if err := d.storeDelete(&networkConfiguration{ID: nid}); err != nil && !errors.Is(err, datastore.ErrKeyNotFound) {
			log.G(context.TODO()).WithFields(log.Fields{
				"error":   err,
				"network": nid,
			}).Warnf("Failed to delete network from bridge store")
		}
		return types.InternalMaskableErrorf("network %s does not exist", nid)
	}

	n.Lock()
	config := n.config
	n.Unlock()

	// delete endpoints belong to this network
	for _, ep := range n.endpoints {
		if err := n.releasePorts(ep); err != nil {
			log.G(context.TODO()).Warn(err)
		}
		if link, err := d.nlh.LinkByName(ep.srcName); err == nil {
			if err := d.nlh.LinkDel(link); err != nil {
				log.G(context.TODO()).WithError(err).Errorf("Failed to delete interface (%s)'s link on endpoint (%s) delete", ep.srcName, ep.id)
			}
		}

		if err := d.storeDelete(ep); err != nil {
			log.G(context.TODO()).Warnf("Failed to remove bridge endpoint %.7s from store: %v", ep.id, err)
		}
	}

	d.mu.Lock()
	delete(d.networks, nid)
	d.mu.Unlock()

	// On failure set network handler back in driver, but
	// only if is not already taken over by some other thread
	defer func() {
		if err != nil {
			d.mu.Lock()
			if _, ok := d.networks[nid]; !ok {
				d.networks[nid] = n
			}
			d.mu.Unlock()
		}
	}()

	switch config.BridgeIfaceCreator {
	case ifaceCreatedByLibnetwork, ifaceCreatorUnknown:
		// We only delete the bridge if it was created by the bridge driver and
		// it is not the default one (to keep the backward compatible behavior.)
		if !config.DefaultBridge && n.bridge != nil && n.bridge.Link != nil {
			if err := d.nlh.LinkDel(n.bridge.Link); err != nil {
				log.G(context.TODO()).Warnf("Failed to remove bridge interface %s on network %s delete: %v", config.BridgeName, nid, err)
			}
		}
	case ifaceCreatedByUser:
		// Don't delete the bridge interface if it was not created by libnetwork.
	}

	if n.firewallerNetwork != nil {
		if err := n.firewallerNetwork.DelNetworkLevelRules(context.TODO()); err != nil {
			log.G(context.TODO()).WithError(err).Warnf("Failed to clean iptables rules for bridge network")
		}
	}
	// Remove the bridge from the docker zone even if iptables is disabled, in
	// case it was added when the daemon ran with iptables enabled. It may not be
	// there: the daemon leaves it out when iptables is disabled, and the user
	// may have put it in another zone.
	if err := iptables.DelInterfaceFirewalld(n.config.BridgeName); err != nil && !cerrdefs.IsNotFound(err) {
		log.G(context.TODO()).WithError(err).Warnf("Failed to clean firewalld rules for bridge network")
	}

	return d.storeDelete(config)
}

func addToBridge(ctx context.Context, nlh nlwrap.Handle, ifaceName, bridgeName string) error {
	ctx, span := otel.Tracer("").Start(ctx, spanPrefix+".addToBridge", trace.WithAttributes(
		attribute.String("ifaceName", ifaceName),
		attribute.String("bridgeName", bridgeName)))
	defer span.End()

	lnk, err := nlh.LinkByName(ifaceName)
	if err != nil {
		return fmt.Errorf("could not find interface %s: %v", ifaceName, err)
	}
	if err := nlh.LinkSetMaster(lnk, &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: bridgeName}}); err != nil {
		log.G(ctx).WithError(err).Errorf("Failed to add %s to bridge via netlink", ifaceName)
		return err
	}
	return nil
}

func setHairpinMode(nlh nlwrap.Handle, link netlink.Link, enable bool) error {
	err := nlh.LinkSetHairpin(link, enable)
	if err != nil {
		return fmt.Errorf("unable to set hairpin mode on %s via netlink: %v",
			link.Attrs().Name, err)
	}
	return nil
}

func (d *driver) CreateEndpoint(ctx context.Context, nid, eid string, ifInfo driverapi.InterfaceInfo, _ map[string]any) error {
	if ifInfo == nil {
		return errors.New("invalid interface info passed")
	}

	ctx, span := otel.Tracer("").Start(ctx, spanPrefix+".CreateEndpoint", trace.WithAttributes(
		attribute.String("nid", nid),
		attribute.String("eid", eid)))
	defer span.End()

	// Get the network handler and make sure it exists
	d.mu.Lock()
	n, ok := d.networks[nid]
	d.mu.Unlock()

	if !ok {
		return types.NotFoundErrorf("network %s does not exist", nid)
	}
	if n == nil {
		return driverapi.ErrNoNetwork(nid)
	}

	// Sanity check
	n.Lock()
	if n.id != nid {
		n.Unlock()
		return invalidNetworkIDError(nid)
	}
	n.Unlock()

	// Check if endpoint id is good and retrieve correspondent endpoint
	ep, err := n.getEndpoint(eid)
	if err != nil {
		return err
	}

	// Endpoint with that id exists either on desired or other sandbox
	if ep != nil {
		return driverapi.ErrEndpointExists(eid)
	}

	// Create and add the endpoint
	n.Lock()
	endpoint := &bridgeEndpoint{id: eid, nid: nid}
	n.endpoints[eid] = endpoint
	n.Unlock()

	// On failure make sure to remove the endpoint
	defer func() {
		if err != nil {
			n.Lock()
			delete(n.endpoints, eid)
			n.Unlock()
		}
	}()

	// Generate a name for what will be the host side pipe interface
	hostIfName, err := netutils.GenerateIfaceName(d.nlh, vethPrefix, vethLen)
	if err != nil {
		return err
	}

	// Generate a name for what will be the sandbox side pipe interface
	containerIfName, err := netutils.GenerateIfaceName(d.nlh, vethPrefix, vethLen)
	if err != nil {
		return err
	}

	// Generate and add the interface pipe host <-> sandbox
	nlhSb := d.nlh
	if nlh, err := createVeth(ctx, hostIfName, containerIfName, ifInfo, d.nlh); err != nil {
		return err
	} else if nlh != nil {
		defer nlh.Close()
		nlhSb = *nlh
	}

	// Get the host side pipe interface handler
	host, err := d.nlh.LinkByName(hostIfName)
	if err != nil {
		return types.InternalErrorf("failed to find host side interface %s: %v", hostIfName, err)
	}
	defer func() {
		if err != nil {
			if err := d.nlh.LinkDel(host); err != nil {
				log.G(ctx).WithError(err).Warnf("Failed to delete host side interface (%s)'s link", hostIfName)
			}
		}
	}()

	// Get the sandbox side pipe interface handler
	sbox, err := nlhSb.LinkByName(containerIfName)
	if err != nil {
		return types.InternalErrorf("failed to find sandbox side interface %s: %v", containerIfName, err)
	}
	defer func() {
		if err != nil {
			if err := nlhSb.LinkDel(sbox); err != nil {
				log.G(ctx).WithError(err).Warnf("Failed to delete sandbox side interface (%s)'s link", containerIfName)
			}
		}
	}()

	n.Lock()
	config := n.config
	n.Unlock()

	// Add bridge inherited attributes to pipe interfaces
	if config.Mtu != 0 {
		err = d.nlh.LinkSetMTU(host, config.Mtu)
		if err != nil {
			return types.InternalErrorf("failed to set MTU on host interface %s: %v", hostIfName, err)
		}
		err = nlhSb.LinkSetMTU(sbox, config.Mtu)
		if err != nil {
			return types.InternalErrorf("failed to set MTU on sandbox interface %s: %v", containerIfName, err)
		}
	}

	// Attach host side pipe interface into the bridge
	if err = addToBridge(ctx, d.nlh, hostIfName, config.BridgeName); err != nil {
		return fmt.Errorf("adding interface %s to bridge %s failed: %v", hostIfName, config.BridgeName, err)
	}

	if !d.config.EnableProxy {
		err = setHairpinMode(d.nlh, host, true)
		if err != nil {
			return err
		}
	}

	// Store the sandbox side pipe interface parameters
	endpoint.srcName = containerIfName
	endpoint.macAddress = ifInfo.MacAddress()
	endpoint.addr = ifInfo.Address()
	endpoint.addrv6 = ifInfo.AddressIPv6()

	if endpoint.macAddress == nil {
		endpoint.macAddress = netutils.GenerateRandomMAC()
		if err := ifInfo.SetMacAddress(endpoint.macAddress); err != nil {
			return err
		}
	}

	netip4, netip6 := endpoint.netipAddrs()
	if err := n.firewallerNetwork.AddEndpoint(ctx, netip4, netip6); err != nil {
		return err
	}

	// Up the host interface after finishing all netlink configuration
	if err = d.linkUp(ctx, host); err != nil {
		return fmt.Errorf("could not set link up for host interface %s: %v", hostIfName, err)
	}
	log.G(ctx).WithFields(log.Fields{
		"hostifname": host.Attrs().Name,
		"ifi":        host.Attrs().Index,
	}).Debug("bridge endpoint host link is up")

	if err = d.storeUpdate(ctx, endpoint); err != nil {
		return fmt.Errorf("failed to save bridge endpoint %.7s to store: %v", endpoint.id, err)
	}

	return nil
}

// netipAddrs converts ep.addr and ep.addrv6 from net.IPNet to netip.Addr. If an address
// is non-nil, it's assumed to be valid.
func (ep *bridgeEndpoint) netipAddrs() (v4, v6 netip.Addr) {
	if ep.addr != nil {
		v4, _ = netip.AddrFromSlice(ep.addr.IP)
		v4 = v4.Unmap()
	}
	if ep.addrv6 != nil {
		v6, _ = netip.AddrFromSlice(ep.addrv6.IP)
	}
	return v4, v6
}

// createVeth creates a veth device with one end in the container's network namespace,
// if it can get hold of the netns path and open the handles. In that case, it returns
// a netlink handle in the container's namespace that must be closed by the caller.
//
// If the netns path isn't available, possibly because the netns hasn't been created
// yet, or it's not possible to get a netns or netlink handle in the container's
// namespace - both ends of the veth device are created in nlh's netns, and no netlink
// handle is returned.
//
// (Only the error from creating the interface is returned. Failure to create the
// interface in the container's netns is not an error.)
func createVeth(ctx context.Context, hostIfName, containerIfName string, ifInfo driverapi.InterfaceInfo, nlh nlwrap.Handle) (nlhCtr *nlwrap.Handle, retErr error) {
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: hostIfName, TxQLen: 0},
		PeerName:  containerIfName,
	}

	if nspath := ifInfo.NetnsPath(); nspath == "" {
		log.G(ctx).WithField("ifname", containerIfName).Debug("No container netns path, creating interface in host netns")
	} else if netnsh, err := netns.GetFromPath(nspath); err != nil {
		log.G(ctx).WithFields(log.Fields{
			"error":  err,
			"netns":  nspath,
			"ifname": containerIfName,
		}).Warn("No container netns, creating interface in host netns")
	} else {
		defer netnsh.Close()

		if nh, err := nlwrap.NewHandleAt(netnsh, syscall.NETLINK_ROUTE); err != nil {
			log.G(ctx).WithFields(log.Fields{
				"error": err,
				"netns": nspath,
			}).Warn("No netlink handle for container, creating interface in host netns")
		} else {
			defer func() {
				if retErr != nil {
					nh.Close()
				}
			}()

			veth.PeerNamespace = netlink.NsFd(netnsh)
			nlhCtr = &nh
			ifInfo.SetCreatedInContainer(true)
		}
	}

	if err := nlh.LinkAdd(veth); err != nil {
		return nil, types.InternalErrorf("failed to add the host (%s) <=> sandbox (%s) pair interfaces: %v", hostIfName, containerIfName, err)
	}
	return nlhCtr, nil
}

func (d *driver) linkUp(ctx context.Context, host netlink.Link) error {
	ctx, span := otel.Tracer("").Start(ctx, spanPrefix+".linkUp", trace.WithAttributes(
		attribute.String("host", host.Attrs().Name)))
	defer span.End()

	return d.nlh.LinkSetUp(host)
}

func (d *driver) DeleteEndpoint(nid, eid string) error {
	var err error

	// Get the network handler and make sure it exists
	d.mu.Lock()
	n, ok := d.networks[nid]
	d.mu.Unlock()

	if !ok {
		return types.InternalMaskableErrorf("network %s does not exist", nid)
	}
	if n == nil {
		return driverapi.ErrNoNetwork(nid)
	}

	// Sanity Check
	n.Lock()
	if n.id != nid {
		n.Unlock()
		return invalidNetworkIDError(nid)
	}
	n.Unlock()

	// Check endpoint id and if an endpoint is actually there
	ep, err := n.getEndpoint(eid)
	if err != nil {
		return err
	}
	if ep == nil {
		return endpointNotFoundError(eid)
	}

	netip4, netip6 := ep.netipAddrs()
	if err := n.firewallerNetwork.DelEndpoint(context.TODO(), netip4, netip6); err != nil {
		return err
	}

	// Remove it
	n.Lock()
	delete(n.endpoints, eid)
	n.Unlock()

	// On failure make sure to set back ep in n.endpoints, but only
	// if it hasn't been taken over already by some other thread.
	defer func() {
		if err != nil {
			n.Lock()
			if _, ok := n.endpoints[eid]; !ok {
				n.endpoints[eid] = ep
			}
			n.Unlock()
		}
	}()

	// Try removal of link. Discard error: it is a best effort.
	// Also make sure defer does not see this error either.
	if link, err := d.nlh.LinkByName(ep.srcName); err == nil {
		if err := d.nlh.LinkDel(link); err != nil {
			log.G(context.TODO()).WithError(err).Errorf("Failed to delete interface (%s)'s link on endpoint (%s) delete", ep.srcName, ep.id)
		}
	}

	if err := d.storeDelete(ep); err != nil {
		log.G(context.TODO()).Warnf("Failed to remove bridge endpoint %.7s from store: %v", ep.id, err)
	}

	return nil
}

func (d *driver) EndpointOperInfo(nid, eid string) (map[string]any, error) {
	// Get the network handler and make sure it exists
	d.mu.Lock()
	n, ok := d.networks[nid]
	d.mu.Unlock()
	if !ok {
		return nil, types.NotFoundErrorf("network %s does not exist", nid)
	}
	if n == nil {
		return nil, driverapi.ErrNoNetwork(nid)
	}

	// Sanity check
	n.Lock()
	if n.id != nid {
		n.Unlock()
		return nil, invalidNetworkIDError(nid)
	}
	n.Unlock()

	// Check if endpoint id is good and retrieve correspondent endpoint
	ep, err := n.getEndpoint(eid)
	if err != nil {
		return nil, err
	}
	if ep == nil {
		return nil, driverapi.ErrNoEndpoint(eid)
	}

	m := make(map[string]any)

	if ep.extConnConfig != nil && ep.extConnConfig.ExposedPorts != nil {
		m[netlabel.ExposedPorts] = slices.Clone(ep.extConnConfig.ExposedPorts)
	}

	if ep.portMapping != nil {
		// Return a copy of the operational data
		pmc := make([]types.PortBinding, 0, len(ep.portMapping))
		for _, pm := range ep.portMapping {
			pmc = append(pmc, pm.PortBinding.Copy())
		}
		m[netlabel.PortMap] = pmc
	}

	if len(ep.macAddress) != 0 {
		m[netlabel.MacAddress] = ep.macAddress
	}

	return m, nil
}

// Join method is invoked when a Sandbox is attached to an endpoint.
func (d *driver) Join(ctx context.Context, nid, eid string, sboxKey string, jinfo driverapi.JoinInfo, epOpts, sbOpts map[string]any) error {
	ctx, span := otel.Tracer("").Start(ctx, spanPrefix+".Join", trace.WithAttributes(
		attribute.String("nid", nid),
		attribute.String("eid", eid),
		attribute.String("sboxKey", sboxKey)))
	defer span.End()

	network, err := d.getNetwork(nid)
	if err != nil {
		return err
	}

	endpoint, err := network.getEndpoint(eid)
	if err != nil {
		return err
	}

	if endpoint == nil {
		return endpointNotFoundError(eid)
	}

	endpoint.containerConfig, err = parseContainerOptions(sbOpts)
	if err != nil {
		return err
	}
	endpoint.extConnConfig, err = parseConnectivityOptions(sbOpts)
	if err != nil {
		return err
	}

	iNames := jinfo.InterfaceName()
	containerVethPrefix := defaultContainerVethPrefix
	if network.config.ContainerIfacePrefix != "" {
		containerVethPrefix = network.config.ContainerIfacePrefix
	}
	if err := iNames.SetNames(endpoint.srcName, containerVethPrefix, netlabel.GetIfname(epOpts)); err != nil {
		return err
	}

	if !network.config.Internal {
		if err := jinfo.SetGateway(network.bridge.gatewayIPv4); err != nil {
			return err
		}
		if err := jinfo.SetGatewayIPv6(network.bridge.gatewayIPv6); err != nil {
			return err
		}
	}

	if !network.config.EnableICC {
		return d.link(network, endpoint, true)
	}

	return nil
}

func (d *driver) ReleaseIPv6(ctx context.Context, nid, eid string) error {
	network, err := d.getNetwork(nid)
	if err != nil {
		return err
	}

	endpoint, err := network.getEndpoint(eid)
	if err != nil {
		return err
	}

	if endpoint == nil {
		return endpointNotFoundError(eid)
	}

	_, netip6 := endpoint.netipAddrs()
	if err := network.firewallerNetwork.DelEndpoint(ctx, netip.Addr{}, netip6); err != nil {
		return fmt.Errorf("removing firewall rules while releasing IPv6 address: %v", err)
	}
	endpoint.addrv6 = nil
	return nil
}

// Leave method is invoked when a Sandbox detaches from an endpoint.
func (d *driver) Leave(nid, eid string) error {
	network, err := d.getNetwork(nid)
	if err != nil {
		return types.InternalMaskableErrorf("%v", err)
	}

	endpoint, err := network.getEndpoint(eid)
	if err != nil {
		return err
	}

	if endpoint == nil {
		return endpointNotFoundError(eid)
	}

	// Ephemeral bindings must be released too - an endpoint can have them without
	// any persisted portMapping (the Swarm ingress load balancer's gateway
	// endpoint only ever has ephemeral ingress ports), and releasePorts is what
	// frees their host-port reservations, rules and proxies.
	if endpoint.portMapping != nil || endpoint.ephemeralPortMapping != nil {
		if err := network.releasePorts(endpoint); err != nil {
			return err
		}
		if err = d.storeUpdate(context.TODO(), endpoint); err != nil {
			return fmt.Errorf("during leave, failed to store bridge endpoint %.7s: %v", endpoint.id, err)
		}
	}

	if !network.config.EnableICC {
		if err = d.link(network, endpoint, false); err != nil {
			return err
		}
	}

	return nil
}

type portBindingMode struct {
	routed bool
	ipv4   bool
	ipv6   bool
}

// added returns the modes that are enabled in want but not already in m. It's
// used to work out which port binding modes need to be programmed when an
// endpoint's set of active modes changes from m to want.
func (m portBindingMode) added(want portBindingMode) portBindingMode {
	return portBindingMode{
		routed: want.routed && !m.routed,
		ipv4:   want.ipv4 && !m.ipv4,
		ipv6:   want.ipv6 && !m.ipv6,
	}
}

func (d *driver) ProgramExternalConnectivity(ctx context.Context, nid, eid string, gw4Id, gw6Id string) (retErr error) {
	ctx, span := otel.Tracer("").Start(ctx, spanPrefix+".ProgramExternalConnectivity", trace.WithAttributes(
		attribute.String("nid", nid),
		attribute.String("eid", eid),
		attribute.String("gw4", gw4Id),
		attribute.String("gw6", gw6Id)))
	defer span.End()

	// Make sure the network isn't deleted, or in the middle of a firewalld reload, while
	// updating its iptables rules.
	d.configNetwork.Lock()
	defer d.configNetwork.Unlock()

	network, err := d.getNetwork(nid)
	if err != nil {
		return err
	}

	endpoint, err := network.getEndpoint(eid)
	if err != nil {
		return err
	}
	if endpoint == nil {
		return endpointNotFoundError(eid)
	}

	// Always include rules for routed-mode port mappings - they'll be removed on Leave.
	pbmReq := portBindingMode{routed: true}
	// Act as the IPv4 gateway if explicitly selected.
	if gw4Id == eid {
		pbmReq.ipv4 = true
	}
	// Act as the IPv6 gateway if explicitly selected - or if there's no IPv6
	// gateway, but this endpoint is the IPv4 gateway (in which case, the userland
	// proxy may proxy between host v6 and container v4 addresses.)
	if gw6Id == eid || (gw6Id == "" && gw4Id == eid) {
		pbmReq.ipv6 = true
	}

	// If no change is needed, return.
	if endpoint.portBindingState == pbmReq {
		return nil
	}

	// Remove port bindings that aren't needed due to a change in mode.
	undoTrim, err := endpoint.trimPortBindings(ctx, network, pbmReq)
	if err != nil {
		return err
	}
	// Restore the recorded mode along with the bindings themselves. A failure
	// after portBindingState has been updated below would otherwise leave the
	// endpoint holding bindings its state says it doesn't have, so the next call
	// would compute the wrong set to add or trim.
	pbmPrev := endpoint.portBindingState
	defer func() {
		if retErr != nil && undoTrim != nil {
			endpoint.portMapping = append(endpoint.portMapping, undoTrim()...)
			endpoint.portBindingState = pbmPrev
		}
	}()

	// Set up port bindings for the modes that have become active, and store them
	// in the endpoint.
	if endpoint.extConnConfig != nil && endpoint.extConnConfig.PortBindings != nil {
		pbmAdd := endpoint.portBindingState.added(pbmReq)
		newPMs, err := network.addPortMappings(ctx, endpoint, endpoint.extConnConfig.PortBindings, network.config.DefaultBindingIP, pbmAdd, pbmReq)
		if err != nil {
			return err
		}
		endpoint.portMapping = append(endpoint.portMapping, newPMs...)
	}

	// Remember the new port binding state.
	endpoint.portBindingState = pbmReq

	// Clean the connection tracker state of the host for the specific endpoint. This is needed because some flows may
	// be bound to the local proxy, or to the host (for UDP packets), and won't be redirected to the new endpoints.
	clearConntrackEntries(d.nlh, endpoint)

	if err = d.storeUpdate(ctx, endpoint); err != nil {
		return fmt.Errorf("failed to update bridge endpoint %.7s to store: %v", endpoint.id, err)
	}

	return nil
}

// AddEphemeralPorts implements [driverapi.PortManager]. It publishes additional
// ports on an endpoint that's already joined to a sandbox, reusing the same
// machinery as the port bindings supplied at Join time. Host port-mappings
// (DNAT) are only programmed for the address families the endpoint is currently
// acting as a gateway for - exactly as for Join-time bindings.
//
// The bindings are kept operational (so they're reprogrammed on a firewalld
// reload and released on leave) but deliberately not written to the store, so
// they aren't restored - and then re-created by their owner - after a restart.
func (d *driver) AddEphemeralPorts(ctx context.Context, nid, eid string, ports []types.PublishedPort) error {
	if len(ports) == 0 {
		return nil
	}

	// Make sure the network isn't deleted, or in the middle of a firewalld reload,
	// while updating its iptables rules. This is also the lock that serialises
	// access to endpoint.ephemeralPortMapping / portBindingState with
	// ProgramExternalConnectivity.
	d.configNetwork.Lock()
	defer d.configNetwork.Unlock()

	network, err := d.getNetwork(nid)
	if err != nil {
		return err
	}
	endpoint, err := network.getEndpoint(eid)
	if err != nil {
		return err
	}
	if endpoint == nil {
		return endpointNotFoundError(eid)
	}

	reqs := sliceutil.Map(ports, func(p types.PublishedPort) portmapperapi.PortBindingReq {
		return portmapperapi.PortBindingReq{PortBinding: p.PortBinding()}
	})

	// Program the new bindings for whichever modes the endpoint is currently
	// serving (it's already joined, so its portBindingState is settled). Host
	// port-mappings are only created for the address families it's acting as a
	// gateway for, exactly as for the bindings supplied at Join time.
	newPMs, err := network.addPortMappings(ctx, endpoint, reqs, network.config.DefaultBindingIP, endpoint.portBindingState, endpoint.portBindingState)
	if err != nil {
		return err
	}
	endpoint.ephemeralPortMapping = append(endpoint.ephemeralPortMapping, newPMs...)

	// Clean the host's conntrack state for the newly-added ports only (not the
	// whole endpoint), so their new NAT rules are applied to any flows that were
	// bound to the host/proxy, while flows to the endpoint's other ports are left
	// undisturbed.
	clearConntrackEntriesForPorts(d.nlh, newPMs)

	return nil
}

// DelEphemeralPorts implements [driverapi.PortManager]. It unpublishes ports
// previously published via AddEphemeralPorts on an endpoint that's still joined
// to a sandbox. Ports that aren't currently published are ignored.
//
// Only ephemeral bindings are searched: this API only ever publishes ephemeral
// ports, and Join-time bindings are released through Leave, not here.
func (d *driver) DelEphemeralPorts(ctx context.Context, nid, eid string, ports []types.PublishedPort) error {
	if len(ports) == 0 {
		return nil
	}

	d.configNetwork.Lock()
	defer d.configNetwork.Unlock()

	network, err := d.getNetwork(nid)
	if err != nil {
		return err
	}
	endpoint, err := network.getEndpoint(eid)
	if err != nil {
		return err
	}
	if endpoint == nil {
		return endpointNotFoundError(eid)
	}

	reqs := sliceutil.Map(ports, types.PublishedPort.PortBinding)

	// Work out the bindings to drop and the ones to keep before mutating the
	// endpoint, so the split is taken from one consistent view of its bindings.
	var toDrop, keepEphemeral []portmapperapi.PortBinding
	for _, pb := range endpoint.ephemeralPortMapping {
		if portBindingMatchesAny(pb.PortBinding, reqs) {
			toDrop = append(toDrop, pb)
		} else {
			keepEphemeral = append(keepEphemeral, pb)
		}
	}

	if len(toDrop) == 0 {
		return nil
	}

	// unmapPBs is best-effort: it releases every binding's host port and stops its
	// proxy, and calls DelPorts over the whole set, collecting errors instead of
	// stopping at the first. So an error does not mean the bindings survived, and
	// the endpoint has to stop tracking them either way - keeping them would leave
	// it claiming reservations that have been released, and a retry would try to
	// unmap them a second time. The error is still returned, to be reported.
	err = network.unmapPBs(ctx, toDrop)

	// Clean the host's conntrack state for the removed ports only, so flows to
	// the endpoint's other, still-published ports are left undisturbed.
	clearConntrackEntriesForPorts(d.nlh, toDrop)

	endpoint.ephemeralPortMapping = keepEphemeral

	return err
}

// portBindingMatchesAny reports whether pb corresponds to any of the requested
// port bindings reqs, comparing the fields a caller specifies when requesting a
// binding: protocol, container IP and port, and (when the request pins them) the
// host IP and port. Operational-only fields (allocated socket, proxy handle) are
// ignored.
func portBindingMatchesAny(pb types.PortBinding, reqs []types.PortBinding) bool {
	for _, req := range reqs {
		if pb.Proto != req.Proto || pb.Port != req.Port {
			continue
		}
		if len(req.IP) > 0 && !pb.IP.Equal(req.IP) {
			continue
		}
		if req.HostPort != 0 && pb.HostPort != req.HostPort {
			continue
		}
		if len(req.HostIP) > 0 && !req.HostIP.IsUnspecified() && !pb.HostIP.Equal(req.HostIP) {
			continue
		}
		return true
	}
	return false
}

// trimPortBindings compares pbmReq with the current port bindings, and removes
// port bindings that are no longer required.
//
// ep.portMapping is updated when bindings are removed.
func (ep *bridgeEndpoint) trimPortBindings(ctx context.Context, n *bridgeNetwork, pbmReq portBindingMode) (func() []portmapperapi.PortBinding, error) {
	// Only the persisted bindings (portMapping) are trimmed on a gateway-mode
	// change. Ephemeral bindings (ephemeralPortMapping) are deliberately not
	// touched: they have no stored config to re-derive from, so their owner is
	// responsible for re-applying them across mode changes, and they're only used
	// on endpoints whose gateway modes are stable (see AddEphemeralPorts).

	// Drop IPv4 bindings if this endpoint is not the IPv4 gateway, unless the
	// network is "routed" (routed bindings get dropped unconditionally by Leave).
	drop4 := !pbmReq.ipv4 && !n.gwMode(firewaller.IPv4).routed()

	// Drop IPv6 bindings if this endpoint is not the IPv6 gateway, and not proxying
	// from host IPv6 to container IPv6 because there is no IPv6 gateway - unless the
	// IPv6 network is "routed" (routed bindings get dropped unconditionally by Leave).
	drop6 := !pbmReq.ipv6 && !n.gwMode(firewaller.IPv6).routed()

	if !drop4 && !drop6 {
		return nil, nil
	}

	toDrop := make([]portmapperapi.PortBinding, 0, len(ep.portMapping))
	toKeep := slices.DeleteFunc(ep.portMapping, func(pb portmapperapi.PortBinding) bool {
		is4 := pb.HostIP.To4() != nil
		if (is4 && drop4) || (!is4 && drop6) {
			toDrop = append(toDrop, pb)
			return true
		}
		return false
	})
	if len(toDrop) == 0 {
		return nil, nil
	}

	if err := n.unmapPBs(ctx, toDrop); err != nil {
		log.G(ctx).WithFields(log.Fields{
			"error": err,
			"gw4":   pbmReq.ipv4,
			"gw6":   pbmReq.ipv6,
			"nid":   stringid.TruncateID(n.id),
			"eid":   stringid.TruncateID(ep.id),
		}).Error("Failed to release port bindings")
		return nil, err
	}
	ep.portMapping = toKeep

	// The dropped bindings are always NATed (routed bindings aren't dropped here),
	// so re-adding them means programming the IPv4/IPv6 modes that were dropped.
	droppedModes := portBindingMode{ipv4: drop4, ipv6: drop6}
	undo := func() []portmapperapi.PortBinding {
		pbReq := make([]portmapperapi.PortBindingReq, 0, len(toDrop))
		for _, pb := range toDrop {
			pbReq = append(pbReq, portmapperapi.PortBindingReq{PortBinding: pb.Copy()})
		}
		pbs, err := n.addPortMappings(ctx, ep, pbReq, n.config.DefaultBindingIP, droppedModes, ep.portBindingState)
		if err != nil {
			log.G(ctx).WithFields(log.Fields{
				"error": err,
				"nid":   stringid.TruncateID(n.id),
				"eid":   stringid.TruncateID(ep.id),
			}).Error("Failed to restore port bindings following join failure")
			return nil
		}
		return pbs
	}

	return undo, nil
}

// clearConntrackEntries flushes conntrack entries matching endpoint IP address
// or matching one of the exposed UDP port.
// In the first case, this could happen if packets were received by the host
// between userland proxy startup and iptables setup.
// In the latter case, this could happen if packets were received whereas there
// were nowhere to route them, as netfilter creates entries in such case.
// This is required because iptables NAT rules are evaluated by netfilter only
// when creating a new conntrack entry. When Docker latter adds NAT rules,
// netfilter ignore them for any packet matching a pre-existing conntrack entry.
// As such, we need to flush all those conntrack entries to make sure NAT rules
// are correctly applied to all packets.
// See: #8795, #44688 & #44742.
func clearConntrackEntries(nlh nlwrap.Handle, ep *bridgeEndpoint) {
	var ipv4List []net.IP
	if ep.addr != nil {
		ipv4List = append(ipv4List, ep.addr.IP)
	}

	var ipv6List []net.IP
	if ep.addrv6 != nil {
		ipv6List = append(ipv6List, ep.addrv6.IP)
	}

	iptables.DeleteConntrackEntries(nlh, ipv4List, ipv6List)
	clearConntrackEntriesForPorts(nlh, ep.portMapping)
}

// clearConntrackEntriesForPorts flushes conntrack entries for the given port
// bindings only, so that the newly-programmed (or newly-removed) NAT rules are
// applied to their flows - see [clearConntrackEntries] for why that's needed.
//
// Unlike [clearConntrackEntries], it does not flush by the endpoint's IP
// address, because that would tear down established flows to the endpoint's
// other, unmodified ports. It's used when ports are added to or removed from an
// endpoint that's already joined to a sandbox. Only UDP entries need clearing;
// a stale TCP flow to a just-changed port is reset by its peer.
func clearConntrackEntriesForPorts(nlh nlwrap.Handle, pbs []portmapperapi.PortBinding) {
	var udpPorts []types.PortBinding
	for _, pb := range pbs {
		if pb.Proto == types.UDP {
			// NAT'd bindings: the conntrack entry's orig-dst-port is the
			// published host port. Routed bindings (no NAT) are covered by
			// the IP-keyed flush above — their flows are never DNAT'd, so
			// a port-keyed flush is not needed for them.
			if !pb.NAT.IsValid() {
				continue
			}
			udpPorts = append(udpPorts, pb.PortBinding)
		}
	}
	iptables.DeleteConntrackEntriesByPort(nlh, types.UDP, udpPorts)
}

func (d *driver) handleFirewalldReload() {
	if !d.config.EnableIPTables && !d.config.EnableIP6Tables {
		return
	}

	d.mu.Lock()
	nids := make([]string, 0, len(d.networks))
	for _, nw := range d.networks {
		nids = append(nids, nw.id)
	}
	d.mu.Unlock()

	for _, nid := range nids {
		d.handleFirewalldReloadNw(nid)
	}
}

func (d *driver) handleFirewalldReloadNw(nid string) {
	// Make sure the network isn't being deleted, and ProgramExternalConnectivity
	// isn't modifying iptables rules, while restoring the rules.
	d.configNetwork.Lock()
	defer d.configNetwork.Unlock()

	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.config.EnableIPTables && !d.config.EnableIP6Tables {
		return
	}

	nw, ok := d.networks[nid]
	if !ok {
		// Network deleted since the reload started, not an error.
		return
	}

	if err := nw.firewallerNetwork.ReapplyNetworkLevelRules(context.TODO()); err != nil {
		log.G(context.Background()).WithFields(log.Fields{
			"nid":   nw.id,
			"error": err,
		}).Error("Failed to re-create packet filter on firewalld reload")
	}

	// Re-add legacy links - only added during ProgramExternalConnectivity, but legacy
	// links are default-bridge-only, and it's not possible to connect a container to
	// the default bridge and a user-defined network. So, the default bridge is always
	// the gateway and, if there are legacy links configured they need to be set up.
	if !nw.config.EnableICC {
		for _, ep := range nw.endpoints {
			if err := d.link(nw, ep, true); err != nil {
				log.G(context.Background()).WithFields(log.Fields{
					"nid":   nw.id,
					"eid":   ep.id,
					"error": err,
				}).Error("Failed to re-create link on firewalld reload")
			}
		}
	}

	// Set up per-port rules. These are also only set up during ProgramExternalConnectivity
	// but the network's port bindings are only configured when it's acting as the
	// gateway network. So, this is a no-op for networks that aren't providing endpoints
	// with the gateway.
	nw.reapplyPerPortIptables()

	if err := iptables.AddInterfaceFirewalld(nw.config.BridgeName); err != nil {
		log.G(context.Background()).WithFields(log.Fields{
			"error":  err,
			"nid":    nw.id,
			"bridge": nw.config.BridgeName,
		}).Error("Failed to add interface to docker zone on firewalld reload")
	}
}

func LegacyContainerLinkOptions(parentEndpoints, childEndpoints []string) map[string]any {
	return options.Generic{
		netlabel.GenericData: options.Generic{
			"ParentEndpoints": parentEndpoints,
			"ChildEndpoints":  childEndpoints,
		},
	}
}

func (d *driver) link(network *bridgeNetwork, endpoint *bridgeEndpoint, enable bool) (retErr error) {
	cc := endpoint.containerConfig
	ec := endpoint.extConnConfig
	if cc == nil || ec == nil || (len(cc.ParentEndpoints) == 0 && len(cc.ChildEndpoints) == 0) {
		// nothing to do
		return nil
	}

	// Try to keep things atomic when adding - if there's an error, recurse with enable=false
	// to delete everything that might have been created.
	if enable {
		defer func() {
			if retErr != nil {
				d.link(network, endpoint, false)
			}
		}()
	}

	if ec.ExposedPorts != nil {
		for _, p := range cc.ParentEndpoints {
			parentEndpoint, err := network.getEndpoint(p)
			if err != nil {
				return err
			}
			if parentEndpoint == nil {
				return invalidEndpointIDError(p)
			}
			parentAddr, ok := netip.AddrFromSlice(parentEndpoint.addr.IP)
			if !ok {
				return fmt.Errorf("invalid parent endpoint IP: %s", parentEndpoint.addr.IP)
			}
			parentAddr = parentAddr.Unmap()
			childAddr, ok := netip.AddrFromSlice(endpoint.addr.IP)
			if !ok {
				return fmt.Errorf("invalid parent endpoint IP: %s", endpoint.addr.IP)
			}
			childAddr = childAddr.Unmap()

			if enable {
				if err := network.firewallerNetwork.AddLink(context.TODO(), parentAddr, childAddr, ec.ExposedPorts); err != nil {
					return err
				}
			} else {
				network.firewallerNetwork.DelLink(context.TODO(), parentAddr, childAddr, ec.ExposedPorts)
			}
		}
	}

	for _, c := range cc.ChildEndpoints {
		childEndpoint, err := network.getEndpoint(c)
		if err != nil {
			return err
		}
		if childEndpoint == nil {
			return invalidEndpointIDError(c)
		}
		if childEndpoint.extConnConfig == nil || childEndpoint.extConnConfig.ExposedPorts == nil {
			continue
		}
		parentAddr, ok := netip.AddrFromSlice(endpoint.addr.IP)
		if !ok {
			return fmt.Errorf("invalid parent endpoint IP: %s", endpoint.addr.IP)
		}
		parentAddr = parentAddr.Unmap()
		childAddr, ok := netip.AddrFromSlice(childEndpoint.addr.IP)
		if !ok {
			return fmt.Errorf("invalid parent endpoint IP: %s", childEndpoint.addr.IP)
		}
		childAddr = childAddr.Unmap()

		if enable {
			if err := network.firewallerNetwork.AddLink(context.TODO(), parentAddr, childAddr, childEndpoint.extConnConfig.ExposedPorts); err != nil {
				return err
			}
		} else {
			network.firewallerNetwork.DelLink(context.TODO(), parentAddr, childAddr, childEndpoint.extConnConfig.ExposedPorts)
		}
	}

	return nil
}

func (d *driver) IsBuiltIn() bool {
	return true
}

func parseContainerOptions(cOptions map[string]any) (*containerConfiguration, error) {
	if cOptions == nil {
		return nil, nil
	}
	genericData := cOptions[netlabel.GenericData]
	if genericData == nil {
		return nil, nil
	}
	switch opt := genericData.(type) {
	case options.Generic:
		return options.GenerateFromModel[*containerConfiguration](opt)
	case *containerConfiguration:
		return opt, nil
	default:
		return nil, nil
	}
}

func parseConnectivityOptions(cOptions map[string]any) (*connectivityConfiguration, error) {
	if cOptions == nil {
		return nil, nil
	}

	cc := &connectivityConfiguration{}

	if opt, ok := cOptions[netlabel.PortMap]; ok {
		if pbs, ok := opt.([]types.PortBinding); ok {
			cc.PortBindings = sliceutil.Map(pbs, func(pb types.PortBinding) portmapperapi.PortBindingReq {
				return portmapperapi.PortBindingReq{
					PortBinding: pb.Copy(),
				}
			})
		} else {
			return nil, types.InvalidParameterErrorf("invalid port mapping data in connectivity configuration: %v", opt)
		}
	}

	if opt, ok := cOptions[netlabel.ExposedPorts]; ok {
		if ports, ok := opt.([]types.TransportPort); ok {
			cc.ExposedPorts = ports
		} else {
			return nil, types.InvalidParameterErrorf("invalid exposed ports data in connectivity configuration: %v", opt)
		}
	}

	return cc, nil
}
