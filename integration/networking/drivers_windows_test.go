package networking

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	networktypes "github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/integration/internal/network"
	"github.com/moby/moby/v2/internal/testutil"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

// TestWindowsNetworkDrivers validates Windows-specific network drivers for Windows.
// Tests: NAT, Transparent, and L2Bridge network drivers.
func TestWindowsNetworkDrivers(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	testcases := []struct {
		name   string
		driver string
	}{
		{
			// NAT connectivity is already tested in TestNatNetworkICC (nat_windows_test.go),
			// so we only validate network creation here.
			name:   "NAT driver network creation",
			driver: "nat",
		},
		{
			// Only test creation of a Transparent driver network, connectivity depends on external
			// network infrastructure.
			name:   "Transparent driver network creation",
			driver: "transparent",
		},
		{
			// L2Bridge driver requires specific host network adapter configuration, test will skip
			// if host configuration is missing.
			name:   "L2Bridge driver network creation",
			driver: "l2bridge",
		},
	}

	for tcID, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)

			netName := fmt.Sprintf("test-%s-%d", tc.driver, tcID)

			// Create network with specified driver
			netResp, err := apiClient.NetworkCreate(ctx, netName, client.NetworkCreateOptions{
				Driver: tc.driver,
			})
			if err != nil {
				// L2Bridge may fail if host network configuration is not available
				if tc.driver == "l2bridge" {
					errStr := strings.ToLower(err.Error())
					if strings.Contains(errStr, "the network does not have a subnet for this endpoint") {
						t.Skipf("Driver %s requires host network configuration: %v", tc.driver, err)
					}
				}
				t.Fatalf("Failed to create network with %s driver: %v", tc.driver, err)
			}
			defer network.RemoveNoError(ctx, t, apiClient, netName)

			// Inspect network to validate driver is correctly set
			netInfo, err := apiClient.NetworkInspect(ctx, netResp.ID, client.NetworkInspectOptions{})
			assert.NilError(t, err)
			assert.Check(t, is.Equal(netInfo.Network.Driver, tc.driver), "Network driver mismatch")
			assert.Check(t, is.Equal(netInfo.Network.Name, netName), "Network name mismatch")
		})
	}
}

// TestWindowsNATDriverPortMapping validates NAT port mapping by testing host connectivity.
func TestWindowsNATDriverPortMapping(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	// Use default NAT network which supports port mapping
	netName := "nat"

	// PowerShell HTTP listener on port 80
	psScript := `
		$listener = New-Object System.Net.HttpListener
		$listener.Prefixes.Add('http://+:80/')
		$listener.Start()
		while ($listener.IsListening) {
			$context = $listener.GetContext()
			$response = $context.Response
			$content = [System.Text.Encoding]::UTF8.GetBytes('OK')
			$response.ContentLength64 = $content.Length
			$response.OutputStream.Write($content, 0, $content.Length)
			$response.OutputStream.Close()
		}
	`

	// Create container with port mapping 80->8080
	ctrName := "port-mapping-test"
	id := container.Run(ctx, t, apiClient,
		container.WithName(ctrName),
		container.WithCmd("powershell", "-Command", psScript),
		container.WithNetworkMode(netName),
		container.WithExposedPorts("80"),
		container.WithPortMap(networktypes.PortMap{
			networktypes.MustParsePort("80"): {{HostIP: netip.IPv4Unspecified(), HostPort: "8080"}},
		}),
	)
	defer apiClient.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})

	// Verify port mapping metadata
	ctrInfo := container.Inspect(ctx, t, apiClient, id)
	portKey := networktypes.MustParsePort("80/tcp")
	assert.Check(t, ctrInfo.NetworkSettings.Ports[portKey] != nil, "Port mapping not found")
	assert.Check(t, len(ctrInfo.NetworkSettings.Ports[portKey]) > 0, "No host port binding")
	assert.Check(t, is.Equal(ctrInfo.NetworkSettings.Ports[portKey][0].HostPort, "8080"))

	// Test actual connectivity from host to container via mapped port
	httpClient := &http.Client{Timeout: 2 * time.Second}
	checkHTTP := func(t poll.LogT) poll.Result {
		resp, err := httpClient.Get("http://localhost:8080")
		if err != nil {
			return poll.Continue("connection failed: %v", err)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return poll.Continue("failed to read body: %v", err)
		}

		if !strings.Contains(string(body), "OK") {
			return poll.Continue("unexpected response body: %s", string(body))
		}
		return poll.Success()
	}

	poll.WaitOn(t, checkHTTP, poll.WithTimeout(10*time.Second))
}

