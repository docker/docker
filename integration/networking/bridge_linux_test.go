package networking

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"math"
	"net"
	"net/netip"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp/cmpopts"
	networktypes "github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge"
	"github.com/moby/moby/v2/daemon/libnetwork/iptables"
	"github.com/moby/moby/v2/daemon/libnetwork/netlabel"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/integration/internal/network"
	"github.com/moby/moby/v2/integration/internal/testutils/networking"
	n "github.com/moby/moby/v2/integration/network"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/icmd"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

// TestBridgeICC tries to ping container ctr1 from container ctr2 using its hostname. Thus, this test checks:
// 1. DNS resolution ; 2. ARP/NDP ; 3. whether containers can communicate with each other ; 4. kernel-assigned SLAAC
// addresses.
func TestBridgeICC(t *testing.T) {
	ctx := setupTest(t)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	c := d.NewClientT(t)
	defer c.Close()

	testcases := []struct {
		name           string
		bridgeOpts     []func(*client.NetworkCreateOptions)
		ctr1MacAddress string
		isIPv6         bool
		isLinkLocal    bool
		pingHost       string
	}{
		{
			name:       "IPv4 non-internal network",
			bridgeOpts: []func(*client.NetworkCreateOptions){},
		},
		{
			name: "IPv4 internal network",
			bridgeOpts: []func(*client.NetworkCreateOptions){
				network.WithInternal(),
			},
		},
		{
			name: "IPv6 ULA on non-internal network",
			bridgeOpts: []func(*client.NetworkCreateOptions){
				network.WithIPv6(),
				network.WithIPAM("fdf1:a844:380c:b200::/64", "fdf1:a844:380c:b200::1"),
			},
			isIPv6: true,
		},
		{
			name: "IPv6 ULA on internal network",
			bridgeOpts: []func(*client.NetworkCreateOptions){
				network.WithIPv6(),
				network.WithInternal(),
				network.WithIPAM("fdf1:a844:380c:b247::/64", "fdf1:a844:380c:b247::1"),
			},
			isIPv6: true,
		},
		{
			name: "IPv6 link-local address on non-internal network",
			bridgeOpts: []func(*client.NetworkCreateOptions){
				network.WithIPv6(),
				// There's no real way to specify an IPv6 network is only used with SLAAC link-local IPv6 addresses.
				// What we can do instead, is to tell the IPAM driver to assign addresses from the link-local prefix.
				// Each container will have two link-local addresses: 1. a SLAAC address assigned by the kernel ;
				// 2. the one dynamically assigned by the IPAM driver.
				network.WithIPAM("fe80::/64", "fe80::1"),
			},
			isLinkLocal: true,
			isIPv6:      true,
		},
		{
			name: "IPv6 link-local address on internal network",
			bridgeOpts: []func(*client.NetworkCreateOptions){
				network.WithIPv6(),
				network.WithInternal(),
				// See the note above about link-local addresses.
				network.WithIPAM("fe80::/64", "fe80::1"),
			},
			isLinkLocal: true,
			isIPv6:      true,
		},
		{
			// As for 'LL non-internal', ping the container by name instead of by address
			// - the busybox test containers only have one interface with a link local
			// address, so the zone index is not required:
			//   RFC-4007, section 6: "[...] for nodes with only a single non-loopback
			//   interface (e.g., a single Ethernet interface), the common case, link-local
			//   addresses need not be qualified with a zone index."
			// So, for this common case, LL addresses should be included in DNS config.
			name: "IPv6 link-local address on non-internal network ping by name",
			bridgeOpts: []func(*client.NetworkCreateOptions){
				network.WithIPv6(),
				network.WithIPAM("fe80::/64", "fe80::1"),
			},
			isIPv6: true,
		},
		{
			name: "IPv6 nonstandard link-local subnet on non-internal network ping by name",
			// No interfaces apart from the one on the bridge network with this non-default
			// subnet will be on this link local subnet (it's not currently possible to
			// configure two networks with the same LL subnet, although perhaps it should
			// be). So, again, no zone index is required and the LL address should be
			// included in DNS config.
			bridgeOpts: []func(*client.NetworkCreateOptions){
				network.WithIPv6(),
				network.WithIPAM("fe80:1234::/64", "fe80:1234::1"),
			},
			isIPv6: true,
		},
		{
			name: "IPv6 non-internal network with SLAAC LL address",
			bridgeOpts: []func(*client.NetworkCreateOptions){
				network.WithIPv6(),
				network.WithIPAM("fdf1:a844:380c:b247::/64", "fdf1:a844:380c:b247::1"),
			},
			// Link-local address is derived from the MAC address, so we need to
			// specify one here to hardcode the SLAAC LL address below.
			ctr1MacAddress: "02:42:ac:11:00:02",
			pingHost:       "fe80::42:acff:fe11:2%eth0",
			isIPv6:         true,
		},
		{
			name: "IPv6 internal network with SLAAC LL address",
			bridgeOpts: []func(*client.NetworkCreateOptions){
				network.WithIPv6(),
				network.WithIPAM("fdf1:a844:380c:b247::/64", "fdf1:a844:380c:b247::1"),
			},
			// Link-local address is derived from the MAC address, so we need to
			// specify one here to hardcode the SLAAC LL address below.
			ctr1MacAddress: "02:42:ac:11:00:02",
			pingHost:       "fe80::42:acff:fe11:2%eth0",
			isIPv6:         true,
		},
	}

	for tcID, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)

			bridgeName := fmt.Sprintf("testnet-icc-%d", tcID)
			network.CreateNoError(ctx, t, c, bridgeName, append(tc.bridgeOpts,
				network.WithDriver("bridge"),
				network.WithOption("com.docker.network.bridge.name", bridgeName))...)
			defer network.RemoveNoError(ctx, t, c, bridgeName)

			ctr1Name := fmt.Sprintf("ctr-icc-%d-1", tcID)
			var ctr1Opts []func(config *container.TestContainerConfig)
			if tc.ctr1MacAddress != "" {
				ctr1Opts = append(ctr1Opts, container.WithMacAddress(bridgeName, tc.ctr1MacAddress))
			}
			id1 := container.Run(ctx, t, c, append(ctr1Opts,
				container.WithName(ctr1Name),
				container.WithImage("busybox:latest"),
				container.WithCmd("top"),
				container.WithNetworkMode(bridgeName))...)
			defer c.ContainerRemove(ctx, id1, client.ContainerRemoveOptions{
				Force: true,
			})

			networking.FirewalldReload(t, d)

			pingHost := tc.pingHost
			if pingHost == "" {
				if tc.isLinkLocal {
					inspect := container.Inspect(ctx, t, c, id1)
					pingHost = inspect.NetworkSettings.Networks[bridgeName].GlobalIPv6Address.WithZone("eth0").String()
				} else {
					pingHost = ctr1Name
				}
			}

			ipv := "-4"
			if tc.isIPv6 {
				ipv = "-6"
			}

			pingCmd := []string{"ping", "-c1", "-W3", ipv, pingHost}

			ctr2Name := fmt.Sprintf("ctr-icc-%d-2", tcID)
			id2 := container.Run(ctx, t, c,
				container.WithName(ctr2Name),
				container.WithImage("busybox:latest"),
				container.WithCmd("top"),
				container.WithNetworkMode(bridgeName))
			defer c.ContainerRemove(ctx, id2, client.ContainerRemoveOptions{
				Force: true,
			})

			var res container.ExecResult
			poll.WaitOn(t, func(_ poll.LogT) poll.Result {
				execCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()

				var err error
				res, err = container.Exec(execCtx, c, id2, pingCmd)
				if err != nil {
					return poll.Error(err)
				}
				if res.ExitCode != 0 {
					return poll.Continue(
						"ping failed with exit code %d, stdout: %s, stderr: %s",
						res.ExitCode, res.Stdout(), res.Stderr(),
					)
				}
				return poll.Success()
			}, poll.WithTimeout(15*time.Second))

			assert.Check(t, is.Equal(res.ExitCode, 0))
			assert.Check(t, is.Equal(res.Stderr(), ""))
			assert.Check(t, is.Contains(res.Stdout(), "1 packets transmitted, 1 packets received"))
		})
	}
}

// TestBridgeINC makes sure two containers on two different bridge networks can't communicate with each other.
func TestBridgeINC(t *testing.T) {
	ctx := setupTest(t)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	c := d.NewClientT(t)
	defer c.Close()

	type bridgesOpts struct {
		bridge1Opts []func(*client.NetworkCreateOptions)
		bridge2Opts []func(*client.NetworkCreateOptions)
	}

	testcases := []struct {
		name    string
		bridges bridgesOpts
		ipv6    bool
		stdout  string
		stderr  string
	}{
		{
			name: "IPv4 non-internal network",
			bridges: bridgesOpts{
				bridge1Opts: []func(*client.NetworkCreateOptions){},
				bridge2Opts: []func(*client.NetworkCreateOptions){},
			},
			stdout: "1 packets transmitted, 0 packets received",
		},
		{
			name: "IPv4 internal network",
			bridges: bridgesOpts{
				bridge1Opts: []func(*client.NetworkCreateOptions){network.WithInternal()},
				bridge2Opts: []func(*client.NetworkCreateOptions){network.WithInternal()},
			},
			stderr: "sendto: Network is unreachable",
		},
		{
			name: "IPv6 ULA on non-internal network",
			bridges: bridgesOpts{
				bridge1Opts: []func(*client.NetworkCreateOptions){
					network.WithIPv6(),
					network.WithIPAM("fdf1:a844:380c:b200::/64", "fdf1:a844:380c:b200::1"),
				},
				bridge2Opts: []func(*client.NetworkCreateOptions){
					network.WithIPv6(),
					network.WithIPAM("fdf1:a844:380c:b247::/64", "fdf1:a844:380c:b247::1"),
				},
			},
			ipv6:   true,
			stdout: "1 packets transmitted, 0 packets received",
		},
		{
			name: "IPv6 ULA on internal network",
			bridges: bridgesOpts{
				bridge1Opts: []func(*client.NetworkCreateOptions){
					network.WithIPv6(),
					network.WithInternal(),
					network.WithIPAM("fdf1:a844:390c:b200::/64", "fdf1:a844:390c:b200::1"),
				},
				bridge2Opts: []func(*client.NetworkCreateOptions){
					network.WithIPv6(),
					network.WithInternal(),
					network.WithIPAM("fdf1:a844:390c:b247::/64", "fdf1:a844:390c:b247::1"),
				},
			},
			ipv6:   true,
			stderr: "sendto: Network is unreachable",
		},
	}

	for tcID, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)

			bridge1 := fmt.Sprintf("testnet-inc-%d-1", tcID)
			bridge2 := fmt.Sprintf("testnet-inc-%d-2", tcID)

			network.CreateNoError(ctx, t, c, bridge1, append(tc.bridges.bridge1Opts,
				network.WithDriver("bridge"),
				network.WithOption("com.docker.network.bridge.name", bridge1))...)
			defer network.RemoveNoError(ctx, t, c, bridge1)
			network.CreateNoError(ctx, t, c, bridge2, append(tc.bridges.bridge2Opts,
				network.WithDriver("bridge"),
				network.WithOption("com.docker.network.bridge.name", bridge2))...)
			defer network.RemoveNoError(ctx, t, c, bridge2)

			ctr1Name := sanitizeCtrName(t.Name() + "-ctr1")
			id1 := container.Run(ctx, t, c,
				container.WithName(ctr1Name),
				container.WithImage("busybox:latest"),
				container.WithCmd("top"),
				container.WithNetworkMode(bridge1))
			defer c.ContainerRemove(ctx, id1, client.ContainerRemoveOptions{
				Force: true,
			})
			networking.FirewalldReload(t, d)

			ctr1Info := container.Inspect(ctx, t, c, id1)
			targetAddr := ctr1Info.NetworkSettings.Networks[bridge1].IPAddress
			if tc.ipv6 {
				targetAddr = ctr1Info.NetworkSettings.Networks[bridge1].GlobalIPv6Address
			}

			pingCmd := []string{"ping", "-c1", "-W3", targetAddr.String()}

			ctr2Name := sanitizeCtrName(t.Name() + "-ctr2")
			attachCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			res := container.RunAttach(attachCtx, t, c,
				container.WithName(ctr2Name),
				container.WithImage("busybox:latest"),
				container.WithCmd(pingCmd...),
				container.WithNetworkMode(bridge2))
			defer c.ContainerRemove(ctx, res.ContainerID, client.ContainerRemoveOptions{
				Force: true,
			})

			assert.Check(t, res.ExitCode != 0, "ping unexpectedly succeeded")
			assert.Check(t, is.Contains(res.Stdout.String(), tc.stdout))
			assert.Check(t, is.Contains(res.Stderr.String(), tc.stderr))
		})
	}
}

