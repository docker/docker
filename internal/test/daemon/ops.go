package daemon

import "github.com/docker/docker/internal/test/environment"

// WithExperimental sets the daemon in experimental mode
func WithExperimental(d *Daemon) {
	d.experimental = true
	d.init = true
}

// WithInit sets the daemon init
func WithInit(d *Daemon) {
	d.init = true
}

// WithDockerdBinary sets the dockerd binary to the specified one
func WithDockerdBinary(dockerdBinary string) func(*Daemon) {
	return func(d *Daemon) {
		d.dockerdBinary = dockerdBinary
	}
}

// WithSwarmPort sets the swarm port to use for swarm mode
func WithSwarmPort(port int) func(*Daemon) {
	return func(d *Daemon) {
		d.SwarmPort = port
	}
}

// WithSwarmListenAddr sets the swarm listen addr to use for swarm mode
func WithSwarmListenAddr(listenAddr string) func(*Daemon) {
	return func(d *Daemon) {
		d.swarmListenAddr = listenAddr
	}
}

// WithSwarmDefaultAddrPool sets the swarm default address pool to use for swarm mode
func WithSwarmDefaultAddrPool(defaultAddrPool []string) func(*Daemon) {
	return func(d *Daemon) {
		d.DefaultAddrPool = defaultAddrPool
	}
}

// WithSwarmDefaultAddrPoolSubnetSize sets the subnet length mask of swarm default address pool to use for swarm mode
func WithSwarmDefaultAddrPoolSubnetSize(subnetSize uint32) func(*Daemon) {
	return func(d *Daemon) {
		d.SubnetSize = subnetSize
	}
}

// WithSwarmDataPathPort sets the  swarm datapath port to use for swarm mode
func WithSwarmDataPathPort(datapathPort uint32) func(*Daemon) {
	return func(d *Daemon) {
		d.DataPathPort = datapathPort
	}
}

// WithEnvironment sets options from internal/test/environment.Execution struct
func WithEnvironment(e environment.Execution) func(*Daemon) {
	return func(d *Daemon) {
		if e.DaemonInfo.ExperimentalBuild {
			d.experimental = true
		}
	}
}

// WithStorageDriver sets store driver option
func WithStorageDriver(driver string) func(d *Daemon) {
	return func(d *Daemon) {
		d.storageDriver = driver
	}
}
