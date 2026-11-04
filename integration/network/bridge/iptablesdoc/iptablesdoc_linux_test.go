// Package iptablesdoc runs docker, creates networks, runs containers and
// captures iptables output for various configurations.
//
// The iptables output is then used with a markdown text/template from the
// "templates" directory for each configuration (for each "section" in "index"),
// to generate a markdown document for each section.
//
// The newly generated documents are placed in:
//
//	bundles/test-integration/TestBridgeIptablesDoc/iptables.md
//
// If the generated doc differs from the "golden" reference in "generated/",
// the test fails. When that happens:
//
//   - check the iptables rules changes in the diff
//   - update the description in the corresponding "_templ.md" file
//   - re-run with TESTFLAGS='-update' to update the reference docs
package iptablesdoc

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"text/template"
	"time"

	networktypes "github.com/moby/moby/api/types/network"
	swarmtypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/daemon/libnetwork/drivers/bridge"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/integration/internal/network"
	"github.com/moby/moby/v2/integration/internal/testutils/networking"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"github.com/vishvananda/netlink"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/golden"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

var (
	docNetworks = []string{"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24"}
	docGateways = []string{"192.0.2.1", "198.51.100.1", "203.0.113.1"}
)

type ctrDesc struct {
	name         string
	portMappings networktypes.PortMap
}

type networkDesc struct {
	name       string
	gwMode     string
	noICC      bool
	internal   bool
	containers []ctrDesc
}

type section struct {
	name            string
	noUserlandProxy bool
	swarm           bool
	networks        []networkDesc
}

var index = []section{
	{
		name: "new-daemon.md",
	},
	{
		name: "usernet-portmap.md",
		networks: []networkDesc{{
			name: "bridge1",
			containers: []ctrDesc{
				{
					name:         "c1",
					portMappings: networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {{HostPort: "8080"}}},
				},
			},
		}},
	},
	{
		name:            "usernet-portmap-noproxy.md",
		noUserlandProxy: true,
		networks: []networkDesc{{
			name: "bridge1",
			containers: []ctrDesc{
				{
					name:         "c1",
					portMappings: networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {{HostPort: "8080"}}},
				},
			},
		}},
	},
	{
		name: "usernet-portmap-noicc.md",
		networks: []networkDesc{{
			name:  "bridge1",
			noICC: true,
			containers: []ctrDesc{
				{
					name:         "c1",
					portMappings: networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {{HostPort: "8080"}}},
				},
			},
		}},
	},
	{
		name: "usernet-internal.md",
		networks: []networkDesc{{
			name:     "bridgeICC",
			internal: true,
			containers: []ctrDesc{
				{
					name: "c1",
				},
			},
		}, {
			name:     "bridgeNoICC",
			internal: true,
			noICC:    true,
			containers: []ctrDesc{
				{
					name: "c1",
				},
			},
		}},
	},
	{
		name: "usernet-portmap-routed.md",
		networks: []networkDesc{{
			name:   "bridge1",
			gwMode: "routed",
			containers: []ctrDesc{
				{
					name:         "c1",
					portMappings: networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {{HostPort: "8080"}}},
				},
			},
		}},
	},
	{
		name: "usernet-portmap-natunprot.md",
		networks: []networkDesc{{
			name:   "bridge1",
			gwMode: "nat-unprotected",
			containers: []ctrDesc{
				{
					name:         "c1",
					portMappings: networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {{HostPort: "8080"}}},
				},
			},
		}},
	},
	{
		name:  "swarm-portmap.md",
		swarm: true,
		networks: []networkDesc{{
			containers: []ctrDesc{
				{
					name:         "c1",
					portMappings: networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {{HostPort: "8080"}}},
				},
			},
		}},
	},
	{
		name: "usernet-portmap-lo.md",
		networks: []networkDesc{{
			name: "bridge1",
			containers: []ctrDesc{
				{
					name:         "c1",
					portMappings: networktypes.PortMap{networktypes.MustParsePort("80/tcp"): {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "8080"}}},
				},
			},
		}},
	},
}

// iptCmdType is used to look up iptCmds in the markdown (can't use an int
// type, or a new string type, so it's just an alias).
type iptCmdType = string

const (
	iptCmdLFilter4       iptCmdType = "LFilter4"
	iptCmdSFilter4       iptCmdType = "SFilter4"
	iptCmdLFilterDocker4 iptCmdType = "LFilterDocker4"
	iptCmdSFilterDocker4 iptCmdType = "SFilterDocker4"
	iptCmdLNat4          iptCmdType = "LNat4"
	iptCmdSNat4          iptCmdType = "SNat4"
	iptCmdLRaw4          iptCmdType = "LRaw4"
	iptCmdSRaw4          iptCmdType = "SRaw4"
)