// TestWindowsNetworkDNSResolution validates DNS resolution on Windows networks.
func TestWindowsNetworkDNSResolution(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	testcases := []struct {
		name       string
		driver     string
		customDNS  bool
		dnsServers []string
	}{
		{
			name:   "Default NAT network DNS resolution",
			driver: "nat",
		},
		{
			name:       "Custom DNS servers on NAT network",
			driver:     "nat",
			customDNS:  true,
			dnsServers: []string{"8.8.8.8", "8.8.4.4"},
		},
	}

	for tcID, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)

			netName := fmt.Sprintf("test-dns-%s-%d", tc.driver, tcID)

			// Create network with optional custom DNS
			netOpts := []func(*client.NetworkCreateOptions){
				network.WithDriver(tc.driver),
			}
			if tc.customDNS {
				// Note: DNS options may need to be set via network options on Windows
				for _, dns := range tc.dnsServers {
					netOpts = append(netOpts, network.WithOption("com.docker.network.windowsshim.dnsservers", dns))
				}
			}

			network.CreateNoError(ctx, t, apiClient, netName, netOpts...)
			defer network.RemoveNoError(ctx, t, apiClient, netName)

			// Create container and verify DNS resolution
			ctrName := fmt.Sprintf("dns-test-%d", tcID)
			id := container.Run(ctx, t, apiClient,
				container.WithName(ctrName),
				container.WithNetworkMode(netName),
			)
			defer apiClient.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})

			// Test DNS resolution by pinging container by name from another container
			pingCmd := []string{"ping", "-n", "1", "-w", "3000", ctrName}

			attachCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			res := container.RunAttach(attachCtx, t, apiClient,
				container.WithCmd(pingCmd...),
				container.WithNetworkMode(netName),
			)
			defer apiClient.ContainerRemove(ctx, res.ContainerID, client.ContainerRemoveOptions{Force: true})

			assert.Check(t, is.Equal(res.ExitCode, 0), "DNS resolution failed")
			assert.Check(t, is.Contains(res.Stdout.String(), "Sent = 1, Received = 1, Lost = 0"))
		})
	}
}

// TestWindowsNetworkLifecycle validates network lifecycle operations on Windows.
// Tests network creation, container attachment, detachment, and deletion.
func TestWindowsNetworkLifecycle(t *testing.T) {
	// Skip this test on Windows Containerd because NetworkConnect operations fail with an
	// unsupported platform request error:
	// https://github.com/moby/moby/issues/51589
	skip.If(t, testEnv.RuntimeIsWindowsContainerd(),
		"Skipping test: fails on Containerd due to unsupported platform request error during NetworkConnect operations")

	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	netName := "lifecycle-test-nat"

	netID := network.CreateNoError(ctx, t, apiClient, netName,
		network.WithDriver("nat"),
	)

	netInfo, err := apiClient.NetworkInspect(ctx, netID, client.NetworkInspectOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(netInfo.Network.Name, netName))

	// Create container on network
	ctrName := "lifecycle-ctr"
	id := container.Run(ctx, t, apiClient,
		container.WithName(ctrName),
		container.WithNetworkMode(netName),
	)

	ctrInfo := container.Inspect(ctx, t, apiClient, id)
	assert.Check(t, ctrInfo.NetworkSettings.Networks[netName] != nil)

	// Disconnect container from network
	_, err = apiClient.NetworkDisconnect(ctx, netID, client.NetworkDisconnectOptions{
		Container: id,
		Force:     false,
	})
	assert.NilError(t, err)

	ctrInfo = container.Inspect(ctx, t, apiClient, id)
	assert.Check(t, ctrInfo.NetworkSettings.Networks[netName] == nil, "Container still connected after disconnect")

	// Reconnect container to network
	_, err = apiClient.NetworkConnect(ctx, netID, client.NetworkConnectOptions{
		Container:      id,
		EndpointConfig: nil,
	})
	assert.NilError(t, err)

	ctrInfo = container.Inspect(ctx, t, apiClient, id)
	assert.Check(t, ctrInfo.NetworkSettings.Networks[netName] != nil, "Container not reconnected")

	apiClient.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})

	network.RemoveNoError(ctx, t, apiClient, netName)

	_, err = apiClient.NetworkInspect(ctx, netID, client.NetworkInspectOptions{})
	assert.Check(t, err != nil, "Network still exists after deletion")
}