// TestBridgeINCRouted makes sure a container on a gateway-mode=nat network can establish
// a connection to a container on a gateway-mode=routed network, but not vice-versa.
func TestBridgeINCRouted(t *testing.T) {
	skip.If(t, testEnv.IsRootless(), "can't set filter-forward policy in rootless netns")
	ctx := setupTest(t)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	t.Cleanup(func() { d.Stop(t) })

	c := d.NewClientT(t)
	t.Cleanup(func() { c.Close() })

	type ctrDesc struct {
		id   string
		ipv4 string
		ipv6 string
	}

	// Create a network and run a container on it.
	// Run http servers on ports 80 and 81, but only map/open port 80.
	createNet := func(gwMode string) ctrDesc {
		netName := "test-" + gwMode
		network.CreateNoError(ctx, t, c, netName,
			network.WithDriver("bridge"),
			network.WithIPv6(),
			network.WithOption(bridge.BridgeName, "br-"+gwMode),
			network.WithOption(bridge.IPv4GatewayMode, gwMode),
			network.WithOption(bridge.IPv6GatewayMode, gwMode),
		)
		t.Cleanup(func() {
			network.RemoveNoError(ctx, t, c, netName)
		})

		ctrId := container.Run(ctx, t, c,
			container.WithNetworkMode(netName),
			container.WithName("ctr-"+gwMode),
			container.WithExposedPorts("80/tcp"),
			// TODO(robmry): this test supplies an empty list of PortBindings.
			// https://github.com/moby/moby/issues/51727 will break it.
			container.WithPortMap(networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {}}),
		)
		t.Cleanup(func() {
			c.ContainerRemove(ctx, ctrId, client.ContainerRemoveOptions{Force: true})
		})

		container.ExecT(ctx, t, c, ctrId, []string{"httpd", "-p", "80"})
		container.ExecT(ctx, t, c, ctrId, []string{"httpd", "-p", "81"})

		insp := container.Inspect(ctx, t, c, ctrId)
		return ctrDesc{
			id:   ctrId,
			ipv4: insp.NetworkSettings.Networks[netName].IPAddress.String(),
			ipv6: insp.NetworkSettings.Networks[netName].GlobalIPv6Address.String(),
		}
	}

	natDesc := createNet("nat")
	routedDesc := createNet("routed")

	const (
		httpSuccess = "404 Not Found"
		httpFail    = "download timed out"
		pingSuccess = 0
		pingFail    = 1
	)

	testcases := []struct {
		name          string
		from          ctrDesc
		to            ctrDesc
		port          string
		expPingExit   int
		expHttpStderr string
	}{
		{
			name:          "nat to routed open port",
			from:          natDesc,
			to:            routedDesc,
			port:          "80",
			expPingExit:   pingSuccess,
			expHttpStderr: httpSuccess,
		},
		{
			name:          "nat to routed closed port",
			from:          natDesc,
			to:            routedDesc,
			port:          "81",
			expPingExit:   pingSuccess,
			expHttpStderr: httpFail,
		},
		{
			name:          "routed to nat open port",
			from:          routedDesc,
			to:            natDesc,
			port:          "80",
			expPingExit:   pingFail,
			expHttpStderr: httpFail,
		},
		{
			name:          "routed to nat closed port",
			from:          routedDesc,
			to:            natDesc,
			port:          "81",
			expPingExit:   pingFail,
			expHttpStderr: httpFail,
		},
	}

	runTests := func(testName, policy string) {
		networking.FirewalldReload(t, d)
		t.Run(testName, func(t *testing.T) {
			if policy != "" {
				networking.SetFilterForwardPolicies(t, policy)
			}
			for _, tc := range testcases {
				t.Run(tc.name+"/v4/ping", func(t *testing.T) {
					t.Parallel()
					ctx := testutil.StartSpan(ctx, t)
					pingRes4 := container.ExecT(ctx, t, c, tc.from.id, []string{
						"ping", "-4", "-c1", "-W3", tc.to.ipv4,
					})
					assert.Check(t, is.Equal(pingRes4.ExitCode, tc.expPingExit))
				})
				t.Run(tc.name+"/v6/ping", func(t *testing.T) {
					t.Parallel()
					ctx := testutil.StartSpan(ctx, t)
					pingRes6 := container.ExecT(ctx, t, c, tc.from.id, []string{
						"ping", "-6", "-c1", "-W3", tc.to.ipv6,
					})
					assert.Check(t, is.Equal(pingRes6.ExitCode, tc.expPingExit))
				})
				t.Run(tc.name+"/v4/http", func(t *testing.T) {
					t.Parallel()
					ctx := testutil.StartSpan(ctx, t)
					httpRes4 := container.ExecT(ctx, t, c, tc.from.id, []string{
						"wget", "-T3", "http://" + net.JoinHostPort(tc.to.ipv4, tc.port),
					})
					assert.Check(t, is.Contains(httpRes4.Stderr(), tc.expHttpStderr))
				})
				t.Run(tc.name+"/v6/http", func(t *testing.T) {
					t.Parallel()
					ctx := testutil.StartSpan(ctx, t)
					httpRes6 := container.ExecT(ctx, t, c, tc.from.id, []string{
						"wget", "-T3", "http://" + net.JoinHostPort(tc.to.ipv6, tc.port),
					})
					assert.Check(t, is.Contains(httpRes6.Stderr(), tc.expHttpStderr))
				})
			}
		})
	}

	if strings.HasPrefix(d.FirewallBackendDriver(t), "iptables") {
		runTests("iptables-ACCEPT", "ACCEPT")
		runTests("iptables-DROP", "DROP")
	} else {
		runTests("nftables", "")
	}
}

// TestAccessToPublishedPort checks that a container in one network can
// access a port published to the host by a container in another network,
// with various combinations of gateway-mode, with and without the
// userland proxy.
//
// Regression test for https://github.com/moby/moby/issues/49509
func TestAccessToPublishedPort(t *testing.T) {
	skip.If(t, testEnv.IsRootless, "Published port not accessible from rootless netns")

	ctx := setupTest(t)

	testcases := []struct {
		name          string
		clientGwMode  string
		userlandProxy bool
	}{
		{
			name:          "client=routed/proxy=true",
			clientGwMode:  "routed",
			userlandProxy: true,
		},
		{
			name:         "client=routed/proxy=false",
			clientGwMode: "routed",
		},
		{
			name:          "client=nat/proxy=true",
			clientGwMode:  "nat",
			userlandProxy: true,
		},
		{
			name:         "client=nat/proxy=false",
			clientGwMode: "nat",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			d := daemon.New(t)
			d.StartWithBusybox(ctx, t, "--ipv6", "--userland-proxy="+strconv.FormatBool(tc.userlandProxy))
			defer d.Stop(t)

			c := d.NewClientT(t)
			defer c.Close()

			const serverNetName = "tnet-server"
			network.CreateNoError(ctx, t, c, serverNetName,
				network.WithDriver("bridge"),
				network.WithIPv6(),
				network.WithOption(bridge.BridgeName, "br-server"),
			)
			defer network.RemoveNoError(ctx, t, c, serverNetName)

			ctrId := container.Run(ctx, t, c,
				container.WithNetworkMode(serverNetName),
				container.WithName("ctr-server"),
				container.WithExposedPorts("80/tcp"),
				container.WithPortMap(networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {networktypes.PortBinding{HostPort: "8080"}}}),
				container.WithCmd("httpd", "-f"),
			)
			defer c.ContainerRemove(ctx, ctrId, client.ContainerRemoveOptions{Force: true})

			const clientNetName = "tnet-client"
			network.CreateNoError(ctx, t, c, clientNetName,
				network.WithDriver("bridge"),
				network.WithIPv6(),
				network.WithOption(bridge.BridgeName, "br-client"),
				network.WithOption(bridge.IPv4GatewayMode, tc.clientGwMode),
				network.WithOption(bridge.IPv6GatewayMode, tc.clientGwMode),
			)
			defer network.RemoveNoError(ctx, t, c, clientNetName)

			networking.FirewalldReload(t, d)

			// Use the default bridge addresses as host addresses (like "host-gateway", but
			// there's no way to tell wget to prefer ipv4/ipv6 transport, so just use the
			// addresses directly).
			res, err := c.NetworkInspect(ctx, "bridge", client.NetworkInspectOptions{})
			assert.NilError(t, err)
			for _, ipamCfg := range res.Network.IPAM.Config {
				ipv := "ipv4"
				if ipamCfg.Gateway.Is6() {
					ipv = "ipv6"
				}
				t.Run(ipv, func(t *testing.T) {
					url := "http://" + net.JoinHostPort(ipamCfg.Gateway.String(), "8080")
					res := container.RunAttach(ctx, t, c,
						container.WithNetworkMode(clientNetName),
						container.WithCmd("wget", "-O-", "-T3", url),
					)
					// 404 Not Found means the server responded, but it's got nothing to serve.
					assert.Check(t, is.Contains(res.Stderr.String(), "404 Not Found"), "url: %s", url)
				})
			}
		})
	}
}

