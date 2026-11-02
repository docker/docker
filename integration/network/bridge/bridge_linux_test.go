package bridge

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/google/go-cmp/cmp/cmpopts"
	containertypes "github.com/moby/moby/api/types/container"
	networktypes "github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/versions"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge"
	"github.com/moby/moby/v2/daemon/libnetwork/netlabel"
	"github.com/moby/moby/v2/daemon/libnetwork/nlwrap"
	ctr "github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/integration/internal/network"
	"github.com/moby/moby/v2/integration/internal/testutils/networking"
	n "github.com/moby/moby/v2/integration/network"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"github.com/vishvananda/netlink"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/icmd"
	"gotest.tools/v3/skip"
)

func TestCreateWithMultiNetworks(t *testing.T) {
	skip.If(t, versions.LessThan(testEnv.DaemonAPIVersion(), "1.44"), "requires API v1.44")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	network.CreateNoError(ctx, t, apiClient, "testnet1")
	defer network.RemoveNoError(ctx, t, apiClient, "testnet1")

	network.CreateNoError(ctx, t, apiClient, "testnet2")
	defer network.RemoveNoError(ctx, t, apiClient, "testnet2")

	attachCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res := ctr.RunAttach(attachCtx, t, apiClient,
		ctr.WithCmd("ip", "-o", "-4", "addr", "show"),
		ctr.WithNetworkMode("testnet1"),
		ctr.WithEndpointSettings("testnet1", &networktypes.EndpointSettings{}),
		ctr.WithEndpointSettings("testnet2", &networktypes.EndpointSettings{}))

	assert.Equal(t, res.ExitCode, 0)
	assert.Equal(t, res.Stderr.String(), "")

	// Only interfaces with an IPv4 address are printed by iproute2 when flag -4 is specified. Here, we should have two
	// interfaces for testnet1 and testnet2, plus lo.
	ifacesWithAddress := strings.Count(res.Stdout.String(), "\n")
	assert.Equal(t, ifacesWithAddress, 3)
}

func TestCreateMultipleNetworksWithOverlappingIPAM(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	prefix := t.Name()
	test0 := prefix + "-test0"
	test1 := prefix + "-test1"
	test2 := prefix + "-test2"
	test3 := prefix + "-test3"
	test4 := prefix + "-test4"
	test5 := prefix + "-test5"

	withDefaultIPAM := func(options *client.NetworkCreateOptions) {
		options.IPAM.Driver = "default"
	}
	remove := func(name string) error {
		_, err := apiClient.NetworkRemove(ctx, name, client.NetworkRemoveOptions{})
		return err
	}

	// test0 bridge network
	network.CreateNoError(ctx, t, apiClient, test0,
		network.WithDriver("bridge"),
		network.WithIPAMRange("192.178.0.0/16", "192.178.128.0/17", "192.178.138.100"),
		withDefaultIPAM,
	)
	test0Created := true
	t.Cleanup(func() {
		if test0Created {
			assert.NilError(t, remove(test0))
		}
	})
	assert.Check(t, n.IsNetworkAvailable(ctx, apiClient, test0))

	test1Created := false
	t.Cleanup(func() {
		if test1Created {
			assert.NilError(t, remove(test1))
		}
	})
	// test1 bridge network overlaps with test0
	_, err := network.Create(ctx, apiClient, test1,
		network.WithDriver("bridge"),
		network.WithIPAM("192.178.128.0/17", "192.178.128.1"),
		withDefaultIPAM,
	)
	if err == nil {
		test1Created = true
	}
	assert.Check(t, is.ErrorType(err, cerrdefs.IsPermissionDenied))
	assert.Check(t, n.IsNetworkNotAvailable(ctx, apiClient, test1))

	// test2 bridge network does not overlap
	network.CreateNoError(ctx, t, apiClient, test2,
		network.WithDriver("bridge"),
		network.WithIPAM("192.169.0.0/16", "192.169.100.100"),
		withDefaultIPAM,
	)
	t.Cleanup(func() { assert.NilError(t, remove(test2)) })
	assert.Check(t, n.IsNetworkAvailable(ctx, apiClient, test2))

	// remove test0 and retry to create test1
	if err := remove(test0); err != nil {
		assert.NilError(t, err)
	} else {
		test0Created = false
	}

	network.CreateNoError(ctx, t, apiClient, test1,
		network.WithDriver("bridge"),
		network.WithIPAM("192.178.128.0/17", "192.178.128.1"),
		withDefaultIPAM,
	)
	test1Created = true
	assert.Check(t, n.IsNetworkAvailable(ctx, apiClient, test1))

	// for networks w/o ipam specified, docker will choose proper non-overlapping subnets
	for _, name := range []string{test3, test4, test5} {
		network.CreateNoError(ctx, t, apiClient, name)
		t.Cleanup(func() { assert.NilError(t, remove(name)) })
		assert.Check(t, n.IsNetworkAvailable(ctx, apiClient, name))
	}
}

func TestCreateWithIPv6DefaultsToULAPrefix(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	const nwName = "testnetula"
	network.CreateNoError(ctx, t, apiClient, nwName, network.WithIPv6())
	defer network.RemoveNoError(ctx, t, apiClient, nwName)

	res, err := apiClient.NetworkInspect(ctx, "testnetula", client.NetworkInspectOptions{})
	assert.NilError(t, err)

	for _, ipam := range res.Network.IPAM.Config {
		if netip.MustParsePrefix("fd00::/8").Overlaps(ipam.Subnet) {
			return
		}
	}

	t.Fatalf("Network %s has no ULA prefix, expected one.", nwName)
}

func TestCreateWithIPv6WithoutEnableIPv6Flag(t *testing.T) {
	ctx := setupTest(t)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t, "-D", "--default-network-opt=bridge=com.docker.network.enable_ipv6=true")
	defer d.Stop(t)

	apiClient := d.NewClientT(t)
	defer apiClient.Close()

	const nwName = "testnetula"
	network.CreateNoError(ctx, t, apiClient, nwName)
	defer network.RemoveNoError(ctx, t, apiClient, nwName)

	res, err := apiClient.NetworkInspect(ctx, "testnetula", client.NetworkInspectOptions{})
	assert.NilError(t, err)

	for _, ipam := range res.Network.IPAM.Config {
		if netip.MustParsePrefix("fd00::/8").Overlaps(ipam.Subnet) {
			return
		}
	}

	t.Fatalf("Network %s has no ULA prefix, expected one.", nwName)
}

// TestDefaultIPvOptOverride checks that when default-network-opts set enable_ipv4 or
// enable_ipv6, and those values are overridden for a network, the default option
// values don't show up in network inspect output. (Because it's confusing if the
// default shows up when it's been overridden with a different value.)
func TestDefaultIPvOptOverride(t *testing.T) {
	ctx := setupTest(t)
	d := daemon.New(t)
	const opt4 = "false"
	const opt6 = "true"
	d.StartWithBusybox(ctx, t,
		"--default-network-opt=bridge=com.docker.network.enable_ipv4="+opt4,
		"--default-network-opt=bridge=com.docker.network.enable_ipv6="+opt6,
	)
	defer d.Stop(t)
	c := d.NewClientT(t)

	t.Run("TestDefaultIPvOptOverride", func(t *testing.T) {
		for _, override4 := range []bool{false, true} {
			for _, override6 := range []bool{false, true} {
				t.Run(fmt.Sprintf("override4=%v,override6=%v", override4, override6), func(t *testing.T) {
					t.Parallel()
					netName := fmt.Sprintf("tdioo-%v-%v", override4, override6)
					var nopts []func(*client.NetworkCreateOptions)
					if override4 {
						nopts = append(nopts, network.WithIPv4(true))
					}
					if override6 {
						nopts = append(nopts, network.WithIPv6())
					}
					network.CreateNoError(ctx, t, c, netName, nopts...)
					defer network.RemoveNoError(ctx, t, c, netName)

					res, err := c.NetworkInspect(ctx, netName, client.NetworkInspectOptions{})
					assert.NilError(t, err)
					t.Log("override4", override4, "override6", override6, "->", res.Network.Options)

					gotOpt4, have4 := res.Network.Options[netlabel.EnableIPv4]
					assert.Check(t, is.Equal(have4, !override4))
					assert.Check(t, is.Equal(res.Network.EnableIPv4, override4))
					if have4 {
						assert.Check(t, is.Equal(gotOpt4, opt4))
					}

					gotOpt6, have6 := res.Network.Options[netlabel.EnableIPv6]
					assert.Check(t, is.Equal(have6, !override6))
					assert.Check(t, is.Equal(res.Network.EnableIPv6, true))
					if have6 {
						assert.Check(t, is.Equal(gotOpt6, opt6))
					}
				})
			}
		}
	})
}

