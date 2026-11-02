package bridge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/libnetwork/netutils"
	"github.com/moby/moby/v2/daemon/libnetwork/portallocator"
	"github.com/moby/moby/v2/daemon/libnetwork/portmapper"
	"github.com/moby/moby/v2/daemon/libnetwork/portmapperapi"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
)

// Allow unit tests to supply a dummy StartProxy.
var startProxy = portmapper.StartProxy

// addPortMappings takes cfg, the configuration for port mappings, selects host
// ports when ranges are given, binds host ports to check they're available and
// reserve them, starts docker-proxy if required, and sets up iptables
// NAT/forwarding rules as necessary. If anything goes wrong, it will undo any
// work it's done and return an error. Otherwise, the returned slice of
// PortBinding has an entry per address family (if cfg describes a mapping for
// 'any' host address, it's expanded into mappings for IPv4 and IPv6, because
// that's how the mapping is presented in 'inspect'). HostPort and HostPortEnd in
// each returned PortBinding are set to the selected and reserved port.
//
// pbmAdd selects which binding modes (IPv4/IPv6 NAT, and/or routed) to program
// for in this call. The bindings are always added to the endpoint's existing
// set - the endpoint's current mode is not consulted - so callers use this both
// to apply a full config for newly-active modes (see
// [driver.ProgramExternalConnectivity]) and to publish additional ports on an
// already-joined endpoint (see [driver.AddEphemeralPorts]). pbmHave describes the modes
// the endpoint provides once the new bindings are in place; it's usually equal
// to pbmAdd (see [bridgeNetwork.sortAndNormPBs]).
func (n *bridgeNetwork) addPortMappings(
	ctx context.Context,
	ep *bridgeEndpoint,
	cfg []portmapperapi.PortBindingReq,
	defHostIP net.IP,
	pbmAdd portBindingMode,
	pbmHave portBindingMode,
) (_ []portmapperapi.PortBinding, retErr error) {
	if len(defHostIP) == 0 {
		defHostIP = net.IPv4zero
	} else if addr4 := defHostIP.To4(); addr4 != nil {
		// Unmap the address if it's IPv4-mapped IPv6.
		defHostIP = addr4
	}

	pms := n.portMappers()

	bindings := make([]portmapperapi.PortBinding, 0, len(cfg)*2)
	defer func() {
		if retErr != nil {
			if err := n.unmapPorts(ctx, bindings); err != nil {
				log.G(ctx).WithFields(log.Fields{
					"bindings": bindings,
					"error":    err,
					"origErr":  retErr,
				}).Warn("Failed to unmap port bindings after error")
			}
		}
	}()

	bindingReqs := n.sortAndNormPBs(ctx, ep, cfg, defHostIP, pbmAdd, pbmHave)

	// toBind accumulates port bindings that should be allocated the same host port
	// (if required by NAT config). If the host address is unspecified, and defHostIP
	// is 0.0.0.0, one iteration of the loop may generate bindings for v4 and v6. If
	// a host address is specified, it'll either be IPv4 or IPv6, and only one
	// binding will be added per iteration. Config for bindings that only differ in
	// host IP are sorted next to each other, the loop continues until toBind has
	// collected them all, for both v4 and v6. The addresses may be 0.0.0.0 and [::],
	// or multiple addresses of both address families. Once there are no more
	// bindings to collect, they're applied and toBind is reset.
	var toBind []portmapperapi.PortBindingReq
	for i, c := range bindingReqs {
		toBind = append(toBind, c)
		if i < len(bindingReqs)-1 && c.Mapper == bindingReqs[i+1].Mapper && needSamePort(c, bindingReqs[i+1]) {
			// This port binding matches the next, apart from host IP. So, continue
			// collecting bindings, then allocate the same host port for all addresses.
			continue
		}

		mapper := c.Mapper
		pm, err := pms.Get(mapper)
		if err != nil {
			return nil, err
		}
		newB, err := pm.MapPorts(ctx, toBind)
		if err != nil {
			return nil, err
		}
		for i := range newB {
			// Record the mapper so rollback and release use the same port mapper.
			newB[i].Mapper = mapper
		}
		bindings = append(bindings, newB...)

		// Reset toBind now the ports are bound.
		toBind = toBind[:0]
	}

	if len(bindings) == 0 {
		return bindings, nil
	}

	// Apply all port rules together, so nftables only rewrites the affected chains
	// once per address family, rather than once for each group of bindings.
	fwPorts := collectFirewallPorts(bindings)
	// AddPorts can fail after installing rules for one address family.
	// With nftables, a failed per-family transaction leaves no rules for that
	// family. Deleting those missing rules may log a warning below even when
	// all successfully installed rules have been removed.
	defer func() {
		if retErr != nil {
			if err := n.firewallerNetwork.DelPorts(ctx, fwPorts); err != nil {
				log.G(ctx).WithFields(log.Fields{
					"bindings": bindings,
					"error":    err,
					"origErr":  retErr,
				}).Warn("Failed to remove firewall rules after error")
			}
		}
	}()
	if err := n.firewallerNetwork.AddPorts(ctx, fwPorts); err != nil {
		return nil, err
	}

	// Start userland proxies only after all firewall rules have been installed.
	if n.driver.config.EnableProxy {
		for i, pb := range bindings {
			if pb.BoundSocket == nil || pb.RootlesskitUnsupported || pb.StopProxy != nil {
				continue
			}
			if err := portallocator.DetachSocketFilter(bindings[i].BoundSocket); err != nil {
				return nil, fmt.Errorf("failed to detach socket filter for port mapping %s: %w", bindings[i].PortBinding, err)
			}
			var err error
			bindings[i].StopProxy, err = startProxy(pb.ChildPortBinding(), n.driver.config.ProxyPath, pb.BoundSocket)
			if err != nil {
				return nil, fmt.Errorf("failed to start userland proxy for port mapping %s: %w", pb.PortBinding, err)
			}
			if err := bindings[i].BoundSocket.Close(); err != nil {
				log.G(ctx).WithFields(log.Fields{
					"error":   err,
					"mapping": pb.PortBinding,
				}).Warnf("failed to close proxy socket")
			}
			bindings[i].BoundSocket = nil
		}
	}

	return bindings, nil
}

