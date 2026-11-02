package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	stdnet "net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/moby/moby/api/types/network"
	swarmtypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/daemon/libnetwork/scope"
	"github.com/moby/moby/v2/integration/internal/container"
	net "github.com/moby/moby/v2/integration/internal/network"
	"github.com/moby/moby/v2/integration/internal/swarm"
	"github.com/moby/moby/v2/integration/internal/testutils/networking"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/icmd"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

func TestDockerNetworkConnectAliasPreV144(t *testing.T) {
	ctx := setupTest(t)

	d := swarm.NewSwarm(ctx, t, testEnv, daemon.WithEnvVars("DOCKER_MIN_API_VERSION=1.43"))
	defer d.Stop(t)
	apiClient := d.NewClientT(t, client.WithAPIVersion("1.43"))
	defer apiClient.Close()

	name := t.Name() + "test-alias"
	net.CreateNoError(ctx, t, apiClient, name,
		net.WithDriver("overlay"),
		net.WithAttachable(),
	)

	cID1 := container.Create(ctx, t, apiClient, func(c *container.TestContainerConfig) {
		c.NetworkingConfig = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				name: {},
			},
		}
	})

	_, err := apiClient.NetworkConnect(ctx, name, client.NetworkConnectOptions{
		Container: cID1,
		EndpointConfig: &network.EndpointSettings{
			Aliases: []string{
				"aaa",
			},
		},
	})
	assert.NilError(t, err)

	_, err = apiClient.ContainerStart(ctx, cID1, client.ContainerStartOptions{})
	assert.NilError(t, err)

	ng1, err := apiClient.ContainerInspect(ctx, cID1, client.ContainerInspectOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(ng1.Container.NetworkSettings.Networks[name].Aliases), 2))
	assert.Check(t, is.Equal(ng1.Container.NetworkSettings.Networks[name].Aliases[0], "aaa"))

	cID2 := container.Create(ctx, t, apiClient, func(c *container.TestContainerConfig) {
		c.NetworkingConfig = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				name: {},
			},
		}
	})

	_, err = apiClient.NetworkConnect(ctx, name, client.NetworkConnectOptions{
		Container: cID2,
		EndpointConfig: &network.EndpointSettings{
			Aliases: []string{
				"bbb",
			},
		},
	})
	assert.NilError(t, err)

	_, err = apiClient.ContainerStart(ctx, cID2, client.ContainerStartOptions{})
	assert.NilError(t, err)

	ng2, err := apiClient.ContainerInspect(ctx, cID2, client.ContainerInspectOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(ng2.Container.NetworkSettings.Networks[name].Aliases), 2))
	assert.Check(t, is.Equal(ng2.Container.NetworkSettings.Networks[name].Aliases[0], "bbb"))
}

func TestDockerNetworkReConnect(t *testing.T) {
	ctx := setupTest(t)

	d := swarm.NewSwarm(ctx, t, testEnv)
	defer d.Stop(t)
	apiClient := d.NewClientT(t)
	defer apiClient.Close()

	name := t.Name() + "dummyNet"
	net.CreateNoError(ctx, t, apiClient, name,
		net.WithDriver("overlay"),
		net.WithAttachable(),
	)

	c1 := container.Create(ctx, t, apiClient, func(c *container.TestContainerConfig) {
		c.NetworkingConfig = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				name: {},
			},
		}
	})

	_, err := apiClient.NetworkConnect(ctx, name, client.NetworkConnectOptions{
		Container:      c1,
		EndpointConfig: &network.EndpointSettings{},
	})
	assert.NilError(t, err)

	_, err = apiClient.ContainerStart(ctx, c1, client.ContainerStartOptions{})
	assert.NilError(t, err)

	n1, err := apiClient.ContainerInspect(ctx, c1, client.ContainerInspectOptions{})
	assert.NilError(t, err)

	_, err = apiClient.NetworkConnect(ctx, name, client.NetworkConnectOptions{
		Container:      c1,
		EndpointConfig: &network.EndpointSettings{},
	})
	assert.ErrorContains(t, err, "is already attached to network")

	n2, err := apiClient.ContainerInspect(ctx, c1, client.ContainerInspectOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(n1.Container, n2.Container, cmpopts.EquateComparable(netip.Addr{}, netip.Prefix{})))
}

// Check that a swarm-scoped network can't have EnableIPv4=false.
func TestSwarmNoDisableIPv4(t *testing.T) {
	ctx := setupTest(t)

	d := swarm.NewSwarm(ctx, t, testEnv)
	defer d.Stop(t)
	apiClient := d.NewClientT(t)
	defer apiClient.Close()

	_, err := net.Create(ctx, apiClient, "overlay-v6-only",
		net.WithDriver("overlay"),
		net.WithAttachable(),
		net.WithIPv4(false),
	)
	assert.Check(t, is.ErrorContains(err, "IPv4 cannot be disabled in a Swarm scoped network"))
}

// Regression test for https://github.com/docker/cli/issues/5857
func TestSwarmScopedNetFromConfig(t *testing.T) {
	skip.If(t, testEnv.IsRootless, "rootless mode doesn't support Swarm-mode")
	ctx := setupTest(t)

	d := swarm.NewSwarm(ctx, t, testEnv)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()

	const configNetName = "config-net"
	_ = net.CreateNoError(ctx, t, c, configNetName,
		net.WithDriver("bridge"),
		net.WithConfigOnly(true),
	)
	const swarmNetName = "swarm-net"
	_, err := net.Create(ctx, c, swarmNetName,
		net.WithDriver("bridge"),
		net.WithConfigFrom(configNetName),
		net.WithAttachable(),
		net.WithScope(scope.Swarm),
	)
	assert.NilError(t, err)

	serviceID := swarm.CreateService(ctx, t, d,
		swarm.ServiceWithName("test-ssnfc"),
		swarm.ServiceWithNetwork(swarmNetName),
	)
	defer func() {
		_, err := c.ServiceRemove(ctx, serviceID, client.ServiceRemoveOptions{})
		assert.NilError(t, err)
	}()

	poll.WaitOn(t, swarm.RunningTasksCount(ctx, c, serviceID, 1), swarm.ServicePoll)
}