// TestInterNetworkDirectRouting checks whether containers in one network
// can access ports on container addresses in other networks for combinations
// of gateway mode, published and unpublished ports, with and without the
// userland-proxy. (This is about direct routing between containers, so the
// docker-proxy shouldn't be involved - but the firewall config is a bit
// different, so it's worth testing.)
//
// Regression test for https://github.com/moby/moby/issues/49509
func TestInterNetworkDirectRouting(t *testing.T) {
	ctx := setupTest(t)

	testcases := []struct {
		name          string
		serverGwMode  string
		userlandProxy bool
		expPubResp    bool
		expUnpubResp  bool
	}{
		{
			name:          "server=nat/proxy=true",
			serverGwMode:  "nat",
			userlandProxy: true,
			expPubResp:    false, // Direct routing is blocked by raw-prerouting rules.
			expUnpubResp:  false, // Direct routing is blocked by raw-prerouting rules.
		},
		{
			name:         "server=nat/proxy=false",
			serverGwMode: "nat",
			expPubResp:   false, // Direct routing is blocked by raw-prerouting rules.
			expUnpubResp: false, // Direct routing is blocked by raw-prerouting rules.
		},
		{
			name:          "server=routed/proxy=true",
			serverGwMode:  "routed",
			userlandProxy: true,
			expPubResp:    true,
			expUnpubResp:  false, // Unpublished ports are blocked by port-filtering rules.
		},
		{
			name:         "server=routed/proxy=false",
			serverGwMode: "routed",
			expPubResp:   true,
			expUnpubResp: false, // Unpublished ports are blocked by port-filtering rules.
		},
		{
			name:          "server=nat-unprotected/proxy=true",
			serverGwMode:  "nat-unprotected",
			userlandProxy: true,
			expPubResp:    true,
			expUnpubResp:  true,
		},
		{
			name:         "server=nat-unprotected/proxy=false",
			serverGwMode: "nat-unprotected",
			expPubResp:   true,
			expUnpubResp: true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			d := daemon.New(t)
			d.StartWithBusybox(ctx, t, "--ipv6", "--userland-proxy="+strconv.FormatBool(tc.userlandProxy))
			defer d.Stop(t)

			c := d.NewClientT(t)
			defer c.Close()

			const serverNetName = "tnet-server"
			network.CreateNoError(ctx, t, c, serverNetName,
				network.WithDriver("bridge"),
				network.WithIPv6(),
				network.WithOption(bridge.BridgeName, "br-server"),
				network.WithOption(bridge.IPv4GatewayMode, tc.serverGwMode),
				network.WithOption(bridge.IPv6GatewayMode, tc.serverGwMode),
			)
			defer network.RemoveNoError(ctx, t, c, serverNetName)

			ctrPubId := container.Run(ctx, t, c,
				container.WithNetworkMode(serverNetName),
				container.WithName("ctr-pub"),
				container.WithExposedPorts("80/tcp"),
				container.WithPortMap(networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {networktypes.PortBinding{HostPort: "8080"}}}),
				container.WithCmd("httpd", "-f"),
			)
			defer c.ContainerRemove(ctx, ctrPubId, client.ContainerRemoveOptions{Force: true})
			inspPub := container.Inspect(ctx, t, c, ctrPubId)
			pub4 := inspPub.NetworkSettings.Networks[serverNetName].IPAddress
			pub6 := inspPub.NetworkSettings.Networks[serverNetName].GlobalIPv6Address

			ctrUnpubId := container.Run(ctx, t, c,
				container.WithNetworkMode(serverNetName),
				container.WithName("ctr-unpub"),
				container.WithCmd("httpd", "-f"),
			)
			defer c.ContainerRemove(ctx, ctrUnpubId, client.ContainerRemoveOptions{Force: true})
			inspUnpub := container.Inspect(ctx, t, c, ctrUnpubId)
			unpub4 := inspUnpub.NetworkSettings.Networks[serverNetName].IPAddress
			unpub6 := inspUnpub.NetworkSettings.Networks[serverNetName].GlobalIPv6Address

			const clientNetName = "tnet-client"
			network.CreateNoError(ctx, t, c, clientNetName,
				network.WithDriver("bridge"),
				network.WithIPv6(),
				network.WithOption(bridge.BridgeName, "br-client"),
			)
			defer network.RemoveNoError(ctx, t, c, clientNetName)

			checkHTTP := func(addr string, expResp bool) func(t *testing.T) {
				return func(t *testing.T) {
					t.Parallel()
					t.Helper()
					url := "http://" + net.JoinHostPort(addr, "80")
					res := container.RunAttach(ctx, t, c,
						container.WithNetworkMode(clientNetName),
						container.WithCmd("wget", "-O-", "-T3", url),
					)
					if expResp {
						// 404 Not Found means the server responded, but it's got nothing to serve.
						assert.Check(t, is.Contains(res.Stderr.String(), "404 Not Found"), "url: %s", url)
					} else {
						assert.Check(t, is.Contains(res.Stderr.String(), "download timed out"), "url: %s", url)
					}
				}
			}
			t.Run("w", func(t *testing.T) { // Wait for the parallel tests to complete.
				t.Run("ipv4/pub", checkHTTP(pub4.String(), tc.expPubResp))
				t.Run("ipv6/pub", checkHTTP(pub6.String(), tc.expPubResp))
				t.Run("ipv4/unpub", checkHTTP(unpub4.String(), tc.expUnpubResp))
				t.Run("ipv6/unpub", checkHTTP(unpub6.String(), tc.expUnpubResp))
			})
		})
	}
}

func TestDefaultBridgeIPv6(t *testing.T) {
	ctx := setupTest(t)

	testcases := []struct {
		name          string
		fixed_cidr_v6 string
	}{
		{
			name: "built in ULA prefix",
		},
		{
			name:          "IPv6 ULA",
			fixed_cidr_v6: "fd00:1235::/64",
		},
		{
			name:          "IPv6 LLA only",
			fixed_cidr_v6: "fe80::/64",
		},
		{
			name:          "IPv6 nonstandard LLA only",
			fixed_cidr_v6: "fe80:1236::/64",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)

			d := daemon.New(t)
			if tc.fixed_cidr_v6 == "" {
				d.StartWithBusybox(ctx, t, "--ipv6")
			} else {
				d.StartWithBusybox(ctx, t, "--ipv6", "--fixed-cidr-v6", tc.fixed_cidr_v6)
			}
			defer d.Stop(t)

			c := d.NewClientT(t)
			defer c.Close()

			cID := container.Run(ctx, t, c,
				container.WithImage("busybox:latest"),
				container.WithCmd("top"),
			)
			defer c.ContainerRemove(ctx, cID, client.ContainerRemoveOptions{
				Force: true,
			})

			const networkName = "bridge"
			inspect := container.Inspect(ctx, t, c, cID)
			gIPv6 := inspect.NetworkSettings.Networks[networkName].GlobalIPv6Address

			attachCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			res := container.RunAttach(attachCtx, t, c,
				container.WithImage("busybox:latest"),
				container.WithCmd("ping", "-c1", "-W3", gIPv6.String()),
			)
			defer c.ContainerRemove(ctx, res.ContainerID, client.ContainerRemoveOptions{
				Force: true,
			})

			assert.Check(t, is.Equal(res.ExitCode, 0))
			assert.Check(t, is.Equal(res.Stderr.String(), ""))
			assert.Check(t, is.Contains(res.Stdout.String(), "1 packets transmitted, 1 packets received"))
		})
	}
}

// Check that it's possible to change 'fixed-cidr-v6' and restart the daemon.
func TestDefaultBridgeAddresses(t *testing.T) {
	ctx := setupTest(t)

	type testStep struct {
		stepName    string
		fixedCIDRV6 string
		expAddrs    []string
	}

	testcases := []struct {
		name  string
		steps []testStep
	}{
		{
			name: "Unique-Local Subnet Changes",
			steps: []testStep{
				{
					stepName:    "Set up initial UL prefix",
					fixedCIDRV6: "fd1c:f1a0:5d8d:aaaa::/64",
					expAddrs:    []string{"fd1c:f1a0:5d8d:aaaa::1/64", "fe80::"},
				},
				{
					// Modify that prefix, the default bridge's address must be deleted and re-added.
					stepName:    "Modify UL prefix - address change",
					fixedCIDRV6: "fd1c:f1a0:5d8d:bbbb::/64",
					expAddrs:    []string{"fd1c:f1a0:5d8d:bbbb::1/64", "fe80::"},
				},
				{
					// Modify the prefix length, the default bridge's address should not change.
					stepName:    "Modify UL prefix - no address change",
					fixedCIDRV6: "fd1c:f1a0:5d8d:bbbb::/80",
					// The prefix length displayed by 'ip a' is not updated - it's informational, and
					// can't be changed without unnecessarily deleting and re-adding the address.
					expAddrs: []string{"fd1c:f1a0:5d8d:bbbb::1/64", "fe80::"},
				},
			},
		},
		{
			name: "Link-Local Subnet Changes",
			steps: []testStep{
				{
					stepName:    "Standard LL subnet prefix",
					fixedCIDRV6: "fe80::/64",
					expAddrs:    []string{"fe80::"},
				},
				{
					// Modify that prefix, the default bridge's address must be deleted and re-added.
					// The bridge must still have an address in the required (standard) LL subnet.
					stepName:    "Nonstandard LL prefix - address change",
					fixedCIDRV6: "fe80:1237::/32",
					expAddrs:    []string{"fe80:1237::1/32", "fe80::"},
				},
				{
					// Modify the prefix length, the addresses should not change.
					stepName:    "Modify LL prefix - no address change",
					fixedCIDRV6: "fe80:1238::/64",
					// The prefix length displayed by 'ip a' is not updated - it's informational, and
					// can't be changed without unnecessarily deleting and re-adding the address.
					expAddrs: []string{"fe80:1238::1/", "fe80::"},
				},
			},
		},
	}

	d := daemon.New(t)
	defer d.Stop(t)
	c := d.NewClientT(t)

	for _, tc := range testcases {
		for _, step := range tc.steps {
			t.Run(tc.name+"/"+step.stepName, func(t *testing.T) {
				ctx := testutil.StartSpan(ctx, t)
				// Check that the daemon starts - regression test for:
				//   https://github.com/moby/moby/issues/46829
				d.StartWithBusybox(ctx, t, "--ipv6", "--fixed-cidr-v6="+step.fixedCIDRV6)

				// Start a container, so that the bridge is set "up" and gets a kernel_ll address.
				cID := container.Run(ctx, t, c)
				defer c.ContainerRemove(ctx, cID, client.ContainerRemoveOptions{Force: true})

				d.Stop(t)

				// Check that the expected addresses have been applied to the bridge. (Skip in
				// rootless mode, because the bridge is in a different network namespace.)
				if !testEnv.IsRootless() {
					res := testutil.RunCommand(ctx, "ip", "-6", "addr", "show", "docker0")
					assert.Equal(t, res.ExitCode, 0, step.stepName)
					stdout := res.Stdout()
					for _, expAddr := range step.expAddrs {
						assert.Check(t, is.Contains(stdout, expAddr))
					}
				}
			})
		}
	}
}