// sortAndNormPBs transforms cfg into a list of portBindingReq, with all fields
// normalized:
//
//   - HostPortEnd=HostPort (rather than 0) if the host port isn't a range
//   - HostIP is set to the default host IP if not specified, and the binding is
//     NATed
//   - DisableNAT is set if the binding is routed, and HostIP is cleared
//
// When no HostIP is specified, and the default HostIP is 0.0.0.0, a duplicate
// IPv6 port binding is created with the same port and protocol, but with
// HostIP set to [::].
//
// Finally, port bindings are sorted into the ordering defined by
// [PortBindingReqs.Compare] in order to form groups of port bindings that
// should be processed in one go.
//
// pbmAdd selects which port binding modes to generate bindings for in this
// call: IPv4 and/or IPv6 NATed bindings, and/or routed bindings. The function
// does not consult the endpoint's current binding state, so it can be used both
// to (re)apply a full config and to add bindings incrementally.
//
// pbmHave describes the modes the endpoint provides once these bindings are in
// place. It's only used to decide whether host-IPv6 traffic can be proxied to an
// IPv4-only container (which needs the endpoint to be an IPv4 gateway), so it's
// usually the same as pbmAdd - it differs only when adding one mode's bindings
// while another is already active.
func (n *bridgeNetwork) sortAndNormPBs(
	ctx context.Context,
	ep *bridgeEndpoint,
	cfg []portmapperapi.PortBindingReq,
	defHostIP net.IP,
	pbmAdd portBindingMode,
	pbmHave portBindingMode,
) []portmapperapi.PortBindingReq {
	var containerIPv4, containerIPv6 net.IP
	if ep.addr != nil {
		containerIPv4 = ep.addr.IP
	}
	if ep.addrv6 != nil {
		containerIPv6 = ep.addrv6.IP
	}

	disableNAT4, disableNAT6 := n.getNATDisabled()

	add4 := pbmAdd.ipv4 || (disableNAT4 && pbmAdd.routed)
	add6 := pbmAdd.ipv6 || (disableNAT6 && pbmAdd.routed)

	reqs := make([]portmapperapi.PortBindingReq, 0, len(cfg))
	for _, c := range cfg {
		if c.HostPortEnd == 0 {
			c.HostPortEnd = c.HostPort
		}

		if add4 {
			if bindingIPv4, ok := configurePortBindingIPv4(ctx, disableNAT4, c, containerIPv4, defHostIP); ok {
				reqs = append(reqs, bindingIPv4)
			}
		}

		// If the container has no IPv6 address, allow proxying host IPv6 traffic to it
		// by setting up the binding with the IPv4 interface if the userland proxy is enabled
		// This change was added to keep backward compatibility
		containerIP := containerIPv6
		if containerIPv6 == nil && pbmHave.ipv4 && add6 {
			if !n.driver.config.EnableProxy {
				// There's no way to map from host-IPv6 to container-IPv4 with the userland proxy
				// disabled.
				// If that is required, don't treat it as an error because, as networks are
				// connected/disconnected, the container's gateway endpoint might change to a
				// network where this config makes more sense.
				if len(c.HostIP) > 0 && c.HostIP.To4() == nil {
					log.G(ctx).WithFields(log.Fields{"mapping": c}).Info(
						"Cannot map from IPv6 to an IPv4-only container because the userland proxy is disabled")
				}
				if len(c.HostIP) == 0 && defHostIP.To4() == nil {
					log.G(ctx).WithFields(log.Fields{
						"mapping": c,
						"default": defHostIP,
					}).Info("Cannot map from default host binding address to an IPv4-only container because the userland proxy is disabled")
				}
			} else {
				containerIP = containerIPv4
			}
		}
		if add6 {
			if bindingIPv6, ok := configurePortBindingIPv6(ctx, disableNAT6, c, containerIP, defHostIP); ok {
				reqs = append(reqs, bindingIPv6)
			}
		}
	}
	slices.SortFunc(reqs, func(a, b portmapperapi.PortBindingReq) int {
		return a.Compare(b)
	})
	return reqs
}

