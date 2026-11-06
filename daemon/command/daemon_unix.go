//go:build unix

package command

import (
	"context"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"time"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/libnetwork/portallocator"
	"github.com/moby/moby/v2/pkg/homedir"
	"github.com/pkg/errors"
	fsutilcopy "github.com/tonistiigi/fsutil/copy" //nolint:depguard // Needed to keep fsutil copy behavior aligned with the daemon umask.
	"golang.org/x/sys/unix"
)

func getDefaultDaemonConfigDir() string {
	if !honorXDG {
		return "/etc/docker"
	}
	// NOTE: CLI uses ~/.docker while the daemon uses ~/.config/docker, because
	// ~/.docker was not designed to store daemon configurations.
	// In future, the daemon directory may be renamed to ~/.config/moby-engine (?).
	configHome, err := homedir.GetConfigHome()
	if err != nil {
		return ""
	}
	return filepath.Join(configHome, "docker")
}

func getDefaultDaemonConfigFile() string {
	dir := getDefaultDaemonConfigDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "daemon.json")
}

// setDefaultUmask sets the umask to 0 to avoid problems
// caused by custom umask
func setDefaultUmask() error {
	desiredUmask := 0
	unix.Umask(desiredUmask)
	if umask := unix.Umask(desiredUmask); umask != desiredUmask {
		return errors.Errorf("failed to set umask: expected %#o, got %#o", desiredUmask, umask)
	}
	fsutilcopy.UmaskIsZero = true

	return nil
}

// setupConfigReloadTrap configures the SIGHUP signal to reload the configuration.
func (cli *daemonCLI) setupConfigReloadTrap(ctx context.Context) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, unix.SIGHUP)

	go func() {
		defer signal.Stop(c)

		for {
			select {
			case <-ctx.Done():
				return
			case <-c:
				if ctx.Err() != nil {
					return
				}
				cli.reloadConfig()
			}
		}
	}()
}

// getSwarmRunRoot gets the root directory for swarm to store runtime state
// For example, the control socket
func getSwarmRunRoot(cfg *config.Config) string {
	return filepath.Join(cfg.ExecRoot, "swarm")
}

// allocateDaemonPort ensures that there are no containers
// that try to use any port allocated for the docker server.
func allocateDaemonPort(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.Wrap(err, "error parsing tcp address")
	}

	intPort, err := strconv.Atoi(port)
	if err != nil {
		return errors.Wrap(err, "error parsing tcp address")
	}

	var hostIPs []net.IP
	if parsedIP := net.ParseIP(host); parsedIP != nil {
		hostIPs = append(hostIPs, parsedIP)
	} else if hostIPs, err = net.LookupIP(host); err != nil {
		return errors.Errorf("failed to lookup %s address in host specification", host)
	}

	pa := portallocator.Get()
	for _, hostIP := range hostIPs {
		if _, err := pa.RequestPort(hostIP, "tcp", intPort); err != nil {
			return errors.Errorf("failed to allocate daemon listening port %d (err: %v)", intPort, err)
		}
	}
	return nil
}

func newCgroupParent(cfg *config.Config) string {
	cgroupParent := "docker"
	useSystemd := daemon.UsingSystemd(cfg)
	if useSystemd {
		cgroupParent = "system.slice"
	}
	if cfg.CgroupParent != "" {
		cgroupParent = cfg.CgroupParent
	}
	if useSystemd {
		cgroupParent = cgroupParent + ":" + "docker" + ":"
	}
	return cgroupParent
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

	waitTimeout, err := cli.initializeContainerd(ctx)
	if errors.Is(err, exec.ErrNotFound) {
		log.G(ctx).WithError(err).Info("containerd binary not found, starting embedded containerd")
		return cli.initEmbeddedContainerd(ctx)
	}
	return waitTimeout, err
}
