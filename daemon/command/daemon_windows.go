package command

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/config"
	"golang.org/x/sys/windows"
)

// getDefaultDaemonConfigFile returns the default location of the daemon's
// configuration file.
//
// On Windows, the location of the config-file is relative to the daemon's
// data-root (config.Root), which is configurable, so we cannot use a fixed
// default location, and this function always returns an empty string.
func getDefaultDaemonConfigFile() string {
	return ""
}

// setPlatformOptions applies platform-specific CLI configuration options.
func setPlatformOptions(cfg *config.Config) error {
	if cfg.Pidfile == "" {
		// On Windows, the pid-file location is relative to the daemon's data-root,
		// which is configurable, so we cannot use a fixed default location.
		// Instead, we set the location here, after we parsed command-line flags
		// and loaded the configuration file (if any).
		cfg.Pidfile = filepath.Join(cfg.Root, "docker.pid")
	}
	return nil
}

// setDefaultUmask doesn't do anything on windows
func setDefaultUmask() error {
	return nil
}

// preNotifyReady sends a message to the host when the API is active, but before the daemon is
func preNotifyReady() error {
	// start the service now to prevent timeouts waiting for daemon to start
	// but still (eventually) complete all requests that are sent after this
	if service != nil {
		err := service.started()
		if err != nil {
			return err
		}
	}
	return nil
}

// notifyReady sends a message to the host when the server is ready to be used
func notifyReady() {
}

// notifyReloading sends a message to the host when the server got signaled to
// reloading its configuration. It is a no-op on Windows.
func notifyReloading() func() { return func() {} }

// notifyStopping sends a message to the host when the server is shutting down
func notifyStopping() {
}

// notifyShutdown is called after the daemon shuts down but before the process exits.
func notifyShutdown(ctx context.Context, err error) {
	if service != nil {
		// log the error, so that it's sent to the event-log.
		if err != nil {
			log.G(ctx).WithError(err).Error("Stopping service")
		} else {
			log.G(ctx).Info("Stopping service")
		}
		service.stopped(err)
	}
}

// setupConfigReloadTrap configures a Win32 event to reload the configuration.
func (cli *daemonCLI) setupConfigReloadTrap(ctx context.Context) {
	go func() {
		event := `Global\docker-daemon-config-` + strconv.Itoa(os.Getpid())
		ev, _ := windows.UTF16PtrFromString(event)
		h, err := windows.CreateEvent(nil, 0, 0, ev)
		if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			log.G(ctx).WithError(err).Errorf("Failed to create config reload event %s", event)
			return
		}
		stopDone := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			// Wake the blocking wait so the goroutine can exit on shutdown.
			_ = windows.SetEvent(h)
			close(stopDone)
		})
		defer func() {
			if !stop() {
				<-stopDone
			}
			_ = windows.CloseHandle(h)
		}()

		log.G(ctx).WithField("event", event).Info("Registered config reload event")
		for {
			windows.WaitForSingleObject(h, windows.INFINITE)
			if ctx.Err() != nil {
				return
			}
			cli.reloadConfig()
		}
	}()
}

// getSwarmRunRoot gets the root directory for swarm to store runtime state
// For example, the control socket
func getSwarmRunRoot(*config.Config) string {
	return ""
}

func allocateDaemonPort(addr string) error {
	return nil
}

func newCgroupParent(*config.Config) string {
	return ""
}

func (cli *daemonCLI) initContainerd(ctx context.Context) (func(time.Duration) error, error) {
	// Check embedded-containerd first so daemon.json can opt in even when
	// packaged service units pass a default --containerd socket.
	if cli.Config.Features["embedded-containerd"] {
		return cli.initEmbeddedContainerd(ctx)
	}
	if cli.Config.ContainerdAddr != "" {
		// use system containerd at the given address.
		cli.warnUnusedContainerdDropIns(ctx)
		return nopWaitFunc, nil
	}

	if cli.Config.DefaultRuntime == "" || cli.Config.DefaultRuntime == config.WindowsV1RuntimeName {
		// Legacy non-containerd runtime is used
		cli.warnUnusedContainerdDropIns(ctx)
		return nopWaitFunc, nil
	}

	waitTimeout, err := cli.initializeContainerd(ctx)
	if errors.Is(err, exec.ErrNotFound) {
		log.G(ctx).WithError(err).Info("containerd binary not found, starting embedded containerd")
		return cli.initEmbeddedContainerd(ctx)
	}
	return waitTimeout, err
}

func validateCPURealtimeOptions(_ *config.Config) error {
	return nil
}