// Check that it's possible to create IPv6 networks with a 64-bit ip-range,
// in 64-bit and bigger subnets, with and without a gateway.
func Test64BitIPRange(t *testing.T) {
	ctx := setupTest(t)
	c := testEnv.APIClient()

	type kv struct{ k, v string }
	subnets := []kv{
		{"64-bit-subnet", "fd2e:b68c:ce26::/64"},
		{"56-bit-subnet", "fd2e:b68c:ce26::/56"},
	}
	ipRanges := []kv{
		{"no-range", ""},
		{"64-bit-range", "fd2e:b68c:ce26::/64"},
	}
	gateways := []kv{
		{"no-gateway", ""},
		{"with-gateway", "fd2e:b68c:ce26::1"},
	}

	for _, sn := range subnets {
		for _, ipr := range ipRanges {
			for _, gw := range gateways {
				ipamSetter := network.WithIPAMRange(sn.v, ipr.v, gw.v)
				t.Run(sn.k+"/"+ipr.k+"/"+gw.k, func(t *testing.T) {
					ctx := testutil.StartSpan(ctx, t)
					const netName = "test64br"
					network.CreateNoError(ctx, t, c, netName, network.WithIPv6(), ipamSetter)
					defer network.RemoveNoError(ctx, t, c, netName)
				})
			}
		}
	}
}

// Demonstrate a limitation of the IP address allocator, it can't
// allocate the last address in range that ends on a 64-bit boundary.
func TestIPRangeAt64BitLimit(t *testing.T) {
	ctx := setupTest(t)
	c := testEnv.APIClient()

	tests := []struct {
		name    string
		subnet  string
		ipRange string
	}{
		{
			name:    "ipRange before end of 64-bit subnet",
			subnet:  "fda9:8d04:086e::/64",
			ipRange: "fda9:8d04:086e::ffff:ffff:ffff:ff0e/127",
		},
		{
			name:    "ipRange at end of 64-bit subnet",
			subnet:  "fda9:8d04:086e::/64",
			ipRange: "fda9:8d04:086e::ffff:ffff:ffff:fffe/127",
		},
		{
			name:    "ipRange at 64-bit boundary inside 56-bit subnet",
			subnet:  "fda9:8d04:086e::/56",
			ipRange: "fda9:8d04:086e:aa:ffff:ffff:ffff:fffe/127",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)
			const netName = "test64bl"
			network.CreateNoError(ctx, t, c, netName,
				network.WithIPv6(),
				network.WithIPAMRange(tc.subnet, tc.ipRange, ""),
			)
			defer network.RemoveNoError(ctx, t, c, netName)

			id := ctr.Create(ctx, t, c, ctr.WithNetworkMode(netName))
			defer c.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
			_, err := c.ContainerStart(ctx, id, client.ContainerStartOptions{})
			assert.NilError(t, err)
		})
	}
}

// TestFilterForwardPolicy tests that, if the daemon enables IP forwarding on the
// host, it also sets the iptables filter-FORWARD policy to DROP (unless it's
// told not to).
func TestFilterForwardPolicy(t *testing.T) {
	skip.If(t, testEnv.IsRootless, "rootless has its own netns")
	skip.If(t, networking.FirewalldRunning(), "can't use firewalld in host netns to add rules in L3Segment")
	skip.If(t, strings.HasPrefix(testEnv.FirewallBackendDriver(), "nftables"), "no policy is set for nftables")

	ctx := setupTest(t)

	// Set up a netns for each test to avoid sysctl and iptables pollution.
	addr4 := netip.MustParseAddr("192.168.125.1")
	addr6 := netip.MustParseAddr("fd76:c828:41f9::1")
	l3 := networking.NewL3Segment(t, "test-ffp",
		netip.PrefixFrom(addr4, 24),
		netip.PrefixFrom(addr6, 64),
	)
	t.Cleanup(func() { l3.Destroy(t) })

	tests := []struct {
		name           string
		initForwarding string
		daemonArgs     []string
		expForwarding  string
		expPolicy      string
	}{
		{
			name:           "enable forwarding",
			initForwarding: "0",
			expForwarding:  "1",
			expPolicy:      "DROP",
		},
		{
			name:           "forwarding already enabled",
			initForwarding: "1",
			expForwarding:  "1",
			expPolicy:      "ACCEPT",
		},
		{
			name:           "no drop",
			initForwarding: "0",
			daemonArgs:     []string{"--ip-forward-no-drop"},
			expForwarding:  "1",
			expPolicy:      "ACCEPT",
		},
		{
			name:           "no forwarding",
			initForwarding: "0",
			daemonArgs:     []string{"--ip-forward=false"},
			expForwarding:  "0",
			expPolicy:      "ACCEPT",
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)

			// Create a netns for this test.
			addr4, addr6 = addr4.Next(), addr6.Next()
			hostname := fmt.Sprintf("docker%d", i)
			l3.AddHost(t, hostname, hostname+"-host", "eth0",
				netip.PrefixFrom(addr4, 24),
				netip.PrefixFrom(addr6, 64),
			)
			host := l3.Hosts[hostname]

			getFwdPolicy := func(cmd string) string {
				t.Helper()
				out := host.MustRun(t, cmd, "-S", "FORWARD")
				if strings.HasPrefix(out, "-P FORWARD ACCEPT") {
					return "ACCEPT"
				}
				if strings.HasPrefix(out, "-P FORWARD DROP") {
					return "DROP"
				}
				t.Fatalf("Failed to determine %s FORWARD policy: %s", cmd, out)
				return ""
			}

			type sysctls struct{ v4, v6def, v6all string }
			getSysctls := func() sysctls {
				t.Helper()
				return sysctls{
					host.MustRun(t, "sysctl", "-n", "net.ipv4.ip_forward")[:1],
					host.MustRun(t, "sysctl", "-n", "net.ipv6.conf.default.forwarding")[:1],
					host.MustRun(t, "sysctl", "-n", "net.ipv6.conf.all.forwarding")[:1],
				}
			}

			// Initial settings for IP forwarding params.
			host.MustRun(t, "sysctl", "-w", "net.ipv4.ip_forward="+tc.initForwarding)
			host.MustRun(t, "sysctl", "-w", "net.ipv6.conf.all.forwarding="+tc.initForwarding)

			// Start the daemon in its own network namespace.
			var d *daemon.Daemon
			host.Do(t, func() {
				// Run without OTel because there's no routing from this netns for it - which
				// means the daemon doesn't shut down cleanly, causing the test to fail.
				d = daemon.New(t, daemon.WithEnvVars("OTEL_EXPORTER_OTLP_ENDPOINT="))
				d.StartWithBusybox(ctx, t, tc.daemonArgs...)
				t.Cleanup(func() { d.Stop(t) })
			})
			c := d.NewClientT(t)
			t.Cleanup(func() { c.Close() })

			// If necessary, the IPv4 policy should have been updated when the default bridge network was created.
			assert.Check(t, is.Equal(getFwdPolicy("iptables"), tc.expPolicy))
			// IPv6 policy should not have been updated yet.
			assert.Check(t, is.Equal(getFwdPolicy("ip6tables"), "ACCEPT"))
			assert.Check(t, is.Equal(getSysctls(), sysctls{tc.expForwarding, tc.initForwarding, tc.initForwarding}))

			// If necessary, creating an IPv6 network should update the sysctls and policy.
			const netName = "testnetffp"
			network.CreateNoError(ctx, t, c, netName, network.WithIPv6())
			t.Cleanup(func() { network.RemoveNoError(ctx, t, c, netName) })
			assert.Check(t, is.Equal(getFwdPolicy("iptables"), tc.expPolicy))
			assert.Check(t, is.Equal(getFwdPolicy("ip6tables"), tc.expPolicy))
			assert.Check(t, is.Equal(getSysctls(), sysctls{tc.expForwarding, tc.expForwarding, tc.expForwarding}))
		})
	}
}

