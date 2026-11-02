//go:build linux

// Package firewaller defines an interface that can be used to manipulate
// firewall configuration for a bridge network.
package firewaller

import (
	"context"
	"net/netip"

	"github.com/moby/moby/v2/daemon/libnetwork/types"
)

type IPVersion uint8

const (
	IPv4 IPVersion = 4
	IPv6 IPVersion = 6
)

// Config contains top-level settings for the firewaller.
type Config struct {
	// IPv4 true means IPv4 firewalling is required.
	IPv4 bool
	// IPv6 true means IPv4 firewalling is required.
	IPv6 bool
	// Hairpin means the userland proxy will not be running.
	Hairpin bool
	// AllowDirectRouting means packets addressed directly to a container's IP address will be
	// accepted, regardless of which network interface they are from.
	AllowDirectRouting bool
	// WSL2Mirrored is true if running under WSL2 with mirrored networking enabled.
	WSL2Mirrored bool
}

// NetworkConfig contains settings for a single bridge network.
type NetworkConfig struct {
	// IfName is the name of the bridge device.
	IfName string
	// Internal is true if the network should have no access to networks outside the Docker host.
	Internal bool
	// ICC is false if containers on the bridge should not be able to communicate (unless it's the
	// default bridge, and legacy-links are set up).
	ICC bool
	// Masquerade is true if the network should use masquerading/SNAT.
	Masquerade bool
	// TrustedHostInterfaces are interfaces that must be treated as part of the network (like the
	// bridge itself). In particular, these are not external interfaces for the purpose of
	// blocking direct-routing to a container's IP address.
	TrustedHostInterfaces []string
	// AcceptFwMark is a firewall mark/mask. Packets with this mark will not be dropped by
	// per-port blocking rules. So, packets with this mark have access to unpublished
	// container ports.
	AcceptFwMark string
	// Config4 contains IPv4-specific configuration for the network.
	Config4 NetworkConfigFam
	// Config6 contains IPv6-specific configuration for the network.
	Config6 NetworkConfigFam
}

// NetworkConfigFam contains network configuration for a single address family.
type NetworkConfigFam struct {
	// HostIP is the address to use for SNAT. If unset, masquerading will be used instead.
	HostIP netip.Addr
	// Prefix is the bridge network's subnet.
	Prefix netip.Prefix
	// Routed is true if containers should be directly addressable, no NAT from the host.
	Routed bool
	// Unprotected is true if no rules to filter unpublished ports or direct access from
	// any remote host are required.
	Unprotected bool
}

// DNATFromOwnBridge returns true if packets that arrive from the network's own
// bridge should be DNATed to pb's container address. If it returns false,
// they're left for the userland proxy.
//
// Without the proxy (config.Hairpin), they're always DNATed. With the proxy,
// DNATing them needs two things the proxy avoids. First, br_netfilter: the
// container's replies are bridged straight back to the client, so conntrack
// only sees them, and restores their source address, if bridged packets go
// through netfilter. The network has that when ICC is disabled. Second, for a
// container that connects to its own published port, hairpin mode on its
// bridge port and a MASQUERADE rule. A binding marked NoSelfConnect doesn't
// need those.
func (nc NetworkConfig) DNATFromOwnBridge(config Config, pb types.PortBinding) bool {
	return config.Hairpin || (!nc.ICC && pb.NoSelfConnect)
}

// Firewaller implements firewall rules for bridge networks.
type Firewaller interface {
	// NewNetwork returns an object that can be used to add published ports and legacy
	// links for a bridge network.
	NewNetwork(ctx context.Context, nc NetworkConfig) (Network, error)
}

// Network can be used to manipulate firewall rules for a bridge network.
type Network interface {
	// ReapplyNetworkLevelRules re-creates the initial set of network-level rules
	// created by [Firewaller.NewNetwork]. It can be called after, for example, a
	// firewalld reload has deleted the rules. Rules for port mappings and legacy
	// links are not re-created.
	ReapplyNetworkLevelRules(ctx context.Context) error
	// DelNetworkLevelRules deletes any configuration set up by [Firewaller.NewNetwork].
	// It does not delete per-port or per-link rules. The caller is responsible for tracking
	// those and deleting them when the network is removed.
	DelNetworkLevelRules(ctx context.Context) error

	// AddEndpoint is used to notify the firewaller about a new container on the
	// network, with its IP addresses.
	AddEndpoint(ctx context.Context, epIPv4, epIPv6 netip.Addr) error
	// DelEndpoint undoes configuration applied by AddEndpoint.
	DelEndpoint(ctx context.Context, epIPv4, epIPv6 netip.Addr) error

	// AddPorts adds the configuration needed for published ports.
	AddPorts(ctx context.Context, pbs []types.PortBinding) error
	// DelPorts deletes the configuration needed for published ports.
	DelPorts(ctx context.Context, pbs []types.PortBinding) error

	// AddLink adds the configuration needed for a legacy link.
	AddLink(ctx context.Context, parentIP, childIP netip.Addr, ports []types.TransportPort) error
	// DelLink deletes the configuration needed for a legacy link.
	DelLink(ctx context.Context, parentIP, childIP netip.Addr, ports []types.TransportPort)
}

// FirewallCleaner is used to delete rules created by previous incarnations of
// the daemon. On startup, once a Firewaller implementation has been selected, if
// rules may have been left behind by a different Firewaller implementation, get
// a FirewallCleaner from the old Firewaller and pass it to the new/current
// Firewaller's SetFirewallCleaner.
type FirewallCleaner interface {
	// DelNetwork removes all firewall rules related to the specified network configuration.
	// It should be called by the new Firewaller when adding a new network.
	DelNetwork(ctx context.Context, nc NetworkConfig)
	// DelEndpoint removes firewall rules related to a specific endpoint.
	// It should be called by the new Firewaller when adding a new endpoint.
	DelEndpoint(ctx context.Context, nc NetworkConfig, epIPv4, epIPv6 netip.Addr)
	// DelPorts removes firewall rules associated with the specified port bindings.
	// It should be called by the new Firewaller when adding new port mappings.
	DelPorts(ctx context.Context, nc NetworkConfig, pbs []types.PortBinding)
	// DelLink removes firewall rules associated with a legacy link.
	// It should be called by the new Firewaller when adding a new legacy link.
	// (Excluded from the interface at present, it's not required by any current
	// Firewaller.)
	// DelLink(ctx context.Context, nc NetworkConfig, parentIP, childIP netip.Addr, ports []types.TransportPort)
}
