package network // import "github.com/docker/docker/integration/network"

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	swarmtypes "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/client"
	"github.com/docker/docker/integration/internal/network"
	"github.com/docker/docker/integration/internal/swarm"
	"github.com/docker/docker/testutil/daemon"
	"gotest.tools/assert"
	"gotest.tools/poll"
	"gotest.tools/skip"
)

func TestDaemonRestartWithLiveRestore(t *testing.T) {
	skip.If(t, testEnv.OSType == "windows")
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.38"), "skip test from new feature")
	d := daemon.New(t)
	defer d.Stop(t)
	d.Start(t)
	d.Restart(t,
		"--live-restore=true",
		"--default-address-pool", "base=175.30.0.0/16,size=16",
		"--default-address-pool", "base=175.33.0.0/16,size=24",
	)

	// Verify bridge network's subnet
	c := d.NewClientT(t)
	defer c.Close()
	out, err := c.NetworkInspect(context.Background(), "bridge", types.NetworkInspectOptions{})
	assert.NilError(t, err)
	// Make sure docker0 doesn't get override with new IP in live restore case
	assert.Equal(t, out.IPAM.Config[0].Subnet, "172.18.0.0/16")
}

func TestDaemonDefaultNetworkPools(t *testing.T) {
	skip.If(t, testEnv.OSType == "windows")
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.38"), "skip test from new feature")

	// Remove docker0 bridge before running the test, as it will be modified
	// when starting the daemon. Also cleanup the interface afterwards to not
	// affect tests after this one.
	DeleteInterface(t, "docker0")
	defer DeleteInterface(t, "docker0")

	d := daemon.New(t)
	defer d.Stop(t)
	d.Start(t,
		"--default-address-pool", "base=175.30.0.0/16,size=16",
		"--default-address-pool", "base=175.33.0.0/16,size=24",
	)

	c := d.NewClientT(t)
	defer c.Close()

	// Verify bridge network's subnet
	out, err := c.NetworkInspect(context.Background(), "bridge", types.NetworkInspectOptions{})
	assert.NilError(t, err)
	assert.Equal(t, out.IPAM.Config[0].Subnet, "175.30.0.0/16")

	// Create a bridge network and verify its subnet is the second default pool
	name := "elango" + t.Name()
	network.CreateNoError(context.Background(), t, c, name,
		network.WithDriver("bridge"),
	)
	out, err = c.NetworkInspect(context.Background(), name, types.NetworkInspectOptions{})
	assert.NilError(t, err)
	assert.Equal(t, out.IPAM.Config[0].Subnet, "175.33.0.0/24")

	// Create a bridge network and verify its subnet is the third default pool
	name = "saanvi" + t.Name()
	network.CreateNoError(context.Background(), t, c, name,
		network.WithDriver("bridge"),
	)
	out, err = c.NetworkInspect(context.Background(), name, types.NetworkInspectOptions{})
	assert.NilError(t, err)
	assert.Equal(t, out.IPAM.Config[0].Subnet, "175.33.1.0/24")
}

func TestDaemonRestartWithExistingNetwork(t *testing.T) {
	skip.If(t, testEnv.OSType == "windows")
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.38"), "skip test from new feature")

	// Remove docker0 bridge before running the test, as it will be modified
	// when starting the daemon. Also cleanup the interface afterwards to not
	// affect tests after this one.
	DeleteInterface(t, "docker0")
	defer DeleteInterface(t, "docker0")

	d := daemon.New(t)
	d.Start(t)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()

	// Create a bridge network
	name := "elango" + t.Name()
	network.CreateNoError(context.Background(), t, c, name,
		network.WithDriver("bridge"),
	)

	// Verify bridge network's subnet
	out, err := c.NetworkInspect(context.Background(), name, types.NetworkInspectOptions{})
	assert.NilError(t, err)
	networkip := out.IPAM.Config[0].Subnet

	// Restart daemon with default address pool option
	d.Restart(t,
		"--default-address-pool", "base=175.30.0.0/16,size=16",
		"--default-address-pool", "base=175.33.0.0/16,size=24")

	out1, err := c.NetworkInspect(context.Background(), name, types.NetworkInspectOptions{})
	assert.NilError(t, err)
	assert.Equal(t, out1.IPAM.Config[0].Subnet, networkip)
}

