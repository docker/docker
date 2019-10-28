// +build !windows

package macvlan // import "github.com/docker/docker/integration/network/macvlan"

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/client"
	"github.com/docker/docker/integration/internal/container"
	net "github.com/docker/docker/integration/internal/network"
	n "github.com/docker/docker/integration/network"
	"github.com/docker/docker/testutil/daemon"
	"gotest.tools/assert"
	"gotest.tools/skip"
)

func TestDockerNetworkMacvlanPersistance(t *testing.T) {
	// verify the driver automatically provisions the 802.1q link (dm-dummy0.60)
	skip.If(t, testEnv.IsRemoteDaemon)

	d := daemon.New(t)
	d.StartWithBusybox(t)
	defer d.Stop(t)

	master := "dm-dummy0"
	n.CreateMasterDummy(t, master)
	defer n.DeleteInterface(t, master)

	c := d.NewClientT(t)

	netName := "dm-persist"
	net.CreateNoError(context.Background(), t, c, netName,
		net.WithMacvlan("dm-dummy0.60"),
	)
	assert.Check(t, n.IsNetworkAvailable(c, netName))
	d.Restart(t)
	assert.Check(t, n.IsNetworkAvailable(c, netName))
}

func TestDockerNetworkMacvlan(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)

	for _, tc := range []struct {
		name string
		test func(client.APIClient) func(*testing.T)
	}{
		{
			name: "Subinterface",
			test: testMacvlanSubinterface,
		}, {
			name: "OverlapParent",
			test: testMacvlanOverlapParent,
		}, {
			name: "NilParent",
			test: testMacvlanNilParent,
		}, {
			name: "InternalMode",
			test: testMacvlanInternalMode,
		}, {
			name: "MultiSubnet",
			test: testMacvlanMultiSubnet,
		}, {
			name: "Addressing",
			test: testMacvlanAddressing,
		},
	} {
		d := daemon.New(t)
		d.StartWithBusybox(t)
		c := d.NewClientT(t)

		t.Run(tc.name, tc.test(c))

		d.Stop(t)
		// FIXME(vdemeester) clean network
	}
}

func testMacvlanOverlapParent(client client.APIClient) func(*testing.T) {
	return func(t *testing.T) {
		// verify the same parent interface cannot be used if already in use by an existing network
		master := "dm-dummy0"
		n.CreateMasterDummy(t, master)
		defer n.DeleteInterface(t, master)

		netName := "dm-subinterface"
		parentName := "dm-dummy0.40"
		net.CreateNoError(context.Background(), t, client, netName,
			net.WithMacvlan(parentName),
		)
		assert.Check(t, n.IsNetworkAvailable(client, netName))

		_, err := net.Create(context.Background(), client, "dm-parent-net-overlap",
			net.WithMacvlan(parentName),
		)
		assert.Check(t, err != nil)

		// delete the network while preserving the parent link
		err = client.NetworkRemove(context.Background(), netName)
		assert.NilError(t, err)

		assert.Check(t, n.IsNetworkNotAvailable(client, netName))
		// verify the network delete did not delete the predefined link
		n.LinkExists(t, master)
	}
}

func testMacvlanSubinterface(client client.APIClient) func(*testing.T) {
	return func(t *testing.T) {
		// verify the same parent interface cannot be used if already in use by an existing network
		master := "dm-dummy0"
		parentName := "dm-dummy0.20"
		n.CreateMasterDummy(t, master)
		defer n.DeleteInterface(t, master)
		n.CreateVlanInterface(t, master, parentName, "20")

		netName := "dm-subinterface"
		net.CreateNoError(context.Background(), t, client, netName,
			net.WithMacvlan(parentName),
		)
		assert.Check(t, n.IsNetworkAvailable(client, netName))

		// delete the network while preserving the parent link
		err := client.NetworkRemove(context.Background(), netName)
		assert.NilError(t, err)

		assert.Check(t, n.IsNetworkNotAvailable(client, netName))
		// verify the network delete did not delete the predefined link
		n.LinkExists(t, parentName)
	}
}

