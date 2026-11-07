package libnetwork

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/containerd/log"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/nftables"
	"github.com/moby/moby/v2/daemon/libnetwork/iptables"
	"github.com/moby/moby/v2/daemon/libnetwork/osl"
)

// FirewallBackend returns FirewallInfo for "docker info".
// Despite the name, FirewallInfo may include information about the userland proxy as well.
func (c *Controller) FirewallBackend() *system.FirewallInfo {
	var info system.FirewallInfo
	info.Driver = "iptables"
	if nftables.Enabled() {
		info.Driver = "nftables"
	}
	if iptables.UsingFirewalld() {
		info.Driver += "+firewalld"
		if reloadedAt := iptables.FirewalldReloadedAt(); !reloadedAt.IsZero() {
			info.Info = [][2]string{{"ReloadedAt", reloadedAt.Format(time.RFC3339)}}
		}
	}
	if c.cfg.EnableUserlandProxy {
		// EnableUserlandProxy is exposed in FirewallInfo since Docker v29.5.
		// In older versions, EnableUserlandProxy is not included in FirewallInfo,
		// but it is still used in most cases as it is enabled by default.
		info.Info = append(info.Info, [2]string{"EnableUserlandProxy", "true"})
		if c.cfg.UserlandProxyPath != "" {
			info.Info = append(info.Info, [2]string{"UserlandProxyPath", c.cfg.UserlandProxyPath})
		}
	}
	return &info
}

// enabledIptablesVersions returns the iptables versions that are enabled
// for the controller.
func (c *Controller) enabledIptablesVersions() []iptables.IPVersion {
	var versions []iptables.IPVersion
	if c.cfg.BridgeConfig.EnableIPTables {
		versions = append(versions, iptables.IPv4)
	}
	if c.cfg.BridgeConfig.EnableIP6Tables {
		versions = append(versions, iptables.IPv6)
	}
	return versions
}

// getDefaultOSLSandbox returns the controller's default [osl.Sandbox]. It
// creates the sandbox if it does not yet exist.
func (c *Controller) getDefaultOSLSandbox(key string) (*osl.Namespace, error) {
	var err error
	c.defOsSboxOnce.Do(func() {
		c.defOsSbox, err = osl.NewSandbox(key, false, false)
	})

	if err != nil {
		c.defOsSboxOnce = sync.Once{}
		return nil, fmt.Errorf("failed to create default sandbox: %v", err)
	}
	return c.defOsSbox, nil
}

// setupOSLSandbox sets the sandbox [osl.Sandbox], and applies operating-
// specific configuration.
//
// Depending on the Sandbox settings, it may either use the Controller's
// default sandbox, or configure a new one.
func (c *Controller) setupOSLSandbox(sb *Sandbox) error {
	if sb.config.useDefaultSandBox {
		defSB, err := c.getDefaultOSLSandbox(sb.Key())
		if err != nil {
			return err
		}
		sb.osSbox = defSB
	}

	if sb.osSbox == nil && !sb.config.useExternalKey {
		newSB, err := osl.NewSandbox(sb.Key(), !sb.config.useDefaultSandBox, false)
		if err != nil {
			return fmt.Errorf("failed to create new osl sandbox: %v", err)
		}
		sb.osSbox = newSB
	}

	if sb.osSbox != nil {
		// Apply operating specific knobs on the load balancer sandbox
		err := sb.osSbox.InvokeFunc(func() {
			sb.osSbox.ApplyOSTweaks(sb.oslTypes)
		})
		if err != nil {
			log.G(context.TODO()).Errorf("Failed to apply performance tuning sysctls to the sandbox: %v", err)
		}
		if !nftables.Enabled() {
			// Keep this just so performance is not changed
			sb.osSbox.ApplyOSTweaks(sb.oslTypes)
		}
	}
	return nil
}