// needSamePort returns true iff a and b only differ in the host IP address,
// meaning they should be allocated the same host port (so that, if v4/v6
// addresses are returned in a DNS response or similar, clients can bind without
// needing to adjust the port number depending on which address is used).
func needSamePort(a, b portmapperapi.PortBindingReq) bool {
	return a.Port == b.Port &&
		a.Proto == b.Proto &&
		a.HostPort == b.HostPort &&
		a.HostPortEnd == b.HostPortEnd
}

// configurePortBindingIPv4 returns a new port binding with the HostIP field
// populated and true, if a binding is required. Else, false and an empty
// binding.
func configurePortBindingIPv4(
	ctx context.Context,
	disableNAT bool,
	bnd portmapperapi.PortBindingReq,
	containerIPv4,
	defHostIP net.IP,
) (portmapperapi.PortBindingReq, bool) {
	if len(containerIPv4) == 0 {
		return portmapperapi.PortBindingReq{}, false
	}
	if len(bnd.HostIP) > 0 && bnd.HostIP.To4() == nil {
		// The mapping is explicitly IPv6.
		return portmapperapi.PortBindingReq{}, false
	}
	// If there's no host address, use the default.
	if len(bnd.HostIP) == 0 {
		if defHostIP.To4() == nil {
			// The default binding address is IPv6.
			return portmapperapi.PortBindingReq{}, false
		}
		// The default binding IP is an IPv4 address, use it - unless NAT is disabled,
		// in which case it's not possible to bind to a specific host address (the port
		// mapping only opens the container's port for direct routing).
		if disableNAT {
			bnd.HostIP = net.IPv4zero
		} else {
			bnd.HostIP = defHostIP
		}
	}

	if disableNAT && len(bnd.HostIP) != 0 && !bnd.HostIP.Equal(net.IPv4zero) {
		// Ignore the default binding when nat is disabled - it may have been set
		// up for IPv6 if nat is enabled there.
		// Don't treat this as an error because, as networks are connected/disconnected,
		// the container's gateway endpoint might change to a network where this config
		// makes more sense.
		log.G(ctx).WithFields(log.Fields{"mapping": bnd}).Info(
			"Using address 0.0.0.0 because NAT is disabled")
		bnd.HostIP = net.IPv4zero
	}

	// Unmap the addresses if they're IPv4-mapped IPv6.
	bnd.HostIP = bnd.HostIP.To4()
	bnd.IP = containerIPv4.To4()
	bnd.Mapper = "nat"
	if disableNAT {
		bnd.Mapper = "routed"
	}
	return bnd, true
}