// Test that a container on an 'internal' network has IP connectivity with
// the host (on its own subnet, because the n/w bridge has an address on that
// subnet, and it's in the host's namespace).
// Regression test for https://github.com/moby/moby/issues/47329
func TestInternalNwConnectivity(t *testing.T) {
	ctx := setupTest(t)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	c := d.NewClientT(t)
	defer c.Close()

	const bridgeName = "intnw"
	const gw4 = "172.30.0.1"
	const gw6 = "fda9:4130:4715::1234"
	network.CreateNoError(ctx, t, c, bridgeName,
		network.WithInternal(),
		network.WithIPv6(),
		network.WithIPAM("172.30.0.0/24", gw4),
		network.WithIPAM("fda9:4130:4715::/64", gw6),
		network.WithDriver("bridge"),
		network.WithOption("com.docker.network.bridge.name", bridgeName),
	)
	defer network.RemoveNoError(ctx, t, c, bridgeName)

	const ctrName = "intctr"
	id := container.Run(ctx, t, c,
		container.WithName(ctrName),
		container.WithImage("busybox:latest"),
		container.WithCmd("top"),
		container.WithNetworkMode(bridgeName),
	)
	defer c.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
	networking.FirewalldReload(t, d)

	execCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	res := container.ExecT(execCtx, t, c, id, []string{"ping", "-c1", "-W3", gw4})
	assert.Check(t, is.Equal(res.ExitCode, 0))
	assert.Check(t, is.Equal(res.Stderr(), ""))
	assert.Check(t, is.Contains(res.Stdout(), "1 packets transmitted, 1 packets received"))

	res = container.ExecT(execCtx, t, c, id, []string{"ping6", "-c1", "-W3", gw6})
	assert.Check(t, is.Equal(res.ExitCode, 0))
	assert.Check(t, is.Equal(res.Stderr(), ""))
	assert.Check(t, is.Contains(res.Stdout(), "1 packets transmitted, 1 packets received"))

	// Addresses outside the internal subnet must not be accessible.
	res = container.ExecT(execCtx, t, c, id, []string{"ping", "-c1", "-W3", "8.8.8.8"})
	assert.Check(t, is.Equal(res.ExitCode, 1))
	assert.Check(t, is.Contains(res.Stderr(), "Network is unreachable"))
}

// Check that the container's interfaces have no IPv6 address when IPv6 is
// disabled in a container via sysctl (including 'lo').
func TestDisableIPv6Addrs(t *testing.T) {
	ctx := setupTest(t)
	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	c := d.NewClientT(t)
	defer c.Close()

	testcases := []struct {
		name    string
		sysctls map[string]string
		expIPv6 bool
	}{
		{
			name:    "IPv6 enabled",
			expIPv6: true,
		},
		{
			name:    "IPv6 disabled",
			sysctls: map[string]string{"net.ipv6.conf.all.disable_ipv6": "1"},
		},
	}

	const netName = "testnet"
	network.CreateNoError(ctx, t, c, netName,
		network.WithIPv6(),
		network.WithIPAM("fda0:ef3d:6430:abcd::/64", "fda0:ef3d:6430:abcd::1"),
	)
	defer network.RemoveNoError(ctx, t, c, netName)

	inet6RE := regexp.MustCompile(`inet6[ \t]+[0-9a-f:]*`)

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)

			opts := []func(config *container.TestContainerConfig){
				container.WithCmd("ip", "a"),
				container.WithNetworkMode(netName),
			}
			if len(tc.sysctls) > 0 {
				opts = append(opts, container.WithSysctls(tc.sysctls))
			}

			runRes := container.RunAttach(ctx, t, c, opts...)
			defer c.ContainerRemove(ctx, runRes.ContainerID,
				client.ContainerRemoveOptions{Force: true},
			)

			stdout := runRes.Stdout.String()
			inet6 := inet6RE.FindAllString(stdout, -1)
			if tc.expIPv6 {
				assert.Check(t, len(inet6) > 0, "Expected IPv6 addresses but found none.")
			} else {
				assert.Check(t, is.DeepEqual(inet6, []string{}, cmpopts.EquateEmpty()))
			}
		})
	}
}

// TestDisableIPv6OnInterface checks that it's possible to disable IPv6 on an
// endpoint in an IPv6 network using a sysctl.
func TestDisableIPv6OnInterface(t *testing.T) {
	ctx := setupTest(t)
	d := daemon.New(t)
	d.StartWithBusybox(ctx, t, "--ipv6")
	defer d.Stop(t)

	c := d.NewClientT(t)
	defer c.Close()

	tests := []struct {
		name    string
		netName string
	}{
		{
			name:    "default bridge",
			netName: "bridge",
		},
		{
			name:    "user-defined bridge",
			netName: "testnet",
		},
	}

	for _, tc := range tests {
		t.Run(tc.netName, func(t *testing.T) {
			if tc.netName != "bridge" {
				network.CreateNoError(ctx, t, c, tc.netName, network.WithIPv6())
				defer network.RemoveNoError(ctx, t, c, tc.netName)
			}

			const ctrName = "ctr"
			ctrId := container.Run(ctx, t, c,
				container.WithName(ctrName),
				container.WithNetworkMode(tc.netName),
				container.WithExposedPorts("80/tcp"),
				container.WithPortMap(networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {{HostPort: "8080"}}}),
				container.WithEndpointSettings(tc.netName, &networktypes.EndpointSettings{
					DriverOpts: map[string]string{
						netlabel.EndpointSysctls: "net.ipv6.conf.IFNAME.disable_ipv6=1",
					},
				}),
			)
			defer c.ContainerRemove(ctx, ctrId, client.ContainerRemoveOptions{Force: true})

			// The interface should not have any IPv6 addresses.
			execRes := container.ExecT(ctx, t, c, ctrId, []string{"ip", "a", "show", "eth0"})
			assert.Check(t, !strings.Contains(execRes.Stdout(), "inet6"),
				"Unexpected IPv6 address in: %s", execRes.Stdout())

			// Inspect should not show an IPv6 container address.
			inspRes2 := container.Inspect(ctx, t, c, ctrId)
			assert.Check(t, is.Equal(netip.Addr{}, inspRes2.NetworkSettings.Networks[tc.netName].GlobalIPv6Address))
			assert.Check(t, is.Equal(0, inspRes2.NetworkSettings.Networks[tc.netName].GlobalIPv6PrefixLen))

			// Port mappings should be IPv4-only - but can't see the proxy processes in the rootless netns.
			if !testEnv.IsRootless() {
				checkProxies(ctx, t, c, d.Pid(), []expProxyCfg{
					{"tcp", "0.0.0.0", "8080", ctrName, tc.netName, true, "80"},
					{"tcp", "::", "8080", ctrName, tc.netName, true, "80"},
				})
			}

			// There should not be an IPv6 DNS or /etc/hosts entry.
			runRes := container.RunAttach(ctx, t, c,
				container.WithNetworkMode(tc.netName),
				container.WithCmd("ping", "-6", "-c1", ctrName),
			)
			assert.Check(t, is.Equal(runRes.ExitCode, 1))
			assert.Check(t, is.Contains(runRes.Stderr.String(), "bad address"))
		})
	}
}

// Check that a container in a network with IPv4 disabled doesn't get
// IPv4 addresses.
func TestDisableIPv4(t *testing.T) {
	ctx := setupTest(t)
	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	tests := []struct {
		name       string
		apiVersion string
		expIPv4    bool
	}{
		{
			name:    "disable ipv4",
			expIPv4: false,
		},
		{
			name:       "old api ipv4 not disabled",
			apiVersion: "1.46",
			expIPv4:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := d.NewClientT(t, client.WithAPIVersion(tc.apiVersion))

			const netName = "testnet"
			network.CreateNoError(ctx, t, c, netName,
				network.WithIPv4(false),
				network.WithIPv6(),
			)
			defer network.RemoveNoError(ctx, t, c, netName)

			id := container.Run(ctx, t, c, container.WithNetworkMode(netName))
			defer c.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})

			loRes := container.ExecT(ctx, t, c, id, []string{"ip", "a", "show", "dev", "lo"})
			assert.Check(t, is.Contains(loRes.Combined(), " inet ")) // 127.0.0.1
			assert.Check(t, is.Contains(loRes.Combined(), " inet6 "))

			eth0Res := container.ExecT(ctx, t, c, id, []string{"ip", "a", "show", "dev", "eth0"})
			if tc.expIPv4 {
				assert.Check(t, is.Contains(eth0Res.Combined(), " inet "))
			} else {
				assert.Check(t, !strings.Contains(eth0Res.Combined(), " inet "),
					"result.Combined(): %s", eth0Res.Combined())
			}
			assert.Check(t, is.Contains(eth0Res.Combined(), " inet6 "))
		})
	}
}

// Check that an interface to an '--ipv6=false' network has no IPv6
// address - either IPAM assigned, or kernel-assigned LL, but the loopback
// interface does still have an IPv6 address ('::1').
func TestNonIPv6Network(t *testing.T) {
	ctx := setupTest(t)
	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	c := d.NewClientT(t)
	defer c.Close()

	const netName = "testnet"
	network.CreateNoError(ctx, t, c, netName)
	defer network.RemoveNoError(ctx, t, c, netName)

	id := container.Run(ctx, t, c, container.WithNetworkMode(netName))
	defer c.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})

	loRes := container.ExecT(ctx, t, c, id, []string{"ip", "a", "show", "dev", "lo"})
	assert.Check(t, is.Contains(loRes.Combined(), " inet "))
	assert.Check(t, is.Contains(loRes.Combined(), " inet6 "))

	eth0Res := container.ExecT(ctx, t, c, id, []string{"ip", "a", "show", "dev", "eth0"})
	assert.Check(t, is.Contains(eth0Res.Combined(), " inet "))
	assert.Check(t, !strings.Contains(eth0Res.Combined(), " inet6 "),
		"result.Combined(): %s", eth0Res.Combined())

	sysctlRes := container.ExecT(ctx, t, c, id, []string{"sysctl", "-n", "net.ipv6.conf.eth0.disable_ipv6"})
	assert.Check(t, is.Equal(strings.TrimSpace(sysctlRes.Combined()), "1"))
}

// Check that starting the daemon with '--ip6tables=false' means no ip6tables
// rules get set up for an IPv6 bridge network.
func TestNoIP6Tables(t *testing.T) {
	skip.If(t, testEnv.IsRootless)

	ctx := setupTest(t)

	testcases := []struct {
		name            string
		option          string
		reloadFirewalld bool
		expIPTables     bool
	}{
		{
			name:        "ip6tables on",
			option:      "--ip6tables=true",
			expIPTables: true,
		},
		{
			name:   "ip6tables off",
			option: "--ip6tables=false",
		},
		{
			name:            "ip6tables off with firewalld reload",
			option:          "--ip6tables=false",
			reloadFirewalld: true,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.reloadFirewalld {
				skip.If(t, !networking.FirewalldRunning(), "firewalld is not running")
			}
			ctx := testutil.StartSpan(ctx, t)

			d := daemon.New(t)
			d.StartWithBusybox(ctx, t, tc.option)
			defer d.Stop(t)

			c := d.NewClientT(t)
			defer c.Close()

			const netName = "testnet"
			const bridgeName = "testbr"
			const subnet = "fdb3:2511:e851:34a9::/64"
			network.CreateNoError(ctx, t, c, netName,
				network.WithIPv6(),
				network.WithOption("com.docker.network.bridge.name", bridgeName),
				network.WithIPAM(subnet, "fdb3:2511:e851:34a9::1"),
			)
			defer network.RemoveNoError(ctx, t, c, netName)

			id := container.Run(ctx, t, c, container.WithNetworkMode(netName))
			defer c.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})

			if tc.reloadFirewalld {
				networking.FirewalldReload(t, d)
			}
			var cmd *exec.Cmd
			if strings.HasPrefix(d.FirewallBackendDriver(t), "nftables") {
				cmd = exec.Command("nft", "list", "table", "ip6", "docker-bridges")
			} else {
				cmd = exec.Command("/usr/sbin/ip6tables-save")
			}
			res, err := cmd.CombinedOutput()
			assert.NilError(t, err)
			dump := string(res)
			if tc.expIPTables {
				assert.Check(t, is.Contains(dump, subnet))
				assert.Check(t, is.Contains(dump, bridgeName))
			} else {
				assert.Check(t, !strings.Contains(dump, subnet),
					fmt.Sprintf("Didn't expect to find '%s' in '%s'", subnet, dump))
				assert.Check(t, !strings.Contains(dump, bridgeName),
					fmt.Sprintf("Didn't expect to find '%s' in '%s'", bridgeName, dump))
			}
		})
	}
}

