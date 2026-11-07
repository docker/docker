// Package types contains types that are common across libnetwork project
package types

import (
	"bytes"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/ishidawataru/sctp"
	"github.com/moby/moby/v2/errdefs"
)

// IPFamily specify IP address type(s).
type IPFamily int

const (
	IP   IPFamily = syscall.AF_UNSPEC // Either IPv4 or IPv6.
	IPv4 IPFamily = syscall.AF_INET   // Internet Protocol version 4 (IPv4).
	IPv6 IPFamily = syscall.AF_INET6  // Internet Protocol version 6 (IPv6).
)

// EncryptionKey is the libnetwork representation of the key distributed by the lead
// manager.
type EncryptionKey struct {
	Subsystem   string
	Algorithm   int32
	Key         []byte
	LamportTime uint64
}

// QosPolicy represents a quality of service policy on an endpoint
type QosPolicy struct {
	MaxEgressBandwidth uint64
}

// TransportPort represents a local Layer 4 endpoint
type TransportPort struct {
	Proto Protocol
	Port  uint16
}

// String returns the TransportPort structure in string form
func (t *TransportPort) String() string {
	return fmt.Sprintf("%s/%d", t.Proto.String(), t.Port)
}

// PortBinding represents a port binding between the container and the host
type PortBinding struct {
	Proto       Protocol
	IP          net.IP
	Port        uint16
	HostIP      net.IP
	HostPort    uint16
	HostPortEnd uint16
	// NoSelfConnect is a hint that nothing behind the endpoint connects to
	// its own published port through the host. Packets from other endpoints
	// on the same network can then be DNATed to the port without hairpin
	// mode or masquerading, instead of being left for the userland proxy.
	NoSelfConnect bool `json:",omitempty"`
}

// PublishedPort is a request to publish a single container port on the host.
// Unlike [PortBinding] it cannot express a host-port range - the host port is
// always a single port. It's the currency of the dynamic port-publishing API
// ([github.com/moby/moby/v2/daemon/libnetwork/driverapi.PortManager]), so that
// callers can't request a range on a path that doesn't support one.
type PublishedPort struct {
	Proto    Protocol
	Port     uint16 // container port
	HostPort uint16 // single host port - there is no range
	// NoSelfConnect is a hint that nothing behind the endpoint connects to
	// its own published port through the host. See [PortBinding.NoSelfConnect].
	NoSelfConnect bool
}

// PortBinding returns the single-port [PortBinding] equivalent of p, with
// HostPortEnd pinned to HostPort so it can never denote a range.
func (p PublishedPort) PortBinding() PortBinding {
	return PortBinding{
		Proto:         p.Proto,
		Port:          p.Port,
		HostPort:      p.HostPort,
		HostPortEnd:   p.HostPort,
		NoSelfConnect: p.NoSelfConnect,
	}
}

// HostAddr returns the host side transport address
func (p PortBinding) HostAddr() (net.Addr, error) {
	switch p.Proto {
	case UDP:
		return &net.UDPAddr{IP: p.HostIP, Port: int(p.HostPort)}, nil
	case TCP:
		return &net.TCPAddr{IP: p.HostIP, Port: int(p.HostPort)}, nil
	case SCTP:
		return &sctp.SCTPAddr{IPAddrs: []net.IPAddr{{IP: p.HostIP}}, Port: int(p.HostPort)}, nil
	default:
		return nil, fmt.Errorf("invalid transport protocol: %s", p.Proto.String())
	}
}

// Copy returns a deep copy of the PortBinding.
func (p PortBinding) Copy() PortBinding {
	return PortBinding{
		Proto:         p.Proto,
		IP:            slices.Clone(p.IP),
		Port:          p.Port,
		HostIP:        slices.Clone(p.HostIP),
		HostPort:      p.HostPort,
		HostPortEnd:   p.HostPortEnd,
		NoSelfConnect: p.NoSelfConnect,
	}
}

// Equal returns true if o has the same values as p, else false.
func (p PortBinding) Equal(o PortBinding) bool {
	return p.Proto == o.Proto &&
		p.IP.Equal(o.IP) &&
		p.Port == o.Port &&
		p.HostIP.Equal(o.HostIP) &&
		p.HostPort == o.HostPort &&
		p.HostPortEnd == o.HostPortEnd &&
		p.NoSelfConnect == o.NoSelfConnect
}