var iptCmds = map[iptCmdType][]string{
	iptCmdLFilter4:       {"iptables", "-vL", "--line-numbers", "-t", "filter"},
	iptCmdSFilter4:       {"iptables", "-S", "-t", "filter"},
	iptCmdLFilterDocker4: {"iptables", "-vL", "DOCKER", "--line-numbers", "-t", "filter"},
	iptCmdSFilterDocker4: {"iptables", "-S", "DOCKER"},
	iptCmdLNat4:          {"iptables", "-vL", "--line-numbers", "-t", "nat"},
	iptCmdSNat4:          {"iptables", "-S", "-t", "nat"},
	iptCmdLRaw4:          {"iptables", "-vL", "--line-numbers", "-t", "raw"},
	iptCmdSRaw4:          {"iptables", "-S", "-t", "raw"},
}

func TestBridgeIptablesDoc(t *testing.T) {
	skip.If(t, networking.FirewalldRunning(), "can't document iptables rules, running under firewalld")
	skip.If(t, testEnv.IsRootless)
	skip.If(t, testEnv.FirewallBackendDriver() == "nftables")
	ctx := setupTest(t)

	// Get the full path for "bundles/TestBridgeIptablesDoc".
	dest := os.Getenv("DOCKER_INTEGRATION_DAEMON_DEST")
	if dest == "" {
		dest = os.Getenv("DEST")
	}
	dest = filepath.Join(dest, t.Name())

	// Set up an L3Segment, which will have a netns for each "section".
	addr4 := netip.MustParseAddr("192.168.124.1")
	addr6 := netip.MustParseAddr("fdc0:36dc:a4dd::1")
	l3 := networking.NewL3Segment(t, "gen-iptables-doc",
		netip.PrefixFrom(addr4, 24),
		netip.PrefixFrom(addr6, 64),
	)
	t.Cleanup(func() { l3.Destroy(t) })

	for i, sec := range index {
		// Create a netns for this section.
		addr4 = addr4.Next()
		addr6 = addr6.Next()
		hostname := fmt.Sprintf("docker%d", i)
		l3.AddHost(t, hostname, hostname+"-host", "eth0",
			netip.PrefixFrom(addr4, 24),
			netip.PrefixFrom(addr6, 64),
		)
		host := l3.Hosts[hostname]
		// Stop the interface, to reduce the chances of stray packets getting counted by iptables.
		host.MustRun(t, "ip", "link", "set", "eth0", "down")

		t.Run("gen_"+sec.name, func(t *testing.T) {
			// t.Parallel() - doesn't speed things up, startup times just extend
			runTestNet(t, testutil.StartSpan(ctx, t), dest, sec, host)
		})
	}
}

func runTestNet(t *testing.T, ctx context.Context, bundlesDir string, section section, host networking.Host) {
	var dArgs []string
	if section.noUserlandProxy {
		dArgs = append(dArgs, "--userland-proxy=false")
	}
	if section.swarm {
		if _, err := netlink.GenlFamilyGet("IPVS"); err != nil {
			t.Skipf("No IPVS, so DOCKER-INGRESS will not be set up: %v", err)
		}
		dArgs = append(dArgs, "--swarm-default-advertise-addr="+host.Iface)
	}

	// Start the daemon in its own network namespace.
	var d *daemon.Daemon
	host.Do(t, func() {
		// Run without OTEL because there's no routing from this netns for it - which
		// means the daemon doesn't shut down cleanly, causing the test to fail.
		d = daemon.New(t, daemon.WithEnvVars("OTEL_EXPORTER_OTLP_ENDPOINT="))
		d.StartWithBusybox(ctx, t, dArgs...)
		t.Cleanup(func() { d.Stop(t) })
	})

	assert.Assert(t, len(section.networks) < len(docNetworks), "Don't have enough container network addresses")
	if section.swarm {
		d.SwarmInit(ctx, t, swarmtypes.InitRequest{})
		createServices(ctx, t, d, section, host)
	} else {
		createBridgeNetworks(ctx, t, d, section)
	}

	iptablesOutput := runIptables(t, host)
	generated := generate(t, section.name, iptablesOutput)

	// Write the output to the 'bundles' directory for easy reference.
	outFile := filepath.Join(bundlesDir, section.name)
	err := os.WriteFile(outFile, []byte(generated), 0o644)
	assert.NilError(t, err)
	t.Log("Wrote ", outFile)

	// Compare against "golden" results.
	// Use full path so that the directory containing generated docs doesn't
	// have to be called 'testdata'.
	wd, err := os.Getwd()
	assert.NilError(t, err)
	golden.Assert(t, generated, filepath.Join(wd, "generated", section.name))
}

