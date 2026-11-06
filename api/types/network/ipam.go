package network

import (
	"encoding/json"
	"net/netip"
)

// IPAM represents IP Address Management
type IPAM struct {
	Driver  string
	Options map[string]string // Per network IPAM driver options
	Config  []IPAMConfig
}

// IPAMConfig represents IPAM configurations
type IPAMConfig struct {
	Subnet     netip.Prefix          `json:"Subnet,omitzero"`
	IPRange    netip.Prefix          `json:"IPRange,omitzero"`
	Gateway    netip.Addr            `json:"Gateway,omitzero"`
	AuxAddress map[string]netip.Addr `json:"AuxiliaryAddresses,omitempty"`
}

// UnmarshalJSON accepts legacy daemon responses that encoded Gateway as a
// prefix, while keeping strict parsing for all non-legacy invalid addresses.
func (c *IPAMConfig) UnmarshalJSON(data []byte) error {
	var raw struct {
		Subnet     netip.Prefix          `json:"Subnet"`
		IPRange    netip.Prefix          `json:"IPRange"`
		Gateway    *string               `json:"Gateway"`
		AuxAddress map[string]netip.Addr `json:"AuxiliaryAddresses"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	if raw.Gateway != nil {
		gateway, err := parseGatewayAddr(*raw.Gateway)
		if err != nil {
			return err
		}
		c.Gateway = gateway
	}

	c.Subnet = raw.Subnet
	c.IPRange = raw.IPRange
	c.AuxAddress = raw.AuxAddress
	return nil
}

func parseGatewayAddr(value string) (netip.Addr, error) {
	if value == "" {
		return netip.Addr{}, nil
	}
	addr, addrErr := netip.ParseAddr(value)
	if addrErr == nil {
		return addr, nil
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return netip.Addr{}, addrErr
	}
	return prefix.Addr(), nil
}

type SubnetStatuses = map[netip.Prefix]SubnetStatus