// Check that a swarm service's published (ingress) port remains accessible
// after a daemon restart. Ingress ports are published as ordinary port mappings
// on the load-balancer sandbox's docker_gwbridge gateway endpoint, so they're
// restored along with every other port mapping when the daemon restarts.
// Regression test for https://github.com/moby/moby/pull/49538
func TestDockerIngressPortAfterRestart(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.IsRootless, "rootless mode doesn't support Swarm-mode")
	skip.If(t, testEnv.FirewallBackendDriver() == "nftables")
	skip.If(t, networking.FirewalldRunning(), "can't use firewalld in host netns to add rules in L3Segment")
	ctx := setupTest(t)

	// Run the test in its own netns, to avoid interfering with iptables on the test host.
	const hostAddr = "192.168.111.222"
	const l3SegHost = "dicp"
	l3 := networking.NewL3Segment(t, "test-"+l3SegHost)
	defer l3.Destroy(t)
	l3.AddHost(t, l3SegHost, "ns-"+l3SegHost, "eth0", netip.MustParsePrefix(hostAddr+"/24"))

	// Check the published port is accessible.
	checkHTTP := func(_ poll.LogT) poll.Result {
		var res *icmd.Result
		// This is called from inside a "Do()" thread in the docker host's netns, but it
		// uses poll.WaitOn - which runs the command in a different goroutine.
		l3.Hosts[l3SegHost].Do(t, func() {
			res = icmd.RunCommand("wget", "-T1", "-t1", "-O-",
				"http://"+stdnet.JoinHostPort(hostAddr, "8080"))
		})
		// A "404 Not Found" means the server responded, but it's got nothing to serve.
		if !strings.Contains(res.Stderr(), "404 Not Found") {
			return poll.Continue("404 Not Found not found in: %s", res.Stderr())
		}
		return poll.Success()
	}

	l3.Hosts[l3SegHost].Do(t, func() {
		d := swarm.NewSwarm(ctx, t, testEnv, daemon.WithSwarmIptables(true))
		defer d.Stop(t)
		c := d.NewClientT(t)
		defer c.Close()

		serviceID := swarm.CreateService(ctx, t, d,
			swarm.ServiceWithName("test-dicp"),
			swarm.ServiceWithCommand([]string{"httpd", "-f"}),
			swarm.ServiceWithEndpoint(&swarmtypes.EndpointSpec{
				Ports: []swarmtypes.PortConfig{
					{
						Protocol:      "tcp",
						TargetPort:    80,
						PublishedPort: 8080,
						PublishMode:   swarmtypes.PortConfigPublishModeIngress,
					},
				},
			}),
		)
		defer func() {
			_, err := c.ServiceRemove(ctx, serviceID, client.ServiceRemoveOptions{})
			assert.NilError(t, err)
		}()

		t.Log("Waiting for the service to start")
		poll.WaitOn(t, swarm.RunningTasksCount(ctx, c, serviceID, 1), swarm.ServicePoll)
		t.Log("Checking http access to the service")
		poll.WaitOn(t, checkHTTP, poll.WithTimeout(30*time.Second))

		t.Log("Restarting the daemon")
		d.Restart(t)

		t.Log("Waiting for the service to start")
		poll.WaitOn(t, swarm.RunningTasksCount(ctx, c, serviceID, 1), swarm.ServicePoll)
		t.Log("Checking http access to the service")
		// It takes a while before this works ...
		poll.WaitOn(t, checkHTTP, poll.WithTimeout(30*time.Second))
	})
}

func TestRestoreIngressRulesOnFirewalldReload(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.IsRootless, "rootless mode doesn't support Swarm-mode")
	skip.If(t, testEnv.FirewallBackendDriver() != "iptables+firewalld", "nftables backend doesn't support Swarm-mode")
	skip.If(t, !networking.FirewalldRunning(), "Need firewalld to test restoration ingress rules")
	ctx := setupTest(t)

	// Check the published port is accessible.
	checkHTTP := func(_ poll.LogT) poll.Result {
		res := icmd.RunCommand("curl", "-v", "-o", "/dev/null", "-w", "%{http_code}\n",
			"http://"+stdnet.JoinHostPort("localhost", "8080"))
		// A "404 Not Found" means the server responded, but it's got nothing to serve.
		if !strings.Contains(res.Stdout(), "404") {
			return poll.Continue("404 - not found in: %s, %+v", res.Stdout(), res)
		}
		return poll.Success()
	}

	d := swarm.NewSwarm(ctx, t, testEnv, daemon.WithSwarmIptables(true))
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()

	serviceID := swarm.CreateService(ctx, t, d,
		swarm.ServiceWithName("test-ingress-on-firewalld-reload"),
		swarm.ServiceWithCommand([]string{"httpd", "-f"}),
		swarm.ServiceWithEndpoint(&swarmtypes.EndpointSpec{
			Ports: []swarmtypes.PortConfig{
				{
					Protocol:      "tcp",
					TargetPort:    80,
					PublishedPort: 8080,
					PublishMode:   swarmtypes.PortConfigPublishModeIngress,
				},
			},
		}),
	)
	defer func() {
		_, err := c.ServiceRemove(ctx, serviceID, client.ServiceRemoveOptions{})
		assert.NilError(t, err)
	}()

	t.Log("Waiting for the service to start")
	poll.WaitOn(t, swarm.RunningTasksCount(ctx, c, serviceID, 1), swarm.ServicePoll)
	t.Log("Checking http access to the service")
	poll.WaitOn(t, checkHTTP, poll.WithTimeout(30*time.Second))

	t.Log("Firewalld reload")
	networking.FirewalldReload(t, d)

	t.Log("Checking http access to the service")
	// It takes a while before this works ...
	poll.WaitOn(t, checkHTTP, poll.WithTimeout(30*time.Second))
}

