//go:build linux

package iptables

import (
	"context"
	"errors"
	"net"
	"syscall"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/libnetwork/nlwrap"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/vishvananda/netlink"
)

// checkConntrackProgrammable checks if the handle supports the
// NETLINK_NETFILTER and the base modules are loaded.
func checkConntrackProgrammable(nlh nlwrap.Handle) error {
	if !nlh.SupportsNetlinkFamily(syscall.NETLINK_NETFILTER) {
		return errors.New("conntrack is not available")
	}
	return nil
}

// DeleteConntrackEntries deletes all the conntrack connections on the host for the specified IP
// Returns the number of flows deleted for IPv4, IPv6 else error
func DeleteConntrackEntries(nlh nlwrap.Handle, ipv4List []net.IP, ipv6List []net.IP) error {
	if len(ipv4List) == 0 && len(ipv6List) == 0 {
		return nil
	}
	if err := checkConntrackProgrammable(nlh); err != nil {
		return err
	}

	var totalIPv4FlowPurged uint
	for _, ipAddress := range ipv4List {
		flowPurged, err := purgeConntrackState(nlh, syscall.AF_INET, ipAddress)
		if err != nil {
			log.G(context.TODO()).WithFields(log.Fields{
				"error":     err,
				"ipAddress": ipAddress.String(),
			}).Warn("Failed to delete conntrack state for IPv4-address")
			continue
		}
		totalIPv4FlowPurged += flowPurged
	}

	var totalIPv6FlowPurged uint
	for _, ipAddress := range ipv6List {
		flowPurged, err := purgeConntrackState(nlh, syscall.AF_INET6, ipAddress)
		if err != nil {
			log.G(context.TODO()).WithFields(log.Fields{
				"error":     err,
				"ipAddress": ipAddress.String(),
			}).Warn("Failed to delete conntrack state for IPv6-address")
			continue
		}
		totalIPv6FlowPurged += flowPurged
	}

	if totalIPv4FlowPurged > 0 || totalIPv6FlowPurged > 0 {
		log.G(context.TODO()).WithFields(log.Fields{
			"ipv4": totalIPv4FlowPurged,
			"ipv6": totalIPv6FlowPurged,
		}).Debug("DeleteConntrackEntries completed deleting conntrack state")
	}

	return nil
}

func DeleteConntrackEntriesByPort(nlh nlwrap.Handle, proto types.Protocol, ports []types.PortBinding) error {
	if len(ports) == 0 {
		return nil
	}
	if err := checkConntrackProgrammable(nlh); err != nil {
		return err
	}
	var totalIPv4FlowPurged uint
	var totalIPv6FlowPurged uint
	for _, port := range ports {
		filter := &netlink.ConntrackFilter{}
		if err := filter.AddProtocol(uint8(port.Proto)); err != nil {
			log.G(context.TODO()).WithFields(log.Fields{
				"error":  err,
				"hostIP": port.HostIP.String(),
				"proto":  port.Proto.String(),
				"port":   port.HostPort,
			}).Warn("Failed to delete conntrack state for port")
			continue
		}
		if err := filter.AddPort(netlink.ConntrackOrigDstPort, port.HostPort); err != nil {
			log.G(context.TODO()).WithFields(log.Fields{
				"error":  err,
				"hostIP": port.HostIP.String(),
				"proto":  port.Proto.String(),
				"port":   port.HostPort,
			}).Warn("Failed to delete conntrack state for port")
			continue
		}
		// Only filter by destination IP when the host IP is specified. When
		// HostIP is 0.0.0.0 or ::, the binding applies to all interfaces, but
		// conntrack entries record the actual interface IP, so filtering by the
		// unspecified address would never match.
		if !port.HostIP.IsUnspecified() {
			if err := filter.AddIP(netlink.ConntrackOrigDstIP, port.HostIP); err != nil {
				log.G(context.TODO()).WithFields(log.Fields{
					"error":  err,
					"hostIP": port.HostIP.String(),
					"proto":  port.Proto.String(),
					"port":   port.HostPort,
				}).Warn("Failed to delete conntrack state for port")
				continue
			}
		}

		v4FlowPurged, err := nlh.ConntrackDeleteFilters(netlink.ConntrackTable, syscall.AF_INET, filter)
		if err != nil {
			log.G(context.TODO()).WithFields(log.Fields{
				"error":  err,
				"hostIP": port.HostIP.String(),
				"proto":  port.Proto.String(),
				"port":   port.HostPort,
			}).Warn("Failed to delete conntrack state for IPv4 port")
		}
		totalIPv4FlowPurged += v4FlowPurged

		v6FlowPurged, err := nlh.ConntrackDeleteFilters(netlink.ConntrackTable, syscall.AF_INET6, filter)
		if err != nil {
			log.G(context.TODO()).WithFields(log.Fields{
				"error":  err,
				"hostIP": port.HostIP.String(),
				"proto":  port.Proto.String(),
				"port":   port.HostPort,
			}).Warn("Failed to delete conntrack state for IPv6 port")
		}
		totalIPv6FlowPurged += v6FlowPurged
	}

	if totalIPv4FlowPurged > 0 || totalIPv6FlowPurged > 0 {
		log.G(context.TODO()).WithFields(log.Fields{
			"ipv4":  totalIPv4FlowPurged,
			"ipv6":  totalIPv6FlowPurged,
			"proto": proto.String(),
		}).Debug("DeleteConntrackEntriesByPort completed deleting conntrack state")
	}

	return nil
}

func purgeConntrackState(nlh nlwrap.Handle, family netlink.InetFamily, ipAddress net.IP) (uint, error) {
	filter := &netlink.ConntrackFilter{}
	// NOTE: doing the flush using the ipAddress is safe because today there cannot be multiple networks with the same subnet
	// so it will not be possible to flush flows that are of other containers
	if err := filter.AddIP(netlink.ConntrackNatAnyIP, ipAddress); err != nil {
		return 0, err
	}
	return nlh.ConntrackDeleteFilters(netlink.ConntrackTable, family, filter)
}
