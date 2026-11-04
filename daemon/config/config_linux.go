package config

import (
	"context"
	"errors"
	"fmt"

	"github.com/containerd/cgroups/v3"
	"github.com/containerd/log"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge"
)

// userlandProxyBinary is the name of the userland-proxy binary.
const userlandProxyBinary = "docker-proxy"

func defaultCgroupNamespaceMode() container.CgroupnsMode {
	if cgroups.Mode() != cgroups.Unified {
		return DefaultCgroupV1NamespaceMode
	}
	return DefaultCgroupNamespaceMode
}

func setUserlandProxyDefaults(cfg *BridgeConfig) {
	var err error
	cfg.EnableUserlandProxy = true
	cfg.UserlandProxyPath, err = lookupBinPath(userlandProxyBinary)
	if err != nil {
		// Log, but don't error here. This allows running a daemon with
		// userland-proxy disabled (which does not require the binary
		// to be present).
		//
		// An error is still produced by [Config.ValidatePlatformConfig] if
		// userland-proxy is enabled in the configuration.
		//
		// We log this at "debug" level, as this code is also executed
		// when running "--version", and we don't want to print logs in
		// that case..
		log.G(context.TODO()).WithError(err).Debug("failed to lookup default userland-proxy binary")
	}
}

func validateFixedCIDRV6(val string) error {
	return bridge.ValidateFixedCIDRV6(val)
}

func validateFirewallBackend(val string) error {
	switch val {
	case "", "iptables", "nftables":
		return nil
	}
	return errors.New(`allowed values are "iptables" and "nftables"`)
}

// validatePlatformExecOpt validates if the given exec-opt and value are valid
// for the current platform.
func validatePlatformExecOpt(opt, value string) error {
	switch opt {
	case "isolation":
		return fmt.Errorf("option '%s' is only supported on windows", opt)
	case "native.cgroupdriver":
		// TODO(thaJeztah): add validation that's currently in daemon.verifyCgroupDriver
		return nil
	default:
		return fmt.Errorf("unknown option: '%s'", opt)
	}
}
