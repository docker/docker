package service

import (
	"fmt"
	"testing"

	swarmtypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/swarm"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

func TestServiceListEmpty(t *testing.T) {
	ctx := setupTest(t)

	d := swarm.NewSwarm(ctx, t, testEnv)
	defer d.Stop(t)
	apiClient := d.NewClientT(t)
	defer apiClient.Close()

	result, err := apiClient.ServiceList(ctx, client.ServiceListOptions{})
	assert.NilError(t, err)
	assert.Assert(t, result.Items != nil)
	assert.Check(t, is.Len(result.Items, 0))
}

// TestServiceListWithStatuses tests that performing a ServiceList operation
// correctly uses the Status parameter, and that the resulting response
// contains correct service statuses.
//
// NOTE(dperny): because it's a pain to elicit the behavior of an unconverged
// service reliably, I'm not testing that an unconverged service returns X
// running and Y desired tasks. Instead, I'm just going to trust that I can
// successfully assign a value to another value without screwing it up. The
// logic for computing service statuses is in swarmkit anyway, not in the
// engine, and is well-tested there, so this test just needs to make sure that
// statuses get correctly associated with the right services.
func TestServiceListWithStatuses(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon)
	skip.If(t, testEnv.DaemonInfo.OSType == "windows")

	ctx := setupTest(t)

	d := swarm.NewSwarm(ctx, t, testEnv)
	defer d.Stop(t)
	apiClient := d.NewClientT(t)
	defer apiClient.Close()

	serviceCount := 3
	// create some services.
	for i := range serviceCount {
		spec := fullSwarmServiceSpec(fmt.Sprintf("test-list-%d", i), uint64(i+1))
		// for whatever reason, the args "-u root", when included, cause these
		// tasks to fail and exit. instead, we'll just pass no args, which
		// works.
		spec.TaskTemplate.ContainerSpec.Args = []string{}
		resp, err := apiClient.ServiceCreate(ctx, client.ServiceCreateOptions{
			Spec:          spec,
			QueryRegistry: false,
		})
		assert.NilError(t, err)
		id := resp.ID
		// we need to wait specifically for the tasks to be running, which the
		// serviceContainerCount function does not do. instead, we'll use a
		// bespoke closure right here.
		poll.WaitOn(t, func(log poll.LogT) poll.Result {
			taskList, err := apiClient.TaskList(ctx, client.TaskListOptions{
				Filters: make(client.Filters).Add("service", id),
			})

			running := 0
			for _, task := range taskList.Items {
				if task.Status.State == swarmtypes.TaskStateRunning {
					running++
				}
			}

			switch {
			case err != nil:
				return poll.Error(err)
			case running == i+1:
				return poll.Success()
			default:
				return poll.Continue(
					"running task count %d (%d total), waiting for %d",
					running, len(taskList.Items), i+1,
				)
			}
		})
	}

	// now, let's do the list operation with no status arg set.
	result, err := apiClient.ServiceList(ctx, client.ServiceListOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Len(result.Items, serviceCount))
	for _, service := range result.Items {
		assert.Check(t, is.Nil(service.ServiceStatus))
	}

	// now try again, but with Status: true. This time, we should have statuses
	result, err = apiClient.ServiceList(ctx, client.ServiceListOptions{Status: true})
	assert.NilError(t, err)
	assert.Check(t, is.Len(result.Items, serviceCount))
	for _, service := range result.Items {
		replicas := *service.Spec.Mode.Replicated.Replicas

		assert.Assert(t, service.ServiceStatus != nil)
		// Use assert.Check to not fail out of the test if this fails
		assert.Check(t, is.Equal(service.ServiceStatus.DesiredTasks, replicas))
		assert.Check(t, is.Equal(service.ServiceStatus.RunningTasks, replicas))
	}
}