// TestServiceVIPAcrossRollingUpdate checks that a service's VIP keeps carrying
// connections while the task behind it is replaced. Start-first ordering makes
// the replacement task run before the one it replaces stops, which is what
// leaves the load balancer holding both backends and having to keep the VIP
// serving through the handover.
func TestServiceVIPAcrossRollingUpdate(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.IsRootless, "rootless mode doesn't support Swarm-mode")
	skip.If(t, strings.HasPrefix(testEnv.FirewallBackendDriver(), "nftables"), "swarm cannot be used with nftables")
	ctx := setupTest(t)

	d := swarm.NewSwarm(ctx, t, testEnv, daemon.WithSwarmIptables(true))
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()

	const netName = "svipu-net"
	netID := net.CreateNoError(ctx, t, c, netName,
		net.WithDriver("overlay"),
		// Attachable so a plain container can join and address the VIP.
		net.WithAttachable(),
	)

	serviceID := swarm.CreateService(ctx, t, d,
		swarm.ServiceWithName("test-svipu"),
		swarm.ServiceWithCommand([]string{"httpd", "-f"}),
		swarm.ServiceWithNetwork(netName),
		func(spec *swarmtypes.ServiceSpec) {
			// Start the replacement task before stopping the one it replaces, so the
			// load balancer holds both backends at once and has to keep the VIP
			// serving through the handover. Under the default stop-first order the
			// old backend is gone before the new one arrives, and the VIP is
			// briefly backed by nothing at all - a weaker thing to assert.
			spec.UpdateConfig = &swarmtypes.UpdateConfig{Order: swarmtypes.UpdateOrderStartFirst}
		},
	)
	defer func() {
		_, err := c.ServiceRemove(ctx, serviceID, client.ServiceRemoveOptions{})
		assert.NilError(t, err)
	}()
	t.Log("Waiting for the service to start")
	poll.WaitOn(t, swarm.RunningTasksCount(ctx, c, serviceID, 1), swarm.ServicePoll)

	svc := getService(ctx, t, c, serviceID)
	var vip string
	for _, v := range svc.Endpoint.VirtualIPs {
		if v.NetworkID == netID {
			vip = v.Addr.Addr().String()
		}
	}
	assert.Assert(t, vip != "", "no VIP allocated on %s: %+v", netName, svc.Endpoint.VirtualIPs)
	t.Log("Service VIP is", vip)

	clientID := container.Run(ctx, t, c, container.WithNetworkMode(netName))
	defer c.ContainerRemove(ctx, clientID, client.ContainerRemoveOptions{Force: true})

	// The tasks run httpd with nothing to serve, so a "404 Not Found" is the proof
	// that the request reached one through the VIP - anything short of a task
	// answering is a connection error instead.
	checkVIP := func(_ poll.LogT) poll.Result {
		res, err := container.Exec(ctx, c, clientID,
			[]string{"wget", "-T1", "-t1", "-O-", "http://" + vip + "/"})
		if err != nil {
			return poll.Error(err)
		}
		if !strings.Contains(res.Combined(), "404 Not Found") {
			return poll.Continue("404 Not Found not found in: %s", res.Combined())
		}
		return poll.Success()
	}

	t.Log("Checking VIP access before the update")
	poll.WaitOn(t, checkVIP, poll.WithTimeout(30*time.Second))

	// Roll the task without changing anything else about the service. Nothing here
	// depends on what triggered the rollout, only that one task replaces another
	// under the VIP.
	t.Log("Forcing a rolling update")
	svc.Spec.TaskTemplate.ForceUpdate++
	_, err := c.ServiceUpdate(ctx, serviceID, client.ServiceUpdateOptions{
		Version: svc.Version,
		Spec:    svc.Spec,
	})
	assert.NilError(t, err)
	poll.WaitOn(t, serviceIsUpdated(ctx, c, serviceID), swarm.ServicePoll)
	poll.WaitOn(t, swarm.RunningTasksCount(ctx, c, serviceID, 1), swarm.ServicePoll)

	// Back to one running task, and it is not the one that answered above. The VIP
	// has to resolve to the replacement.
	t.Log("Checking VIP access after the update")
	poll.WaitOn(t, checkVIP, poll.WithTimeout(30*time.Second))
}

// checkPublishedPort returns a poll check that a service's published port is
// accessible from host. The tasks run httpd with nothing to serve, so a "404 Not
// Found" is the proof that the request reached one - anything short of a task
// answering is a connection error instead.
func checkPublishedPort(t *testing.T, host networking.Host, hostAddr, port string) func(poll.LogT) poll.Result {
	return func(_ poll.LogT) poll.Result {
		var res *icmd.Result
		// This is called from inside a "Do()" thread in the docker host's netns, but it
		// uses poll.WaitOn - which runs the command in a different goroutine.
		host.Do(t, func() {
			res = icmd.RunCommand("wget", "-T1", "-t1", "-O-",
				"http://"+stdnet.JoinHostPort(hostAddr, port))
		})
		if !strings.Contains(res.Stderr(), "404 Not Found") {
			return poll.Continue("404 Not Found not found in: %s", res.Stderr())
		}
		return poll.Success()
	}
}

// ingressPortSpec is an endpoint spec publishing each of published on the ingress
// network over TCP, every one of them mapped to target port 80.
func ingressPortSpec(published ...uint32) *swarmtypes.EndpointSpec {
	return portSpec(network.TCP, swarmtypes.PortConfigPublishModeIngress, published...)
}

func portSpec(proto network.IPProtocol, mode swarmtypes.PortConfigPublishMode, published ...uint32) *swarmtypes.EndpointSpec {
	spec := &swarmtypes.EndpointSpec{}
	for _, p := range published {
		spec.Ports = append(spec.Ports, swarmtypes.PortConfig{
			Protocol:      proto,
			TargetPort:    80,
			PublishedPort: p,
			PublishMode:   mode,
		})
	}
	return spec
}

