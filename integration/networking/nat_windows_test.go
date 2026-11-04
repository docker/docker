package networking

import (
	"context"
	"net"
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
)

// TestNatNetworkICC tries to ping container ctr1 from container ctr2 using its hostname.
// Checks DNS resolution, and whether containers can communicate with each other.
// Regression test for https://github.com/moby/moby/issues/47370
func TestNatNetworkICC(t *testing.T) {
	ctx := setupTest(t)
	c := testEnv.APIClient()

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
				network.CreateNoError(ctx, t, c, tc.netName,
					network.WithDriver("nat"),
				)
				t.Cleanup(func() {
					network.RemoveNoError(context.Background(), t, c, tc.netName)
				})
			}

			safeName := strings.ReplaceAll(tc.name, " ", "_")
			ctr1Name := safeName + "-ctr1"
			id1 := container.Run(ctx, t, c,
				container.WithName(ctr1Name),
				container.WithNetworkMode(tc.netName),
			)
			t.Cleanup(func() {
				c.ContainerRemove(context.Background(), id1, client.ContainerRemoveOptions{Force: true}) //nolint:errcheck
			})

			pingCmd := []string{"ping", "-n", "1", "-w", "3000", ctr1Name}

			ctr2Name := safeName + "-ctr2"
			// Register cleanup before RunAttach so it fires even if RunAttach exits via t.FailNow.
			t.Cleanup(func() {
				c.ContainerRemove(context.Background(), ctr2Name, client.ContainerRemoveOptions{Force: true}) //nolint:errcheck
			})
			attachCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			res := container.RunAttach(attachCtx, t, c,
				container.WithName(ctr2Name),
				container.WithCmd(pingCmd...),
				container.WithNetworkMode(tc.netName),
			)

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
	c := testEnv.APIClient()

	// Find an address on the test host.
	conn, err := net.Dial("tcp4", "hub.docker.com:80")
	assert.NilError(t, err)
	hostAddr := conn.LocalAddr().(*net.TCPAddr).IP.String()
	conn.Close()

	const serverNetName = "servernet"
	network.CreateNoError(ctx, t, c, serverNetName, network.WithDriver("nat"))
	defer network.RemoveNoError(ctx, t, c, serverNetName)
	const clientNetName = "clientnet"
	network.CreateNoError(ctx, t, c, clientNetName, network.WithDriver("nat"))
	defer network.RemoveNoError(ctx, t, c, clientNetName)

	serverId := container.Run(ctx, t, c,
		container.WithNetworkMode(serverNetName),
		container.WithExposedPorts("80"),
		container.WithPortMap(networktypes.PortMap{networktypes.MustParsePort("80"): {{HostIP: netip.IPv4Unspecified()}}}),
		container.WithCmd("httpd", "-f"),
	)
	defer c.ContainerRemove(ctx, serverId, client.ContainerRemoveOptions{Force: true})

	inspect := container.Inspect(ctx, t, c, serverId)
	hostPort := inspect.NetworkSettings.Ports[networktypes.MustParsePort("80/tcp")][0].HostPort

	attachCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res := container.RunAttach(attachCtx, t, c,
		container.WithNetworkMode(clientNetName),
		container.WithCmd("wget", "http://"+hostAddr+":"+hostPort),
	)
	defer c.ContainerRemove(ctx, res.ContainerID, client.ContainerRemoveOptions{Force: true})
	assert.Check(t, is.Contains(res.Stderr.String(), "404 Not Found"))
}