// TestPointToPoint checks that a "/31" --internal network with inhibit_ipv4,
// or gateway mode "isolated" has two addresses available for containers (no
// address is reserved for a gateway, because it won't be used).
func TestPointToPoint(t *testing.T) {
	ctx := setupTest(t)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	t.Cleanup(func() { d.Stop(t) })

	apiClient := d.NewClientT(t)
	t.Cleanup(func() { apiClient.Close() })

	testcases := []struct {
		name   string
		netOpt func(*client.NetworkCreateOptions)
	}{
		{
			name:   "inhibit_ipv4",
			netOpt: network.WithOption(bridge.InhibitIPv4, "true"),
		},
		{
			name:   "isolated",
			netOpt: network.WithOption(bridge.IPv4GatewayMode, "isolated"),
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)

			const netName = "testp2pbridge"
			network.CreateNoError(ctx, t, apiClient, netName,
				network.WithIPAM("192.168.135.0/31", ""),
				network.WithInternal(),
				tc.netOpt,
			)
			defer network.RemoveNoError(ctx, t, apiClient, netName)

			const ctrName = "ctr1"
			id := ctr.Run(ctx, t, apiClient,
				ctr.WithNetworkMode(netName),
				ctr.WithName(ctrName),
			)
			defer apiClient.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})

			attachCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			res := ctr.RunAttach(attachCtx, t, apiClient,
				ctr.WithCmd([]string{"ping", "-c1", "-W3", ctrName}...),
				ctr.WithNetworkMode(netName),
			)
			defer apiClient.ContainerRemove(ctx, res.ContainerID, client.ContainerRemoveOptions{Force: true})
			assert.Check(t, is.Equal(res.ExitCode, 0))
			assert.Check(t, is.Equal(res.Stderr.Len(), 0))
			assert.Check(t, is.Contains(res.Stdout.String(), "1 packets transmitted, 1 packets received"))
		})
	}
}

// TestIsolated tests an internal network with gateway mode "isolated".
func TestIsolated(t *testing.T) {
	skip.If(t, testEnv.IsRootless, "can't inspect bridge addrs in rootless netns")

	ctx := setupTest(t)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	t.Cleanup(func() { d.Stop(t) })

	apiClient := d.NewClientT(t)
	t.Cleanup(func() { apiClient.Close() })

	const netName = "testisol"
	const bridgeName = "br-" + netName
	network.CreateNoError(ctx, t, apiClient, netName,
		network.WithIPv6(),
		network.WithInternal(),
		network.WithOption(bridge.IPv4GatewayMode, "isolated"),
		network.WithOption(bridge.IPv6GatewayMode, "isolated"),
		network.WithOption(bridge.BridgeName, bridgeName),
	)
	defer network.RemoveNoError(ctx, t, apiClient, netName)

	// The bridge should not have any IP addresses.
	link, err := nlwrap.LinkByName(bridgeName)
	assert.NilError(t, err)
	addrs, err := nlwrap.AddrList(link, netlink.FAMILY_ALL)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(addrs), 0))

	const ctrName = "ctr1"
	id := ctr.Run(ctx, t, apiClient,
		ctr.WithNetworkMode(netName),
		ctr.WithName(ctrName),
	)
	defer apiClient.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})

	ping := func(t *testing.T, ipv string) {
		t.Helper()
		attachCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		res := ctr.RunAttach(attachCtx, t, apiClient,
			ctr.WithCmd([]string{"ping", "-c1", "-W3", ipv, "ctr1"}...),
			ctr.WithNetworkMode(netName),
		)
		defer apiClient.ContainerRemove(ctx, res.ContainerID, client.ContainerRemoveOptions{Force: true})
		if ipv == "-6" && networking.FirewalldRunning() {
			// FIXME(robmry) - this fails due to https://github.com/moby/moby/issues/49680
			if res.ExitCode != 1 {
				t.Log("Unexpected pass!")
				t.Log(icmd.RunCommand("nft", "list ruleset").Stdout())
				t.Log(icmd.RunCommand("ip", "a").Stdout())
				t.Log(icmd.RunCommand("route", "-6").Stdout())
			}
			t.Skip("XFAIL - IPv6, firewalld, isolated - see https://github.com/moby/moby/issues/49680")
		}
		assert.Check(t, is.Equal(res.ExitCode, 0))
		assert.Check(t, is.Equal(res.Stderr.Len(), 0))
		assert.Check(t, is.Contains(res.Stdout.String(), "1 packets transmitted, 1 packets received"))
	}
	t.Run("ipv4", func(t *testing.T) { ping(t, "-4") })
	t.Run("ipv6", func(t *testing.T) { ping(t, "-6") })
}

func TestEndpointWithCustomIfname(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	ctrID := ctr.Run(ctx, t, apiClient,
		ctr.WithCmd("ip", "-o", "link", "show", "foobar"),
		ctr.WithEndpointSettings("bridge", &networktypes.EndpointSettings{
			DriverOpts: map[string]string{
				netlabel.Ifname: "foobar",
			},
		}))
	defer ctr.Remove(ctx, t, apiClient, ctrID, client.ContainerRemoveOptions{Force: true})

	out, err := ctr.Output(ctx, apiClient, ctrID)
	assert.NilError(t, err)
	assert.Assert(t, strings.Contains(out.Stdout, ": foobar@if"), "expected ': foobar@if' in 'ip link show':\n%s", out.Stdout)
}

// TestPublishedPortAlreadyInUse checks that a container that can't start
// because of one its published port being already in use doesn't end up
// triggering the restart loop.
//
// Regression test for: https://github.com/moby/moby/issues/49501
func TestPublishedPortAlreadyInUse(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()
	mappedPort := networktypes.MustParsePort("80/tcp")

	ctr1 := ctr.Run(ctx, t, apiClient,
		ctr.WithCmd("top"),
		ctr.WithExposedPorts("80/tcp"),
		ctr.WithPortMap(networktypes.PortMap{mappedPort: {{HostPort: "8000"}}}))
	defer ctr.Remove(ctx, t, apiClient, ctr1, client.ContainerRemoveOptions{Force: true})

	ctr2 := ctr.Create(ctx, t, apiClient,
		ctr.WithCmd("top"),
		ctr.WithRestartPolicy(containertypes.RestartPolicyAlways),
		ctr.WithExposedPorts("80/tcp"),
		ctr.WithPortMap(networktypes.PortMap{mappedPort: {{HostPort: "8000"}}}))
	defer ctr.Remove(ctx, t, apiClient, ctr2, client.ContainerRemoveOptions{Force: true})

	_, err := apiClient.ContainerStart(ctx, ctr2, client.ContainerStartOptions{})
	assert.Assert(t, is.ErrorContains(err, "failed to set up container networking"))

	inspect, err := apiClient.ContainerInspect(ctx, ctr2, client.ContainerInspectOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(inspect.Container.State.Status, containertypes.StateCreated))

	_, err = apiClient.ContainerStart(ctx, ctr2, client.ContainerStartOptions{})
	assert.Assert(t, is.ErrorContains(err, "failed to set up container networking"))

	inspect, err = apiClient.ContainerInspect(ctx, ctr2, client.ContainerInspectOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(inspect.Container.State.Status, containertypes.StateCreated))
	assert.Assert(t, is.Len(inspect.Container.HostConfig.PortBindings[mappedPort], 1))
	assert.Check(t, is.Equal(inspect.Container.HostConfig.PortBindings[mappedPort][0].HostPort, "8000"))
	// The bridge entry must survive the failed start, otherwise the next start
	// would silently succeed with no networking attached.
	assert.Assert(t, is.Contains(inspect.Container.NetworkSettings.Networks, "bridge"))

	// The container list must not report the endpoint that was rolled back.
	res, err := apiClient.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("id", ctr2),
	})
	assert.NilError(t, err)
	assert.Assert(t, is.Len(res.Items, 1))
	assert.Assert(t, res.Items[0].NetworkSettings != nil)
	ep := res.Items[0].NetworkSettings.Networks["bridge"]
	assert.Assert(t, ep != nil)
	assert.Check(t, is.Equal(ep.EndpointID, ""))
	assert.Check(t, !ep.IPAddress.IsValid())
}