// createPublishingService creates a service publishing endpoint's ports, and waits
// for replicas of its tasks to be running. It serves HTTP unless opts override
// the command.
func createPublishingService(ctx context.Context, t *testing.T, d *daemon.Daemon, c *client.Client, name string, replicas uint64, endpoint *swarmtypes.EndpointSpec, opts ...swarm.ServiceSpecOpt) string {
	t.Helper()
	id := swarm.CreateService(ctx, t, d, append([]swarm.ServiceSpecOpt{
		swarm.ServiceWithName(name),
		swarm.ServiceWithCommand([]string{"httpd", "-f"}),
		swarm.ServiceWithEndpoint(endpoint),
		swarm.ServiceWithReplicas(replicas),
		func(spec *swarmtypes.ServiceSpec) {
			// Start the replacement task before stopping the one it replaces, so that an
			// update to the published-port set really does have both generations of the
			// service - and so both claims on a port they share - bound at once. Under
			// the default stop-first order the old binding can be gone before the new
			// one arrives, leaving nothing shared to get wrong.
			spec.UpdateConfig = &swarmtypes.UpdateConfig{Order: swarmtypes.UpdateOrderStartFirst}
		},
	}, opts...)...)
	t.Log("Waiting for service", name, "to start")
	poll.WaitOn(t, swarm.RunningTasksCount(ctx, c, id, replicas), swarm.ServicePoll)
	return id
}

// TestIngressPortsAcrossServiceUpdate follows a service's host-published ingress
// ports through the two events that change which host-port reservations it holds:
// a published-port change, which leaves two generations of the service alive at
// once, and the service's removal, which has to release the reservations for good.
//
// Those reservations are ref-counted, and both ways of getting the count wrong are
// silent. Releasing too eagerly takes down a port the surviving generation is
// still serving; not releasing at all leaves the count non-zero, and the next
// service to publish that port is told there is nothing to plumb and never becomes
// reachable. Neither shows up as an error from any API call, which is why this
// drives the ports over the wire rather than inspecting the daemon's bookkeeping.
func TestIngressPortsAcrossServiceUpdate(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.IsRootless, "rootless mode doesn't support Swarm-mode")
	skip.If(t, testEnv.FirewallBackendDriver() == "nftables")
	skip.If(t, networking.FirewalldRunning(), "can't use firewalld in host netns to add rules in L3Segment")
	ctx := setupTest(t)

	// Run the test in its own netns, to avoid interfering with iptables on the test
	// host - and so that the host ports it publishes are its own.
	const hostAddr = "192.168.112.222"
	const l3SegHost = "ipsu"
	l3 := networking.NewL3Segment(t, "test-"+l3SegHost)
	defer l3.Destroy(t)
	l3.AddHost(t, l3SegHost, "ns-"+l3SegHost, "eth0", netip.MustParsePrefix(hostAddr+"/24"))

	checkHTTP := func(port string) func(poll.LogT) poll.Result {
		return checkPublishedPort(t, l3.Hosts[l3SegHost], hostAddr, port)
	}

	l3.Hosts[l3SegHost].Do(t, func() {
		d := swarm.NewSwarm(ctx, t, testEnv, daemon.WithSwarmIptables(true))
		defer d.Stop(t)
		c := d.NewClientT(t)
		defer c.Close()

		createService := func(name string, endpoint *swarmtypes.EndpointSpec) string {
			return createPublishingService(ctx, t, d, c, name, 1, endpoint)
		}

		serviceID := createService("test-"+l3SegHost, ingressPortSpec(8080))
		t.Log("Checking http access on the initial published port")
		poll.WaitOn(t, checkHTTP("8080"), poll.WithTimeout(30*time.Second))

		// Publish a second port. The service's port set is part of its identity in
		// libnetwork, so two services are bound on the ingress network, both
		// publishing 8080, for as long as the rollout takes. Only one of them may
		// hold that reservation, and the old one draining must not release it.
		t.Log("Adding a published port")
		svc := getService(ctx, t, c, serviceID)
		svc.Spec.EndpointSpec = ingressPortSpec(8080, 9090)
		_, err := c.ServiceUpdate(ctx, serviceID, client.ServiceUpdateOptions{
			Version: svc.Version,
			Spec:    svc.Spec,
		})
		assert.NilError(t, err)
		poll.WaitOn(t, serviceIsUpdated(ctx, c, serviceID), swarm.ServicePoll)
		poll.WaitOn(t, swarm.RunningTasksCount(ctx, c, serviceID, 1), swarm.ServicePoll)

		t.Log("Checking http access on the added published port")
		poll.WaitOn(t, checkHTTP("9090"), poll.WithTimeout(30*time.Second))
		t.Log("Checking http access on the port that spanned the update")
		poll.WaitOn(t, checkHTTP("8080"), poll.WithTimeout(30*time.Second))

		// Remove the service, then have a fresh one publish the same two ports. It
		// can only reach them if the first service released both its reservation and
		// the underlying host-port binding, so this stands in for the unpublishing
		// that has no observable output of its own.
		t.Log("Removing the service")
		_, err = c.ServiceRemove(ctx, serviceID, client.ServiceRemoveOptions{})
		assert.NilError(t, err)
		poll.WaitOn(t, swarm.NoTasksForService(ctx, c, serviceID), swarm.ServicePoll)

		successor := createService("test-"+l3SegHost+"-2", ingressPortSpec(8080, 9090))
		defer func() {
			_, err := c.ServiceRemove(ctx, successor, client.ServiceRemoveOptions{})
			assert.NilError(t, err)
		}()
		t.Log("Checking http access to the successor service's published ports")
		poll.WaitOn(t, checkHTTP("8080"), poll.WithTimeout(30*time.Second))
		poll.WaitOn(t, checkHTTP("9090"), poll.WithTimeout(30*time.Second))
	})
}

// refusedIngressPort is the published port the iptables wrapper in
// TestIngressPortsAfterFailedPublish refuses to program.
const refusedIngressPort = 9091