func TestDaemonRestartWithExistingNetworkWithDefaultPoolRange(t *testing.T) {
	skip.If(t, testEnv.OSType == "windows")
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.38"), "skip test from new feature")

	// Remove docker0 bridge before running the test, as it will be modified
	// when starting the daemon. Also cleanup the interface afterwards to not
	// affect tests after this one.
	DeleteInterface(t, "docker0")
	defer DeleteInterface(t, "docker0")

	d := daemon.New(t)
	d.Start(t)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()

	// Create a bridge network
	name := "elango" + t.Name()
	network.CreateNoError(context.Background(), t, c, name,
		network.WithDriver("bridge"),
	)

	// Verify bridge network's subnet
	out, err := c.NetworkInspect(context.Background(), name, types.NetworkInspectOptions{})
	assert.NilError(t, err)
	networkip := out.IPAM.Config[0].Subnet

	// Create a bridge network
	name = "sthira" + t.Name()
	network.CreateNoError(context.Background(), t, c, name,
		network.WithDriver("bridge"),
	)
	out, err = c.NetworkInspect(context.Background(), name, types.NetworkInspectOptions{})
	assert.NilError(t, err)
	networkip2 := out.IPAM.Config[0].Subnet

	// Restart daemon with default address pool option
	d.Restart(t,
		"--default-address-pool", "base=175.18.0.0/16,size=16",
		"--default-address-pool", "base=175.19.0.0/16,size=24",
	)

	// Create a bridge network
	name = "saanvi" + t.Name()
	network.CreateNoError(context.Background(), t, c, name,
		network.WithDriver("bridge"),
	)
	out1, err := c.NetworkInspect(context.Background(), name, types.NetworkInspectOptions{})
	assert.NilError(t, err)

	assert.Check(t, out1.IPAM.Config[0].Subnet != networkip)
	assert.Check(t, out1.IPAM.Config[0].Subnet != networkip2)
}

func TestDaemonWithBipAndDefaultNetworkPool(t *testing.T) {
	skip.If(t, testEnv.OSType == "windows")
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.38"), "skip test from new feature")

	// Remove docker0 bridge before running the test, as it will be modified
	// when starting the daemon. Also cleanup the interface afterwards to not
	// affect tests after this one.
	DeleteInterface(t, "docker0")
	defer DeleteInterface(t, "docker0")

	d := daemon.New(t)
	defer d.Stop(t)
	d.Start(t,
		"--bip=172.60.0.1/16",
		"--default-address-pool", "base=175.30.0.0/16,size=16",
		"--default-address-pool", "base=175.33.0.0/16,size=24",
	)

	c := d.NewClientT(t)
	defer c.Close()

	// Verify bridge network's subnet
	out, err := c.NetworkInspect(context.Background(), "bridge", types.NetworkInspectOptions{})
	assert.NilError(t, err)
	// Make sure BIP IP doesn't get override with new default address pool .
	assert.Equal(t, out.IPAM.Config[0].Subnet, "172.60.0.1/16")
}

func TestServiceWithPredefinedNetwork(t *testing.T) {
	skip.If(t, testEnv.OSType == "windows")
	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()

	hostName := "host"
	var instances uint64 = 1
	serviceName := "TestService" + t.Name()

	serviceID := swarm.CreateService(t, d,
		swarm.ServiceWithReplicas(instances),
		swarm.ServiceWithName(serviceName),
		swarm.ServiceWithNetwork(hostName),
	)

	poll.WaitOn(t, swarm.RunningTasksCount(c, serviceID, instances), swarm.ServicePoll)

	_, _, err := c.ServiceInspectWithRaw(context.Background(), serviceID, types.ServiceInspectOptions{})
	assert.NilError(t, err)

	err = c.ServiceRemove(context.Background(), serviceID)
	assert.NilError(t, err)
}

const ingressNet = "ingress"

