package config

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/containerd/cgroups/v3"
	"github.com/containerd/log"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge"
	"github.com/pkg/errors"
)

// userlandProxyBinary is the name of the userland-proxy binary.
const userlandProxyBinary = "docker-proxy"

func defaultCgroupNamespaceMode() container.CgroupnsMode {
	if cgroups.Mode() != cgroups.Unified {
		return DefaultCgroupV1NamespaceMode
	}
	return DefaultCgroupNamespaceMode
}

func defaultUserlandProxyPath() string {
	path, err := lookupBinPath(userlandProxyBinary)
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
	return path
}

// verifyUserlandProxyConfig verifies if a valid userland-proxy path
// is configured if userland-proxy is enabled.
func verifyUserlandProxyConfig(conf *Config) error {
	if !conf.EnableUserlandProxy {
		return nil
	}
	if conf.UserlandProxyPath == "" {
		return errors.New("invalid userland-proxy-path: userland-proxy is enabled, but userland-proxy-path is not set")
	}
	if !filepath.IsAbs(conf.UserlandProxyPath) {
		return errors.New("invalid userland-proxy-path: must be an absolute path: " + conf.UserlandProxyPath)
	}
	// Using exec.LookPath here, because it also produces an error if the
	// given path is not a valid executable or a directory.
	if _, err := exec.LookPath(conf.UserlandProxyPath); err != nil {
		return errors.Wrap(err, "invalid userland-proxy-path")
	}

	return nil
}

func validateFixedCIDRV6(val string) error {
	return bridge.ValidateFixedCIDRV6(val)
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