// TestAllPortMappingsAreReturned check that dual-stack ports mapped through
// different networks are correctly reported as dual-stakc.
//
// Regression test for https://github.com/moby/moby/issues/49654.
func TestAllPortMappingsAreReturned(t *testing.T) {
	ctx := setupTest(t)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t, "--userland-proxy=false")
	defer d.Stop(t)

	apiClient := d.NewClientT(t)
	defer apiClient.Close()

	nwV4 := network.CreateNoError(ctx, t, apiClient, "testnetv4")
	defer network.RemoveNoError(ctx, t, apiClient, nwV4)

	nwV6 := network.CreateNoError(ctx, t, apiClient, "testnetv6",
		network.WithIPv4(false),
		network.WithIPv6())
	defer network.RemoveNoError(ctx, t, apiClient, nwV6)

	ctrID := ctr.Run(ctx, t, apiClient,
		ctr.WithExposedPorts("80/tcp", "81/tcp"),
		ctr.WithPortMap(networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {{HostPort: "8000"}}}),
		ctr.WithEndpointSettings("testnetv4", &networktypes.EndpointSettings{}),
		ctr.WithEndpointSettings("testnetv6", &networktypes.EndpointSettings{}))
	defer ctr.Remove(ctx, t, apiClient, ctrID, client.ContainerRemoveOptions{Force: true})

	inspect := ctr.Inspect(ctx, t, apiClient, ctrID)
	assert.DeepEqual(t, inspect.NetworkSettings.Ports, networktypes.PortMap{
		networktypes.MustParsePort("80/tcp"): []networktypes.PortBinding{
			{HostIP: netip.IPv4Unspecified(), HostPort: "8000"},
			{HostIP: netip.IPv6Unspecified(), HostPort: "8000"},
		},
		networktypes.MustParsePort("81/tcp"): nil,
	}, cmpopts.EquateComparable(netip.Addr{}))
}

// TestFirewalldReloadNoZombies checks that when firewalld is reloaded, rules
// belonging to deleted networks/containers do not reappear.
func TestFirewalldReloadNoZombies(t *testing.T) {
	skip.If(t, !networking.FirewalldRunning(), "firewalld is not running")
	skip.If(t, testEnv.IsRootless, "no firewalld in rootless netns")

	ctx := setupTest(t)
	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)
	c := d.NewClientT(t)

	const bridgeName = "br-fwdreload"
	removed := false
	nw := network.CreateNoError(ctx, t, c, "testnet",
		network.WithOption(bridge.BridgeName, bridgeName))
	defer func() {
		if !removed {
			network.RemoveNoError(ctx, t, c, nw)
		}
	}()

	cid := ctr.Run(ctx, t, c,
		ctr.WithExposedPorts("80/tcp", "81/tcp"),
		ctr.WithPortMap(networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {{HostPort: "8000"}}}))
	defer func() {
		if !removed {
			ctr.Remove(ctx, t, c, cid, client.ContainerRemoveOptions{Force: true})
		}
	}()

	saveCmd := []string{"iptables-save"}
	if strings.HasPrefix(d.FirewallBackendDriver(t), "nftables") {
		saveCmd = []string{"nft", "list ruleset"}
	}
	saveRules := icmd.Command(saveCmd[0], saveCmd[1:]...)
	resBeforeDel := icmd.RunCmd(saveRules)
	assert.NilError(t, resBeforeDel.Error)
	assert.Check(t, strings.Contains(resBeforeDel.Combined(), bridgeName),
		"With container: expected rules for %s in: %s", bridgeName, resBeforeDel.Combined())

	// Delete the container and its network.
	ctr.Remove(ctx, t, c, cid, client.ContainerRemoveOptions{Force: true})
	network.RemoveNoError(ctx, t, c, nw)
	removed = true

	// Check the network does not appear in iptables rules.
	resAfterDel := icmd.RunCmd(saveRules)
	assert.NilError(t, resAfterDel.Error)
	assert.Check(t, !strings.Contains(resAfterDel.Combined(), bridgeName),
		"After deletes: did not expect rules for %s in: %s", bridgeName, resAfterDel.Combined())

	// firewall-cmd --reload, and wait for the daemon to restore rules.
	networking.FirewalldReload(t, d)

	// Check that rules for the deleted container/network have not reappeared.
	resAfterReload := icmd.RunCmd(saveRules)
	assert.NilError(t, resAfterReload.Error)
	assert.Check(t, !strings.Contains(resAfterReload.Combined(), bridgeName),
		"After deletes: did not expect rules for %s in: %s", bridgeName, resAfterReload.Combined())
}

// TestFirewalldBridgeInOtherZone checks that a bridge network is restored when
// the daemon restarts after its bridge has been moved from firewalld's "docker"
// zone to another zone.
//
// Regression test for https://github.com/moby/moby/issues/53901
func TestFirewalldBridgeInOtherZone(t *testing.T) {
	skip.If(t, !networking.FirewalldRunning(), "firewalld is not running")
	skip.If(t, testEnv.IsRootless, "no firewalld in rootless netns")

	ctx := setupTest(t)

	testcases := []struct {
		name       string
		daemonArgs []string
		// Whether the daemon puts the bridge in firewalld's docker zone.
		inDockerZone bool
	}{
		{
			name:         "iptables",
			inDockerZone: true,
		},
		{
			name:       "no iptables",
			daemonArgs: []string{"--iptables=false", "--ip6tables=false"},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)
			d := daemon.New(t)
			d.StartWithBusybox(ctx, t, tc.daemonArgs...)
			defer d.Stop(t)
			c := d.NewClientT(t)

			const (
				bridgeName = "br-otherzone"
				zone       = "drop"
			)
			nw := network.CreateNoError(ctx, t, c, "testnet",
				network.WithOption(bridge.BridgeName, bridgeName))
			defer func() {
				// The daemon does not delete a bridge it failed to restore.
				if link, err := nlwrap.LinkByName(bridgeName); err == nil {
					assert.Check(t, netlink.LinkDel(link))
				}
			}()
			defer network.RemoveNoError(ctx, t, c, nw)

			res := icmd.RunCommand("firewall-cmd", "--get-zone-of-interface="+bridgeName)
			assert.Check(t, is.Equal(strings.TrimSpace(res.Stdout()) == "docker", tc.inDockerZone),
				"zone of %s: %s", bridgeName, res.Combined())

			res = icmd.RunCommand("firewall-cmd", "--zone="+zone, "--change-interface="+bridgeName)
			res.Assert(t, icmd.Success)
			defer icmd.RunCommand("firewall-cmd", "--zone="+zone, "--remove-interface="+bridgeName)

			d.Restart(t, tc.daemonArgs...)

			res = icmd.RunCommand("firewall-cmd", "--get-zone-of-interface="+bridgeName)
			assert.Check(t, is.Equal(strings.TrimSpace(res.Stdout()), zone))

			cid := ctr.Run(ctx, t, c, ctr.WithNetworkMode(nw))
			defer ctr.Remove(ctx, t, c, cid, client.ContainerRemoveOptions{Force: true})
		})
	}
}

// TestLegacyLink checks that a legacy link ("--link" in the default bridge network)
// sets up a hostname and opens ports when the daemon is running with icc=false.
func TestLegacyLink(t *testing.T) {
	ctx := setupTest(t)

	// Tidy up after the test by starting a new daemon, which will remove the icc=false
	// rules this test will create for docker0.
	defer func() {
		d := daemon.New(t)
		d.StartWithBusybox(ctx, t)
		defer d.Stop(t)
	}()

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t, "--icc=false")
	defer d.Stop(t)
	c := d.NewClientT(t)

	// Run an http server.
	const svrName = "svr"
	cid := ctr.Run(ctx, t, c,
		ctr.WithExposedPorts("80/tcp"),
		ctr.WithName(svrName),
		ctr.WithCmd("httpd", "-f"),
	)

	defer ctr.Remove(ctx, t, c, cid, client.ContainerRemoveOptions{Force: true})
	insp := ctr.Inspect(ctx, t, c, cid)
	svrAddr := insp.NetworkSettings.Networks["bridge"].IPAddress

	const svrAlias = "thealias"
	testcases := []struct {
		name   string
		host   string
		links  []string
		expect string
	}{
		{
			name:   "no link",
			host:   svrAddr.String(),
			expect: "download timed out",
		},
		{
			name:   "access by address",
			links:  []string{svrName},
			host:   svrAddr.String(),
			expect: "404 Not Found", // Got a response, but the server has nothing to serve.
		},
		{
			name:   "access by name",
			links:  []string{svrName},
			host:   svrName,
			expect: "404 Not Found", // Got a response, but the server has nothing to serve.
		},
		{
			name:   "access by alias",
			links:  []string{svrName + ":" + svrAlias},
			host:   svrAlias,
			expect: "404 Not Found", // Got a response, but the server has nothing to serve.
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)
			res := ctr.RunAttach(ctx, t, c,
				ctr.WithLinks(tc.links...),
				ctr.WithCmd("wget", "-T3", "http://"+tc.host),
			)
			assert.Check(t, is.Contains(res.Stderr.String(), tc.expect))
		})
	}
}

