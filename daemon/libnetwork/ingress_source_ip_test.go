package libnetwork

import (
	"net"
	"runtime"
	"testing"

	"github.com/moby/moby/v2/daemon/libnetwork/netlabel"
	"gotest.tools/v3/assert"
)

func TestIngressSourceIPOption(t *testing.T) {
	for _, tc := range []struct {
		name, value, driver string
		ingress, ipv4, ipv6 bool
		dsr                 bool
		wantError           bool
	}{
		{name: "default", driver: "overlay", ingress: true, ipv4: true},
		{name: "disabled", value: "false", driver: "bridge", ipv4: true},
		{name: "enabled", value: "true", driver: "overlay", ingress: true, ipv4: true, wantError: runtime.GOOS != "linux"},
		{name: "invalid boolean", value: "enabled", driver: "overlay", ingress: true, ipv4: true, wantError: true},
		{name: "bridge", value: "true", driver: "bridge", ingress: true, ipv4: true, wantError: true},
		{name: "ordinary overlay", value: "true", driver: "overlay", ipv4: true, wantError: true},
		{name: "IPv6", value: "true", driver: "overlay", ingress: true, ipv4: true, ipv6: true, wantError: true},
		{name: "no IPv4", value: "true", driver: "overlay", ingress: true, wantError: true},
		{name: "DSR", value: "true", driver: "overlay", ingress: true, ipv4: true, dsr: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := map[string]string{}
			if tc.value != "" {
				opts[ingressSourceIPOption] = tc.value
			}
			if tc.dsr {
				opts[overlayDSROptionString] = "true"
			}
			n := &Network{networkType: tc.driver, ingress: tc.ingress, enableIPv4: tc.ipv4, enableIPv6: tc.ipv6,
				loadBalancerMode: loadBalancerModeNAT, generic: map[string]any{netlabel.GenericData: opts}}
			err := n.validateIngressSourceIP()
			if tc.wantError {
				assert.Assert(t, err != nil)
			} else {
				assert.NilError(t, err)
				want := loadBalancerModeNAT
				if tc.value == "true" {
					want = loadBalancerModeSourceIP
				}
				assert.Equal(t, n.loadBalancerMode, want)
			}
		})
	}
}

func TestIngressSourceIPPool(t *testing.T) {
	for _, cidr := range []string{"10.66.0.0/16", "10.66.0.0/24", "10.64.0.0/12", "2001:db8::/64"} {
		t.Run(cidr, func(t *testing.T) {
			_, pool, err := net.ParseCIDR(cidr)
			assert.NilError(t, err)
			info := &IpamInfo{}
			info.Pool = pool
			n := &Network{loadBalancerMode: loadBalancerModeSourceIP, ipamV4Info: []*IpamInfo{info}}
			err = n.validateIngressSourceIPPool()
			if cidr == "10.66.0.0/16" || cidr == "10.66.0.0/24" {
				assert.NilError(t, err)
			} else {
				assert.Assert(t, err != nil)
			}
		})
	}
	n := &Network{loadBalancerMode: loadBalancerModeSourceIP}
	assert.Assert(t, n.validateIngressSourceIPPool() != nil)
	n.loadBalancerMode = loadBalancerModeNAT
	assert.NilError(t, n.validateIngressSourceIPPool())
}