// String returns the PortBinding structure in the form "HostIP:HostPort:IP:Port/Proto",
// omitting un-set fields apart from Port.
func (p PortBinding) String() string {
	var ret strings.Builder
	if len(p.HostIP) > 0 {
		is6 := p.HostIP.To4() == nil
		if is6 {
			ret.WriteRune('[')
		}
		ret.WriteString(p.HostIP.String())
		if is6 {
			ret.WriteRune(']')
		}
		ret.WriteRune(':')
	}
	if p.HostPort != 0 {
		ret.WriteString(strconv.Itoa(int(p.HostPort)))
		if p.HostPortEnd != 0 && p.HostPortEnd != p.HostPort {
			ret.WriteRune('-')
			ret.WriteString(strconv.Itoa(int(p.HostPortEnd)))
		}
	}
	if ret.Len() > 0 {
		ret.WriteRune(':')
	}
	if len(p.IP) > 0 {
		is6 := p.IP.To4() == nil
		if is6 {
			ret.WriteRune('[')
		}
		ret.WriteString(p.IP.String())
		if is6 {
			ret.WriteRune(']')
		}
		ret.WriteRune(':')
	}
	ret.WriteString(strconv.Itoa(int(p.Port)))
	if p.Proto != 0 {
		ret.WriteRune('/')
		ret.WriteString(p.Proto.String())
	}
	return ret.String()
}

const (
	ICMP Protocol = 1   // ICMP (Internet Control Message Protocol) — [syscall.IPPROTO_ICMP]
	TCP  Protocol = 6   // TCP (Transmission Control Protocol) — [syscall.IPPROTO_TCP]
	UDP  Protocol = 17  // UDP (User Datagram Protocol) — [syscall.IPPROTO_UDP]
	SCTP Protocol = 132 // SCTP (Stream Control Transmission Protocol) — [syscall.IPPROTO_SCTP] (not supported on Windows).
)

// Protocol represents an IP protocol number
type Protocol uint8

func (p Protocol) String() string {
	switch p {
	case ICMP:
		return "icmp"
	case TCP:
		return "tcp"
	case UDP:
		return "udp"
	case SCTP:
		return "sctp"
	default:
		return strconv.Itoa(int(p))
	}
}

// ParseProtocol returns the respective Protocol type for the passed string
func ParseProtocol(s string) Protocol {
	switch strings.ToLower(s) {
	case "icmp":
		return ICMP
	case "udp":
		return UDP
	case "tcp":
		return TCP
	case "sctp":
		return SCTP
	default:
		return 0
	}
}

// GetIPNetCopy returns a copy of the passed IP Network. If the input is nil,
// it returns nil.
func GetIPNetCopy(from *net.IPNet) *net.IPNet {
	if from == nil {
		return nil
	}
	return &net.IPNet{
		IP:   slices.Clone(from.IP),
		Mask: slices.Clone(from.Mask),
	}
}

// GetIPNetCanonical returns the canonical form of the given IP network,
// where the IP is masked with the subnet mask. If the input is nil,
// it returns nil.
func GetIPNetCanonical(nw *net.IPNet) *net.IPNet {
	if nw == nil {
		return nil
	}
	return &net.IPNet{
		IP:   slices.Clone(nw.IP.Mask(nw.Mask)),
		Mask: slices.Clone(nw.Mask),
	}
}

// CompareIPNet returns equal if the two IP Networks are equal
func CompareIPNet(a, b *net.IPNet) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.IP.Equal(b.IP) && bytes.Equal(a.Mask, b.Mask)
}

var v4inV6MaskPrefix = []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// compareIPMask checks if the passed ip and mask are semantically compatible.
// It returns the byte indexes for the address and mask so that caller can
// do bitwise operations without modifying address representation.
func compareIPMask(ip net.IP, mask net.IPMask) (is int, ms int, _ error) {
	// Find the effective starting of address and mask
	if len(ip) == net.IPv6len && ip.To4() != nil {
		is = 12
	}
	if len(ip[is:]) == net.IPv4len && len(mask) == net.IPv6len && bytes.Equal(mask[:12], v4inV6MaskPrefix) {
		ms = 12
	}
	// Check if address and mask are semantically compatible
	if len(ip[is:]) != len(mask[ms:]) {
		return 0, 0, fmt.Errorf("ip and mask are not compatible: (%s, %s)", ip, mask)
	}
	return is, ms, nil
}

// GetHostPartIP returns the host portion of the ip address identified by the mask.
// IP address representation is not modified. If address and mask are not compatible
// an error is returned.
func GetHostPartIP(ip net.IP, mask net.IPMask) (net.IP, error) {
	// Find the effective starting of address and mask
	is, ms, err := compareIPMask(ip, mask)
	if err != nil {
		return nil, fmt.Errorf("cannot compute host portion ip address because %s", err)
	}

	// Compute host portion
	out := slices.Clone(ip)
	for i := 0; i < len(mask[ms:]); i++ {
		out[is+i] &= ^mask[ms+i]
	}

	return out, nil
}