// Test that it's possible to set a sysctl on an interface in the container
// when using API 1.46 (in later versions of the API, per-interface sysctls
// must be set using driver option 'com.docker.network.endpoint.sysctls').
// Regression test for https://github.com/moby/moby/issues/47619
func TestSetInterfaceSysctl(t *testing.T) {
	ctx := setupTest(t)
	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	c := d.NewClientT(t, client.WithAPIVersion("1.46"))
	defer c.Close()

	const scName = "net.ipv4.conf.eth0.forwarding"
	opts := []func(config *container.TestContainerConfig){
		container.WithCmd("sysctl", scName),
		container.WithSysctls(map[string]string{scName: "1"}),
	}

	runRes := container.RunAttach(ctx, t, c, opts...)
	defer c.ContainerRemove(ctx, runRes.ContainerID,
		client.ContainerRemoveOptions{Force: true},
	)

	stdout := runRes.Stdout.String()
	assert.Check(t, is.Contains(stdout, scName))
}

// With a read-only "/proc/sys/net" filesystem (simulated using env var
// DOCKER_TEST_RO_DISABLE_IPV6), check that if IPv6 can't be disabled on a
// container interface, container creation fails.
// Regression test for https://github.com/moby/moby/issues/47751
func TestReadOnlySlashProc(t *testing.T) {
	ctx := setupTest(t)

	testcases := []struct {
		name      string
		daemonEnv []string
		expErr    string
	}{
		{
			name: "Normality",
		},
		{
			name: "Read only",
			daemonEnv: []string{
				"DOCKER_TEST_RO_DISABLE_IPV6=1",
			},
			expErr: "failed to disable IPv6 on container's interface eth0",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)

			d := daemon.New(t, daemon.WithEnvVars(tc.daemonEnv...))
			d.StartWithBusybox(ctx, t)
			defer d.Stop(t)
			c := d.NewClientT(t)

			const net4Name = "testnet4"
			network.CreateNoError(ctx, t, c, net4Name)
			defer network.RemoveNoError(ctx, t, c, net4Name)
			id4 := container.Create(ctx, t, c,
				container.WithNetworkMode(net4Name),
				container.WithCmd("ls"),
			)
			defer c.ContainerRemove(ctx, id4, client.ContainerRemoveOptions{Force: true})
			_, err := c.ContainerStart(ctx, id4, client.ContainerStartOptions{})
			if tc.expErr == "" {
				assert.Check(t, err)
			} else {
				assert.Check(t, is.ErrorContains(err, tc.expErr))
			}

			// It should always be possible to create a container on an IPv6 network (IPv6
			// doesn't need to be disabled on the interface).
			const net6Name = "testnet6"
			network.CreateNoError(ctx, t, c, net6Name,
				network.WithIPv6(),
				network.WithIPAM("fd5c:15e3:0b62:5395::/64", "fd5c:15e3:0b62:5395::1"),
			)
			defer network.RemoveNoError(ctx, t, c, net6Name)
			id6 := container.Run(ctx, t, c,
				container.WithNetworkMode(net6Name),
				container.WithCmd("ls"),
			)
			defer c.ContainerRemove(ctx, id6, client.ContainerRemoveOptions{Force: true})
		})
	}
}

// Test that it's possible to set a sysctl on an interface in the container
// using DriverOpts.
func TestSetEndpointSysctl(t *testing.T) {
	ctx := setupTest(t)
	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	c := d.NewClientT(t)
	defer c.Close()

	const scName = "net.ipv4.conf.eth0.forwarding"
	for _, ifname := range []string{"IFNAME", "ifname"} {
		for _, val := range []string{"0", "1"} {
			t.Run("ifname="+ifname+"/val="+val, func(t *testing.T) {
				ctx := testutil.StartSpan(ctx, t)
				runRes := container.RunAttach(ctx, t, c,
					container.WithCmd("sysctl", "-qn", scName),
					container.WithEndpointSettings(networktypes.NetworkBridge, &networktypes.EndpointSettings{
						DriverOpts: map[string]string{
							netlabel.EndpointSysctls: "net.ipv4.conf." + ifname + ".forwarding=" + val,
						},
					}),
				)
				defer c.ContainerRemove(ctx, runRes.ContainerID, client.ContainerRemoveOptions{Force: true})

				stdout := runRes.Stdout.String()
				assert.Check(t, is.Equal(strings.TrimSpace(stdout), val))
			})
		}
	}
}

// TestContainerDisabledIPv6 checks that a container with IPv6 disabled does not
// get an IPv6 address when joining an IPv6 network. (TestDisableIPv6Addrs checks
// that no IPv6 addresses assigned to interfaces, this test checks that there are
// no IPv6 records in the DNS and that the IPv4 DNS response is correct.)
func TestContainerDisabledIPv6(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")

	ctx := setupTest(t)
	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	c := d.NewClientT(t)
	defer c.Close()

	const netName = "ipv6br"
	subnet6 := netip.MustParsePrefix("fd64:40cd:7fb4:8971::/64")
	network.CreateNoError(ctx, t, c, netName,
		network.WithDriver("bridge"),
		network.WithOption(bridge.BridgeName, netName),
		network.WithIPv6(),
		network.WithIPAM(subnet6.String(), "fd64:40cd:7fb4:8971::1"),
	)
	defer network.RemoveNoError(ctx, t, c, netName)

	// Run a container with IPv6 enabled.
	ctrWith6 := container.Run(ctx, t, c,
		container.WithNetworkMode(netName),
	)
	defer c.ContainerRemove(ctx, ctrWith6, client.ContainerRemoveOptions{Force: true})
	inspect := container.Inspect(ctx, t, c, ctrWith6)
	addr := inspect.NetworkSettings.Networks[netName].GlobalIPv6Address
	assert.Check(t, subnet6.Contains(addr))

	// Run a container with IPv6 disabled.
	const ctrNo6Name = "ctrNo6"
	ctrNo6 := container.Run(ctx, t, c,
		container.WithName(ctrNo6Name),
		container.WithNetworkMode(netName),
		container.WithSysctls(map[string]string{"net.ipv6.conf.all.disable_ipv6": "1"}),
	)
	defer c.ContainerRemove(ctx, ctrNo6, client.ContainerRemoveOptions{Force: true})
	inspect = container.Inspect(ctx, t, c, ctrNo6)
	addr = inspect.NetworkSettings.Networks[netName].GlobalIPv6Address
	assert.Check(t, !addr.IsValid())

	execCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Check that the with-IPv6 container can ping the other using its IPv4 address.
	res := container.ExecT(execCtx, t, c, ctrWith6, []string{"ping", "-4", "-c1", "-W3", ctrNo6Name})
	assert.Check(t, is.Equal(res.ExitCode, 0))
	assert.Check(t, is.Contains(res.Stdout(), "1 packets transmitted, 1 packets received"))
	assert.Check(t, is.Equal(res.Stderr(), ""))

	// Check that the with-IPv6 container doesn't find an IPv6 address for the other
	// (fail fast on the address lookup, rather than timing out on the ping).
	res = container.ExecT(execCtx, t, c, ctrWith6, []string{"ping", "-6", "-c1", "-W3", ctrNo6Name})
	assert.Check(t, is.Equal(res.ExitCode, 1))
	assert.Check(t, is.Equal(res.Stdout(), ""))
	assert.Check(t, is.Contains(res.Stderr(), "bad address"))
}

type expProxyCfg struct {
	proto      string
	hostIP     string
	hostPort   string
	ctrName    string
	ctrNetName string
	ctrIPv4    bool
	ctrPort    string
}

func TestGatewaySelection(t *testing.T) {
	skip.If(t, testEnv.IsRootless, "proxies run in child namespace")
	skip.If(t, !testEnv.IsKernelModuleLoadable("ipvlan"), "ipvlan kernel module is not loadable")

	ctx := setupTest(t)
	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()

	const netName4 = "net4"
	network.CreateNoError(ctx, t, c, netName4)
	defer network.RemoveNoError(ctx, t, c, netName4)

	const netName6 = "net6"
	netId6 := network.CreateNoError(ctx, t, c, netName6, network.WithIPv6(), network.WithIPv4(false))
	defer network.RemoveNoError(ctx, t, c, netName6)

	const netName46 = "net46"
	netId46 := network.CreateNoError(ctx, t, c, netName46, network.WithIPv6())
	defer network.RemoveNoError(ctx, t, c, netName46)

	master := "dm-dummy0"
	n.CreateMasterDummy(ctx, t, master)
	defer n.DeleteInterface(ctx, t, master)
	const netNameIpvlan6 = "ipvlan6"
	netIdIpvlan6 := network.CreateNoError(ctx, t, c, netNameIpvlan6,
		network.WithIPvlan("dm-dummy0", "l2"),
		network.WithIPv4(false),
		network.WithIPv6(),
	)
	defer network.RemoveNoError(ctx, t, c, netNameIpvlan6)

	const ctrName = "ctr"
	ctrId := container.Run(ctx, t, c,
		container.WithName(ctrName),
		container.WithNetworkMode(netName4),
		container.WithExposedPorts("80"),
		container.WithPortMap(networktypes.PortMap{networktypes.MustParsePort("80"): {{HostPort: "8080"}}}),
		container.WithCmd("httpd", "-f"),
	)
	defer c.ContainerRemove(ctx, ctrId, client.ContainerRemoveOptions{Force: true})

	// The container only has an IPv4 endpoint, it should be the gateway, and
	// the host-IPv6 should be proxied to container-IPv4.
	checkProxies(ctx, t, c, d.Pid(), []expProxyCfg{
		{"tcp", "0.0.0.0", "8080", ctrName, netName4, true, "80"},
		{"tcp", "::", "8080", ctrName, netName4, true, "80"},
	})

	// Connect the IPv6-only network. The IPv6 endpoint should become the
	// gateway for IPv6, the IPv4 endpoint should be reconfigured as the
	// gateway for IPv4 only.
	_, err := c.NetworkConnect(ctx, netId6, client.NetworkConnectOptions{
		Container: ctrId,
	})
	assert.NilError(t, err)
	checkProxies(ctx, t, c, d.Pid(), []expProxyCfg{
		{"tcp", "0.0.0.0", "8080", ctrName, netName4, true, "80"},
		{"tcp", "::", "8080", ctrName, netName6, false, "80"},
	})

	// Disconnect the IPv6-only network, the IPv4 should get back the mapping
	// from host-IPv6.
	_, err = c.NetworkDisconnect(ctx, netId6, client.NetworkDisconnectOptions{Container: ctrId, Force: false})
	assert.NilError(t, err)
	checkProxies(ctx, t, c, d.Pid(), []expProxyCfg{
		{"tcp", "0.0.0.0", "8080", ctrName, netName4, true, "80"},
		{"tcp", "::", "8080", ctrName, netName4, true, "80"},
	})

	// Connect the dual-stack network, it should become the gateway for v6 and v4.
	_, err = c.NetworkConnect(ctx, netId46, client.NetworkConnectOptions{
		Container: ctrId,
	})
	assert.NilError(t, err)
	checkProxies(ctx, t, c, d.Pid(), []expProxyCfg{
		{"tcp", "0.0.0.0", "8080", ctrName, netName46, true, "80"},
		{"tcp", "::", "8080", ctrName, netName46, false, "80"},
	})

	// Go back to the IPv4-only gateway, with proxy from host IPv6.
	_, err = c.NetworkDisconnect(ctx, netId46, client.NetworkDisconnectOptions{Container: ctrId, Force: false})
	assert.NilError(t, err)
	checkProxies(ctx, t, c, d.Pid(), []expProxyCfg{
		{"tcp", "0.0.0.0", "8080", ctrName, netName4, true, "80"},
		{"tcp", "::", "8080", ctrName, netName4, true, "80"},
	})

	// Connect the IPv6-only ipvlan network, its new Endpoint should become the IPv6
	// gateway, so the IPv4-only bridge is expected to drop its mapping from host IPv6.
	_, err = c.NetworkConnect(ctx, netIdIpvlan6, client.NetworkConnectOptions{
		Container: ctrId,
	})
	assert.NilError(t, err)
	checkProxies(ctx, t, c, d.Pid(), []expProxyCfg{
		{"tcp", "0.0.0.0", "8080", ctrName, netName4, true, "80"},
	})
}

