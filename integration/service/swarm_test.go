package service

import (
	"strings"
	"testing"

	swarmtypes "github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/integration/internal/swarm"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"gotest.tools/v3/assert"
	"gotest.tools/v3/poll"
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

func TestSwarmNodeDrainPause(t *testing.T) {
	ctx := setupTest(t)

	d1 := swarm.NewSwarm(ctx, t, testEnv)
	defer d1.Stop(t)

	d2 := daemon.New(t)
	defer d2.Stop(t)
	d2.StartAndSwarmJoin(ctx, t, d1, false)

	apiClient := d1.NewClientT(t)
	defer apiClient.Close()

	managerNodeID := d1.NodeID()
	workerNodeID := d2.NodeID()
	nodeIDs := []string{managerNodeID, workerNodeID}

	poll.WaitOn(t, func(log poll.LogT) poll.Result {
		for _, nodeID := range nodeIDs {
			result, err := apiClient.NodeInspect(ctx, nodeID, client.NodeInspectOptions{})
			if err != nil {
				return poll.Error(err)
			}
			node := result.Node
			if node.Status.State != swarmtypes.NodeStateReady || node.Spec.Availability != swarmtypes.NodeAvailabilityActive {
				return poll.Continue("waiting for node %s to be ready and active: state=%s availability=%s", nodeID, node.Status.State, node.Spec.Availability)
			}
		}
		return poll.Success()
	}, swarm.ServicePoll)

	serviceID := swarm.CreateService(ctx, t, d1, swarm.ServiceWithReplicas(2))
	defer func() {
		_, err := apiClient.ServiceRemove(ctx, serviceID, client.ServiceRemoveOptions{})
		assert.NilError(t, err)
	}()

	runningTasks := func() ([]swarmtypes.Task, error) {
		result, err := apiClient.TaskList(ctx, client.TaskListOptions{
			Filters: make(client.Filters).
				Add("service", serviceID).
				Add("desired-state", "running"),
		})
		if err != nil {
			return nil, err
		}

		tasks := make([]swarmtypes.Task, 0, len(result.Items))
		for _, task := range result.Items {
			if task.Status.State == swarmtypes.TaskStateRunning {
				tasks = append(tasks, task)
			}
		}
		return tasks, nil
	}

	waitForTaskDistribution := func(expected map[string]int, preservedTasks map[string]string) func(log poll.LogT) poll.Result {
		return func(log poll.LogT) poll.Result {
			tasks, err := runningTasks()
			if err != nil {
				return poll.Error(err)
			}

			actual := make(map[string]int, len(expected))
			runningTaskNodes := make(map[string]string, len(tasks))
			for _, task := range tasks {
				actual[task.NodeID]++
				runningTaskNodes[task.ID] = task.NodeID
			}

			for nodeID, count := range expected {
				if actual[nodeID] != count {
					return poll.Continue("running task distribution is %v, expected %v", actual, expected)
				}
			}
			if len(tasks) != expected[managerNodeID]+expected[workerNodeID] {
				return poll.Continue("running task distribution is %v, expected %v", actual, expected)
			}
			for taskID, nodeID := range preservedTasks {
				if runningTaskNodes[taskID] != nodeID {
					return poll.Continue("waiting for task %s to remain running on node %s; running tasks: %v", taskID, nodeID, runningTaskNodes)
				}
			}
			return poll.Success()
		}
	}

	updateNodeAvailability := func(nodeID string, availability swarmtypes.NodeAvailability) {
		result, err := apiClient.NodeInspect(ctx, nodeID, client.NodeInspectOptions{})
		assert.NilError(t, err)
		node := result.Node
		node.Spec.Availability = availability
		_, err = apiClient.NodeUpdate(ctx, nodeID, client.NodeUpdateOptions{
			Version: node.Version,
			Spec:    node.Spec,
		})
		assert.NilError(t, err)
	}

	waitForNodeAvailability := func(nodeID string, availability swarmtypes.NodeAvailability) {
		poll.WaitOn(t, func(log poll.LogT) poll.Result {
			result, err := apiClient.NodeInspect(ctx, nodeID, client.NodeInspectOptions{})
			if err != nil {
				return poll.Error(err)
			}
			if result.Node.Spec.Availability != availability {
				return poll.Continue("waiting for node %s availability %s, currently %s", nodeID, availability, result.Node.Spec.Availability)
			}
			return poll.Success()
		}, swarm.ServicePoll)
	}

	scaleService := func(replicas uint64) {
		result, err := apiClient.ServiceInspect(ctx, serviceID, client.ServiceInspectOptions{})
		assert.NilError(t, err)
		service := result.Service
		service.Spec.Mode.Replicated.Replicas = &replicas
		_, err = apiClient.ServiceUpdate(ctx, serviceID, client.ServiceUpdateOptions{
			Version: service.Version,
			Spec:    service.Spec,
		})
		assert.NilError(t, err)
	}

	poll.WaitOn(t, waitForTaskDistribution(map[string]int{
		managerNodeID: 1,
		workerNodeID:  1,
	}, nil), swarm.ServicePoll)

	updateNodeAvailability(workerNodeID, swarmtypes.NodeAvailabilityDrain)
	poll.WaitOn(t, waitForTaskDistribution(map[string]int{
		managerNodeID: 2,
		workerNodeID:  0,
	}, nil), swarm.ServicePoll)

	updateNodeAvailability(workerNodeID, swarmtypes.NodeAvailabilityActive)
	waitForNodeAvailability(workerNodeID, swarmtypes.NodeAvailabilityActive)

	scaleService(1)
	poll.WaitOn(t, waitForTaskDistribution(map[string]int{
		managerNodeID: 1,
		workerNodeID:  0,
	}, nil), swarm.ServicePoll)

	scaleService(2)
	poll.WaitOn(t, waitForTaskDistribution(map[string]int{
		managerNodeID: 1,
		workerNodeID:  1,
	}, nil), swarm.ServicePoll)

	tasks, err := runningTasks()
	assert.NilError(t, err)
	workerTasks := make(map[string]string, 1)
	for _, task := range tasks {
		if task.NodeID == workerNodeID {
			workerTasks[task.ID] = task.NodeID
		}
	}
	assert.Equal(t, len(workerTasks), 1)

	updateNodeAvailability(workerNodeID, swarmtypes.NodeAvailabilityPause)
	waitForNodeAvailability(workerNodeID, swarmtypes.NodeAvailabilityPause)

	scaleService(4)
	poll.WaitOn(t, waitForTaskDistribution(map[string]int{
		managerNodeID: 3,
		workerNodeID:  1,
	}, workerTasks), swarm.ServicePoll)
}