// TestRemoveLegacyLink checks that a legacy link can be deleted while the
// linked containers are running.
//
// Replacement for DockerDaemonSuite/TestDaemonLinksIpTablesRulesWhenLinkAndUnlink
func TestRemoveLegacyLink(t *testing.T) {
	ctx := setupTest(t)

	// Tidy up after the test by starting a new daemon, which will remove the icc=false
	// rules this test will create for docker0.
	defer func() {
		d := daemon.New(t)
		d.StartWithBusybox(ctx, t)
		defer d.Stop(t)
	}()

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t, "--icc=false")
	defer d.Stop(t)
	c := d.NewClientT(t)

	// Run an http server.
	const svrName = "svr"
	svrId := ctr.Run(ctx, t, c,
		ctr.WithExposedPorts("80/tcp"),
		ctr.WithName(svrName),
		ctr.WithCmd("httpd", "-f"),
	)
	defer ctr.Remove(ctx, t, c, svrId, client.ContainerRemoveOptions{Force: true})

	// Run a container linked to the http server.
	const svrAlias = "thealias"
	const clientName = "client"
	clientId := ctr.Run(ctx, t, c,
		ctr.WithName(clientName),
		ctr.WithLinks(svrName+":"+svrAlias),
	)
	defer ctr.Remove(ctx, t, c, clientId, client.ContainerRemoveOptions{Force: true})

	// Check the link works.
	res := ctr.ExecT(ctx, t, c, clientId, []string{"wget", "-T3", "http://" + svrName})
	assert.Check(t, is.Contains(res.Stderr(), "404 Not Found"))

	// Remove the link ("docker rm --link client/thealias").
	_, err := c.ContainerRemove(ctx, clientName+"/"+svrAlias, client.ContainerRemoveOptions{RemoveLinks: true})
	assert.Check(t, err)

	// Check both containers are still running.
	inspSvr := ctr.Inspect(ctx, t, c, svrId)
	assert.Check(t, is.Equal(inspSvr.State.Running, true))
	inspClient := ctr.Inspect(ctx, t, c, clientId)
	assert.Check(t, is.Equal(inspClient.State.Running, true))

	// Check the link's alias doesn't work.
	res = ctr.ExecT(ctx, t, c, clientId, []string{"wget", "-T3", "http://" + svrName})
	assert.Check(t, is.Contains(res.Stderr(), "bad address"))

	// Check the icc=false rules now block access by address.
	svrAddr := inspSvr.NetworkSettings.Networks["bridge"].IPAddress
	res = ctr.ExecT(ctx, t, c, clientId, []string{"wget", "-T3", "http://" + svrAddr.String()})
	assert.Check(t, is.Contains(res.Stderr(), "download timed out"))
}

// TestPortMappingRestore check that port mappings are restored when a container
// is restarted after a daemon restart.
//
// Replacement for integration-cli test DockerDaemonSuite/TestDaemonIptablesCreate
func TestPortMappingRestore(t *testing.T) {
	skip.If(t, testEnv.IsRootless(), "fails before and after restart")

	ctx := setupTest(t)
	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)
	c := d.NewClientT(t)

	const svrName = "svr"
	cid := ctr.Run(ctx, t, c,
		ctr.WithExposedPorts("80/tcp"),
		// TODO(robmry): this test supplies an empty list of PortBindings.
		// https://github.com/moby/moby/issues/51727 will break it.
		ctr.WithPortMap(networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {}}),
		ctr.WithName(svrName),
		ctr.WithRestartPolicy(containertypes.RestartPolicyUnlessStopped),
		ctr.WithCmd("httpd", "-f"),
	)
	defer func() { ctr.Remove(ctx, t, c, cid, client.ContainerRemoveOptions{Force: true}) }()

	check := func() {
		t.Helper()
		insp := ctr.Inspect(ctx, t, c, cid)
		assert.Check(t, is.Equal(insp.State.Running, true))
		if assert.Check(t, is.Contains(insp.NetworkSettings.Ports, networktypes.MustParsePort("80/tcp"))) &&
			assert.Check(t, is.Len(insp.NetworkSettings.Ports[networktypes.MustParsePort("80/tcp")], 2)) {
			hostPort := insp.NetworkSettings.Ports[networktypes.MustParsePort("80/tcp")][0].HostPort
			res := ctr.RunAttach(ctx, t, c,
				ctr.WithExtraHost("thehost:host-gateway"),
				ctr.WithCmd("wget", "-T3", "http://"+net.JoinHostPort("thehost", hostPort)),
			)
			// 404 means the http request worked, but the http server had nothing to serve.
			assert.Check(t, is.Contains(res.Stderr.String(), "404 Not Found"))
		}
	}

	check()
	d.Restart(t)
	check()
}

// TestNoSuchExternalBridge checks that the daemon won't start if it's given a "--bridge"
// that doesn't exist.
//
// Replacement for part of DockerDaemonSuite/TestDaemonBridgeExternal
func TestNoSuchExternalBridge(t *testing.T) {
	_ = setupTest(t)
	d := daemon.New(t)
	defer d.Stop(t)
	err := d.StartWithError("--bridge", "nosuchbridge")
	assert.Check(t, err != nil, "Expected daemon startup to fail")
}

