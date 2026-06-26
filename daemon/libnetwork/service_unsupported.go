//go:build !linux && !windows

package libnetwork

import (
	"errors"
	"net"
)

// osSandbox is the OS-specific state a [Sandbox] carries. There is none on this
// platform.
type osSandbox struct{}

type osLoadBalancer struct{}

func (c *Controller) cleanupServiceDiscovery(cleanupNID string) {}

func (c *Controller) cleanupServiceBindings(nid string) {}

func (c *Controller) addServiceBinding(name, sid, nid, eid string, vip net.IP, ingressPorts []*PortConfig, aliases []string, ip net.IP) error {
	return errors.New("not supported")
}

func (c *Controller) rmServiceBinding(name, sid, nid, eid string, vip net.IP, ingressPorts []*PortConfig, aliases []string, ip net.IP) error {
	return errors.New("not supported")
}

func (sb *Sandbox) populateLoadBalancers(*Endpoint) {}

func arrangeIngressFilterRule() {}
