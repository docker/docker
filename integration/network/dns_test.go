package network

import (
	"net/netip"
	"strings"
	"testing"

	networktypes "github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/integration/internal/network"
	"github.com/moby/moby/v2/internal/sliceutil"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

func TestDaemonDNSFallback(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon, "cannot start daemon on remote test run")
	skip.If(t, testEnv.DaemonInfo.OSType != "linux")
	skip.If(t, testEnv.IsUserNamespace)
	ctx := setupTest(t)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t, "-b", "none", "--dns", "127.127.127.1", "--dns", "8.8.8.8")
	defer d.Stop(t)

	c := d.NewClientT(t)

	network.CreateNoError(ctx, t, c, "test")
	defer c.NetworkRemove(ctx, "test", client.NetworkRemoveOptions{})

	cid := container.Run(ctx, t, c, container.WithNetworkMode("test"), container.WithCmd("nslookup", "docker.com"))
	defer c.ContainerRemove(ctx, cid, client.ContainerRemoveOptions{Force: true})

	poll.WaitOn(t, container.IsSuccessful(ctx, c, cid))
}

// Check that, when the internal DNS server's address is supplied as an external
// DNS server, the daemon doesn't start talking to itself.
func TestIntDNSAsExtDNS(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "cannot start daemon on Windows test run")
	skip.If(t, testEnv.IsRemoteDaemon, "cannot start daemon on remote test run")

	ctx := setupTest(t)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	c := d.NewClientT(t)
	defer c.Close()

	testcases := []struct {
		name        string
		extServers  []string
		expExitCode int
		expStdout   string
	}{
		{
			name:        "only self",
			extServers:  []string{"127.0.0.11"},
			expExitCode: 1,
			expStdout:   "SERVFAIL",
		},
		{
			name:        "self then ext",
			extServers:  []string{"127.0.0.11", "8.8.8.8"},
			expExitCode: 0,
			expStdout:   "Non-authoritative answer",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)

			const netName = "testnet"
			network.CreateNoError(ctx, t, c, netName)
			defer network.RemoveNoError(ctx, t, c, netName)

			res := container.RunAttach(ctx, t, c,
				container.WithNetworkMode(netName),
				container.WithDNS(sliceutil.Map(tc.extServers, netip.MustParseAddr)),
				container.WithCmd("nslookup", "docker.com"),
			)
			defer c.ContainerRemove(ctx, res.ContainerID, client.ContainerRemoveOptions{Force: true})

			assert.Check(t, is.Equal(res.ExitCode, tc.expExitCode))
			assert.Check(t, is.Contains(res.Stdout.String(), tc.expStdout))
		})
	}
}