func testMacvlanNilParent(client client.APIClient) func(*testing.T) {
	return func(t *testing.T) {
		// macvlan bridge mode - dummy parent interface is provisioned dynamically
		netName := "dm-nil-parent"
		net.CreateNoError(context.Background(), t, client, netName,
			net.WithMacvlan(""),
		)
		assert.Check(t, n.IsNetworkAvailable(client, netName))

		ctx := context.Background()
		id1 := container.Run(ctx, t, client, container.WithNetworkMode(netName))
		id2 := container.Run(ctx, t, client, container.WithNetworkMode(netName))

		_, err := container.Exec(ctx, client, id2, []string{"ping", "-c", "1", id1})
		assert.Check(t, err == nil)
	}
}

func testMacvlanInternalMode(client client.APIClient) func(*testing.T) {
	return func(t *testing.T) {
		// macvlan bridge mode - dummy parent interface is provisioned dynamically
		netName := "dm-internal"
		net.CreateNoError(context.Background(), t, client, netName,
			net.WithMacvlan(""),
			net.WithInternal(),
		)
		assert.Check(t, n.IsNetworkAvailable(client, netName))

		ctx := context.Background()
		id1 := container.Run(ctx, t, client, container.WithNetworkMode(netName))
		id2 := container.Run(ctx, t, client, container.WithNetworkMode(netName))

		timeoutCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		_, err := container.Exec(timeoutCtx, client, id1, []string{"ping", "-c", "1", "-w", "1", "8.8.8.8"})
		// FIXME(vdemeester) check the time of error ?
		assert.Check(t, err != nil)

		type timeoutError interface{ Timeout() bool }
		assert.ErrorType(t, timeoutCtx.Err(), (*timeoutError)(nil))
		assert.Equal(t, timeoutCtx.Err().(timeoutError).Timeout(), true)

		_, err = container.Exec(ctx, client, id2, []string{"ping", "-c", "1", id1})
		assert.Check(t, err == nil)
	}
}

