package service

import (
	"net/http"
	"strings"
	"testing"

	types "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/swarm"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"github.com/moby/moby/v2/internal/testutil/request"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/skip"
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

func TestSwarmInvalidAddress(t *testing.T) {
	ctx := setupTest(t)
	d := daemon.New(t)
	d.Start(t)
	defer d.Stop(t)

	req := types.InitRequest{
		ListenAddr: "",
	}
	res, _, err := request.Post(ctx, "/swarm/init", request.Host(d.Sock()), request.JSONBody(req))
	assert.NilError(t, err)
	assert.Equal(t, res.StatusCode, http.StatusBadRequest)

	req2 := types.JoinRequest{
		ListenAddr:  "0.0.0.0:2377",
		RemoteAddrs: []string{""},
	}
	res, _, err = request.Post(ctx, "/swarm/join", request.Host(d.Sock()), request.JSONBody(req2))
	assert.NilError(t, err)
	assert.Equal(t, res.StatusCode, http.StatusBadRequest)
}