// TestCrossNetworkInternalDomain checks that a container can resolve a
// container in another user-defined network through the
// "<container>.<network>.moby.internal" pseudo-domain, but only when that
// container has a published port reachable at its own address.
// See https://github.com/moby/moby/issues/49572.
func TestCrossNetworkInternalDomain(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "test only works on linux")
	skip.If(t, testEnv.IsRemoteDaemon, "cannot start daemon on remote test run")

	ctx := setupTest(t)

	d := daemon.New(t)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	c := d.NewClientT(t)
	defer c.Close()

	const (
		netA      = "x49572_netA"
		netRouted = "x49572_routed"
		netNAT    = "x49572_nat"
		c1        = "x49572_c1"
		cRouted   = "x49572_c_routed"
		cNoPort   = "x49572_c_noport"
		cNAT      = "x49572_c_nat"
		isolated  = "x49572_isolated"
		isolated2 = "x49572_isolated_target"
		// Docker treats these as two networks; DNS can't tell them apart.
		netDup      = "x49572_dup"
		netDupUpper = "x49572_Dup"
		cDup        = "x49572_c_dup"
	)
	publish80 := []func(*container.TestContainerConfig){
		container.WithExposedPorts("80/tcp"),
		container.WithPortMap(networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {{HostIP: netip.MustParseAddr("0.0.0.0")}}}),
	}

	network.CreateNoError(ctx, t, c, netA)
	defer network.RemoveNoError(ctx, t, c, netA)
	network.CreateNoError(ctx, t, c, netRouted, network.WithOption(bridge.IPv4GatewayMode, "routed"))
	defer network.RemoveNoError(ctx, t, c, netRouted)
	network.CreateNoError(ctx, t, c, netNAT)
	defer network.RemoveNoError(ctx, t, c, netNAT)
	network.CreateNoError(ctx, t, c, isolated, network.WithInternal(), network.WithOption(bridge.IPv4GatewayMode, "routed"))
	defer network.RemoveNoError(ctx, t, c, isolated)
	network.CreateNoError(ctx, t, c, netDup, network.WithOption(bridge.IPv4GatewayMode, "routed"))
	defer network.RemoveNoError(ctx, t, c, netDup)
	network.CreateNoError(ctx, t, c, netDupUpper, network.WithOption(bridge.IPv4GatewayMode, "routed"))
	defer network.RemoveNoError(ctx, t, c, netDupUpper)

	c1Id := container.Run(ctx, t, c,
		container.WithName(c1),
		container.WithNetworkMode(netA),
	)
	defer c.ContainerRemove(ctx, c1Id, client.ContainerRemoveOptions{Force: true})

	cRoutedId := container.Run(ctx, t, c, append(publish80,
		container.WithName(cRouted),
		container.WithNetworkMode(netRouted),
	)...)
	defer c.ContainerRemove(ctx, cRoutedId, client.ContainerRemoveOptions{Force: true})
	cRoutedIP := container.Inspect(ctx, t, c, cRoutedId).NetworkSettings.Networks[netRouted].IPAddress.String()

	cNoPortId := container.Run(ctx, t, c,
		container.WithName(cNoPort),
		container.WithNetworkMode(netRouted),
	)
	defer c.ContainerRemove(ctx, cNoPortId, client.ContainerRemoveOptions{Force: true})

	cNATId := container.Run(ctx, t, c, append(publish80,
		container.WithName(cNAT),
		container.WithNetworkMode(netNAT),
	)...)
	defer c.ContainerRemove(ctx, cNATId, client.ContainerRemoveOptions{Force: true})

	isoId := container.Run(ctx, t, c, append(publish80,
		container.WithName(isolated2),
		container.WithNetworkMode(isolated),
	)...)
	defer c.ContainerRemove(ctx, isoId, client.ContainerRemoveOptions{Force: true})

	cDupId := container.Run(ctx, t, c, append(publish80,
		container.WithName(cDup),
		container.WithNetworkMode(netDup),
	)...)
	defer c.ContainerRemove(ctx, cDupId, client.ContainerRemoveOptions{Force: true})

	tests := []struct {
		name       string
		query      string
		qtype      string // "" -> "a"
		wantIP     string // empty -> expect the lookup to fail, unless wantNoData
		wantNoData bool   // the name exists but has no record of this type
	}{
		{
			name:   "routed network resolves",
			query:  cRouted + "." + netRouted + ".moby.internal",
			wantIP: cRoutedIP,
		},
		{
			name:   "name is case-insensitive",
			query:  strings.ToUpper(cRouted + "." + netRouted + ".moby.internal"),
			wantIP: cRoutedIP,
		},
		{
			name:  "networks differing only in case are ambiguous",
			query: cDup + "." + netDup + ".moby.internal",
		},
		{
			// The name exists, so the query must not be forwarded to
			// the external resolver, which would make nslookup fail.
			name:       "AAAA for an IPv4-only target is NODATA",
			query:      cRouted + "." + netRouted + ".moby.internal",
			qtype:      "aaaa",
			wantNoData: true,
		},
		{
			name:  "routed network, no published port",
			query: cNoPort + "." + netRouted + ".moby.internal",
		},
		{
			name:  "nat network is excluded",
			query: cNAT + "." + netNAT + ".moby.internal",
		},
		{
			name:  "internal network is excluded",
			query: isolated2 + "." + isolated + ".moby.internal",
		},
		{
			name:  "unknown network",
			query: cRouted + ".does-not-exist.moby.internal",
		},
		{
			name:  "plain .internal is not a pseudo-domain",
			query: cRouted + "." + netRouted + ".internal",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Ask for one record type: busybox nslookup's default A+AAAA lookup
			// prints "No answer" and exits 0 whether or not the name resolves.
			qtype := tc.qtype
			if qtype == "" {
				qtype = "a"
			}
			res, err := container.Exec(ctx, c, c1Id, []string{"nslookup", "-type=" + qtype, tc.query})
			assert.NilError(t, err)
			switch {
			case tc.wantNoData:
				// On NODATA, nslookup succeeds with an empty answer section.
				// On NXDOMAIN, or if the query was forwarded, it fails.
				assert.Check(t, is.Equal(res.ExitCode, 0), "stdout=%q stderr=%q", res.Stdout(), res.Stderr())
				assert.Check(t, !strings.Contains(res.Stdout(), cRoutedIP), "unexpected address in stdout=%q", res.Stdout())
			case tc.wantIP != "":
				assert.Check(t, is.Equal(res.ExitCode, 0), "stdout=%q stderr=%q", res.Stdout(), res.Stderr())
				assert.Check(t, is.Contains(res.Stdout(), tc.wantIP))
			default:
				assert.Check(t, res.ExitCode != 0, "expected nslookup to fail, stdout=%q", res.Stdout())
			}
		})
	}
}

// TestExtDNSInIPv6OnlyNw checks that an IPv6-only bridge network has external
// DNS access.
func TestExtDNSInIPv6OnlyNw(t *testing.T) {
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "No IPv6 Windows")
	skip.If(t, testEnv.IsRootless, "Can't use resolver on host in rootless mode")
	ctx := setupTest(t)

	// Start a DNS server on the loopback interface.
	network.StartDaftDNS(t, "127.0.0.1")

	// Set up a temp resolv.conf pointing at that DNS server, and a daemon using it.
	d := daemon.New(t,
		daemon.WithResolvConf(network.GenResolvConf("127.0.0.1")),
	)
	d.StartWithBusybox(ctx, t)
	defer d.Stop(t)

	c := d.NewClientT(t)
	defer c.Close()

	netName := "v6only"
	network.CreateNoError(ctx, t, c, netName,
		network.WithDriver("bridge"),
		network.WithIPv4(false),
		network.WithIPv6(),
	)
	defer network.RemoveNoError(ctx, t, c, netName)

	ctrId := container.Run(ctx, t, c, container.WithNetworkMode(netName))
	defer c.ContainerRemove(ctx, ctrId, client.ContainerRemoveOptions{Force: true})

	res, err := container.Exec(ctx, c, ctrId, []string{"nslookup", "test.example"})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(res.ExitCode, 0))
	assert.Check(t, is.Contains(res.Stdout(), network.DNSRespAddr))
}