func checkProxies(ctx context.Context, t *testing.T, c *client.Client, daemonPid int, exp []expProxyCfg) {
	t.Helper()
	makeExpStr := func(proto, hostIP, hostPort, ctrIP, ctrPort string) string {
		return fmt.Sprintf("%s:%s/%s <-> %s:%s", hostIP, hostPort, proto, ctrIP, ctrPort)
	}

	wantProxies := make([]string, 0, len(exp))
	for _, e := range exp {
		inspect := container.Inspect(ctx, t, c, e.ctrName)
		nw := inspect.NetworkSettings.Networks[e.ctrNetName]
		ctrIP := nw.GlobalIPv6Address
		if e.ctrIPv4 {
			ctrIP = nw.IPAddress
		}
		wantProxies = append(wantProxies, makeExpStr(e.proto, e.hostIP, e.hostPort, ctrIP.String(), e.ctrPort))
	}

	gotProxies := make([]string, 0, len(exp))
	res := icmd.RunCommand("ps", "-f", "--ppid", strconv.Itoa(daemonPid))
	if res.Error != nil {
		t.Error(res)
		return
	}
	for line := range strings.SplitSeq(res.Stdout(), "\n") {
		_, args, ok := strings.Cut(line, "docker-proxy")
		if !ok {
			continue
		}
		var proto, hostIP, hostPort, ctrIP, ctrPort string
		var useListenFd bool
		fs := flag.NewFlagSet("docker-proxy", flag.ContinueOnError)
		fs.StringVar(&proto, "proto", "", "Protocol")
		fs.StringVar(&hostIP, "host-ip", "", "Host IP")
		fs.StringVar(&hostPort, "host-port", "", "Host Port")
		fs.StringVar(&ctrIP, "container-ip", "", "Container IP")
		fs.StringVar(&ctrPort, "container-port", "", "Container Port")
		fs.BoolVar(&useListenFd, "use-listen-fd", false, "Use listen fd")
		fs.Parse(strings.Split(strings.TrimSpace(args), " "))
		gotProxies = append(gotProxies, makeExpStr(proto, hostIP, hostPort, ctrIP, ctrPort))
	}

	assert.Check(t, is.DeepEqual(gotProxies, wantProxies))
}

// Check that a gratuitous ARP / neighbour advertisement is sent for a new
// container's addresses.
// - start ctr1, ctr2
// - ping ctr2 from ctr1, ctr1's arp/neighbour caches learns ctr2's addresses.
// - restart ctr2 with the same IP addresses, it should get new random MAC addresses.
// - check that ctr1's arp/neighbour caches are updated.
func TestAdvertiseAddresses(t *testing.T) {
	skip.If(t, testEnv.IsRootless, "can't listen for ARP/NA messages in rootlesskit's namespace")

	ctx := setupTest(t)
	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()

	testcases := []struct {
		name            string
		netOpts         []func(*client.NetworkCreateOptions)
		ipv6LinkLocal   bool
		stopCtr2After   time.Duration
		expNetCreateErr string
		expNoMACUpdate  bool
		expNMsgs        int
		expInterval     time.Duration
	}{
		{
			name:        "defaults",
			expNMsgs:    3,
			expInterval: time.Second,
		},
		{
			name: "disable advertise addrs",
			netOpts: []func(*client.NetworkCreateOptions){
				network.WithOption(netlabel.AdvertiseAddrNMsgs, "0"),
			},
			expNoMACUpdate: true,
		},
		{
			name: "single message",
			netOpts: []func(*client.NetworkCreateOptions){
				network.WithOption(netlabel.AdvertiseAddrNMsgs, "1"),
			},
			expNMsgs: 1,
		},
		{
			name: "min interval",
			netOpts: []func(*client.NetworkCreateOptions){
				network.WithOption(netlabel.AdvertiseAddrIntervalMs, "100"),
			},
			expNMsgs:    3,
			expInterval: 100 * time.Millisecond,
		},
		{
			name: "cancel",
			netOpts: []func(*client.NetworkCreateOptions){
				network.WithOption(netlabel.AdvertiseAddrIntervalMs, "2000"),
			},
			stopCtr2After: 200 * time.Millisecond,
			expNMsgs:      1,
		},
		{
			name:          "ipv6 link local subnet",
			ipv6LinkLocal: true,
			expNMsgs:      3,
			expInterval:   time.Second,
		},
		{
			name: "interval too short",
			netOpts: []func(*client.NetworkCreateOptions){
				network.WithOption(netlabel.AdvertiseAddrIntervalMs, "99"),
			},
			expNetCreateErr: "Error response from daemon: com.docker.network.advertise_addr_ms must be in the range 100 to 2000",
		},
		{
			name: "interval too long",
			netOpts: []func(*client.NetworkCreateOptions){
				network.WithOption(netlabel.AdvertiseAddrIntervalMs, "2001"),
			},
			expNetCreateErr: "Error response from daemon: com.docker.network.advertise_addr_ms must be in the range 100 to 2000",
		},
		{
			name: "nonsense interval",
			netOpts: []func(*client.NetworkCreateOptions){
				network.WithOption(netlabel.AdvertiseAddrIntervalMs, "nonsense"),
			},
			expNetCreateErr: `Error response from daemon: value for option com.docker.network.advertise_addr_ms "nonsense" must be integer milliseconds`,
		},
		{
			name: "negative msg count",
			netOpts: []func(*client.NetworkCreateOptions){
				network.WithOption(netlabel.AdvertiseAddrNMsgs, "-1"),
			},
			expNetCreateErr: "Error response from daemon: com.docker.network.advertise_addr_nmsgs must be in the range 0 to 3",
		},
		{
			name: "too many msgs",
			netOpts: []func(*client.NetworkCreateOptions){
				network.WithOption(netlabel.AdvertiseAddrNMsgs, "4"),
			},
			expNetCreateErr: "Error response from daemon: com.docker.network.advertise_addr_nmsgs must be in the range 0 to 3",
		},
		{
			name: "nonsense msg count",
			netOpts: []func(*client.NetworkCreateOptions){
				network.WithOption(netlabel.AdvertiseAddrNMsgs, "nonsense"),
			},
			expNetCreateErr: `Error response from daemon: value for option com.docker.network.advertise_addr_nmsgs "nonsense" must be an integer`,
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)

			const netName = "dsnet"
			const brName = "br-advaddr"
			netOpts := append([]func(*client.NetworkCreateOptions){
				network.WithOption(bridge.BridgeName, brName),
				network.WithIPv6(),
				network.WithIPAM("172.22.22.0/24", "172.22.22.1"),
			}, tc.netOpts...)
			if tc.ipv6LinkLocal {
				netOpts = append(netOpts, network.WithIPAM("fe80:1240::/64", "fe80:1240::1"))
			} else {
				netOpts = append(netOpts, network.WithIPAM("fd3c:e70a:962c::/64", "fd3c:e70a:962c::1"))
			}
			_, err := network.Create(ctx, c, netName, netOpts...)
			if tc.expNetCreateErr != "" {
				assert.ErrorContains(t, err, tc.expNetCreateErr)
				return
			}
			defer network.RemoveNoError(ctx, t, c, netName)

			stopARPListen := network.CollectBcastARPs(t, brName)
			defer stopARPListen()
			stopICMP6Listen := network.CollectICMP6(t, brName)
			defer stopICMP6Listen()

			ctr1Id := container.Run(ctx, t, c, container.WithName("ctr1"), container.WithNetworkMode(netName))
			defer c.ContainerRemove(ctx, ctr1Id, client.ContainerRemoveOptions{Force: true})

			const ctr2Name = "ctr2"
			const ctr2Addr4 = "172.22.22.22"
			ctr2Addr6 := "fd3c:e70a:962c::2222"
			if tc.ipv6LinkLocal {
				ctr2Addr6 = "fe80:1240::2222"
			}
			ctr2Id := container.Run(ctx, t, c,
				container.WithName(ctr2Name),
				container.WithNetworkMode(netName),
				container.WithIPv4(netName, ctr2Addr4),
				container.WithIPv6(netName, ctr2Addr6),
			)
			// Defer a closure so the updated ctr2Id is used after the container's restarted.
			defer func() {
				if ctr2Id != "" {
					c.ContainerRemove(ctx, ctr2Id, client.ContainerRemoveOptions{Force: true})
				}
			}()

			ctr2OrigMAC := container.Inspect(ctx, t, c, ctr2Id).NetworkSettings.Networks[netName].MacAddress

			// Ping from ctr1 to ctr2 using both IPv4 and IPv6, to populate ctr1's arp/neighbour caches.
			pingRes := container.ExecT(ctx, t, c, ctr1Id, []string{"ping", "-4", "-c1", ctr2Name})
			assert.Assert(t, is.Equal(pingRes.ExitCode, 0))
			pingRes = container.ExecT(ctx, t, c, ctr1Id, []string{"ping", "-6", "-c1", ctr2Name})
			assert.Assert(t, is.Equal(pingRes.ExitCode, 0))

			// Search the output from "ip neigh show" for entries for ip, return
			// the associated MAC address.
			findNeighMAC := func(neighOut, ip string) string {
				t.Helper()
				for line := range strings.SplitSeq(neighOut, "\n") {
					// Lines look like ...
					// 172.22.22.22 dev eth0 lladdr 36:bc:ce:67:f3:e4 ref 1 used 0/7/0 probes 1 DELAY
					fields := strings.Fields(line)
					if len(fields) >= 5 && fields[0] == ip {
						return fields[4]
					}
				}
				t.Fatalf("No entry for %s in '%s'", ip, neighOut)
				return ""
			}

			// ctr1 should now have arp/neighbour entries for ctr2
			ctr1Neighs := container.ExecT(ctx, t, c, ctr1Id, []string{"ip", "neigh", "show"})
			assert.Assert(t, is.Equal(ctr1Neighs.ExitCode, 0))
			t.Logf("ctr1 initial neighbours:\n%s", ctr1Neighs.Combined())
			macBefore := findNeighMAC(ctr1Neighs.Stdout(), ctr2Addr4)
			assert.Equal(t, macBefore, findNeighMAC(ctr1Neighs.Stdout(), ctr2Addr6))

			// Stop ctr2, start a new container with the same addresses.
			c.ContainerRemove(ctx, ctr2Id, client.ContainerRemoveOptions{Force: true})
			ctr1Neighs = container.ExecT(ctx, t, c, ctr1Id, []string{"ip", "neigh", "show"})
			assert.Assert(t, is.Equal(ctr1Neighs.ExitCode, 0))
			t.Logf("ctr1 neighbours after ctr2 stop:\n%s", ctr1Neighs.Combined())
			ctr2Id = container.Run(ctx, t, c,
				container.WithName(ctr2Name),
				container.WithNetworkMode(netName),
				container.WithIPv4(netName, ctr2Addr4),
				container.WithIPv6(netName, ctr2Addr6),
			)
			// The original defer will stop ctr2Id.

			ctr2NewMAC := container.Inspect(ctx, t, c, ctr2Id).NetworkSettings.Networks[netName].MacAddress
			assert.Check(t, !slices.Equal(ctr2OrigMAC, ctr2NewMAC), "expected restarted ctr2 to have a different MAC address")

			ctr1Neighs = container.ExecT(ctx, t, c, ctr1Id, []string{"ip", "neigh", "show"})
			assert.Assert(t, is.Equal(ctr1Neighs.ExitCode, 0))
			t.Logf("ctr1 neighbours after ctr2 restart:\n%s", ctr1Neighs.Combined())
			macAfter := findNeighMAC(ctr1Neighs.Stdout(), ctr2Addr4)
			assert.Check(t, is.Equal(macAfter, findNeighMAC(ctr1Neighs.Stdout(), ctr2Addr6)))
			if tc.expNoMACUpdate {
				// The neighbour table shouldn't have changed.
				assert.Check(t, macBefore == macAfter, "Expected ctr1's ARP/ND cache not to have updated")
			} else {
				// The new ctr2's interface should have a new random MAC address, and ctr1's
				// arp/neigh caches should have been updated by ctr2's gratuitous ARP/NA.
				assert.Check(t, macBefore != macAfter, "Expected ctr1's ARP/ND cache to have updated")
			}

			if tc.stopCtr2After > 0 {
				time.Sleep(tc.stopCtr2After)
				c.ContainerRemove(ctx, ctr2Id, client.ContainerRemoveOptions{Force: true})
				ctr2Id = ""
			}

			t.Log("Sleeping for 5s to collect ARP/NA messages...")
			time.Sleep(5 * time.Second)

			// Check ARP/NA messages received for ctr2's new address (all unsolicited).

			checkPkts := func(pktDesc string, pkts []network.TimestampedPkt, matchIP netip.Addr, unpack func(pkt network.TimestampedPkt) (sh net.HardwareAddr, sp netip.Addr, err error)) {
				t.Helper()
				var count int
				var lastTimestamp time.Time

				// Find the packets of-interest, and check the intervals between them.
				for i, p := range pkts {
					ha, pa, err := unpack(p)
					if err != nil {
						t.Logf("%s %d: %s: %s: %s",
							pktDesc, i+1, p.ReceivedAt.Format("15:04:05.000"), hex.EncodeToString(p.Data), err)
						continue
					}
					t.Logf("%s %d: %s '%s' is at '%s'", pktDesc, i+1, p.ReceivedAt.Format("15:04:05.000"), pa, ha)
					if pa != matchIP || slices.Compare(ha, net.HardwareAddr(ctr2NewMAC)) != 0 {
						continue
					}
					count++
					var interval time.Duration
					if !lastTimestamp.IsZero() {
						interval = p.ReceivedAt.Sub(lastTimestamp)
						// For test pass/fail, arbitrary limit on how early or late ARP/NA messages can be.
						// They should never be sent early, but if there's a delay in receiving one packet
						// the interval to the next may be shorted than the configured interval.
						// Send variance should be a lot less than this but, this is enough to check that
						// the interval is configurable, while (hopefully) avoiding flakiness on a busy host ...
						const okIntervalDelta = 100 * time.Millisecond
						assert.Check(t, time.Duration(math.Abs(float64(interval-tc.expInterval))) < okIntervalDelta,
							"interval %s is expected to be within %s of configured interval %s",
							interval, okIntervalDelta, tc.expInterval)
					}
					t.Logf("---> found %s %d, interval:%s", pktDesc, count, interval)
					lastTimestamp = p.ReceivedAt
				}

				assert.Check(t, is.Equal(count, tc.expNMsgs), pktDesc+" message count")
			}

			arps := stopARPListen()
			checkPkts("ARP", arps, netip.MustParseAddr(ctr2Addr4), network.UnpackUnsolARP)

			icmps := stopICMP6Listen()
			checkPkts("ICMP6", icmps, netip.MustParseAddr(ctr2Addr6), network.UnpackUnsolNA)
			if t.Failed() {
				d.TailLogsT(t, 100)
			}
		})
	}
}

