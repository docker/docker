// Package nftablesdoc runs docker, creates networks, runs containers and
// captures nftables output for various configurations.
//
// The nftables output is then used with a markdown text/template from the
// "templates" directory for each configuration (for each "section" in "index"),
// to generate a markdown document for each section.
//
// The newly generated documents are placed in:
//
//	bundles/test-integration/TestBridgeNftablesDoc/nftables.md
//
// If the generated doc differs from the "golden" reference in "generated/",
// the test fails. When that happens:
//
//   - check the nftables rules changes in the diff
//   - update the description in the corresponding "_templ.md" file
//   - re-run with TESTFLAGS='-update' to update the reference docs
package nftablesdoc

import (
	"context"
	"fmt"
	"iter"
	"net/netip"
	"os"
	"path/filepath"
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

func TestBridgeNftablesDoc(t *testing.T) {
	skip.If(t, networking.FirewalldRunning(), "can't document nftables rules, running under firewalld")
	skip.If(t, testEnv.IsRootless)
	skip.If(t, testEnv.FirewallBackendDriver() != "nftables")
	ctx := setupTest(t)

	// Get the full path for "bundles/TestBridgeNftablesDoc".
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
	var sandboxKeys []string
	if section.swarm {
		d.SwarmInit(ctx, t, swarmtypes.InitRequest{})
		sandboxKeys = createServices(ctx, t, d, section, host)
	} else {
		createBridgeNetworks(ctx, t, d, section)
	}

	nftablesOutput := runNftables(t, host)
	if section.swarm {
		runSwarmNftables(t, host, sandboxKeys, nftablesOutput)
	}
	generated := generate(t, section.name, nftablesOutput)

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
	c := d.NewClientT(t)
	defer c.Close()

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
		network.CreateNoError(ctx, t, c, nw.name, netOpts...)
		t.Cleanup(func() { network.RemoveNoError(ctx, t, c, nw.name) })

		for _, ctr := range nw.containers {
			var exposedPorts []string
			for ep := range ctr.portMappings {
				exposedPorts = append(exposedPorts, ep.String())
			}
			id := container.Run(ctx, t, c,
				container.WithNetworkMode(nw.name),
				container.WithExposedPorts(exposedPorts...),
				container.WithPortMap(ctr.portMappings),
			)
			t.Cleanup(func() {
				c.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
			})
		}
	}
}

// createServices creates a service for each container in the section, and
// returns the sandbox key (network namespace path) of each service's task.
func createServices(ctx context.Context, t *testing.T, d *daemon.Daemon, section section, host networking.Host) []string {
	c := d.NewClientT(t)
	defer c.Close()

	var sandboxKeys []string
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
			var ctrID string
			poll.WaitOn(t, func(_ poll.LogT) poll.Result {
				var res poll.Result
				ctrID, res = pollService(ctx, t, c, host)
				return res
			}, poll.WithTimeout(10*time.Second), poll.WithDelay(100*time.Millisecond))
			inspect, err := c.ContainerInspect(ctx, ctrID, client.ContainerInspectOptions{})
			assert.NilError(t, err)
			sandboxKeys = append(sandboxKeys, inspect.Container.NetworkSettings.SandboxKey)
		}
	}
	return sandboxKeys
}

// pollService waits for a service's single task to be running, with its
// ingress port published. It returns the task's container ID.
func pollService(ctx context.Context, t *testing.T, c *client.Client, host networking.Host) (string, poll.Result) {
	list, err := c.ContainerList(ctx, client.ContainerListOptions{})
	if err != nil {
		return "", poll.Error(fmt.Errorf("failed to list containers: %w", err))
	}
	if len(list.Items) != 1 {
		return "", poll.Continue("got %d containers, want 1", len(list.Items))
	}
	// Ingress ports are published as ordinary port mappings on the load-balancer
	// sandbox's docker_gwbridge gateway endpoint, once the load balancer has a
	// backend for them. So, wait for the DNAT rule to appear.
	out, err := host.Run(t, "nft", "list", "chain", "ip", "docker-bridges", "nat-prerouting-and-output")
	if err != nil || !strings.Contains(out, "dnat") {
		return "", poll.Continue("ingress DNAT rule not ready, got: %s", out)
	}
	return list.Items[0].ID, poll.Success()
}