// TestIngressPortsAfterFailedPublish checks that a service whose ingress publish
// fails leaves behind no claim on the host ports it was already serving.
//
// Publishing takes a reference to every port in the service's set, then plumbs
// only those referenced for the first time. If that plumbing fails, the
// references have to be dropped for the whole set: dropping only the
// newly-referenced subset leaks one on every port another generation of the
// service already held. Nothing is left to release a leaked reference, so the host
// port stays reserved - bound by the daemon, unusable by anything else - for the
// rest of the daemon's lifetime, long after the service that published it is gone.
//
// The leak is per failed attempt, and a failed publish is retried for each of the
// service's tasks - the load-balancer service is only recorded once publishing has
// succeeded, so every task rediscovers that there is none. Two replicas is
// therefore the smallest service that outlives its own teardown: with one, the
// single leaked reference is cancelled by the two generations' releases and the
// port comes free either way.
func TestIngressPortsAfterFailedPublish(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.IsRootless, "rootless mode doesn't support Swarm-mode")
	skip.If(t, testEnv.FirewallBackendDriver() == "nftables")
	skip.If(t, networking.FirewalldRunning(), "can't use firewalld in host netns to add rules in L3Segment")
	ctx := setupTest(t)

	// Run the test in its own netns, to avoid interfering with iptables on the test
	// host - and so that the host ports it publishes are its own.
	const hostAddr = "192.168.113.222"
	const l3SegHost = "ipfp"
	l3 := networking.NewL3Segment(t, "test-"+l3SegHost)
	defer l3.Destroy(t)
	l3.AddHost(t, l3SegHost, "ns-"+l3SegHost, "eth0", netip.MustParsePrefix(hostAddr+"/24"))

	checkHTTP := func(port string) func(poll.LogT) poll.Result {
		return checkPublishedPort(t, l3.Hosts[l3SegHost], hostAddr, port)
	}

	// The daemon finds iptables on its PATH, so putting a wrapper ahead of the real
	// one is a seam for failing a chosen rule on demand. Refuse anything mentioning
	// the port added below, and nothing else - the daemon needs a working iptables
	// for everything from bringing up the bridge to publishing the first port.
	refusedPort := strconv.Itoa(refusedIngressPort)
	realIptables, err := exec.LookPath("iptables")
	assert.NilError(t, err)
	binDir := t.TempDir()
	refusals := filepath.Join(binDir, "refusals")
	assert.NilError(t, os.WriteFile(filepath.Join(binDir, "iptables"),
		fmt.Appendf(nil, `#!/bin/sh
for arg in $@; do
   if [ $arg = %q ]; then
     echo "$*" >> %q
     echo "iptables: refused by test wrapper" >&2
     exit 1
   fi
 done
 exec %s "$@"`, refusedPort, refusals, realIptables), 0o755))

	l3.Hosts[l3SegHost].Do(t, func() {
		d := swarm.NewSwarm(ctx, t, testEnv, daemon.WithSwarmIptables(true),
			daemon.WithEnvVars("PATH="+binDir+":"+os.Getenv("PATH")))
		defer d.Stop(t)
		c := d.NewClientT(t)
		defer c.Close()

		serviceID := createPublishingService(ctx, t, d, c, "test-"+l3SegHost, 2, ingressPortSpec(8080))
		t.Log("Checking http access on the initial published port")
		poll.WaitOn(t, checkHTTP("8080"), poll.WithTimeout(30*time.Second))

		// Add the port the wrapper refuses. Every task of the arriving generation
		// tries, and fails, to publish it - each attempt having first claimed 8080,
		// which the departing generation is still serving.
		t.Log("Adding a published port that can't be published")
		svc := getService(ctx, t, c, serviceID)
		svc.Spec.EndpointSpec = ingressPortSpec(8080, refusedIngressPort)
		_, err = c.ServiceUpdate(ctx, serviceID, client.ServiceUpdateOptions{
			Version: svc.Version,
			Spec:    svc.Spec,
		})
		assert.NilError(t, err)
		poll.WaitOn(t, serviceIsUpdated(ctx, c, serviceID), swarm.ServicePoll)
		poll.WaitOn(t, swarm.RunningTasksCount(ctx, c, serviceID, 2), swarm.ServicePoll)

		// Remove the service. Every reference the failed attempts took has to have
		// been given back, and reserving a host port means holding a listening socket
		// on it - so a reference nobody is left to release keeps 8080 bound for the
		// rest of the daemon's lifetime. Binding it from here is the assertion.
		//
		// Whether the port is still *reachable* proves nothing either way: the
		// binding a leaked reference preserves is the one this service published, and
		// it keeps working for whichever service publishes 8080 next.
		t.Log("Removing the service")
		_, err = c.ServiceRemove(ctx, serviceID, client.ServiceRemoveOptions{})
		assert.NilError(t, err)
		poll.WaitOn(t, swarm.NoTasksForService(ctx, c, serviceID), swarm.ServicePoll)

		// Fail loudly rather than vacuously passing if the wrapper never got a look in
		// - an ineffective PATH would mean the publish succeeded and nothing under test
		// here ever ran.
		refused, err := os.ReadFile(refusals)
		assert.NilError(t, err, "iptables wrapper was never consulted")
		assert.Check(t, is.Contains(string(refused), refusedPort))

		t.Log("Checking the host port the service published has been released")
		poll.WaitOn(t, func(_ poll.LogT) poll.Result {
			var lerr error
			// poll.WaitOn runs this in a goroutine of its own, so re-enter the docker
			// host's netns - the reservation this is probing for is in there.
			l3.Hosts[l3SegHost].Do(t, func() {
				var ln stdnet.Listener
				// #nosec G102 -- listening in an isolated netns with no external connectivity
				if ln, lerr = stdnet.Listen("tcp", ":8080"); lerr == nil {
					ln.Close()
				}
			})
			if lerr != nil {
				return poll.Continue("host port 8080 is still reserved: %v", lerr)
			}
			return poll.Success()
		}, poll.WithTimeout(20*time.Second))
	})
}