// TestWindowsNetworkIsolation validates network isolation between containers on different networks.
// Ensures containers on different networks cannot communicate, validating Windows network driver isolation.
func TestWindowsNetworkIsolation(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	// Create two separate NAT networks
	net1Name := "isolation-net1"
	net2Name := "isolation-net2"

	network.CreateNoError(ctx, t, apiClient, net1Name, network.WithDriver("nat"))
	defer network.RemoveNoError(ctx, t, apiClient, net1Name)

	network.CreateNoError(ctx, t, apiClient, net2Name, network.WithDriver("nat"))
	defer network.RemoveNoError(ctx, t, apiClient, net2Name)

	// Create container on first network
	ctr1Name := "isolated-ctr1"
	id1 := container.Run(ctx, t, apiClient,
		container.WithName(ctr1Name),
		container.WithNetworkMode(net1Name),
	)
	defer apiClient.ContainerRemove(ctx, id1, client.ContainerRemoveOptions{Force: true})

	ctr1Info := container.Inspect(ctx, t, apiClient, id1)
	ctr1IP := ctr1Info.NetworkSettings.Networks[net1Name].IPAddress
	assert.Check(t, ctr1IP.IsValid(), "Container IP not assigned")

	// Create container on second network and try to ping first container
	pingCmd := []string{"ping", "-n", "1", "-w", "2000", ctr1IP.String()}

	attachCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res := container.RunAttach(attachCtx, t, apiClient,
		container.WithCmd(pingCmd...),
		container.WithNetworkMode(net2Name),
	)
	defer apiClient.ContainerRemove(ctx, res.ContainerID, client.ContainerRemoveOptions{Force: true})

	// Ping should fail, demonstrating network isolation
	assert.Check(t, res.ExitCode != 0, "Ping succeeded unexpectedly - networks are not isolated")
	// Windows ping failure can have various error messages, but we should see some indication of failure
	stdout := res.Stdout.String()
	stderr := res.Stderr.String()

	// Check for common Windows ping failure indicators
	hasFailureIndicator := strings.Contains(stdout, "Destination host unreachable") ||
		strings.Contains(stdout, "Request timed out") ||
		strings.Contains(stdout, "100% loss") ||
		strings.Contains(stdout, "Lost = 1") ||
		strings.Contains(stderr, "unreachable") ||
		strings.Contains(stderr, "timeout")

	assert.Check(t, hasFailureIndicator,
		"Expected ping failure indicators not found. Exit code: %d, stdout: %q, stderr: %q",
		res.ExitCode, stdout, stderr)
}

// TestWindowsNetworkEndpointManagement validates endpoint creation and management on Windows networks.
// Tests that multiple containers can be created and managed on the same network.
func TestWindowsNetworkEndpointManagement(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	netName := "endpoint-test-nat"
	network.CreateNoError(ctx, t, apiClient, netName, network.WithDriver("nat"))
	t.Cleanup(func() {
		network.RemoveNoError(ctx, t, apiClient, netName)
	})

	// Create multiple containers on the same network
	const numContainers = 3
	containerIDs := make([]string, numContainers)

	for i := range numContainers {
		ctrName := fmt.Sprintf("endpoint-ctr-%d", i)
		id := container.Run(ctx, t, apiClient,
			container.WithName(ctrName),
			container.WithNetworkMode(netName),
		)
		containerIDs[i] = id
		t.Cleanup(func() {
			container.Remove(ctx, t, apiClient, id, client.ContainerRemoveOptions{Force: true})
		})
	}

	netInfo, err := apiClient.NetworkInspect(ctx, netName, client.NetworkInspectOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(netInfo.Network.Containers), numContainers),
		"Expected %d containers, got %d", numContainers, len(netInfo.Network.Containers))

	// Verify each container has network connectivity to others
	for i := range numContainers - 1 {
		targetName := fmt.Sprintf("endpoint-ctr-%d", i)
		pingCmd := []string{"ping", "-n", "1", "-w", "3000", targetName}

		sourceName := fmt.Sprintf("endpoint-ctr-%d", i+1)
		attachCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		res := container.RunAttach(attachCtx, t, apiClient,
			container.WithName(fmt.Sprintf("%s-pinger", sourceName)),
			container.WithCmd(pingCmd...),
			container.WithNetworkMode(netName),
		)
		t.Cleanup(func() {
			container.Remove(ctx, t, apiClient, res.ContainerID, client.ContainerRemoveOptions{Force: true})
		})

		assert.Check(t, is.Equal(res.ExitCode, 0),
			"Container %s failed to ping %s", sourceName, targetName)
	}
}