// configurePortBindingIPv6 returns a new port binding with the HostIP field
// populated and true, if a binding is required. Else, false and an empty
// binding.
func configurePortBindingIPv6(
	ctx context.Context,
	disableNAT bool,
	bnd portmapperapi.PortBindingReq,
	containerIP, defHostIP net.IP,
) (portmapperapi.PortBindingReq, bool) {
	if containerIP == nil {
		return portmapperapi.PortBindingReq{}, false
	}
	if len(bnd.HostIP) > 0 && bnd.HostIP.To4() != nil {
		// The mapping is explicitly IPv4.
		return portmapperapi.PortBindingReq{}, false
	}

	// If there's no host address, use the default.
	if len(bnd.HostIP) == 0 {
		if defHostIP.Equal(net.IPv4zero) {
			if !netutils.IsV6Listenable() {
				// No implicit binding if the host has no IPv6 support.
				return portmapperapi.PortBindingReq{}, false
			}
			// Implicit binding to "::", no explicit HostIP and the default is 0.0.0.0
			bnd.HostIP = net.IPv6zero
		} else if defHostIP.To4() == nil {
			// The default binding IP is an IPv6 address, use it - unless NAT is disabled, in
			// which case it's not possible to bind to a specific host address (the port
			// mapping only opens the container's port for direct routing).
			if disableNAT {
				bnd.HostIP = net.IPv6zero
			} else {
				bnd.HostIP = defHostIP
			}
		} else {
			// The default binding IP is an IPv4 address, nothing to do here.
			return portmapperapi.PortBindingReq{}, false
		}
	}

	if disableNAT && len(bnd.HostIP) != 0 && !bnd.HostIP.Equal(net.IPv6zero) {
		// Ignore the default binding when nat is disabled - it may have been set
		// up for IPv4 if nat is enabled there.
		// Don't treat this as an error because, as networks are connected/disconnected,
		// the container's gateway endpoint might change to a network where this config
		// makes more sense.
		log.G(ctx).WithFields(log.Fields{"mapping": bnd}).Info(
			"Using address [::] because NAT is disabled")
		bnd.HostIP = net.IPv6zero
	}

	bnd.IP = containerIP
	bnd.Mapper = "nat"
	if disableNAT {
		bnd.Mapper = "routed"
	}
	return bnd, true
}

// releasePorts attempts to release all port bindings, does not stop on failure
func (n *bridgeNetwork) releasePorts(ep *bridgeEndpoint) error {
	n.Lock()
	pbs := slices.Collect(ep.allPortBindings())
	ep.portMapping = nil
	ep.ephemeralPortMapping = nil
	ep.portBindingState = portBindingMode{}
	n.Unlock()

	return n.unmapPBs(context.TODO(), pbs)
}

func (n *bridgeNetwork) unmapPBs(ctx context.Context, bindings []portmapperapi.PortBinding) error {
	err := n.unmapPorts(ctx, bindings)
	fwErr := n.firewallerNetwork.DelPorts(ctx, collectFirewallPorts(bindings))
	return errors.Join(err, fwErr)
}