func TestServiceRemoveKeepsIngressNetwork(t *testing.T) {
	t.Skip("FLAKY_TEST")

	skip.If(t, testEnv.OSType == "windows")
	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()

	poll.WaitOn(t, swarmIngressReady(c), swarm.NetworkPoll)

	var instances uint64 = 1

	serviceID := swarm.CreateService(t, d,
		swarm.ServiceWithReplicas(instances),
		swarm.ServiceWithName(t.Name()+"-service"),
		swarm.ServiceWithEndpoint(&swarmtypes.EndpointSpec{
			Ports: []swarmtypes.PortConfig{
				{
					Protocol:    swarmtypes.PortConfigProtocolTCP,
					TargetPort:  80,
					PublishMode: swarmtypes.PortConfigPublishModeIngress,
				},
			},
		}),
	)

	poll.WaitOn(t, swarm.RunningTasksCount(c, serviceID, instances), swarm.ServicePoll)

	ctx := context.Background()
	_, _, err := c.ServiceInspectWithRaw(ctx, serviceID, types.ServiceInspectOptions{})
	assert.NilError(t, err)

	err = c.ServiceRemove(ctx, serviceID)
	assert.NilError(t, err)

	poll.WaitOn(t, noServices(ctx, c), swarm.ServicePoll)
	poll.WaitOn(t, swarm.NoTasks(ctx, c), swarm.ServicePoll)

	// Ensure that "ingress" is not removed or corrupted
	time.Sleep(10 * time.Second)
	netInfo, err := c.NetworkInspect(ctx, ingressNet, types.NetworkInspectOptions{
		Verbose: true,
		Scope:   "swarm",
	})
	assert.NilError(t, err, "Ingress network was removed after removing service!")
	assert.Assert(t, len(netInfo.Containers) != 0, "No load balancing endpoints in ingress network")
	assert.Assert(t, len(netInfo.Peers) != 0, "No peers (including self) in ingress network")
	_, ok := netInfo.Containers["ingress-sbox"]
	assert.Assert(t, ok, "ingress-sbox not present in ingress network")
}

func swarmIngressReady(client client.NetworkAPIClient) func(log poll.LogT) poll.Result {
	return func(log poll.LogT) poll.Result {
		netInfo, err := client.NetworkInspect(context.Background(), ingressNet, types.NetworkInspectOptions{
			Verbose: true,
			Scope:   "swarm",
		})
		if err != nil {
			return poll.Error(err)
		}
		np := len(netInfo.Peers)
		nc := len(netInfo.Containers)
		if np == 0 || nc == 0 {
			return poll.Continue("ingress not ready: %d peers and %d containers", nc, np)
		}
		_, ok := netInfo.Containers["ingress-sbox"]
		if !ok {
			return poll.Continue("ingress not ready: does not contain the ingress-sbox")
		}
		return poll.Success()
	}
}

func noServices(ctx context.Context, client client.ServiceAPIClient) func(log poll.LogT) poll.Result {
	return func(log poll.LogT) poll.Result {
		services, err := client.ServiceList(ctx, types.ServiceListOptions{})
		switch {
		case err != nil:
			return poll.Error(err)
		case len(services) == 0:
			return poll.Success()
		default:
			return poll.Continue("waiting for all services to be removed: service count at %d", len(services))
		}
	}
}

