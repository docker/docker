package service

import (
	"strings"
	"testing"

	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/swarm"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"gotest.tools/v3/assert"
)

func TestSwarmCAHash(t *testing.T) {
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