func testMacvlanMultiSubnet(client client.APIClient) func(*testing.T) {
	return func(t *testing.T) {
		netName := "dualstackbridge"
		net.CreateNoError(context.Background(), t, client, netName,
			net.WithMacvlan(""),
			net.WithIPv6(),
			net.WithIPAM("172.28.100.0/24", ""),
			net.WithIPAM("172.28.102.0/24", "172.28.102.254"),
			net.WithIPAM("2001:db8:abc2::/64", ""),
			net.WithIPAM("2001:db8:abc4::/64", "2001:db8:abc4::254"),
		)

		assert.Check(t, n.IsNetworkAvailable(client, netName))

		// start dual stack containers and verify the user specified --ip and --ip6 addresses on subnets 172.28.100.0/24 and 2001:db8:abc2::/64
		ctx := context.Background()
		id1 := container.Run(ctx, t, client,
			container.WithNetworkMode("dualstackbridge"),
			container.WithIPv4("dualstackbridge", "172.28.100.20"),
			container.WithIPv6("dualstackbridge", "2001:db8:abc2::20"),
		)
		id2 := container.Run(ctx, t, client,
			container.WithNetworkMode("dualstackbridge"),
			container.WithIPv4("dualstackbridge", "172.28.100.21"),
			container.WithIPv6("dualstackbridge", "2001:db8:abc2::21"),
		)
		c1, err := client.ContainerInspect(ctx, id1)
		assert.NilError(t, err)

		// verify ipv4 connectivity to the explicit --ipv address second to first
		_, err = container.Exec(ctx, client, id2, []string{"ping", "-c", "1", c1.NetworkSettings.Networks["dualstackbridge"].IPAddress})
		assert.NilError(t, err)
		// verify ipv6 connectivity to the explicit --ipv6 address second to first
		_, err = container.Exec(ctx, client, id2, []string{"ping6", "-c", "1", c1.NetworkSettings.Networks["dualstackbridge"].GlobalIPv6Address})
		assert.NilError(t, err)

		// start dual stack containers and verify the user specified --ip and --ip6 addresses on subnets 172.28.102.0/24 and 2001:db8:abc4::/64
		id3 := container.Run(ctx, t, client,
			container.WithNetworkMode("dualstackbridge"),
			container.WithIPv4("dualstackbridge", "172.28.102.20"),
			container.WithIPv6("dualstackbridge", "2001:db8:abc4::20"),
		)
		id4 := container.Run(ctx, t, client,
			container.WithNetworkMode("dualstackbridge"),
			container.WithIPv4("dualstackbridge", "172.28.102.21"),
			container.WithIPv6("dualstackbridge", "2001:db8:abc4::21"),
		)
		c3, err := client.ContainerInspect(ctx, id3)
		assert.NilError(t, err)

		// verify ipv4 connectivity to the explicit --ipv address from third to fourth
		_, err = container.Exec(ctx, client, id4, []string{"ping", "-c", "1", c3.NetworkSettings.Networks["dualstackbridge"].IPAddress})
		assert.NilError(t, err)
		// verify ipv6 connectivity to the explicit --ipv6 address from third to fourth
		_, err = container.Exec(ctx, client, id4, []string{"ping6", "-c", "1", c3.NetworkSettings.Networks["dualstackbridge"].GlobalIPv6Address})
		assert.NilError(t, err)

		// Inspect the v4 gateway to ensure the proper default GW was assigned
		assert.Equal(t, c1.NetworkSettings.Networks["dualstackbridge"].Gateway, "172.28.100.1")
		// Inspect the v6 gateway to ensure the proper default GW was assigned
		assert.Equal(t, c1.NetworkSettings.Networks["dualstackbridge"].IPv6Gateway, "2001:db8:abc2::1")
		// Inspect the v4 gateway to ensure the proper explicitly assigned default GW was assigned
		assert.Equal(t, c3.NetworkSettings.Networks["dualstackbridge"].Gateway, "172.28.102.254")
		// Inspect the v6 gateway to ensure the proper explicitly assigned default GW was assigned
		assert.Equal(t, c3.NetworkSettings.Networks["dualstackbridge"].IPv6Gateway, "2001:db8:abc4::254")
	}
}

func testMacvlanAddressing(client client.APIClient) func(*testing.T) {
	return func(t *testing.T) {
		// Ensure the default gateways, next-hops and default dev devices are properly set
		netName := "dualstackbridge"
		net.CreateNoError(context.Background(), t, client, netName,
			net.WithMacvlan(""),
			net.WithIPv6(),
			net.WithOption("macvlan_mode", "bridge"),
			net.WithIPAM("172.28.130.0/24", ""),
			net.WithIPAM("2001:db8:abca::/64", "2001:db8:abca::254"),
		)
		assert.Check(t, n.IsNetworkAvailable(client, netName))

		ctx := context.Background()
		id1 := container.Run(ctx, t, client,
			container.WithNetworkMode("dualstackbridge"),
		)

		// Validate macvlan bridge mode defaults gateway sets the default IPAM next-hop inferred from the subnet
		result, err := container.Exec(ctx, client, id1, []string{"ip", "route"})
		assert.NilError(t, err)
		assert.Check(t, strings.Contains(result.Combined(), "default via 172.28.130.1 dev eth0"))
		// Validate macvlan bridge mode sets the v6 gateway to the user specified default gateway/next-hop
		result, err = container.Exec(ctx, client, id1, []string{"ip", "-6", "route"})
		assert.NilError(t, err)
		assert.Check(t, strings.Contains(result.Combined(), "default via 2001:db8:abca::254 dev eth0"))
	}
}