func TestServiceWithDataPathPortInit(t *testing.T) {
	skip.If(t, testEnv.OSType == "windows")
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.40"), "DataPathPort was added in API v1.40")
	defer setupTest(t)()
	var datapathPort uint32 = 7777
	d := swarm.NewSwarm(t, testEnv, daemon.WithSwarmDataPathPort(datapathPort))
	c := d.NewClientT(t)
	ctx := context.Background()
	// Create a overlay network
	name := "saanvisthira" + t.Name()
	overlayID := network.CreateNoError(context.Background(), t, c, name,
		network.WithDriver("overlay"))

	var instances uint64 = 1
	serviceID := swarm.CreateService(t, d,
		swarm.ServiceWithReplicas(instances),
		swarm.ServiceWithName(name),
		swarm.ServiceWithNetwork(name),
	)

	poll.WaitOn(t, swarm.RunningTasksCount(c, serviceID, instances), swarm.ServicePoll)

	info := d.Info(t)
	assert.Equal(t, info.Swarm.Cluster.DataPathPort, datapathPort)
	err := c.ServiceRemove(ctx, serviceID)
	assert.NilError(t, err)
	poll.WaitOn(t, noServices(ctx, c), swarm.ServicePoll)
	poll.WaitOn(t, swarm.NoTasks(ctx, c), swarm.ServicePoll)
	err = c.NetworkRemove(ctx, overlayID)
	assert.NilError(t, err)
	c.Close()
	err = d.SwarmLeave(t, true)
	assert.NilError(t, err)
	d.Stop(t)

	// Clean up , set it back to original one to make sure other tests don't fail
	// call without datapath port option.
	d = swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	nc := d.NewClientT(t)
	defer nc.Close()
	// Create a overlay network
	name = "not-saanvisthira" + t.Name()
	overlayID = network.CreateNoError(ctx, t, nc, name,
		network.WithDriver("overlay"))

	serviceID = swarm.CreateService(t, d,
		swarm.ServiceWithReplicas(instances),
		swarm.ServiceWithName(name),
		swarm.ServiceWithNetwork(name),
	)

	poll.WaitOn(t, swarm.RunningTasksCount(nc, serviceID, instances), swarm.ServicePoll)

	info = d.Info(t)
	var defaultDataPathPort uint32 = 4789
	assert.Equal(t, info.Swarm.Cluster.DataPathPort, defaultDataPathPort)
	err = nc.ServiceRemove(ctx, serviceID)
	assert.NilError(t, err)
	poll.WaitOn(t, noServices(ctx, nc), swarm.ServicePoll)
	poll.WaitOn(t, swarm.NoTasks(ctx, nc), swarm.ServicePoll)
	err = nc.NetworkRemove(ctx, overlayID)
	assert.NilError(t, err)
	err = d.SwarmLeave(t, true)
	assert.NilError(t, err)
}

func TestServiceWithDefaultAddressPoolInit(t *testing.T) {
	skip.If(t, testEnv.OSType == "windows")
	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv,
		daemon.WithSwarmDefaultAddrPool([]string{"20.20.0.0/16"}),
		daemon.WithSwarmDefaultAddrPoolSubnetSize(24))
	defer d.Stop(t)
	cli := d.NewClientT(t)
	defer cli.Close()
	ctx := context.Background()

	// Create a overlay network
	name := "sthira" + t.Name()
	overlayID := network.CreateNoError(ctx, t, cli, name,
		network.WithDriver("overlay"),
		network.WithCheckDuplicate(),
	)

	var instances uint64 = 1
	serviceName := "TestService" + t.Name()
	serviceID := swarm.CreateService(t, d,
		swarm.ServiceWithReplicas(instances),
		swarm.ServiceWithName(serviceName),
		swarm.ServiceWithNetwork(name),
	)

	poll.WaitOn(t, swarm.RunningTasksCount(cli, serviceID, instances), swarm.ServicePoll)

	_, _, err := cli.ServiceInspectWithRaw(ctx, serviceID, types.ServiceInspectOptions{})
	assert.NilError(t, err)

	out, err := cli.NetworkInspect(ctx, overlayID, types.NetworkInspectOptions{Verbose: true})
	assert.NilError(t, err)
	t.Logf("%s: NetworkInspect: %+v", t.Name(), out)
	assert.Assert(t, len(out.IPAM.Config) > 0)
	assert.Equal(t, out.IPAM.Config[0].Subnet, "20.20.0.0/24")

	err = cli.ServiceRemove(ctx, serviceID)
	poll.WaitOn(t, noServices(ctx, cli), swarm.ServicePoll)
	poll.WaitOn(t, swarm.NoTasks(ctx, cli), swarm.ServicePoll)
	assert.NilError(t, err)
	err = cli.NetworkRemove(ctx, overlayID)
	assert.NilError(t, err)
	err = d.SwarmLeave(t, true)
	assert.NilError(t, err)

}