// TestAdvertiseAddressesLiveRestore verifies that unsolicited ARP/NA messages are
// sent when the daemon restarts with live-restore enabled. This ensures that
// neighbor caches on other hosts are updated with the container's MAC address
// after a daemon restart.
func TestAdvertiseAddressesLiveRestore(t *testing.T) {
	skip.If(t, testEnv.IsRootless, "can't listen for ARP/NA messages in rootlesskit's namespace")

	ctx := setupTest(t)
	d := daemon.New(t)
	d.StartWithBusybox(ctx, t, "--live-restore")
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()

	const netName = "dsnet-lr"
	const brName = "br-advaddrlr"
	network.CreateNoError(ctx, t, c, netName,
		network.WithOption(bridge.BridgeName, brName),
		network.WithIPv6(),
		network.WithIPAM("172.23.23.0/24", "172.23.23.1"),
		network.WithIPAM("fd4c:f70b:973d::/64", "fd4c:f70b:973d::1"),
	)
	defer network.RemoveNoError(ctx, t, c, netName)

	// Create ctr1 which will be used to verify neighbor cache updates.
	ctr1Id := container.Run(ctx, t, c, container.WithName("ctr1-lr"), container.WithNetworkMode(netName))
	defer c.ContainerRemove(ctx, ctr1Id, client.ContainerRemoveOptions{Force: true})

	// Create ctr2 with fixed IP addresses.
	const ctr2Name = "ctr2-lr"
	const ctr2Addr4 = "172.23.23.22"
	const ctr2Addr6 = "fd4c:f70b:973d::2222"
	ctr2Id := container.Run(ctx, t, c,
		container.WithName(ctr2Name),
		container.WithNetworkMode(netName),
		container.WithIPv4(netName, ctr2Addr4),
		container.WithIPv6(netName, ctr2Addr6),
	)
	defer c.ContainerRemove(ctx, ctr2Id, client.ContainerRemoveOptions{Force: true})

	ctr2MAC := container.Inspect(ctx, t, c, ctr2Id).NetworkSettings.Networks[netName].MacAddress

	// Ping from ctr1 to ctr2 to populate ctr1's neighbor caches.
	pingRes := container.ExecT(ctx, t, c, ctr1Id, []string{"ping", "-4", "-c1", ctr2Name})
	assert.Assert(t, is.Equal(pingRes.ExitCode, 0))
	pingRes = container.ExecT(ctx, t, c, ctr1Id, []string{"ping", "-6", "-c1", ctr2Name})
	assert.Assert(t, is.Equal(pingRes.ExitCode, 0))

	// Verify ctr1 has neighbor entries for ctr2.
	ctr1Neighs := container.ExecT(ctx, t, c, ctr1Id, []string{"ip", "neigh", "show"})
	assert.Assert(t, is.Equal(ctr1Neighs.ExitCode, 0))
	t.Logf("ctr1 neighbours before restart:\n%s", ctr1Neighs.Combined())

	// Wait for initial ARP/NA retransmits from container creation to settle.
	// The daemon sends unsolicited ARP/NA messages for a couple of seconds after
	// AddInterface, so we need to wait before starting to listen to avoid counting
	// those messages instead of the ones sent during restore.
	t.Log("Waiting for initial ARP/NA retransmits to settle...")
	time.Sleep(5 * time.Second)

	// Now start listening for ARP/NA messages.
	stopARPListen := network.CollectBcastARPs(t, brName)
	defer stopARPListen()
	stopICMP6Listen := network.CollectICMP6(t, brName)
	defer stopICMP6Listen()

	// Restart the daemon - this should trigger RestoreInterfaces which sends ARP/NA.
	d.Restart(t, "--live-restore")

	// Give time for ARP/NA messages to be sent after restart.
	t.Log("Sleeping for 5s to collect ARP/NA messages after daemon restart...")
	time.Sleep(5 * time.Second)

	// Verify that ARP/NA messages were sent for ctr2's addresses.
	arps := stopARPListen()
	var arpCount int
	for i, p := range arps {
		ha, pa, err := network.UnpackUnsolARP(p)
		if err != nil {
			t.Logf("ARP %d: %s: %s: %s", i+1, p.ReceivedAt.Format("15:04:05.000"), hex.EncodeToString(p.Data), err)
			continue
		}
		t.Logf("ARP %d: %s '%s' is at '%s'", i+1, p.ReceivedAt.Format("15:04:05.000"), pa, ha)
		if pa == netip.MustParseAddr(ctr2Addr4) && slices.Compare(ha, net.HardwareAddr(ctr2MAC)) == 0 {
			arpCount++
			t.Logf("---> found ARP for ctr2")
		}
	}
	assert.Check(t, arpCount >= 1, "expected at least 1 ARP message for ctr2 after live-restore, got %d", arpCount)

	icmps := stopICMP6Listen()
	var naCount int
	for i, p := range icmps {
		ha, pa, err := network.UnpackUnsolNA(p)
		if err != nil {
			t.Logf("ICMP6 %d: %s: %s: %s", i+1, p.ReceivedAt.Format("15:04:05.000"), hex.EncodeToString(p.Data), err)
			continue
		}
		t.Logf("ICMP6 %d: %s '%s' is at '%s'", i+1, p.ReceivedAt.Format("15:04:05.000"), pa, ha)
		if pa == netip.MustParseAddr(ctr2Addr6) && slices.Compare(ha, net.HardwareAddr(ctr2MAC)) == 0 {
			naCount++
			t.Logf("---> found NA for ctr2")
		}
	}
	assert.Check(t, naCount >= 1, "expected at least 1 NA message for ctr2 after live-restore, got %d", naCount)

	// Verify ctr1 still has valid neighbor entries (connectivity should work).
	ctr1Neighs = container.ExecT(ctx, t, c, ctr1Id, []string{"ip", "neigh", "show"})
	assert.Assert(t, is.Equal(ctr1Neighs.ExitCode, 0))
	t.Logf("ctr1 neighbours after restart:\n%s", ctr1Neighs.Combined())

	// Verify connectivity still works after restart.
	pingRes = container.ExecT(ctx, t, c, ctr1Id, []string{"ping", "-4", "-c1", ctr2Name})
	assert.Assert(t, is.Equal(pingRes.ExitCode, 0))
	pingRes = container.ExecT(ctx, t, c, ctr1Id, []string{"ping", "-6", "-c1", ctr2Name})
	assert.Assert(t, is.Equal(pingRes.ExitCode, 0))

	if t.Failed() {
		d.TailLogsT(t, 100)
	}
}

