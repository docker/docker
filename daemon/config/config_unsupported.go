//go:build !linux && !windows

package config

import (
	"fmt"

	"github.com/moby/moby/api/types/container"
)

// defaultCgroupNamespaceMode matches Linux hosts without cgroup v2.
func defaultCgroupNamespaceMode() container.CgroupnsMode {
	return DefaultCgroupV1NamespaceMode
}

// defaultUserlandProxyPath returns an empty path because docker-proxy is only
// built for Linux.
func defaultUserlandProxyPath() string {
	return ""
}

// verifyUserlandProxyConfig accepts any userland-proxy settings because only
// the Linux bridge driver uses them.
func verifyUserlandProxyConfig(*Config) error {
	return nil
}

// validateFixedCIDRV6 accepts any value because the bridge driver that
// validates it is Linux-only.
func validateFixedCIDRV6(string) error {
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
