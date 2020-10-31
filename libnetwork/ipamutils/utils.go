// Package ipamutils provides utility functions for ipam management
package ipamutils

import (
	"fmt"
	"net"
	"sync"
)

var (
	// predefinedLocalScopeDefaultNetworks contains a list of 31 IPv4 private
	// networks with host size 16 and 12 (172.17-31.x.x/16, 192.168.x.x/20)
	// which do not overlap with the networks in predefinedGlobalScopeDefaultNetworks
	predefinedLocalScopeDefaultNetworks []*net.IPNet

	// predefinedGlobalScopeDefaultNetworks contains a list of 64K IPv4 private
	// networks with host size 8 (10.x.x.x/24) which do not overlap with the
	// networks in predefinedLocalScopeDefaultNetworks
	predefinedGlobalScopeDefaultNetworks []*net.IPNet

	mutex sync.RWMutex

	localScopeDefaultNetworks = []*NetworkToSplit{
		{"172.17.0.0/16", 16},
		{"172.18.0.0/16", 16},
		{"172.19.0.0/16", 16},
		{"172.20.0.0/14", 16},
		{"172.24.0.0/14", 16},
		{"172.28.0.0/14", 16},
		{"192.168.0.0/16", 20},
	}

	globalScopeDefaultNetworks = []*NetworkToSplit{
		{"10.0.0.0/8", 24},
	}
)

// NetworkToSplit represent a network that has to be split in chunks with mask length Size.
// Each subnet in the set is derived from the Base pool. Base is to be passed
// in CIDR format.
// Example: a Base "10.10.0.0/16 with Size 24 will define the set of 256
// 10.10.[0-255].0/24 address pools
type NetworkToSplit struct {
	Base string `json:"base"`
	Size int    `json:"size"`
}

func init() {
	var err error
	if predefinedGlobalScopeDefaultNetworks, err = splitNetworks(globalScopeDefaultNetworks); err != nil {
		// we are going to panic in case of error as we should never get into this state
		panic("InitAddressPools failed to initialize the global scope default address pool")
	}

	if predefinedLocalScopeDefaultNetworks, err = splitNetworks(localScopeDefaultNetworks); err != nil {
		// we are going to panic in case of error as we should never get into this state
		panic("InitAddressPools failed to initialize the local scope default address pool")
	}
}

// configDefaultNetworks configures local as well global default pool based on input
func configDefaultNetworks(defaultAddressPool []*NetworkToSplit, result *[]*net.IPNet) error {
	mutex.Lock()
	defer mutex.Unlock()
	defaultNetworks, err := splitNetworks(defaultAddressPool)
	if err != nil {
		return err
	}
	*result = defaultNetworks
	return nil
}

// GetGlobalScopeDefaultNetworks returns a list of 64K IPv4 private networks with
// host size 8 (10.x.x.x/24) which do not overlap with the networks returned by
// GetLocalScopeDefaultNetworks
func GetGlobalScopeDefaultNetworks() []*net.IPNet {
	mutex.RLock()
	defer mutex.RUnlock()
	return predefinedGlobalScopeDefaultNetworks
}

// GetLocalScopeDefaultNetworks returns a list of 31 IPv4 private networks with
// host size 16 and 12 (172.17-31.x.x/16, 192.168.x.x/20) which do not overlap
// with the networks returned by GetGlobalScopeDefaultNetworks.
func GetLocalScopeDefaultNetworks() []*net.IPNet {
	mutex.RLock()
	defer mutex.RUnlock()
	return predefinedLocalScopeDefaultNetworks
}

// ConfigGlobalScopeDefaultNetworks configures global default pool.
// Ideally this will be called from SwarmKit as part of swarm init
func ConfigGlobalScopeDefaultNetworks(defaultAddressPool []*NetworkToSplit) error {
	if defaultAddressPool == nil {
		defaultAddressPool = globalScopeDefaultNetworks
	}
	return configDefaultNetworks(defaultAddressPool, &predefinedGlobalScopeDefaultNetworks)
}

// ConfigLocalScopeDefaultNetworks configures local default pool.
// Ideally this will be called during libnetwork init
func ConfigLocalScopeDefaultNetworks(defaultAddressPool []*NetworkToSplit) error {
	if defaultAddressPool == nil {
		return nil
	}
	return configDefaultNetworks(defaultAddressPool, &predefinedLocalScopeDefaultNetworks)
}

// splitNetworks takes a slice of networks, split them accordingly and returns them
func splitNetworks(list []*NetworkToSplit) ([]*net.IPNet, error) {
	localPools := make([]*net.IPNet, 0, len(list))

	for _, p := range list {
		_, b, err := net.ParseCIDR(p.Base)
		if err != nil {
			return nil, fmt.Errorf("invalid base pool %q: %v", p.Base, err)
		}
		ones, _ := b.Mask.Size()
		if p.Size <= 0 || p.Size < ones {
			return nil, fmt.Errorf("invalid pools size: %d", p.Size)
		}
		localPools = append(localPools, splitNetwork(p.Size, b)...)
	}
	return localPools, nil
}

func splitNetwork(size int, base *net.IPNet) []*net.IPNet {
	one, bits := base.Mask.Size()
	mask := net.CIDRMask(size, bits)
	n := 1 << uint(size-one)
	s := uint(bits - size)
	list := make([]*net.IPNet, 0, n)

	for i := 0; i < n; i++ {
		ip := copyIP(base.IP)
		addIntToIP(ip, uint(i<<s))
		list = append(list, &net.IPNet{IP: ip, Mask: mask})
	}
	return list
}

func copyIP(from net.IP) net.IP {
	ip := make([]byte, len(from))
	copy(ip, from)
	return ip
}

func addIntToIP(array net.IP, ordinal uint) {
	for i := len(array) - 1; i >= 0; i-- {
		array[i] |= (byte)(ordinal & 0xff)
		ordinal >>= 8
	}
}