func runNftables(t *testing.T, host networking.Host) map[string]string {
	res := map[string]string{}
	out := host.MustRun(t, "nft", "-s", "list", "table", "ip", "docker-bridges")

	// nftables before v1.0.9 don't stringify the dstnat priority for an
	// output hook, and print its numeric value (-100) instead. Normalize the
	// output for stable test results.
	//
	// - https://git.netfilter.org/nftables/commit/?id=8beafab74c391130fbb9111bfccab8613644e3b9
	// - https://github.com/moby/moby/pull/50745 / https://github.com/moby/moby/commit/fbde2bcb9a8a8fcf06cc6fe1675560cf46237e7b
	out = strings.ReplaceAll(out, "type nat hook output priority -100", "type nat hook output priority dstnat")
	// Indent the result, so that it's treated as preformatted markdown.
	res["Ruleset4"] = "    " + strings.ReplaceAll(out, "\n", "\n    ")

	// Before gathering the table's contents, drop the first line - "table ip docker-bridges {".
	firstLineEnd := strings.Index(out, "\n")
	if firstLineEnd == -1 {
		return res
	}
	out = out[firstLineEnd+1:]

	// Break the remainder into blocks (individual maps, chains), and store each in the result
	// table, using the first line without its "{" as a key.
	//
	// So, from:
	//    table ip docker-bridges {
	//    	map filter-forward-in-jumps {
	//	    	type ifname : verdict
	//	    	elements = { "docker0" : jump filter-forward-in__docker0 }
	//    	}
	//
	//	    map filter-forward-out-jumps {
	//	    	type ifname : verdict
	//	    	elements = { "docker0" : jump filter-forward-out__docker0 }
	//	    }
	//
	//      ...
	//    }
	//
	// Generate two entries in res, four lines each, with keys "map filter-forward-in-jumps"
	// and "map filter-forward-out-jumps". The text template can refer to them using:
	//     {{index . "map filter-forward-in-jumps"}}
	//     {{index . "map filter-forward-out-jumps"}}

	var sb strings.Builder
	for line := range lines(out) {
		if line == "\n" || (line != "" && line[0] == '}') {
			block := sb.String()
			sb.Reset()
			if keyEnd := strings.Index(block, " {"); keyEnd > 0 {
				res[strings.TrimSpace(block[:keyEnd])] = block
			}
			continue
		}
		sb.WriteString("    ") // Indent preformatted line for Markdown.
		sb.WriteString(line)
	}
	return res
}

// runSwarmNftables adds the nftables tables in the ingress load-balancer
// sandbox and in each task's network namespace to res, as "IngressLB4" and
// "Task<n>Ingress4". The sandboxes' namespaces sit together in the daemon's
// netns directory, so the ingress sandbox's is found next to the first task's.
func runSwarmNftables(t *testing.T, host networking.Host, sandboxKeys []string, res map[string]string) {
	t.Helper()
	if len(sandboxKeys) == 0 {
		return
	}
	listTable := func(nsPath, table string) string {
		out := host.MustRun(t, "nsenter", "--net="+nsPath, "nft", "-s", "list", "table", "ip", table)
		return "    " + strings.ReplaceAll(strings.TrimRight(out, "\n"), "\n", "\n    ")
	}
	res["IngressLB4"] = listTable(filepath.Join(filepath.Dir(sandboxKeys[0]), "ingress_sbox"), "docker-lb-nat")
	for i, key := range sandboxKeys {
		res[fmt.Sprintf("Task%dIngress4", i)] = listTable(key, "docker-container-ingress")
	}
}

// lines produces a line iterator
// TODO: (When Go 1.24 is min version) Replace with `strings.Lines(out)`.
func lines(s string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for s != "" {
			var line string
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				line, s = s[:i+1], s[i+1:]
			} else {
				line, s = s, ""
			}
			if !yield(line) {
				return
			}
		}
	}
}

const genHeader = "<!-- This is a generated file; DO NOT EDIT. -->\n\n"

func generate(t *testing.T, name string, data map[string]string) string {
	t.Helper()
	templ, err := template.New(name).ParseFiles(filepath.Join("templates", name))
	assert.NilError(t, err)

	var wr strings.Builder
	wr.WriteString(genHeader)
	err = templ.ExecuteTemplate(&wr, name, data)
	assert.NilError(t, err)
	return wr.String()
}
