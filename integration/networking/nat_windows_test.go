package networking

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	networktypes "github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/container"
	"github.com/moby/moby/v2/integration/internal/network"
	"github.com/moby/moby/v2/internal/testutil"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

// TestNatNetworkICC tries to ping container ctr1 from container ctr2 using its hostname.
// Checks DNS resolution, and whether containers can communicate with each other.
// Regression test for https://github.com/moby/moby/issues/47370
func TestNatNetworkICC(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	testcases := []struct {
		name    string
		netName string
	}{
		{
			name:    "default nat network",
			netName: "nat",
		},
		{
			name:    "user-defined nat network",
			netName: "mynat",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testutil.StartSpan(ctx, t)

			if tc.netName != "nat" {
				network.CreateNoError(ctx, t, apiClient, tc.netName,
					network.WithDriver("nat"),
				)
				defer network.RemoveNoError(ctx, t, apiClient, tc.netName)
			}

			const ctr1Name = "ctr1"
			id1 := container.Run(ctx, t, apiClient,
				container.WithName(ctr1Name),
				container.WithNetworkMode(tc.netName),
			)
			defer container.Remove(ctx, t, apiClient, id1, client.ContainerRemoveOptions{Force: true})

			const ctr2Name = "ctr2"
			attachCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			res := container.RunAttach(attachCtx, t, apiClient,
				container.WithName(ctr2Name),
				container.WithCmd("ping", "-n", "1", "-w", "3000", ctr1Name),
				container.WithNetworkMode(tc.netName),
			)
			defer container.Remove(ctx, t, apiClient, res.ContainerID, client.ContainerRemoveOptions{Force: true})

			assert.Check(t, is.Equal(res.ExitCode, 0))
			assert.Check(t, is.Equal(res.Stderr.Len(), 0))
			assert.Check(t, is.Contains(res.Stdout.String(), "Sent = 1, Received = 1, Lost = 0"))
		})
	}
}

// Check that a container on one network can reach a service in a container on
// another network, via a mapped port on the host.
//
// FIXME: flaky test; see https://github.com/moby/moby/issues/48881
func TestFlakyPortMappedHairpinWindows(t *testing.T) {
	ctx := setupTest(t)
	apiClient := testEnv.APIClient()

	// Find an address on the test host.
	conn, err := net.Dial("tcp4", "hub.docker.com:80")
	assert.NilError(t, err)
	hostAddr := conn.LocalAddr().(*net.TCPAddr).IP.String()
	_ = conn.Close()

	const serverNetName = "servernet"
	network.CreateNoError(ctx, t, apiClient, serverNetName, network.WithDriver("nat"))
	defer network.RemoveNoError(ctx, t, apiClient, serverNetName)
	const clientNetName = "clientnet"
	network.CreateNoError(ctx, t, apiClient, clientNetName, network.WithDriver("nat"))
	defer network.RemoveNoError(ctx, t, apiClient, clientNetName)

	serverId := container.Run(ctx, t, apiClient,
		container.WithNetworkMode(serverNetName),
		container.WithExposedPorts("80"),
		container.WithPortMap(networktypes.PortMap{networktypes.MustParsePort("80"): {{HostIP: netip.IPv4Unspecified()}}}),
		container.WithCmd("httpd", "-f"),
	)
	defer container.Remove(ctx, t, apiClient, serverId, client.ContainerRemoveOptions{Force: true})

	inspect := container.Inspect(ctx, t, apiClient, serverId)
	hostPort := inspect.NetworkSettings.Ports[networktypes.MustParsePort("80/tcp")][0].HostPort

	attachCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res := container.RunAttach(attachCtx, t, apiClient,
		container.WithNetworkMode(clientNetName),
		container.WithCmd("wget", "http://"+hostAddr+":"+hostPort),
	)
	defer container.Remove(ctx, t, apiClient, res.ContainerID, client.ContainerRemoveOptions{Force: true})
	assert.Check(t, is.Contains(res.Stderr.String(), "404 Not Found"))
}
