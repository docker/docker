package service

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/swarm"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/skip"
)

const (
	defaultSwarmPort = 2477
)

func TestSwarmCAHash(t *testing.T) {
	skip.If(t, strings.HasPrefix(testEnv.FirewallBackendDriver(), "nftables"), "swarm cannot be used with nftables")
	ctx := setupTest(t)

	d1 := swarm.NewSwarm(ctx, t, testEnv)
	defer d1.Stop(t)
	d2 := daemon.New(t)
	d2.Start(t)
	defer d2.Stop(t)

	splitToken := strings.Split(d1.JoinTokens(t).Worker, "-")
	splitToken[2] = "1kxftv4ofnc6mt30lmgipg6ngf9luhwqopfk1tz6bdmnkubg0e"
	replacementToken := strings.Join(splitToken, "-")
	c2 := d2.NewClientT(t)
	defer c2.Close()

	_, err := c2.SwarmJoin(ctx, client.SwarmJoinOptions{
		ListenAddr:  d2.SwarmListenAddr(),
		JoinToken:   replacementToken,
		RemoteAddrs: []string{d1.SwarmListenAddr()},
	})
	assert.ErrorContains(t, err, "remote CA does not match fingerprint")
}

// #29885
func TestSwarmErrorHandling(t *testing.T) {
	ctx := setupTest(t)

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", defaultSwarmPort))
	assert.NilError(t, err)
	defer ln.Close()

	d := daemon.New(t)
	d.Start(t)
	defer d.Stop(t)

	apiClient := d.NewClientT(t)
	defer apiClient.Close()

	_, err = apiClient.SwarmInit(ctx, client.SwarmInitOptions{
		ListenAddr: d.SwarmListenAddr(),
	})
	assert.ErrorContains(t, err, "address already in use")
}
