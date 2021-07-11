package network // import "github.com/docker/docker/integration/network"

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/integration/internal/network"
	"github.com/docker/docker/integration/internal/swarm"
	"github.com/docker/docker/testutil/daemon"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

func pollSetting(config *poll.Settings) {
	config.Timeout = 100 * time.Second
	config.Delay = 100 * time.Millisecond
	if runtime.GOARCH == "arm64" || runtime.GOARCH == "arm" {
		config.Timeout = 500 * time.Second
	}
}

func createService(t *testing.T, d *daemon.Daemon, cmd []string, net, serviceName string) (serviceID string) {
	replicas := uint64(1)
	serviceID = swarm.CreateService(t, d,
		swarm.ServiceWithReplicas(replicas),
		swarm.ServiceWithName(serviceName),
		swarm.ServiceWithNetwork(net),
		swarm.ServiceWithCommand(cmd),
		swarm.ServiceWithRestartAttempts(uint64(1)),
	)
	return
}

func createServiceWithHealthCheck(t *testing.T, d *daemon.Daemon, cmd []string,
	net, serviceName string, healthConfig *container.HealthConfig) (serviceID string) {
	replicas := uint64(1)
	serviceID = swarm.CreateService(t, d,
		swarm.ServiceWithReplicas(replicas),
		swarm.ServiceWithName(serviceName),
		swarm.ServiceWithNetwork(net),
		swarm.ServiceWithCommand(cmd),
		swarm.ServiceWithRestartAttempts(uint64(1)),
		swarm.ServiceWithHealthCheck(healthConfig),
	)
	return
}

func checkNetworkRemoved(ctx context.Context, t *testing.T, c *client.Client, net string) error {
	// network removal is asynchronous, retry testing at most 30 secs.
	for count := 0; count < 6; count++ {
		time.Sleep(5 * time.Second)
		nr, err := c.NetworkInspect(ctx, net, types.NetworkInspectOptions{})
		assert.NilError(t, err)
		if nr.Containers == nil {
			return nil
		}
	}
	return errors.New("assertNetworkRemoved failed")
}

func testNormalService(t *testing.T, d *daemon.Daemon, c *client.Client, net string) {
	fmt.Println("Running testNormalService ...")
	ctx := context.Background()
	cmd := []string{"sleep", "3d"}
	serviceID := createService(t, d, cmd, net, "testNormalService")
	poll.WaitOn(t, swarm.RunningTasksCount(c, serviceID, uint64(1)), pollSetting)
	err := c.ServiceRemove(ctx, serviceID)
	assert.NilError(t, err)
	err = checkNetworkRemoved(ctx, t, c, net)
	assert.NilError(t, err)
}

func testStartServiceFail(t *testing.T, d *daemon.Daemon, c *client.Client, net string) {
	fmt.Println("Running testStartServiceFail ...")
	ctx := context.Background()
	cmd := []string{"non-existing-file"}
	serviceID := createService(t, d, cmd, net, "testStartServiceFail")
	poll.WaitOn(t, swarm.FailedTasksCount(c, serviceID, uint64(2)), pollSetting)
	err := checkNetworkRemoved(ctx, t, c, net)
	assert.NilError(t, err)
	err = c.ServiceRemove(ctx, serviceID)
	assert.NilError(t, err)
}

func testServiceCompletion(t *testing.T, d *daemon.Daemon, c *client.Client, net string) {
	fmt.Println("Running testServiceCompletion ...")
	ctx := context.Background()
	cmd := []string{"true"}
	serviceID := createService(t, d, cmd, net, "testServiceCompletion")
	poll.WaitOn(t, swarm.CompletedTasksCount(c, serviceID, uint64(2)), pollSetting)
	err := checkNetworkRemoved(ctx, t, c, net)
	assert.NilError(t, err)
	err = c.ServiceRemove(ctx, serviceID)
	assert.NilError(t, err)
}

func testServiceExitFail(t *testing.T, d *daemon.Daemon, c *client.Client, net string) {
	fmt.Println("Running testServiceExitFail ...")
	ctx := context.Background()
	cmd := []string{"false"}
	serviceID := createService(t, d, cmd, net, "testServiceExitFail")
	poll.WaitOn(t, swarm.FailedTasksCount(c, serviceID, uint64(2)), pollSetting)
	err := checkNetworkRemoved(ctx, t, c, net)
	assert.NilError(t, err)
	err = c.ServiceRemove(ctx, serviceID)
	assert.NilError(t, err)
}

func testServiceHealthCheckFail(t *testing.T, d *daemon.Daemon, c *client.Client, net string) {
	fmt.Println("Running testServiceHealthCheckFail ...")
	ctx := context.Background()
	cmd := []string{"sleep", "3d"}
	healthConfig := container.HealthConfig{
		Test:        []string{"CMD-SHELL", "false"},
		Interval:    time.Second,
		Timeout:     time.Second,
		StartPeriod: time.Second,
		Retries:     1,
	}
	serviceID := createServiceWithHealthCheck(t, d, cmd, net, "testServiceHealthCheckFail", &healthConfig)
	poll.WaitOn(t, swarm.FailedTasksCount(c, serviceID, uint64(2)), pollSetting)
	err := checkNetworkRemoved(ctx, t, c, net)
	assert.NilError(t, err)
	err = c.ServiceRemove(ctx, serviceID)
	assert.NilError(t, err)
}

func testServiceHealthCheckExitEarly(t *testing.T, d *daemon.Daemon, c *client.Client, net string) {
	fmt.Println("Running testServiceHealthCheckExitEarly ...")
	ctx := context.Background()
	cmd := []string{"sleep", "1s"}
	healthConfig := container.HealthConfig{
		Test:        []string{"CMD-SHELL", "false"},
		Interval:    time.Second,
		Timeout:     time.Second,
		StartPeriod: time.Second,
		Retries:     3,
	}
	serviceID := createServiceWithHealthCheck(t, d, cmd, net, "testServiceHealthCheckExitEarly", &healthConfig)
	poll.WaitOn(t, swarm.CompletedTasksCount(c, serviceID, uint64(2)), pollSetting)
	err := checkNetworkRemoved(ctx, t, c, net)
	assert.NilError(t, err)
	err = c.ServiceRemove(ctx, serviceID)
	assert.NilError(t, err)
}

func TestNodeLBCleanup(t *testing.T) {
	skip.If(t, testEnv.OSType == "windows")
	skip.If(t, testEnv.IsRootless, "rootless mode doesn't support Swarm-mode")
	defer setupTest(t)()
	d := swarm.NewSwarm(t, testEnv)
	defer d.Stop(t)
	c := d.NewClientT(t)
	defer c.Close()

	ctx := context.Background()

	// create an overlay network
	net := "ol_" + t.Name()
	netID := network.CreateNoError(ctx, t, c, net,
		network.WithDriver("overlay"))

	testNormalService(t, d, c, net)
	testStartServiceFail(t, d, c, net)
	testServiceCompletion(t, d, c, net)
	testServiceExitFail(t, d, c, net)
	testServiceHealthCheckFail(t, d, c, net)
	testServiceHealthCheckExitEarly(t, d, c, net)

	err := c.NetworkRemove(ctx, netID)
	assert.NilError(t, err)
	err = d.SwarmLeave(t, true)
	assert.NilError(t, err)

}