func createBridgeNetworks(ctx context.Context, t *testing.T, d *daemon.Daemon, section section) {
	apiClient := d.NewClientT(t)

	for i, nw := range section.networks {
		gwMode := nw.gwMode
		if gwMode == "" {
			gwMode = "nat"
		}
		netOpts := []func(*client.NetworkCreateOptions){
			network.WithIPAM(docNetworks[i], docGateways[i]),
			network.WithOption(bridge.BridgeName, nw.name),
			network.WithOption(bridge.IPv4GatewayMode, gwMode),
		}
		if nw.noICC {
			netOpts = append(netOpts, network.WithOption(bridge.EnableICC, "false"))
		}
		if nw.internal {
			netOpts = append(netOpts, network.WithInternal())
		}
		network.CreateNoError(ctx, t, apiClient, nw.name, netOpts...)
		t.Cleanup(func() { network.RemoveNoError(ctx, t, apiClient, nw.name) })

		for _, ctr := range nw.containers {
			var exposedPorts []string
			for ep := range ctr.portMappings {
				exposedPorts = append(exposedPorts, ep.String())
			}
			id := container.Run(ctx, t, apiClient,
				container.WithNetworkMode(nw.name),
				container.WithExposedPorts(exposedPorts...),
				container.WithPortMap(ctr.portMappings),
			)
			t.Cleanup(func() {
				apiClient.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
			})
		}
	}
}

func createServices(ctx context.Context, t *testing.T, d *daemon.Daemon, section section, host networking.Host) {
	apiClient := d.NewClientT(t)

	for _, nw := range section.networks {
		for _, ctr := range nw.containers {
			// Convert portMap to swarm PortConfig, just well-enough for this test.
			var portConfig []swarmtypes.PortConfig
			for ctrPP, hostPorts := range ctr.portMappings {
				for _, hostPort := range hostPorts {
					hp, err := strconv.Atoi(hostPort.HostPort)
					assert.NilError(t, err)
					portConfig = append(portConfig, swarmtypes.PortConfig{
						Protocol:      ctrPP.Proto(),
						PublishedPort: uint32(hp),
						TargetPort:    uint32(ctrPP.Num()),
					})
				}
			}
			id := d.CreateService(ctx, t, func(s *swarmtypes.Service) {
				s.Spec = swarmtypes.ServiceSpec{
					TaskTemplate: swarmtypes.TaskSpec{
						ContainerSpec: &swarmtypes.ContainerSpec{
							Image:   "busybox:latest",
							Command: []string{"/bin/top"},
							// top ignores SIGTERM as PID 1; an init forwards it.
							Init: new(true),
						},
					},
					EndpointSpec: &swarmtypes.EndpointSpec{
						Ports: portConfig,
					},
				}
			})
			t.Cleanup(func() { d.RemoveService(ctx, t, id) })
			poll.WaitOn(t, func(_ poll.LogT) poll.Result {
				return pollService(ctx, t, apiClient, host)
			}, poll.WithTimeout(10*time.Second), poll.WithDelay(100*time.Millisecond))
		}
	}
}

func pollService(ctx context.Context, t *testing.T, c *client.Client, host networking.Host) poll.Result {
	list, err := c.ContainerList(ctx, client.ContainerListOptions{})
	if err != nil {
		return poll.Error(fmt.Errorf("failed to list containers: %w", err))
	}
	if len(list.Items) != 1 {
		return poll.Continue("got %d containers, want 1", len(list.Items))
	}
	// Ingress ports are published as ordinary port mappings on the load-balancer
	// sandbox's docker_gwbridge gateway endpoint, a few milliseconds after the
	// task container starts. So, wait for the DNAT rule to appear in the nat
	// DOCKER chain.
	// TODO(robmry) - is there something better to poll?
	di, err := host.Run(t, "iptables", "-t", "nat", "-S", "DOCKER")
	if err != nil || !strings.Contains(di, "DNAT") {
		return poll.Continue("ingress DNAT rule not ready, got: %s", di)
	}
	return poll.Success()
}

var rePacketByteCounts = regexp.MustCompile(`\d+ packets, \d+ bytes`)

func runIptables(t *testing.T, host networking.Host) map[iptCmdType]string {
	host.MustRun(t, "iptables", "-Z")
	host.MustRun(t, "iptables", "-Z", "-t", "nat")
	res := map[iptCmdType]string{}
	for k, cmd := range iptCmds {
		d := host.MustRun(t, cmd[0], cmd[1:]...)
		// In CI, the OUTPUT chain sometimes sees a packet. Remove the counts.
		d = rePacketByteCounts.ReplaceAllString(d, "0 packets, 0 bytes")
		// Indent the result, so that it's treated as preformatted markdown.
		res[k] = strings.ReplaceAll(d, "\n", "\n    ")
	}
	return res
}

const genHeader = "<!-- This is a generated file; DO NOT EDIT. -->\n\n"

func generate(t *testing.T, name string, data map[iptCmdType]string) string {
	t.Helper()
	templ, err := template.New(name).ParseFiles(filepath.Join("templates", name))
	assert.NilError(t, err)

	var wr strings.Builder
	wr.WriteString(genHeader)
	err = templ.ExecuteTemplate(&wr, name, data)
	assert.NilError(t, err)
	return wr.String()
}
