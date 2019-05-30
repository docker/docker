package portmapper

import (
	"net"
	"sync"

	"github.com/docker/libnetwork/iptables"
	"github.com/docker/libnetwork/portallocator"
)

// PortMapper manages the network address translation
type PortMapper struct {
	bridgeName string

	// udp:ip:port
	currentMappings map[string]*mapping
	lock            sync.Mutex

	proxyPath string

	Allocator *portallocator.PortAllocator
	chain     *iptables.ChainInfo
}

// SetIptablesChain sets the specified chain into portmapper
func (pm *PortMapper) SetIptablesChain(c *iptables.ChainInfo, bridgeName string) {
	pm.chain = c
	pm.bridgeName = bridgeName
}

// AppendForwardingTableEntry adds a port mapping to the forwarding table
func (pm *PortMapper) AppendForwardingTableEntry(proto string, sourceIP net.IP, sourcePort int, containerIP string, containerPort int) error {
	return pm.forward(iptables.Append, proto, sourceIP, sourcePort, containerIP, containerPort)
}

// DeleteForwardingTableEntry removes a port mapping from the forwarding table
func (pm *PortMapper) DeleteForwardingTableEntry(proto string, sourceIP net.IP, sourcePort int, containerIP string, containerPort int) error {
	return pm.forward(iptables.Delete, proto, sourceIP, sourcePort, containerIP, containerPort)
}

func (pm *PortMapper) forward(action iptables.Action, proto string, sourceIP net.IP, sourcePort int, containerIP string, containerPort int) error {
	if pm.chain == nil {
		return nil
	}
	return pm.chain.Forward(action, sourceIP, sourcePort, proto, containerIP, containerPort, pm.bridgeName)
}