// TestFirewallBackendSwitch checks that when started with an nftables or iptables
// backend after running with the other backend, old rules are removed.
func TestFirewallBackendSwitch(t *testing.T) {
	skip.If(t, testEnv.IsRootless, "rootless has its own netns")
	skip.If(t, networking.FirewalldRunning(), "can't use firewalld in host netns to add rules in L3Segment")
	ctx := setupTest(t)

	// Run in a clean netns.
	addr4 := netip.MustParseAddr("192.168.125.1")
	addr6 := netip.MustParseAddr("fd76:c828:41f9::1")
	l3 := networking.NewL3Segment(t, "test-fwbeswitch",
		netip.PrefixFrom(addr4, 24),
		netip.PrefixFrom(addr6, 64),
	)
	defer l3.Destroy(t)

	addr4, addr6 = addr4.Next(), addr6.Next()
	const hostname = "fwbeswitch"
	l3.AddHost(t, hostname, hostname+"-netns", "eth0",
		netip.PrefixFrom(addr4, 24),
		netip.PrefixFrom(addr6, 64),
	)
	host := l3.Hosts[hostname]

	// Run without OTel because there's no routing from this netns for it - which
	// means the daemon doesn't shut down cleanly, causing the test to fail.
	d := daemon.New(t, daemon.WithEnvVars("OTEL_EXPORTER_OTLP_ENDPOINT="))

	networkCreated := false
	runDaemon := func(backend string) {
		host.Do(t, func() {
			d.StartWithBusybox(ctx, t, "--firewall-backend="+backend)
			defer d.Stop(t)

			// Create a network (and its firewall rules) first time through.
			// On restarts, the daemon should find it and clean up the rules if the
			// firewall backend changed.
			// No need to clean up, the netns will be deleted.
			// (Ideally, would start a container - but would need to kill the daemon
			// to leave its firewall rules in place for the next daemon to clean up,
			// and that risks leaving a container process running on the test host
			// when things go wrong.)
			if !networkCreated {
				c := d.NewClientT(t)
				defer c.Close()
				_ = network.CreateNoError(ctx, t, c, "testnet",
					network.WithIPv6(),
					network.WithIPAM("192.0.2.0/24", "192.0.2.1"),
					network.WithIPAM("2001:db8::/64", "2001:db8::1"),
				)
				networkCreated = true
			}
		})
	}

	summariseIptables := func() (dockerChains []string, numRules int, dump string) {
		host.Do(t, func() {
			dump = icmd.RunCommand("iptables-save").Combined()
			dump += icmd.RunCommand("ip6tables-save").Combined()
		})

		// TODO: (When Go 1.24 is min version) Replace with `strings.Lines(dump)`.
		for line := range strings.SplitSeq(dump, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			// Ignore DOCKER-USER and jumps to it, it's not cleaned.
			if strings.HasPrefix(line, ":DOCKER") && !strings.HasPrefix(line, ":DOCKER-USER") {
				dockerChains = append(dockerChains, line[1:])
			} else if strings.HasPrefix(line, "-A") && !strings.Contains(line, "FORWARD -j DOCKER-USER") {
				numRules++
			}
		}
		return dockerChains, numRules, dump
	}

	nftablesTablesExist := func() bool {
		var exist bool
		host.Do(t, func() {
			res4 := icmd.RunCommand("nft", "list table ip docker-bridges")
			res6 := icmd.RunCommand("nft", "list table ip6 docker-bridges")
			exist = res4.ExitCode == 0 || res6.ExitCode == 0
		})
		return exist
	}

	// Create iptables rules.
	runDaemon("iptables")
	dockerChains, numRules, dump := summariseIptables()
	t.Logf("iptables created, %d rules, %d docker chains, dump:\n%s", numRules, len(dockerChains), dump)
	assert.Check(t, numRules > 0, "Expected iptables to have at least one rule")
	assert.Check(t, len(dockerChains) > 0, "Expected iptables to have at least one docker chain")
	assert.Check(t, !nftablesTablesExist(), "nftables tables exist after running with iptables")

	// Use nftables, expect the iptables rules to be deleted.
	runDaemon("nftables")
	dockerChains, numRules, dump = summariseIptables()
	t.Logf("iptables cleaned, %d rules, %d docker chains, dump:\n%s", numRules, len(dockerChains), dump)
	assert.Check(t, numRules == 0, "Unexpected iptables rules after starting with nftables")
	assert.Check(t, len(dockerChains) == 0, "Unexpected iptables chains after starting with nftables")
	assert.Check(t, nftablesTablesExist(), "nftables tables do not exist after running with nftables")

	// Use iptables, expect the nftables rules to be deleted.
	runDaemon("iptables")
	dockerChains, numRules, dump = summariseIptables()
	t.Logf("iptables created, %d rules, %d docker chains, dump:\n%s", numRules, len(dockerChains), dump)
	assert.Check(t, numRules > 0, "Expected iptables to have at least one rule")
	assert.Check(t, len(dockerChains) > 0, "Expected iptables to have at least one docker chain")
	assert.Check(t, !nftablesTablesExist(), "nftables tables exist after running with iptables")
}

func TestEmptyPortBindingsBC(t *testing.T) {
	ctx := setupTest(t)
	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	createInspect := func(t *testing.T, version string, pbs []networktypes.PortBinding) (networktypes.PortMap, []string) {
		apiClient := d.NewClientT(t, client.WithAPIVersion(version))
		defer apiClient.Close()

		// Skip this subtest if the daemon doesn't support the client version.
		// TODO(aker): drop this once the Engine supports API version >= 1.53
		_, err := apiClient.ServerVersion(ctx, client.ServerVersionOptions{})
		if err != nil && strings.Contains(err.Error(), fmt.Sprintf("client version %s is too new", version)) {
			t.Skipf("requires API %s", version)
		}
		assert.NilError(t, err)

		// Create a container with an empty list of port bindings for container port 80/tcp.
		config := ctr.NewTestConfig(ctr.WithCmd("top"),
			ctr.WithExposedPorts("80/tcp"),
			ctr.WithPortMap(networktypes.PortMap{networktypes.MustParsePort("80/tcp"): pbs}))
		c, err := apiClient.ContainerCreate(ctx, client.ContainerCreateOptions{
			Config:           config.Config,
			HostConfig:       config.HostConfig,
			NetworkingConfig: config.NetworkingConfig,
			Platform:         config.Platform,
			Name:             config.Name,
		})
		assert.NilError(t, err)
		defer apiClient.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true})

		// Inspect the container and return its port bindings, along with
		// warnings returns on container create.
		inspect, err := apiClient.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{})
		assert.NilError(t, err)
		return inspect.Container.HostConfig.PortBindings, c.Warnings
	}

	t.Run("backfilling on old client version", func(t *testing.T) {
		expMappings := networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {
			{}, // An empty PortBinding is backfilled
		}}
		expWarnings := make([]string, 0)

		mappings, warnings := createInspect(t, "1.51", []networktypes.PortBinding{})
		assert.DeepEqual(t, expMappings, mappings, cmpopts.EquateComparable(netip.Addr{}))
		assert.DeepEqual(t, expWarnings, warnings, cmpopts.EquateComparable(netip.Addr{}))
	})

	t.Run("backfilling on API 1.52, with a warning", func(t *testing.T) {
		expMappings := networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {
			{}, // An empty PortBinding is backfilled
		}}
		expWarnings := []string{
			"Following container port(s) have an empty list of port-bindings: 80/tcp. Such bindings will be discarded in a future version.",
		}

		mappings, warnings := createInspect(t, "1.52", []networktypes.PortBinding{})
		assert.DeepEqual(t, expMappings, mappings, cmpopts.EquateComparable(netip.Addr{}))
		assert.DeepEqual(t, expWarnings, warnings, cmpopts.EquateComparable(netip.Addr{}))
	})

	t.Run("no backfilling on API 1.53", func(t *testing.T) {
		t.Skip("Backfilling was not removed in 1.53. See https://github.com/moby/moby/issues/51727")
		expMappings := networktypes.PortMap{}
		expWarnings := make([]string, 0)

		mappings, warnings := createInspect(t, "1.53", []networktypes.PortBinding{})
		assert.DeepEqual(t, expMappings, mappings, cmpopts.EquateComparable(netip.Addr{}))
		assert.DeepEqual(t, expWarnings, warnings, cmpopts.EquateComparable(netip.Addr{}))
	})

	for _, apiVersion := range []string{"1.51", "1.52", "1.53"} {
		t.Run("no backfilling on API "+apiVersion+" with non-empty bindings", func(t *testing.T) {
			expMappings := networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {
				{HostPort: "8080"},
			}}
			expWarnings := make([]string, 0)

			mappings, warnings := createInspect(t, apiVersion, []networktypes.PortBinding{{HostPort: "8080"}})
			assert.DeepEqual(t, expMappings, mappings, cmpopts.EquateComparable(netip.Addr{}))
			assert.DeepEqual(t, expWarnings, warnings, cmpopts.EquateComparable(netip.Addr{}))
		})
	}
}

// TestPortBindingBackfillingForOlderContainers verify that the daemon
// correctly backfills empty port bindings for containers created with prior
// versions of the Engine.
func TestPortBindingBackfillingForOlderContainers(t *testing.T) {
	ctx := setupTest(t)
	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	// We don't really care which version of the API is used here as we're
	// going to tamper with the on-disk state of the container. Even if the
	// daemon backfills the empty port bindings on ContainerCreate (e.g.,
	// API < 1.53), the tampering will reinitialize the PortBindings slice to
	// an empty list.
	c := d.NewClientT(t)

	cid := ctr.Create(ctx, t, c,
		ctr.WithExposedPorts("80/tcp"),
		ctr.WithPortMap(networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {}}))
	defer c.ContainerRemove(ctx, cid, client.ContainerRemoveOptions{Force: true})

	// Stop the daemon to safely tamper with the on-disk state.
	d.Stop(t)

	d.TamperWithContainerConfig(t, cid, func(container *container.Container) {
		// Simulate a container created with an older version of the Engine
		// by setting an empty list of port bindings.
		container.HostConfig.PortBindings = networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {}}
	})

	// Restart the daemon — it should backfill the empty port binding slice.
	d.StartWithBusybox(ctx, t)

	inspect := ctr.Inspect(ctx, t, c, cid)

	expMappings := networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {
		{}, // An empty PortBinding is backfilled
	}}
	assert.DeepEqual(t, expMappings, inspect.HostConfig.PortBindings, cmpopts.EquateComparable(netip.Addr{}))
}

