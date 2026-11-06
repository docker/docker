package command

import (
	cdcgroups "github.com/containerd/cgroups/v3"
	systemdDaemon "github.com/coreos/go-systemd/v22/daemon"
	"github.com/moby/moby/v2/daemon"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/pkg/sysinfo"
	"github.com/pkg/errors"
)

// setPlatformOptions applies platform-specific CLI configuration options.
func setPlatformOptions(conf *config.Config) error {
	if conf.RemappedRoot == "" {
		return nil
	}

	containerdNamespace, containerdPluginNamespace, err := daemon.RemapContainerdNamespaces(conf)
	if err != nil {
		return err
	}
	conf.ContainerdNamespace = containerdNamespace
	conf.ContainerdPluginNamespace = containerdPluginNamespace

	// Migrating graphdriver layers to the containerd image store is not
	// supported with userns remapping.
	if conf.Features["containerd-migration"] {
		return errors.New("containerd-migration is enabled, but is not compatible with userns remapping. Please disable userns remapping or containerd-migration")
	}

	return nil
}

// preNotifyReady sends a message to the host when the API is active, but before the daemon is
func preNotifyReady() error {
	return nil
}

// notifyReady sends a message to the host when the server is ready to be used
func notifyReady() {
	// Tell the init daemon we are accepting requests
	go systemdDaemon.SdNotify(false, systemdDaemon.SdNotifyReady)
}

// notifyStopping sends a message to the host when the server is shutting down
func notifyStopping() {
	go systemdDaemon.SdNotify(false, systemdDaemon.SdNotifyStopping)
}

// notifyReloading sends a message to the host when the server got signaled to
// reloading its configuration, see [sd_notify(3)]. The server should be running
// as a systemd unit with "Type=notify" or "Type=notify-reload" (see
// [systemd.service(5)]).
//
// notifyReloading returns a callback that must be called after reloading completes
// (either successfully or unsuccessfully) to send [notifyReady].
//
// [sd_notify(3)]: https://www.freedesktop.org/software/systemd/man/latest/sd_notify.html#RELOADING=1
// [systemd.service(5)]: https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html#Type=
func notifyReloading() (done func()) {
	sent, _ := systemdDaemon.SdNotify(false, systemdDaemon.SdNotifyReloading+"\n"+systemdDaemon.SdNotifyMonotonicUsec())
	if !sent {
		// Nothing to do if no reloading event was sent.
		return func() {}
	}
	return notifyReady
}

func validateCPURealtimeOptions(cfg *config.Config) error {
	if cfg.CPURealtimePeriod == 0 && cfg.CPURealtimeRuntime == 0 {
		return nil
	}
	if cdcgroups.Mode() == cdcgroups.Unified {
		return errors.New("daemon-scoped cpu-rt-period and cpu-rt-runtime are not implemented for cgroup v2")
	}
	if !sysinfo.New().CPURealtime {
		return errors.New("daemon-scoped cpu-rt-period and cpu-rt-runtime are not supported by the kernel")
	}
	return nil
}
