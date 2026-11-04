//go:build linux

package overlay

import (
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	networktypes "github.com/moby/moby/api/types/network"
	swarmtypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/network"
	"github.com/moby/moby/v2/integration/internal/swarm"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

// TestIngressSourceIPTaskLifecycle exercises option propagation from the Swarm
// API through task namespace setup, task replacement and service removal.
func TestIngressSourceIPTaskLifecycle(t *testing.T) {
	skip.If(t, testEnv.IsRootless, "rootless mode doesn't support overlay networks")
	ctx := setupTest(t)
	d := daemon.New(t)
	d.StartNodeWithBusybox(ctx, t)
	defer d.Stop(t)
	d.SwarmInit(ctx, t, swarmtypes.InitRequest{AdvertiseAddr: "127.0.0.1:2377"})
	defer d.SwarmLeave(ctx, t, true)
	apiClient := d.NewClientT(t)
	defer apiClient.Close()
	network.RemoveNoError(ctx, t, apiClient, "ingress")
	options := client.NetworkCreateOptions{Ingress: true}
	for _, opt := range []func(*client.NetworkCreateOptions){
		network.WithDriver("overlay"),
		network.WithIPAM("10.66.0.0/24", ""),
		network.WithOption("com.docker.network.overlay.preserve_ingress_source_ip", "true"),
	} {
		opt(&options)
	}
	// Swarm removal and the node's local ingress teardown are asynchronous.
	// A transient NotFound from inspect is not proof that teardown finished.
	poll.WaitOn(t, func(poll.LogT) poll.Result {
		_, err := apiClient.NetworkCreate(ctx, "ingress", options)
		if cerrdefs.IsConflict(err) {
			return poll.Continue("waiting for the previous ingress network to be removed: %v", err)
		}
		if err != nil {
			return poll.Error(err)
		}
		return poll.Success()
	}, swarm.ServicePoll)
	svcID := swarm.CreateService(ctx, t, d,
		swarm.ServiceWithName("ingress-source-ip"),
		swarm.ServiceWithSysctls(map[string]string{"net.ipv4.conf.all.rp_filter": "0"}),
		swarm.ServiceWithEndpoint(&swarmtypes.EndpointSpec{Ports: []swarmtypes.PortConfig{
			{Protocol: networktypes.TCP, PublishedPort: 8080, TargetPort: 18080, PublishMode: swarmtypes.PortConfigPublishModeIngress},
		}}))
	defer apiClient.ServiceRemove(ctx, svcID, client.ServiceRemoveOptions{})
	poll.WaitOn(t, swarm.RunningTasksCount(ctx, apiClient, svcID, 1), swarm.ServicePoll)
	service, err := apiClient.ServiceInspect(ctx, svcID, client.ServiceInspectOptions{})
	assert.NilError(t, err)
	service.Service.Spec.TaskTemplate.ForceUpdate++
	_, err = apiClient.ServiceUpdate(ctx, svcID, client.ServiceUpdateOptions{Version: service.Service.Version, Spec: service.Service.Spec})
	assert.NilError(t, err)
	poll.WaitOn(t, func(poll.LogT) poll.Result {
		tasks, err := apiClient.TaskList(ctx, client.TaskListOptions{Filters: make(client.Filters).Add("service", svcID)})
		if err != nil {
			return poll.Error(err)
		}
		var running, updated int
		for _, task := range tasks.Items {
			if task.Status.State == swarmtypes.TaskStateRunning {
				running++
				if task.Spec.ForceUpdate == service.Service.Spec.TaskTemplate.ForceUpdate {
					updated++
				}
			}
		}
		if running == 1 && updated == 1 {
			return poll.Success()
		}
		return poll.Continue("waiting for the replacement task: running=%d, updated=%d", running, updated)
	}, swarm.ServicePoll)
}
