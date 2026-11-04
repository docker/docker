//go:build !linux && !windows

package config

import (
	"errors"
	"fmt"

	"github.com/moby/moby/api/types/container"
)

// defaultCgroupNamespaceMode matches Linux hosts without cgroup v2.
func defaultCgroupNamespaceMode() container.CgroupnsMode {
	return DefaultCgroupV1NamespaceMode
}

// setUserlandProxyDefaults leaves the userland proxy disabled because
// docker-proxy is only built for Linux.
func setUserlandProxyDefaults(*BridgeConfig) {}

// validateFixedCIDRV6 accepts any value because the bridge driver that
// validates it is Linux-only.
func validateFixedCIDRV6(string) error {
	return nil
}

func validateFirewallBackend(val string) error {
	if val != "" {
		return errors.New("can only be configured on Linux")
	}
	return nil
}

// validatePlatformExecOpt validates if the given exec-opt and value are valid
// for the current platform.
func validatePlatformExecOpt(opt, value string) error {
	switch opt {
	case "isolation":
		return fmt.Errorf("option '%s' is only supported on windows", opt)
	case "native.cgroupdriver":
		return fmt.Errorf("option '%s' is only supported on linux", opt)
	default:
		return fmt.Errorf("unknown option: '%s'", opt)
	}
}
