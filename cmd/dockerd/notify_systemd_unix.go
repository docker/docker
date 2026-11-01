//go:build !no_systemd && !freebsd

package main

import (
	cdcgroups "github.com/containerd/cgroups/v3"
	systemdDaemon "github.com/coreos/go-systemd/v22/daemon"
	"github.com/docker/docker/daemon/config"
	"github.com/docker/docker/pkg/sysinfo"
	"github.com/pkg/errors"
)

// preNotifyReady sends a message to the host when the API is active, but before the daemon is
func preNotifyReady() {
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

func validateCPURealtimeOptions(config *config.Config) error {
	if config.CPURealtimePeriod == 0 && config.CPURealtimeRuntime == 0 {
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