// TestSwarmPublishedPortsOverIPv6 checks that the ports a Swarm service publishes
// are reachable over IPv6 - and that when the daemon is configured so they can't
// be, an IPv6 client is refused rather than left waiting.
//
// Neither the ingress network nor docker_gwbridge has an IPv6 address, so there is
// no IPv6 path to a task. Host IPv6 traffic reaches one the way it reaches any
// other IPv4-only container: docker-proxy accepts it on "[::]" and forwards over
// IPv4. Disabling the userland proxy takes that path away, along with the IPv6
// binding, so the traffic is refused.
//
// Regression test for https://github.com/moby/moby/issues/53091.
func TestSwarmPublishedPortsOverIPv6(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.IsRootless, "rootless mode doesn't support Swarm-mode")
	skip.If(t, testEnv.FirewallBackendDriver() == "nftables")
	skip.If(t, networking.FirewalldRunning(), "can't use firewalld in host netns to add rules in L3Segment")
	ctx := setupTest(t)

	// Run the test in its own netns, to avoid interfering with iptables on the test
	// host - and so that the host ports it publishes are its own. A dual-stack
	// segment, with a host per userland-proxy setting so the two can run at once
	// without contending for ports, and a neighbour to probe from over the wire.
	const (
		hostProxy   = "spv6a"
		hostNoProxy = "spv6b"
		neighbour   = "spv6n"
	)
	l3 := networking.NewL3Segment(t, "test-spv6",
		netip.MustParsePrefix("192.168.114.1/24"),
		netip.MustParsePrefix("fd6a:9c1e:2f3b::1/64"))
	// Cleanup rather than defer: the parallel subtests below don't start until this
	// function has returned, and a defer would tear the segment down first.
	t.Cleanup(func() { l3.Destroy(t) })
	l3.AddHost(t, neighbour, "ns-"+neighbour, "eth0",
		netip.MustParsePrefix("192.168.114.3/24"),
		netip.MustParsePrefix("fd6a:9c1e:2f3b::3/64"))

	// Ingress and host mode reach the bridge driver by different routes - the
	// ingress sandbox's gateway endpoint, and the task's own - and TCP and UDP are
	// bound and refused by different machinery, so all four combinations are
	// published, by one service so there's only one to wait for.
	const (
		tcpIngressPort = 8080
		tcpHostPort    = 8081
		udpIngressPort = 8082
		udpHostPort    = 8083

		// httpd serves both TCP ports. The UDP ports get one container port each -
		// a listening nc serves one peer at a time, so probes would otherwise queue.
		httpPort         = 80
		udpIngressTarget = 81
		udpHostTarget    = 82
	)

	for _, tc := range []struct {
		hostname      string
		userlandProxy bool
		addr4, addr6  string
	}{
		{hostname: hostProxy, userlandProxy: true, addr4: "192.168.114.2", addr6: "fd6a:9c1e:2f3b::2"},
		{hostname: hostNoProxy, userlandProxy: false, addr4: "192.168.114.4", addr6: "fd6a:9c1e:2f3b::4"},
	} {
		l3.AddHost(t, tc.hostname, "ns-"+tc.hostname, "eth0",
			netip.MustParsePrefix(tc.addr4+"/24"),
			netip.MustParsePrefix(tc.addr6+"/64"))
		// A refused UDP port answers with an ICMPv6 port-unreachable, rate limited
		// by default (net.ipv6.icmp.ratelimit, 100ms) - and a refusal the limiter
		// swallows is indistinguishable from the silent drop under test. Loopback
		// is exempt, so it would pass locally and flake in CI. Spacing the probes
		// out is no defence; turn the limit off where the errors originate.
		l3.Hosts[tc.hostname].MustRun(t, "sysctl", "-w", "net.ipv6.icmp.ratelimit=0")

		t.Run(fmt.Sprintf("userland-proxy=%t", tc.userlandProxy), func(t *testing.T) {
			// Each setting has a netns and a daemon to itself, so the two can run
			// side by side.
			t.Parallel()

			published := []*publishedTestPort{
				{proto: network.TCP, mode: swarmtypes.PortConfigPublishModeIngress, modeName: "ingress", port: tcpIngressPort, target: httpPort},
				{proto: network.TCP, mode: swarmtypes.PortConfigPublishModeHost, modeName: "host", port: tcpHostPort, target: httpPort},
				{proto: network.UDP, mode: swarmtypes.PortConfigPublishModeIngress, modeName: "ingress", port: udpIngressPort, target: udpIngressTarget},
				{proto: network.UDP, mode: swarmtypes.PortConfigPublishModeHost, modeName: "host", port: udpHostPort, target: udpHostTarget},
			}
			endpoint := &swarmtypes.EndpointSpec{}
			for _, p := range published {
				endpoint.Ports = append(endpoint.Ports, swarmtypes.PortConfig{
					Protocol:      p.proto,
					TargetPort:    uint32(p.target),
					PublishedPort: uint32(p.port),
					PublishMode:   p.mode,
				})
			}

			var c *client.Client
			l3.Hosts[tc.hostname].Do(t, func() {
				d := swarm.NewSwarm(ctx, t, testEnv,
					daemon.WithSwarmIptables(true),
					daemon.WithUserlandProxy(tc.userlandProxy))
				c = d.NewClientT(t)
				// Cleanup rather than defer, again: the parallel checks below run
				// after this returns, and a defer would stop the daemon first.
				t.Cleanup(func() {
					c.Close()
					d.Stop(t)
				})

				// A listening nc latches onto the first peer it hears from and
				// ignores every other source address for as long as it runs, so
				// without -w only the first probe would ever be seen. It quits after
				// a second's quiet; the loop brings up a fresh one.
				id := createPublishingService(ctx, t, d, c, "test-"+tc.hostname, 1, endpoint,
					swarm.ServiceWithCommand([]string{"sh", "-c", fmt.Sprintf(
						"httpd -f & while true; do nc -u -l -p %d -w 1; done & while true; do nc -u -l -p %d -w 1; done",
						udpIngressTarget, udpHostTarget)}))
				for _, p := range published {
					p.serviceID = id
				}
			})

			for _, p := range published {
				for _, from := range []struct {
					name  string
					host  networking.Host
					addr4 string
					addr6 string
				}{
					{name: "loopback", host: l3.Hosts[tc.hostname], addr4: "127.0.0.1", addr6: "::1"},
					{name: "host-address", host: l3.Hosts[tc.hostname], addr4: tc.addr4, addr6: tc.addr6},
					{name: "remote-host", host: l3.Hosts[neighbour], addr4: tc.addr4, addr6: tc.addr6},
				} {
					// A subtest per combination, so a check giving up takes only its
					// own case down with it - poll.WaitOn ends the goroutine it runs
					// on, and which ports broke shows how far a regression reaches.
					// They only read, so they run in parallel: a broken port costs
					// one timeout for the whole matrix, not one apiece.
					t.Run(fmt.Sprintf("%s/%s/from-%s", p.proto, p.modeName, from.name), func(t *testing.T) {
						t.Parallel()

						// IPv4 works either way - through the proxy, or by DNAT
						// without it - so checking it first settles that the port is
						// published at all, and what IPv6 finds below is the steady
						// state rather than a service still coming up.
						t.Log("Checking IPv4 access")
						p.checkReachable(ctx, t, c, from.host, from.addr4, from.name+"-v4")

						if tc.userlandProxy {
							t.Log("Checking IPv6 access")
							p.checkReachable(ctx, t, c, from.host, from.addr6, from.name+"-v6")
							return
						}

						// Without the proxy there's no IPv6 path to the task, and the
						// port has to say so. ECONNREFUSED specifically: traffic
						// accepted and never served fails a reachability check just as
						// refused traffic does, while looking like success to a client
						// that only asks whether it was accepted.
						t.Log("Checking IPv6 is refused")
						err := p.probeRefused(t, from.host, from.addr6)
						assert.Check(t, errors.Is(err, syscall.ECONNREFUSED),
							"connecting to [%s]:%d expected ECONNREFUSED, got %v",
							from.addr6, p.port, err)
					})
				}
			}
		})
	}
}