func TestBridgeIPAMStatus(t *testing.T) {
	ctx := testutil.StartSpan(baseContext, t)
	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	c := d.NewClientT(t, client.WithAPIVersion("1.52"))

	checkSubnets := func(
		netName string, want networktypes.SubnetStatuses,
	) bool {
		t.Helper()
		res, err := c.NetworkInspect(ctx, netName, client.NetworkInspectOptions{})
		if assert.Check(t, err) && assert.Check(t, res.Network.Status != nil) {
			return assert.Check(t, is.DeepEqual(want, res.Network.Status.IPAM.Subnets))
		}
		return false
	}

	t.Run("DualStack", func(t *testing.T) {
		const (
			netName = "testipambridge"

			ipv4gw             = "192.168.0.1"
			ipv4Range          = "192.168.0.64/31"
			prefIPv4OutOfRange = "192.168.0.129"
			auxIPv4FromRange   = "192.168.0.65"
			auxIPv4OutOfRange  = "192.168.0.128"

			ipv6gw             = "2001:db8:abcd::1"
			ipv6Range          = "2001:db8:abcd::/120"
			prefIPv6OutOfRange = "2001:db8:abcd::9000"
			auxIPv6FromRange   = "2001:db8:abcd::2a"
			auxIPv6OutOfRange  = "2001:db8:abcd::8000"
		)
		var (
			cidrv4 = netip.MustParsePrefix("192.168.0.0/24")
			cidrv6 = netip.MustParsePrefix("2001:db8:abcd::/64")
		)

		network.CreateNoError(ctx, t, c, netName,
			network.WithIPv4(true),
			network.WithIPAMConfig(networktypes.IPAMConfig{
				Subnet:  cidrv4,
				IPRange: netip.MustParsePrefix(ipv4Range),
				Gateway: netip.MustParseAddr(ipv4gw),
				AuxAddress: map[string]netip.Addr{
					"reserved":   netip.MustParseAddr(auxIPv4FromRange),
					"reserved_1": netip.MustParseAddr(auxIPv4OutOfRange),
				},
			}),
			network.WithIPv6(),
			network.WithIPAMConfig(networktypes.IPAMConfig{
				Subnet:  cidrv6,
				IPRange: netip.MustParsePrefix(ipv6Range),
				Gateway: netip.MustParseAddr(ipv6gw),
				AuxAddress: map[string]netip.Addr{
					"reserved1": netip.MustParseAddr(auxIPv6FromRange),
					"reserved2": netip.MustParseAddr(auxIPv6OutOfRange),
				},
			}),
		)
		defer c.NetworkRemove(ctx, netName, client.NetworkRemoveOptions{})

		checkSubnets(netName, map[netip.Prefix]networktypes.SubnetStatus{
			cidrv4: {
				// 1 subnet + 1 gateway + 1 broadcast + 2 aux addresses
				IPsInUse: 5,
				// IPv4 /31 IPRange (2 addresses) - aux in-range
				DynamicIPsAvailable: 1,
			},
			cidrv6: {
				IPsInUse:            4,   // 1 gateway + 1 anycast + 2 aux addresses
				DynamicIPsAvailable: 253, // IPv6 /120 IPRange (256 addresses) - 1 router-anycast - 1 gateway - 1 aux in-range
			},
		})

		func() {
			// From IPRange pool: both counters should be changed by 1
			id := ctr.Run(ctx, t, c, ctr.WithNetworkMode(netName))
			defer c.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})

			checkSubnets(netName, map[netip.Prefix]networktypes.SubnetStatus{
				cidrv4: {
					IPsInUse:            6,
					DynamicIPsAvailable: 0,
				},
				cidrv6: {
					IPsInUse:            5,
					DynamicIPsAvailable: 252,
				},
			})

			// Out of IPRange pools: subnet counter should be changed by 1
			id = ctr.Run(ctx, t, c,
				ctr.WithNetworkMode(netName),
				ctr.WithIPv4(netName, prefIPv4OutOfRange),
				ctr.WithIPv6(netName, prefIPv6OutOfRange),
			)
			defer c.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})

			checkSubnets(netName, map[netip.Prefix]networktypes.SubnetStatus{
				cidrv4: {
					IPsInUse:            7,
					DynamicIPsAvailable: 0, // unchanged
				},
				cidrv6: {
					IPsInUse:            6,
					DynamicIPsAvailable: 252, // unchanged
				},
			})
		}()

		// Counters should decrease after container removal
		checkSubnets(netName, map[netip.Prefix]networktypes.SubnetStatus{
			cidrv4: {
				IPsInUse:            5,
				DynamicIPsAvailable: 1,
			},
			cidrv6: {
				IPsInUse:            4,
				DynamicIPsAvailable: 253,
			},
		})

		oldc := d.NewClientT(t, client.WithAPIVersion("1.51"))
		res, err := oldc.NetworkInspect(ctx, netName, client.NetworkInspectOptions{})
		if assert.Check(t, err) {
			assert.Check(t, res.Network.Status == nil, "expected nil Status with API version 1.51")
		}
	})

	t.Run("IPv6", func(t *testing.T) {
		const netName = "testipambridgev6"
		cidr := netip.MustParsePrefix("2001:db8:abcd::/56")
		network.CreateNoError(ctx, t, c, netName,
			network.WithIPv4(false),
			network.WithIPv6(),
			network.WithIPAMConfig(networktypes.IPAMConfig{
				Subnet: cidr,
			}),
		)
		defer c.NetworkRemove(ctx, netName, client.NetworkRemoveOptions{})

		checkSubnets(netName, map[netip.Prefix]networktypes.SubnetStatus{
			cidr: {
				IPsInUse:            2,
				DynamicIPsAvailable: math.MaxUint64,
			},
		})
	})
}

// TestJoinError checks that if network connection fails late in the process, it's
// rolled back properly - the failed connection should not show up in container
// or network inspect, and the container should not gain a network interface.
func TestJoinError(t *testing.T) {
	skip.If(t, testEnv.IsUserNamespace, "privileged mode is incompatible with user namespaces")

	ctx := setupTest(t)
	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)
	c := d.NewClientT(t)

	const intNet = "intnet"
	const gwAddr = "192.168.123.1"
	network.CreateNoError(ctx, t, c, intNet,
		network.WithInternal(),
		network.WithIPAM("192.168.123.0/24", gwAddr),
	)
	defer network.RemoveNoError(ctx, t, c, intNet)

	const extNet = "extnet"
	network.CreateNoError(ctx, t, c, extNet)
	defer network.RemoveNoError(ctx, t, c, extNet)

	cid := ctr.Run(ctx, t, c,
		ctr.WithNetworkMode(intNet),
		ctr.WithPrivileged(true),
	)
	defer c.ContainerRemove(ctx, cid, client.ContainerRemoveOptions{Force: true})

	// Add a default route to the container, so that connecting extNet will fail to
	// set up its own default route.
	res := ctr.ExecT(ctx, t, c, cid, []string{"ip", "route", "add", "default", "via", gwAddr})
	assert.Equal(t, res.ExitCode, 0)

	// Expect an error when connecting extNet.
	_, err := c.NetworkConnect(ctx, extNet, client.NetworkConnectOptions{
		Container: cid,
	})
	assert.Check(t, is.ErrorContains(err, "failed to set gateway: file exists"))

	// Only intNet should show up in container inspect.
	ctrInsp := ctr.Inspect(ctx, t, c, cid)
	assert.Check(t, is.Len(ctrInsp.NetworkSettings.Networks, 1))
	assert.Check(t, is.Contains(ctrInsp.NetworkSettings.Networks, intNet))

	// extNet should not report any attached containers
	extNetInsp, err := c.NetworkInspect(ctx, extNet, client.NetworkInspectOptions{})
	assert.Check(t, err)
	assert.Check(t, is.Len(extNetInsp.Network.Containers, 0))

	// The container should have an eth0, but no eth1.
	res = ctr.ExecT(ctx, t, c, cid, []string{"ip", "link", "show", "eth0"})
	assert.Check(t, is.Equal(res.ExitCode, 0), "container should have an eth0")
	res = ctr.ExecT(ctx, t, c, cid, []string{"ip", "link", "show", "eth1"})
	assert.Check(t, is.Contains(res.Stderr(), "can't find device"), "container should not have an eth1")

	// Remove the dodgy route.
	res = ctr.ExecT(ctx, t, c, cid, []string{"ip", "route", "del", "default", "via", gwAddr})
	assert.Equal(t, res.ExitCode, 0)

	// Check network connect now succeeds.
	_, err = c.NetworkConnect(ctx, extNet, client.NetworkConnectOptions{
		Container: cid,
	})
	assert.Check(t, err)
	ctrInsp = ctr.Inspect(ctx, t, c, cid)
	assert.Check(t, is.Len(ctrInsp.NetworkSettings.Networks, 2))
	assert.Check(t, is.Contains(ctrInsp.NetworkSettings.Networks, intNet))
	assert.Check(t, is.Contains(ctrInsp.NetworkSettings.Networks, extNet))
	extNetInsp, err = c.NetworkInspect(ctx, extNet, client.NetworkInspectOptions{})
	assert.Check(t, err)
	assert.Check(t, is.Len(extNetInsp.Network.Containers, 1))
	res = ctr.ExecT(ctx, t, c, cid, []string{"ip", "link", "show", "eth0"})
	assert.Check(t, is.Equal(res.ExitCode, 0), "container should have an eth0")
	res = ctr.ExecT(ctx, t, c, cid, []string{"ip", "link", "show", "eth1"})
	assert.Check(t, is.Equal(res.ExitCode, 0), "container should have an eth1")
}

