package daemon

import (
	"net/netip"
	"os/user"

	"github.com/moby/moby/v2/internal/testutil/environment"
)

// Option is used to configure a daemon.
type Option func(*Daemon)

// WithContainerdSocket sets the --containerd option on the daemon.
// Use an empty string to remove the option.
//
// On Unix, the default is /var/run/docker/containerd/containerd.sock.
// On Windows, the option is omitted by default.
func WithContainerdSocket(socket string) Option {
	return func(d *Daemon) {
		d.containerdSocket = socket
	}
}

func WithUserNsRemap(remap string) Option {
	return func(d *Daemon) {
		d.usernsRemap = remap
	}
}

// WithDefaultCgroupNamespaceMode sets the default cgroup namespace mode for the daemon
func WithDefaultCgroupNamespaceMode(mode string) Option {
	return func(d *Daemon) {
		d.defaultCgroupNamespaceMode = mode
	}
}

// WithTestLogger causes the daemon to log certain actions to the provided test.
func WithTestLogger(t LogT) Option {
	return func(d *Daemon) {
		d.log = t
	}
}

// WithExperimental sets the daemon in experimental mode
func WithExperimental() Option {
	return func(d *Daemon) {
		d.experimental = true
	}
}

// WithInit sets the daemon init
func WithInit() Option {
	return func(d *Daemon) {
		d.init = true
	}
}

// WithDockerdBinary sets the dockerd binary to the specified one
func WithDockerdBinary(dockerdBinary string) Option {
	return func(d *Daemon) {
		d.dockerdBinary = dockerdBinary
	}
}

// WithSwarmPort sets the swarm port to use for swarm mode
func WithSwarmPort(port int) Option {
	return func(d *Daemon) {
		d.SwarmPort = port
	}
}

// WithSwarmListenAddr sets the swarm listen addr to use for swarm mode
func WithSwarmListenAddr(listenAddr string) Option {
	return func(d *Daemon) {
		d.swarmListenAddr = listenAddr
	}
}

// WithSwarmIptables enabled/disables iptables for swarm nodes
func WithSwarmIptables(useIptables bool) Option {
	return func(d *Daemon) {
		d.swarmWithIptables = useIptables
	}
}

// WithUserlandProxy enables/disables the userland proxy, overriding the
// DOCKER_USERLANDPROXY environment variable.
func WithUserlandProxy(enable bool) Option {
	return func(d *Daemon) {
		d.userlandProxy = enable
	}
}

// WithSwarmDefaultAddrPool sets the swarm default address pool to use for swarm mode
func WithSwarmDefaultAddrPool(defaultAddrPool ...netip.Prefix) Option {
	return func(d *Daemon) {
		d.DefaultAddrPool = defaultAddrPool
	}
}

// WithSwarmDefaultAddrPoolSubnetSize sets the subnet length mask of swarm default address pool to use for swarm mode
func WithSwarmDefaultAddrPoolSubnetSize(subnetSize uint32) Option {
	return func(d *Daemon) {
		d.SubnetSize = subnetSize
	}
}

// WithSwarmDataPathPort sets the  swarm datapath port to use for swarm mode
func WithSwarmDataPathPort(datapathPort uint32) Option {
	return func(d *Daemon) {
		d.DataPathPort = datapathPort
	}
}

// WithEnvironment sets options from testutil/environment.Execution struct
func WithEnvironment(e environment.Execution) Option {
	return func(d *Daemon) {
		if e.DaemonInfo.ExperimentalBuild {
			d.experimental = true
		}
	}
}

// WithStorageDriver sets store driver option
func WithStorageDriver(driver string) Option {
	return func(d *Daemon) {
		d.storageDriver = driver
	}
}

// WithRootlessUser sets the daemon to be rootless
func WithRootlessUser(username string) Option {
	return func(d *Daemon) {
		u, err := user.Lookup(username)
		if err != nil {
			panic(err)
		}
		d.rootlessUser = u
	}
}

// WithOOMScoreAdjust sets OOM score for the daemon
func WithOOMScoreAdjust(score int) Option {
	return func(d *Daemon) {
		d.OOMScoreAdjust = score
	}
}

// WithEnvVars sets additional environment variables for the daemon
func WithEnvVars(vars ...string) Option {
	return func(d *Daemon) {
		d.extraEnv = append(d.extraEnv, vars...)
	}
}

// WithResolvConf allows a test to provide content for a resolv.conf file to be used
// as the basis for resolv.conf in the container, instead of the host's /etc/resolv.conf.
func WithResolvConf(content string) Option {
	return func(d *Daemon) {
		d.resolvConfContent = content
	}
}