// publishedTestPort is one port published by TestSwarmPublishedPortsOverIPv6,
// and the service publishing it.
type publishedTestPort struct {
	proto     network.IPProtocol
	mode      swarmtypes.PortConfigPublishMode
	modeName  string // mode, for test output
	port      int
	target    int // container port, which the UDP modes keep to themselves
	serviceID string
}

// checkReachable waits for the published port to be reachable at hostAddr from
// host, failing its subtest if it doesn't become so. label distinguishes this
// check from the others in the run.
func (p *publishedTestPort) checkReachable(ctx context.Context, t *testing.T, c *client.Client, host networking.Host, hostAddr, label string) {
	t.Helper()
	port := strconv.Itoa(p.port)
	check := checkPublishedPort(t, host, hostAddr, port)
	if p.proto == network.UDP {
		payload := fmt.Sprintf("probe-%s-%s-%s", p.proto, p.modeName, label)
		check = checkUDPPublishedPort(ctx, t, c, host, p.serviceID, hostAddr, port, payload)
	}
	poll.WaitOn(t, check, poll.WithTimeout(30*time.Second))
}

// probeRefused returns the error from a single attempt to reach the published
// port at hostAddr from host - nil if the traffic was accepted.
func (p *publishedTestPort) probeRefused(t *testing.T, host networking.Host, hostAddr string) error {
	t.Helper()
	port := strconv.Itoa(p.port)
	if p.proto == network.UDP {
		return probeUDPPort(t, host, hostAddr, port)
	}
	return dialPublishedPort(t, host, hostAddr, port)
}

// checkUDPPublishedPort returns a poll check that a datagram sent to a published
// UDP port reaches a task. A UDP sender gets no feedback, so delivery is confirmed
// from what the task logs. Every attempt re-sends: a datagram arriving while the
// listener is between peers is lost, and UDP won't retransmit it.
func checkUDPPublishedPort(ctx context.Context, t *testing.T, c *client.Client, host networking.Host, serviceID, hostAddr, port, payload string) func(poll.LogT) poll.Result {
	return func(_ poll.LogT) poll.Result {
		var sendErr error
		// This is called from inside a "Do()" thread in the docker host's netns, but
		// it uses poll.WaitOn - which runs the check in a different goroutine.
		host.Do(t, func() {
			var conn stdnet.Conn
			conn, sendErr = stdnet.Dial("udp", stdnet.JoinHostPort(hostAddr, port))
			if sendErr != nil {
				return
			}
			defer conn.Close()
			_, sendErr = conn.Write([]byte(payload + "\n"))
		})
		if sendErr != nil {
			return poll.Continue("sending to %s: %v", stdnet.JoinHostPort(hostAddr, port), sendErr)
		}

		logs, err := serviceStdout(ctx, c, serviceID)
		if err != nil {
			return poll.Continue("reading service logs: %v", err)
		}
		if !strings.Contains(logs, payload) {
			return poll.Continue("%q not found in service logs", payload)
		}
		return poll.Success()
	}
}

func serviceStdout(ctx context.Context, c *client.Client, serviceID string) (string, error) {
	rdr, err := c.ServiceLogs(ctx, serviceID, client.ServiceLogsOptions{ShowStdout: true})
	if err != nil {
		return "", err
	}
	defer rdr.Close()
	out, err := io.ReadAll(rdr)
	return string(out), err
}

