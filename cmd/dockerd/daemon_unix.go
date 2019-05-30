// +build !windows

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"time"

	"github.com/containerd/containerd/runtime/v1/linux"
	"github.com/docker/docker/cmd/dockerd/hack"
	"github.com/docker/docker/daemon"
	"github.com/docker/docker/daemon/config"
	"github.com/docker/docker/libcontainerd/supervisor"
	"github.com/docker/docker/pkg/homedir"
	"github.com/docker/libnetwork/portallocator"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

func getDefaultDaemonConfigDir() (string, error) {
	if !honorXDG {
		return "/etc/docker", nil
	}
	// NOTE: CLI uses ~/.docker while the daemon uses ~/.config/docker, because
	// ~/.docker was not designed to store daemon configurations.
	// In future, the daemon directory may be renamed to ~/.config/moby-engine (?).
	configHome, err := homedir.GetConfigHome()
	if err != nil {
		return "", nil
	}
	return filepath.Join(configHome, "docker"), nil
}

func getDefaultDaemonConfigFile() (string, error) {
	dir, err := getDefaultDaemonConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.json"), nil
}

// setDefaultUmask sets the umask to 0022 to avoid problems
// caused by custom umask
func setDefaultUmask() error {
	desiredUmask := 0022
	unix.Umask(desiredUmask)
	if umask := unix.Umask(desiredUmask); umask != desiredUmask {
		return fmt.Errorf("failed to set umask: expected %#o, got %#o", desiredUmask, umask)
	}

	return nil
}

func (cli *DaemonCli) getPlatformContainerdDaemonOpts() ([]supervisor.DaemonOpt, error) {
	opts := []supervisor.DaemonOpt{
		supervisor.WithOOMScore(cli.Config.OOMScoreAdjust),
		supervisor.WithPlugin("linux", &linux.Config{
			Shim:        daemon.DefaultShimBinary,
			Runtime:     daemon.DefaultRuntimeBinary,
			RuntimeRoot: filepath.Join(cli.Config.Root, "runc"),
			ShimDebug:   cli.Config.Debug,
		}),
	}

	return opts, nil
}

// setupConfigReloadTrap configures the USR2 signal to reload the configuration.
func (cli *DaemonCli) setupConfigReloadTrap() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, unix.SIGHUP)
	go func() {
		for range c {
			cli.reloadConfig()
		}
	}()
}

// getSwarmRunRoot gets the root directory for swarm to store runtime state
// For example, the control socket
func (cli *DaemonCli) getSwarmRunRoot() string {
	return filepath.Join(cli.Config.ExecRoot, "swarm")
}

// allocateDaemonPort ensures that there are no containers
// that try to use any port allocated for the docker server.
func allocateDaemonPort(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}

	intPort, err := strconv.Atoi(port)
	if err != nil {
		return err
	}

	var hostIPs []net.IP
	if parsedIP := net.ParseIP(host); parsedIP != nil {
		hostIPs = append(hostIPs, parsedIP)
	} else if hostIPs, err = net.LookupIP(host); err != nil {
		return fmt.Errorf("failed to lookup %s address in host specification", host)
	}

	pa := portallocator.Get()
	for _, hostIP := range hostIPs {
		if _, err := pa.RequestPort(hostIP, "tcp", intPort); err != nil {
			return fmt.Errorf("failed to allocate daemon listening port %d (err: %v)", intPort, err)
		}
	}
	return nil
}

func wrapListeners(proto string, ls []net.Listener) []net.Listener {
	switch proto {
	case "unix":
		ls[0] = &hack.MalformedHostHeaderOverride{Listener: ls[0]}
	case "fd":
		for i := range ls {
			ls[i] = &hack.MalformedHostHeaderOverride{Listener: ls[i]}
		}
	}
	return ls
}

func newCgroupParent(config *config.Config) string {
	cgroupParent := "docker"
	useSystemd := daemon.UsingSystemd(config)
	if useSystemd {
		cgroupParent = "system.slice"
	}
	if config.CgroupParent != "" {
		cgroupParent = config.CgroupParent
	}
	if useSystemd {
		cgroupParent = cgroupParent + ":" + "docker" + ":"
	}
	return cgroupParent
}

func (cli *DaemonCli) initContainerD(ctx context.Context) (func(time.Duration) error, error) {
	var waitForShutdown func(time.Duration) error
	if cli.Config.ContainerdAddr == "" {
		systemContainerdAddr, ok, err := systemContainerdRunning(honorXDG)
		if err != nil {
			return nil, errors.Wrap(err, "could not determine whether the system containerd is running")
		}
		if !ok {
			logrus.Debug("Containerd not running, starting daemon managed containerd")
			opts, err := cli.getContainerdDaemonOpts()
			if err != nil {
				return nil, errors.Wrap(err, "failed to generate containerd options")
			}

			r, err := supervisor.Start(ctx, filepath.Join(cli.Config.Root, "containerd"), filepath.Join(cli.Config.ExecRoot, "containerd"), opts...)
			if err != nil {
				return nil, errors.Wrap(err, "failed to start containerd")
			}
			logrus.Debug("Started daemon managed containerd")
			cli.Config.ContainerdAddr = r.Address()

			// Try to wait for containerd to shutdown
			waitForShutdown = r.WaitTimeout
		} else {
			cli.Config.ContainerdAddr = systemContainerdAddr
		}
	}

	return waitForShutdown, nil
}