// TestNetworkInspectGateway checks that gateways reported in inspect output are parseable as addresses.
func TestNetworkInspectGateway(t *testing.T) {
	ctx := setupTest(t)
	c := testEnv.APIClient()

	const netName = "test-inspgw"
	nid, err := network.Create(ctx, c, netName, network.WithIPv6())
	assert.NilError(t, err)
	defer network.RemoveNoError(ctx, t, c, netName)

	res, err := c.NetworkInspect(ctx, nid, client.NetworkInspectOptions{})
	assert.NilError(t, err)
	for _, ipamCfg := range res.Network.IPAM.Config {
		assert.Check(t, ipamCfg.Gateway.IsValid())
	}
}

// TestDropInForwardChain checks that a DROP rule appended to the filter-FORWARD chain
// by some other application is processed after docker's rules (so, it doesn't break docker's
// networking).
// Regression test for https://github.com/moby/moby/pull/49518
func TestDropInForwardChain(t *testing.T) {
	skip.If(t, networking.FirewalldRunning(), "can't use firewalld in host netns to add rules in L3Segment")
	skip.If(t, testEnv.IsRootless, "rootless has its own netns")
	skip.If(t, !strings.Contains(testEnv.FirewallBackendDriver(), "iptables"),
		"test is iptables specific, and iptables isn't in use")

	// Run the test in its own netns, to avoid interfering with iptables on the test host.
	const l3SegHost = "difc"
	l3 := networking.NewL3Segment(t, "test-"+l3SegHost)
	defer l3.Destroy(t)
	hostAddrs := []netip.Prefix{
		netip.MustParsePrefix("192.168.111.222/24"),
		netip.MustParsePrefix("fdeb:6de4:e407::111/64"),
	}
	l3.AddHost(t, l3SegHost, "ns-"+l3SegHost, "eth0", hostAddrs...)

	// Insert DROP rules at the end of the FORWARD chain. If these end up out-of-order, packets
	// will be dropped before Docker's rules can accept them.
	l3.Hosts[l3SegHost].Do(t, func() {
		dropRule := []string{"-A", "FORWARD", "-j", "DROP", "-m", "comment", "--comment", "test drop rule"}
		out, err := iptables.GetIptable(iptables.IPv4).Raw(dropRule...)
		assert.NilError(t, err, "adding drop rule: %s", out)
		out, err = iptables.GetIptable(iptables.IPv6).Raw(dropRule...)
		assert.NilError(t, err, "adding drop rule: %s", out)

		// Run without OTEL because there's no routing from this netns for it - which
		// means the daemon doesn't shut down cleanly, causing the test to fail.
		ctx := setupTest(t)
		d := daemon.New(t, daemon.WithEnvVars("OTEL_EXPORTER_OTLP_ENDPOINT="))
		// Disable docker-proxy, so the iptables rules aren't bypassed.
		d.StartWithBusybox(ctx, t, "--userland-proxy=false")
		defer d.Stop(t)
		c := d.NewClientT(t)
		defer c.Close()

		const netName46 = "net46"
		_ = network.CreateNoError(ctx, t, c, netName46, network.WithIPv6())
		defer network.RemoveNoError(ctx, t, c, netName46)

		// Start an http server.
		const hostPort = "8080"
		ctrId := container.Run(ctx, t, c,
			container.WithNetworkMode(netName46),
			container.WithExposedPorts("80"),
			container.WithPortMap(networktypes.PortMap{networktypes.MustParsePort("80"): {{HostPort: hostPort}}}),
			container.WithCmd("httpd", "-f"),
		)
		defer c.ContainerRemove(ctx, ctrId, client.ContainerRemoveOptions{Force: true})

		// Make an HTTP request from a new container, via the published port on the host addresses.
		// Expect a "404", not a timeout due to packets dropped by the FORWARD chain's extra rule.
		for _, ha := range hostAddrs {
			url := "http://" + net.JoinHostPort(ha.Addr().String(), hostPort)
			res := container.RunAttach(ctx, t, c,
				container.WithNetworkMode(netName46),
				container.WithCmd("wget", "-T3", url),
			)
			assert.Check(t, is.Contains(res.Stderr.String(), "404 Not Found"), "URL: %s", url)
		}
	})
}

// TestLegacyLinksEnvVars verify that legacy links environment variables are set in containers when the daemon is
// started with DOCKER_KEEP_DEPRECATED_LEGACY_LINKS_ENV_VARS=1, and are skipped when the daemon is started without that
// environment variable.
func TestLegacyLinksEnvVars(t *testing.T) {
	for _, tc := range []struct {
		name          string
		expectEnvVars bool
	}{
		{"with legacy links env vars", true},
		{"without legacy links env vars", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dEnv []string
			if tc.expectEnvVars {
				dEnv = []string{"DOCKER_KEEP_DEPRECATED_LEGACY_LINKS_ENV_VARS=1"}
			}

			ctx := setupTest(t)
			d := daemon.New(t, daemon.WithEnvVars(dEnv...))
			d.StartWithBusybox(ctx, t)
			defer d.Stop(t)
			c := d.NewClientT(t)
			defer c.Close()

			ctr1 := container.Run(ctx, t, c,
				container.WithName("ctr1"),
				container.WithCmd("httpd", "-f"))
			defer c.ContainerRemove(ctx, ctr1, client.ContainerRemoveOptions{Force: true})

			exportRes := container.RunAttach(ctx, t, c,
				container.WithName("ctr2"),
				container.WithLinks("ctr1"),
				container.WithCmd("/bin/sh", "-c", "export"),
				container.WithAutoRemove)

			// Check the list of environment variables set in the linking container.
			var found bool
			for l := range strings.SplitSeq(exportRes.Stdout.String(), "\n") {
				if strings.HasPrefix(l, "export CTR1_") {
					// Legacy links env var found, but not expected.
					if !tc.expectEnvVars {
						t.Fatalf("unexpected env var %q", l)
					}

					// Legacy links env var found, and expected. No need to check further.
					found = true
					break
				}
			}

			if !found && tc.expectEnvVars {
				t.Fatal("no legacy links env vars found")
			}
		})
	}
}

// TestDNSNamesForNonSwarmScopedNetworks checks that container names can be resolved for non-swarm-scoped networks once
// a node has joined a Swarm cluster.
//
// Regression test for https://github.com/moby/moby/issues/51491.
func TestDNSNamesForNonSwarmScopedNetworks(t *testing.T) {
	ctx := setupTest(t)

	d := daemon.New(t)
	d.StartAndSwarmInit(ctx, t)
	defer d.Stop(t)

	c := d.NewClientT(t)
	defer c.Close()

	const bridgeName = "dnsnames-with-swarm"
	network.CreateNoError(ctx, t, c, bridgeName)
	defer network.RemoveNoError(ctx, t, c, bridgeName)

	res := container.RunAttach(ctx, t, c,
		container.WithName("test"),
		container.WithCmd("nslookup", "-type=a", "test."),
		container.WithNetworkMode(bridgeName),
		container.WithAutoRemove)
	assert.Equal(t, res.ExitCode, 0, "exit code: %d, expected 0; stdout:\n%s", res.ExitCode, res.Stdout)
}

// Check that when a network is created with no --subnet, a container can be
// started with a --ip in the subnet allocated from the default pools.
//
// Regression test for https://github.com/moby/moby/issues/51569
func TestSetIPWithNoConfiguredSubnet(t *testing.T) {
	ctx := setupTest(t)
	c := testEnv.APIClient()

	const bridgeName = "subnet-from-pools"
	network.CreateNoError(ctx, t, c, bridgeName, network.WithIPv6())
	defer network.RemoveNoError(ctx, t, c, bridgeName)

	insp := network.InspectNoError(ctx, t, c, bridgeName, client.NetworkInspectOptions{})
	assert.Assert(t, is.Len(insp.Network.IPAM.Config, 2))
	ip4 := insp.Network.IPAM.Config[0].Subnet.Addr().Next().Next().String()
	ip6 := insp.Network.IPAM.Config[1].Subnet.Addr().Next().Next().String()
	if insp.Network.IPAM.Config[0].Subnet.Addr().Is6() {
		ip4, ip6 = ip6, ip4
	}

	res := container.RunAttach(ctx, t, c,
		container.WithCmd("ip", "addr", "show", "eth0"),
		container.WithNetworkMode(bridgeName),
		container.WithIPv4(bridgeName, ip4),
		container.WithIPv6(bridgeName, ip6),
	)
	if assert.Check(t, is.Equal(res.ExitCode, 0)) {
		assert.Check(t, is.Contains(res.Stdout.String(), ip4))
		assert.Check(t, is.Contains(res.Stdout.String(), ip6))
	}
}

// Regression test for https://github.com/moby/moby/issues/51578
func TestGatewayErrorOnNetDisconnect(t *testing.T) {
	ctx := setupTest(t)
	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)
	c := d.NewClientT(t)

	network.CreateNoError(ctx, t, c, "n1")
	defer network.RemoveNoError(ctx, t, c, "n1")
	network.CreateNoError(ctx, t, c, "n2")
	defer network.RemoveNoError(ctx, t, c, "n2")

	// Run a container attached to both networks, with n1 providing the default gateway
	// and n2's interface named "eth2".
	ctrID := container.Run(ctx, t, c,
		container.WithEndpointSettings("n1", &networktypes.EndpointSettings{GwPriority: 1}),
		container.WithEndpointSettings("n2", &networktypes.EndpointSettings{DriverOpts: map[string]string{
			netlabel.Ifname: "eth2",
		}}),
		container.WithCapability("NET_ADMIN"),
	)
	defer container.Remove(ctx, t, c, ctrID, client.ContainerRemoveOptions{Force: true})

	// Break n2 so it can't be used as a gateway (there will be no route).
	execRes := container.ExecT(ctx, t, c, ctrID, []string{"ip", "link", "set", "eth2", "down"})
	assert.Assert(t, is.Equal(execRes.ExitCode, 0))

	// Disconnect n1, n2 will be selected as the gateway and its config will fail.
	// The error is only logged and the disconnect proceeds.
	_, err := c.NetworkDisconnect(ctx, "n1", client.NetworkDisconnectOptions{Container: ctrID})
	assert.Check(t, err)

	// Check n1 can be reconnected.
	_, err = c.NetworkConnect(ctx, "n1", client.NetworkConnectOptions{Container: ctrID})
	assert.Check(t, err)

	// Check the container can be restarted.
	timeout := 0
	_, err = c.ContainerRestart(ctx, ctrID, client.ContainerRestartOptions{Timeout: &timeout})
	assert.Check(t, err)

	// Both networks should be attached.
	ctrInsp := container.Inspect(ctx, t, c, ctrID)
	assert.Check(t, is.Len(ctrInsp.NetworkSettings.Networks, 2))
	assert.Check(t, is.Contains(ctrInsp.NetworkSettings.Networks, "n1"))
	assert.Check(t, is.Contains(ctrInsp.NetworkSettings.Networks, "n2"))
}

// Regression test for https://github.com/moby/moby/issues/51620
func TestPublishAllWithNilPortBindings(t *testing.T) {
	ctx := setupTest(t)
	c := testEnv.APIClient()

	ctrID := container.Create(ctx, t, c,
		container.WithImage("busybox:latest"),
		container.WithExposedPorts("80/tcp"),
		container.WithPublishAllPorts(true),
	)
	container.Remove(ctx, t, c, ctrID, client.ContainerRemoveOptions{Force: true})
}