// dialPublishedPort opens a TCP connection to hostAddr:port from host's netns and
// returns the error the attempt failed with, or nil if it was accepted. Unlike
// checkPublishedPort it says nothing about what's behind the port - only what the
// host did with the handshake.
func dialPublishedPort(t *testing.T, host networking.Host, hostAddr, port string) error {
	t.Helper()
	var err error
	host.Do(t, func() {
		var conn stdnet.Conn
		// Dial on this goroutine, which Do has locked to a thread in the host's
		// netns - the connection must be made from in there.
		conn, err = stdnet.DialTimeout("tcp", stdnet.JoinHostPort(hostAddr, port), 5*time.Second)
		if err == nil {
			conn.Close()
		}
	})
	return err
}

// probeUDPPort sends datagrams to hostAddr:port from host's netns until one of them
// draws an error, and returns that error - or nil if they were all swallowed.
//
// UDP has no handshake to refuse, so nothing fails at connect time: a host with
// nothing bound answers with an ICMP port-unreachable, which the kernel reports on
// the connected socket's next operation. Re-sending, rather than sending once and
// reading, is what makes that robust - a read only ever reports on the datagram
// already sent, and one lost on the way draws no error to wait for. A miss reads as
// no error at all, just like a reintroduced bug, and repeating costs nothing here:
// ICMP rate limiting is off in this netns.
func probeUDPPort(t *testing.T, host networking.Host, hostAddr, port string) error {
	t.Helper()
	var err error
	host.Do(t, func() {
		var conn stdnet.Conn
		conn, err = stdnet.Dial("udp", stdnet.JoinHostPort(hostAddr, port))
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err = conn.Write([]byte("probe\n")); err != nil {
			return
		}
		for range 5 {
			time.Sleep(200 * time.Millisecond)
			if _, err = conn.Write([]byte("probe\n")); err != nil {
				return
			}
		}
	})
	return err
}

// TestIngressPortFromTaskOnSameNode checks that a container on the node can
// reach a service's published port via one of the node's own addresses, with
// and without the userland proxy, while the node drops unsolicited input.
//
// The ingress load balancer's published ports are mapped onto its
// docker_gwbridge endpoint, and docker_gwbridge is created with inter-container
// communication disabled. The container's packets are DNATed across that
// bridge, so they have to get past the rules that implement icc=false. With
// the userland proxy enabled, packets from a container's own bridge are
// normally left for docker-proxy instead, which receives them through the
// host's INPUT chain, where a host firewall such as ufw drops them. Ingress
// ports are DNATed anyway, so that reaching a published port doesn't depend
// on the userland proxy or the host's input policy. Both modes are checked
// and expected to behave the same.
//
// Regression test for https://github.com/moby/moby/issues/53713 and
// https://github.com/moby/moby/issues/53962
func TestIngressPortFromTaskOnSameNode(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.IsRootless, "rootless mode doesn't support Swarm-mode")
	skip.If(t, testEnv.FirewallBackendDriver() == "nftables")
	skip.If(t, networking.FirewalldRunning(), "can't use firewalld in host netns to add rules in L3Segment")
	ctx := setupTest(t)

	for i, tc := range []struct {
		name          string
		userlandProxy bool
	}{
		{name: "with-proxy", userlandProxy: true},
		{name: "no-proxy", userlandProxy: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Run each case in its own netns, so that it doesn't interfere with
			// iptables on the test host and the host ports it publishes are its
			// own. The other tests in this file use 111-114, and each needs its L3
			// segment's prefix to itself.
			hostAddr := fmt.Sprintf("192.168.%d.222", 115+i)
			l3SegHost := fmt.Sprintf("ipft%d", i)
			l3 := networking.NewL3Segment(t, "test-"+l3SegHost)
			defer l3.Destroy(t)
			l3.AddHost(t, l3SegHost, "ns-"+l3SegHost, "eth0", netip.MustParsePrefix(hostAddr+"/24"))

			// Drop unsolicited input, like ufw's default "deny incoming" policy.
			host := l3.Hosts[l3SegHost]
			host.MustRun(t, "iptables", "-A", "INPUT", "-i", "lo", "-j", "ACCEPT")
			host.MustRun(t, "iptables", "-A", "INPUT", "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT")
			host.MustRun(t, "iptables", "-P", "INPUT", "DROP")

			l3.Hosts[l3SegHost].Do(t, func() {
				// Not swarm.NewSwarm, because it can't pass extra daemon flags.
				d := daemon.New(t, daemon.WithSwarmIptables(true))
				d.StartWithBusybox(ctx, t, "--swarm-default-advertise-addr=lo",
					fmt.Sprintf("--userland-proxy=%v", tc.userlandProxy))
				defer d.Stop(t)
				d.SwarmInit(ctx, t, swarmtypes.InitRequest{})
				c := d.NewClientT(t)
				defer c.Close()

				serviceID := createPublishingService(ctx, t, d, c, "test-"+l3SegHost, 1, ingressPortSpec(8080))
				defer func() {
					_, err := c.ServiceRemove(ctx, serviceID, client.ServiceRemoveOptions{})
					assert.NilError(t, err)
				}()

				t.Log("Checking http access from outside the node")
				poll.WaitOn(t, checkPublishedPort(t, l3.Hosts[l3SegHost], hostAddr, "8080"), poll.WithTimeout(30*time.Second))

				// An attachable overlay network, so that the container below gets an
				// endpoint on docker_gwbridge and routes to the node's address over
				// it, like any Swarm task would.
				const netName = "ipft-overlay"
				net.CreateNoError(ctx, t, c, netName, net.WithDriver("overlay"), net.WithAttachable())
				defer net.RemoveNoError(ctx, t, c, netName)

				t.Log("Checking http access from a container on the node")
				res := container.RunAttach(ctx, t, c,
					container.WithNetworkMode(netName),
					container.WithCmd("wget", "-T3", "-t1", "-O-", "http://"+stdnet.JoinHostPort(hostAddr, "8080")),
				)
				defer c.ContainerRemove(ctx, res.ContainerID, client.ContainerRemoveOptions{Force: true})
				assert.Check(t, is.Contains(res.Stderr.String(), "404 Not Found"),
					"a container on the node should reach the published port via the node's address; got: %s", res.Stderr.String())
			})
		})
	}
}