// GetBroadcastIP returns the broadcast ip address for the passed network (ip and mask).
// IP address representation is not modified. If address and mask are not compatible
// an error is returned.
func GetBroadcastIP(ip net.IP, mask net.IPMask) (net.IP, error) {
	// Find the effective starting of address and mask
	is, ms, err := compareIPMask(ip, mask)
	if err != nil {
		return nil, fmt.Errorf("cannot compute broadcast ip address because %s", err)
	}

	// Compute broadcast address
	out := slices.Clone(ip)
	for i := 0; i < len(mask[ms:]); i++ {
		out[is+i] |= ^mask[ms+i]
	}

	return out, nil
}

// ParseCIDR returns the *net.IPNet represented by the passed CIDR notation
func ParseCIDR(cidr string) (*net.IPNet, error) {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	ipNet.IP = ip
	return ipNet, nil
}

type RouteType int

const (
	NEXTHOP   RouteType = iota // NEXTHOP indicates a StaticRoute with an IP next hop.
	CONNECTED                  // CONNECTED indicates a StaticRoute with an interface for directly connected peers.
)

// StaticRoute is a statically provisioned IP route.
type StaticRoute struct {
	Destination *net.IPNet
	RouteType   RouteType // NEXTHOP or CONNECTED
	NextHop     net.IP    // NextHop will be resolved by the kernel (i.e., as a loose hop).
}

// Copy returns a copy of this StaticRoute structure
func (r *StaticRoute) Copy() *StaticRoute {
	return &StaticRoute{
		Destination: GetIPNetCopy(r.Destination),
		RouteType:   r.RouteType,
		NextHop:     slices.Clone(r.NextHop),
	}
}

// InterfaceStatistics represents the interface's statistics
type InterfaceStatistics struct {
	RxBytes   uint64
	RxPackets uint64
	RxErrors  uint64
	RxDropped uint64
	TxBytes   uint64
	TxPackets uint64
	TxErrors  uint64
	TxDropped uint64
}

func (is *InterfaceStatistics) String() string {
	return fmt.Sprintf("\nRxBytes: %d, RxPackets: %d, RxErrors: %d, RxDropped: %d, TxBytes: %d, TxPackets: %d, TxErrors: %d, TxDropped: %d",
		is.RxBytes, is.RxPackets, is.RxErrors, is.RxDropped, is.TxBytes, is.TxPackets, is.TxErrors, is.TxDropped)
}

/******************************
 * Well-known Error Interfaces
 ******************************/

// MaskableError is an interface for errors which can be ignored by caller
type MaskableError interface {
	// Maskable makes implementer into MaskableError type
	Maskable()
}

// InternalError is an interface for errors raised because of an internal error
type InternalError interface {
	// Internal makes implementer into InternalError type
	Internal()
}

/******************************
 * Well-known Error Formatters
 ******************************/

// InvalidParameterErrorf creates an instance of [errdefs.ErrInvalidParameter].
func InvalidParameterErrorf(format string, params ...any) error {
	return errdefs.InvalidParameter(fmt.Errorf(format, params...))
}

// NotFoundErrorf creates an instance of [errdefs.ErrNotFound].
func NotFoundErrorf(format string, params ...any) error {
	return errdefs.NotFound(fmt.Errorf(format, params...))
}

// ForbiddenErrorf creates an instance of [errdefs.ErrForbidden].
func ForbiddenErrorf(format string, params ...any) error {
	return errdefs.Forbidden(fmt.Errorf(format, params...))
}

// UnavailableErrorf creates an instance of [errdefs.ErrUnavailable]
func UnavailableErrorf(format string, params ...any) error {
	return errdefs.Unavailable(fmt.Errorf(format, params...))
}

// NotImplementedErrorf creates an instance of [errdefs.ErrNotImplemented].
func NotImplementedErrorf(format string, params ...any) error {
	return errdefs.NotImplemented(fmt.Errorf(format, params...))
}

// InternalErrorf creates an instance of InternalError
func InternalErrorf(format string, params ...any) error {
	return internal(fmt.Sprintf(format, params...))
}

// InternalMaskableErrorf creates an instance of InternalError and MaskableError
func InternalMaskableErrorf(format string, params ...any) error {
	return maskInternal(fmt.Sprintf(format, params...))
}

/***********************
 * Internal Error Types
 ***********************/

type internal string

func (nt internal) Error() string {
	return string(nt)
}
func (nt internal) Internal() {}

type maskInternal string

func (mnt maskInternal) Error() string {
	return string(mnt)
}
func (mnt maskInternal) Internal() {}
func (mnt maskInternal) Maskable() {}
