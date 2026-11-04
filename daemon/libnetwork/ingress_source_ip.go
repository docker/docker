package libnetwork

import (
	"errors"
	"net"
	"runtime"
	"strconv"

	"github.com/moby/moby/v2/daemon/libnetwork/netlabel"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
)

const (
	ingressSourceIPOption = "com.docker.network.overlay.preserve_ingress_source_ip"
	ingressGatewayTable   = "ingress_gateway_table"
)

func (n *Network) validateIngressSourceIP() error {
	opts, _ := n.generic[netlabel.GenericData].(map[string]string)
	value, ok := opts[ingressSourceIPOption]
	if !ok {
		return nil
	}
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return types.InvalidParameterErrorf("invalid %s value %q: expected a boolean", ingressSourceIPOption, value)
	}
	if !enabled {
		return nil
	}
	if runtime.GOOS != "linux" {
		return types.NotImplementedErrorf("preserving ingress source IPs is only supported on Linux")
	}
	if n.networkType != "overlay" || !n.ingress || !n.enableIPv4 || n.enableIPv6 {
		return types.InvalidParameterErrorf("%s requires an IPv4-only overlay ingress network", ingressSourceIPOption)
	}
	if _, dsr := opts[overlayDSROptionString]; dsr {
		return types.InvalidParameterErrorf("%s cannot be combined with dsr", ingressSourceIPOption)
	}
	n.loadBalancerMode = loadBalancerModeSourceIP
	return nil
}

func (n *Network) ingressSourceIPPool() (*net.IPNet, error) {
	if len(n.ipamV4Info) != 1 || n.ipamV4Info[0].Pool == nil {
		return nil, errors.New("preserving ingress source IPs requires one IPv4 subnet")
	}
	pool := n.ipamV4Info[0].Pool
	ones, bits := pool.Mask.Size()
	if pool.IP.To4() == nil || bits != 32 || ones < 16 {
		return nil, errors.New("preserving ingress source IPs requires an IPv4 subnet with prefix length of at least /16")
	}
	return pool, nil
}

func (n *Network) validateIngressSourceIPPool() error {
	if n.loadBalancerMode != loadBalancerModeSourceIP {
		return nil
	}
	if _, err := n.ingressSourceIPPool(); err != nil {
		return types.InvalidParameterErrorf("%s: %v", ingressSourceIPOption, err)
	}
	return nil
}
