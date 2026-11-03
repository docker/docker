package service

import (
	stdnet "net"
	"strings"
	"testing"
	"time"

	swarmtypes "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/integration/internal/swarm"
	"github.com/docker/docker/internal/testutils/networking"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/icmd"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

func TestRestoreIngressRulesOnFirewalldReload(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.IsRootless, "rootless mode doesn't support Swarm-mode")
	//skip.If(t, testEnv.FirewallBackendDriver() == "iptables")
	skip.If(t, !networking.FirewalldRunning(), "Need firewalld to test restoration ingress rules")
	ctx := setupTest(t)

	// Check the published port is accessible.
	checkHTTP := func(_ poll.LogT) poll.Result {
		res := icmd.RunCommand("curl", "-v", "-o", "/dev/null", "-w", "%{http_code}\n",
			"http://"+stdnet.JoinHostPort("127.0.0.1", "8080"))
		// A "404 Not Found" means the server responded, but it's got nothing to serve.
		if !strings.Contains(res.Stdout(), "404") {
			return poll.Continue("404 - not found in: %s, %+v", res.Stdout(), res)
		}
		return poll.Success()
	}

	d := swarm.NewSwarm(ctx, t, testEnv)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()

	serviceID := swarm.CreateService(ctx, t, d,
		swarm.ServiceWithName("test-ingress-on-firewalld-reload"),
		swarm.ServiceWithCommand([]string{"httpd", "-f"}),
		swarm.ServiceWithEndpoint(&swarmtypes.EndpointSpec{
			Ports: []swarmtypes.PortConfig{
				{
					Protocol:      "tcp",
					TargetPort:    80,
					PublishedPort: 8080,
					PublishMode:   swarmtypes.PortConfigPublishModeIngress,
				},
			},
		}),
	)
	defer func() {
		err := c.ServiceRemove(ctx, serviceID)
		assert.NilError(t, err)
	}()

	t.Log("Waiting for the service to start")
	poll.WaitOn(t, swarm.RunningTasksCount(ctx, c, serviceID, 1), swarm.ServicePoll)
	t.Log("Checking http access to the service")
	poll.WaitOn(t, checkHTTP, poll.WithTimeout(30*time.Second))

	t.Log("Firewalld reload")
	networking.FirewalldReload(t, d)

	t.Log("Checking http access to the service")
	// It takes a while before this works ...
	poll.WaitOn(t, checkHTTP, poll.WithTimeout(30*time.Second))
}
