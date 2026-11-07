package network_test

import (
	"net/netip"
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/network"
)

func TestEndpointSettingsCopy(t *testing.T) {
	t.Parallel()

	original := &network.EndpointSettings{
		IPAMConfig: &network.EndpointIPAMConfig{
			IPv4Address:  netip.MustParseAddr("192.0.2.2"),
			LinkLocalIPs: []netip.Addr{netip.MustParseAddr("169.254.1.1")},
		},
		Links:      []string{"linked:alias"},
		Aliases:    []string{"alias"},
		DNSNames:   []string{"container"},
		DriverOpts: map[string]string{"key": "value"},
		MacAddress: network.HardwareAddr{0x02, 0x42, 0xac, 0x11, 0x00, 0x02},
	}
	copied := original.Copy()
	if !reflect.DeepEqual(copied, original) {
		t.Fatalf("Copy() = %+v, want %+v", copied, original)
	}

	copied.IPAMConfig.IPv4Address = netip.MustParseAddr("192.0.2.3")
	copied.IPAMConfig.LinkLocalIPs[0] = netip.MustParseAddr("169.254.1.2")
	copied.Links[0] = "other:alias"
	copied.Aliases[0] = "other-alias"
	copied.DNSNames[0] = "other-container"
	copied.DriverOpts["key"] = "other-value"
	copied.MacAddress[5] = 0x03

	assertEqual(t, original.IPAMConfig.IPv4Address, netip.MustParseAddr("192.0.2.2"))
	assertEqual(t, original.IPAMConfig.LinkLocalIPs[0], netip.MustParseAddr("169.254.1.1"))
	assertEqual(t, original.Links[0], "linked:alias")
	assertEqual(t, original.Aliases[0], "alias")
	assertEqual(t, original.DNSNames[0], "container")
	assertEqual(t, original.DriverOpts["key"], "value")
	assertEqual(t, original.MacAddress.String(), "02:42:ac:11:00:02")
}