// unmapPorts releases port mapper resources and stops proxies without changing
// firewall rules, which may not have been installed yet during rollback.
func (n *bridgeNetwork) unmapPorts(ctx context.Context, bindings []portmapperapi.PortBinding) error {
	pms := n.portMappers()

	var errs []error
	for _, b := range bindings {
		pm, err := pms.Get(b.Mapper)
		if err != nil {
			errs = append(errs, fmt.Errorf("unmapping port binding %s: %w", b.PortBinding, err))
			continue
		}

		if err := pm.UnmapPorts(ctx, []portmapperapi.PortBinding{b}); err != nil {
			errs = append(errs, fmt.Errorf("unmapping port binding %s: %w", b.PortBinding, err))
		}
		if b.StopProxy != nil {
			if err := b.StopProxy(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				errs = append(errs, fmt.Errorf("unmapping port binding %s: failed to stop userland proxy: %w", b.PortBinding, err))
			}
		}
	}

	return errors.Join(errs...)
}

func (n *bridgeNetwork) reapplyPerPortIptables() {
	n.Lock()
	var allPBs []portmapperapi.PortBinding
	var allEPs []*bridgeEndpoint
	for _, ep := range n.endpoints {
		allPBs = slices.AppendSeq(allPBs, ep.allPortBindings())
		allEPs = append(allEPs, ep)
	}
	n.Unlock()

	for _, ep := range allEPs {
		netip4, netip6 := ep.netipAddrs()
		if err := n.firewallerNetwork.AddEndpoint(context.TODO(), netip4, netip6); err != nil {
			log.G(context.TODO()).Warnf("Failed to reconfigure Endpoint: %s", err)
		}
	}

	if err := n.firewallerNetwork.AddPorts(context.Background(), collectFirewallPorts(allPBs)); err != nil {
		log.G(context.TODO()).Warnf("Failed to reconfigure NAT: %s", err)
	}
}

// collectFirewallPorts collects all the types.PortBinding needed to
// reconfigure the host firewall for a given list of port bindings. If one of
// the pbs is NATed, but has an invalid NAT field (i.e. multicast address, or a
// port 0), an error is returned.
func collectFirewallPorts(pbs []portmapperapi.PortBinding) []types.PortBinding {
	var fwPBs []types.PortBinding
	for _, pb := range pbs {
		if pb.NAT.IsValid() {
			if pb.NAT.Addr().IsMulticast() || pb.NAT.Port() == 0 {
				log.G(context.Background()).WithFields(log.Fields{"pb": pb}).Error("invalid NAT address")
				continue
			}
			fwPBs = append(fwPBs, toNATBinding(pb))
		} else if pb.Forwarding {
			fwPBs = append(fwPBs, toFwdBinding(pb))
		}
	}
	return fwPBs
}

// toNATBinding converts a portmapperapi.PortBinding to a types.PortBinding
// that can be passed to firewaller.Network for setting up a NAT rule.
func toNATBinding(pb portmapperapi.PortBinding) types.PortBinding {
	return types.PortBinding{
		IP:            pb.IP,
		Port:          pb.Port,
		Proto:         pb.Proto,
		HostIP:        pb.NAT.Addr().AsSlice(),
		HostPort:      pb.NAT.Port(),
		HostPortEnd:   pb.NAT.Port(),
		NoSelfConnect: pb.NoSelfConnect,
	}
}

// toFwdBinding converts a portmapperapi.PortBinding to a types.PortBinding
// that can be passed to firewaller.Network for setting up forwarding.
func toFwdBinding(pb portmapperapi.PortBinding) types.PortBinding {
	unspecAddr := netip.IPv4Unspecified()
	if pb.IP.To4() == nil {
		unspecAddr = netip.IPv6Unspecified()
	}
	return types.PortBinding{
		IP:     pb.IP,
		Port:   pb.Port,
		Proto:  pb.Proto,
		HostIP: unspecAddr.AsSlice(),
	}
}