// TestEndpointCreateErrorRollback checks that if connecting a running container
// to a network fails while creating the endpoint (here, because the requested
// static IPv4 address is already in use), the failed network is not left in the
// container's network settings. Otherwise, the container could not be restarted.
func TestEndpointCreateErrorRollback(t *testing.T) {
	ctx := setupTest(t)
	c := testEnv.APIClient()

	const netName = "testnetrollback"
	const gwAddr = "192.168.126.1"
	const staticIP = "192.168.126.10"
	network.CreateNoError(ctx, t, c, netName,
		network.WithIPAM("192.168.126.0/24", gwAddr),
	)
	defer network.RemoveNoError(ctx, t, c, netName)

	// Hold the static address, so that connecting another container to it fails.
	holderID := ctr.Run(ctx, t, c,
		ctr.WithNetworkMode(netName),
		ctr.WithIPv4(netName, staticIP),
	)
	defer c.ContainerRemove(ctx, holderID, client.ContainerRemoveOptions{Force: true})

	cid := ctr.Run(ctx, t, c)
	defer c.ContainerRemove(ctx, cid, client.ContainerRemoveOptions{Force: true})

	_, err := c.NetworkConnect(ctx, netName, client.NetworkConnectOptions{
		Container: cid,
		EndpointConfig: &networktypes.EndpointSettings{
			IPAMConfig: &networktypes.EndpointIPAMConfig{
				IPv4Address: netip.MustParseAddr(staticIP),
			},
		},
	})
	assert.Check(t, is.ErrorContains(err, "Address already in use"))

	// Only the default bridge network should show up in container inspect.
	ctrInsp := ctr.Inspect(ctx, t, c, cid)
	assert.Check(t, is.Len(ctrInsp.NetworkSettings.Networks, 1))
	assert.Check(t, is.Contains(ctrInsp.NetworkSettings.Networks, "bridge"))

	// Restarting would fail if the failed network config had been kept.
	_, err = c.ContainerRestart(ctx, cid, client.ContainerRestartOptions{})
	assert.NilError(t, err)
	ctrInsp = ctr.Inspect(ctx, t, c, cid)
	assert.Check(t, is.Equal(ctrInsp.State.Running, true))
	assert.Check(t, is.Len(ctrInsp.NetworkSettings.Networks, 1))
}

func TestPreferredSubnetRestore(t *testing.T) {
	skip.If(t, testEnv.IsRootless(), "fails before and after restart")

	ctx := setupTest(t)
	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)
	c := d.NewClientT(t)

	const v4netName = "testnetv4restore"
	network.CreateNoError(ctx, t, c, v4netName,
		network.WithIPv4(true),
		network.WithIPAMConfig(networktypes.IPAMConfig{
			Subnet: netip.MustParsePrefix("0.0.0.0/24"),
		}),
	)

	defer func() { network.RemoveNoError(ctx, t, c, v4netName) }()

	const v6netName = "testnetv6restore"
	network.CreateNoError(ctx, t, c, v6netName,
		network.WithIPv4(false),
		network.WithIPv6(),
		network.WithIPAMConfig(networktypes.IPAMConfig{
			Subnet: netip.MustParsePrefix("::/120"),
		}),
	)

	defer func() { network.RemoveNoError(ctx, t, c, v6netName) }()

	const dualStackNetName = "testnetdualrestore"
	network.CreateNoError(ctx, t, c, dualStackNetName,
		network.WithIPv4(true),
		network.WithIPv6(),
		network.WithIPAMConfig(networktypes.IPAMConfig{
			Subnet: netip.MustParsePrefix("0.0.0.0/24"),
		}, networktypes.IPAMConfig{
			Subnet: netip.MustParsePrefix("::/120"),
		}),
	)

	defer func() { network.RemoveNoError(ctx, t, c, dualStackNetName) }()

	inspOpts := client.NetworkInspectOptions{}

	v4Insp := network.InspectNoError(ctx, t, c, v4netName, inspOpts)
	assert.Check(t, is.Len(v4Insp.Network.IPAM.Config, 1))
	v4allocCidr := v4Insp.Network.IPAM.Config[0].Subnet
	assert.Check(t, is.Equal(v4allocCidr.Addr().IsUnspecified(), false), "expected specific subnet")

	v6Insp := network.InspectNoError(ctx, t, c, v6netName, inspOpts)
	assert.Check(t, is.Len(v6Insp.Network.IPAM.Config, 1))
	v6allocCidr := v6Insp.Network.IPAM.Config[0].Subnet
	assert.Check(t, is.Equal(v6allocCidr.Addr().IsUnspecified(), false), "expected specific subnet")

	dualStackInsp := network.InspectNoError(ctx, t, c, dualStackNetName, inspOpts)
	assert.Check(t, is.Len(dualStackInsp.Network.IPAM.Config, 2))
	var dualv4, dualv6 netip.Prefix
	if dualStackInsp.Network.IPAM.Config[0].Subnet.Addr().Is4() {
		dualv4 = dualStackInsp.Network.IPAM.Config[0].Subnet
		dualv6 = dualStackInsp.Network.IPAM.Config[1].Subnet
	} else {
		dualv4 = dualStackInsp.Network.IPAM.Config[1].Subnet
		dualv6 = dualStackInsp.Network.IPAM.Config[0].Subnet
	}
	assert.Check(t, is.Equal(dualv4.Addr().IsUnspecified(), false), "expected specific v4 subnet")
	assert.Check(t, is.Equal(dualv6.Addr().IsUnspecified(), false), "expected specific v6 subnet")

	d.Restart(t)

	v4Insp = network.InspectNoError(ctx, t, c, v4netName, inspOpts)
	assert.Check(t, is.Len(v4Insp.Network.IPAM.Config, 1))
	assert.Check(t, is.Equal(v4Insp.Network.IPAM.Config[0].Subnet, v4allocCidr))

	v6Insp = network.InspectNoError(ctx, t, c, v6netName, inspOpts)
	assert.Check(t, is.Len(v6Insp.Network.IPAM.Config, 1))
	assert.Check(t, is.Equal(v6Insp.Network.IPAM.Config[0].Subnet, v6allocCidr))

	dualStackInsp = network.InspectNoError(ctx, t, c, dualStackNetName, inspOpts)
	assert.Check(t, is.Len(dualStackInsp.Network.IPAM.Config, 2))
	var dualv4after, dualv6after netip.Prefix
	if dualStackInsp.Network.IPAM.Config[0].Subnet.Addr().Is4() {
		dualv4after = dualStackInsp.Network.IPAM.Config[0].Subnet
		dualv6after = dualStackInsp.Network.IPAM.Config[1].Subnet
	} else {
		dualv4after = dualStackInsp.Network.IPAM.Config[1].Subnet
		dualv6after = dualStackInsp.Network.IPAM.Config[0].Subnet
	}
	assert.Check(t, is.Equal(dualv4after, dualv4), "expected same v4 subnet after restart")
	assert.Check(t, is.Equal(dualv6after, dualv6), "expected same v6 subnet after restart")
}
