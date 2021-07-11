package swarm

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	swarmtypes "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"gotest.tools/v3/poll"
)

// NoTasksForService verifies that there are no more tasks for the given service
func NoTasksForService(ctx context.Context, client client.ServiceAPIClient, serviceID string) func(log poll.LogT) poll.Result {
	return func(log poll.LogT) poll.Result {
		tasks, err := client.TaskList(ctx, types.TaskListOptions{
			Filters: filters.NewArgs(
				filters.Arg("service", serviceID),
			),
		})
		if err == nil {
			if len(tasks) == 0 {
				return poll.Success()
			}
			if len(tasks) > 0 {
				return poll.Continue("task count for service %s at %d waiting for 0", serviceID, len(tasks))
			}
			return poll.Continue("waiting for tasks for service %s to be deleted", serviceID)
		}
		// TODO we should not use an error as indication that the tasks are gone. There may be other reasons for an error to occur.
		return poll.Success()
	}
}

// NoTasks verifies that all tasks are gone
func NoTasks(ctx context.Context, client client.ServiceAPIClient) func(log poll.LogT) poll.Result {
	return func(log poll.LogT) poll.Result {
		tasks, err := client.TaskList(ctx, types.TaskListOptions{})
		switch {
		case err != nil:
			return poll.Error(err)
		case len(tasks) == 0:
			return poll.Success()
		default:
			return poll.Continue("waiting for all tasks to be removed: task count at %d", len(tasks))
		}
	}
}

// RunningTasksCount verifies there are `instances` tasks running for `serviceID`
func RunningTasksCount(client client.ServiceAPIClient, serviceID string, instances uint64) func(log poll.LogT) poll.Result {
	return func(log poll.LogT) poll.Result {
		filter := filters.NewArgs()
		filter.Add("service", serviceID)
		tasks, err := client.TaskList(context.Background(), types.TaskListOptions{
			Filters: filter,
		})
		var running int
		var taskError string
		for _, task := range tasks {
			switch task.Status.State {
			case swarmtypes.TaskStateRunning:
				running++
			case swarmtypes.TaskStateFailed:
				if task.Status.Err != "" {
					taskError = task.Status.Err
				}
			}
		}

		switch {
		case err != nil:
			return poll.Error(err)
		case running > int(instances):
			return poll.Continue("waiting for tasks to terminate")
		case running < int(instances) && taskError != "":
			return poll.Continue("waiting for tasks to enter run state. task failed with error: %s", taskError)
		case running == int(instances):
			return poll.Success()
		default:
			return poll.Continue("running task count at %d waiting for %d (total tasks: %d)", running, instances, len(tasks))
		}
	}
}

// JobComplete is a poll function for determining that a ReplicatedJob is
// completed additionally, while polling, it verifies that the job never
// exceeds MaxConcurrent running tasks
func JobComplete(client client.CommonAPIClient, service swarmtypes.Service) func(log poll.LogT) poll.Result {
	filter := filters.NewArgs()
	filter.Add("service", service.ID)

	var jobIteration swarmtypes.Version
	if service.JobStatus != nil {
		jobIteration = service.JobStatus.JobIteration
	}

	maxRaw := service.Spec.Mode.ReplicatedJob.MaxConcurrent
	totalRaw := service.Spec.Mode.ReplicatedJob.TotalCompletions

	max := int(*maxRaw)
	total := int(*totalRaw)

	previousResult := ""

	return func(log poll.LogT) poll.Result {
		tasks, err := client.TaskList(context.Background(), types.TaskListOptions{
			Filters: filter,
		})

		if err != nil {
			poll.Error(err)
		}

		var running int
		var completed int

		var runningSlot []int
		var runningID []string

		for _, task := range tasks {
			// make sure the task has the same job iteration
			if task.JobIteration == nil || task.JobIteration.Index != jobIteration.Index {
				continue
			}
			switch task.Status.State {
			case swarmtypes.TaskStateRunning:
				running++
				runningSlot = append(runningSlot, task.Slot)
				runningID = append(runningID, task.ID)
			case swarmtypes.TaskStateComplete:
				completed++
			}
		}

		switch {
		case running > max:
			return poll.Error(fmt.Errorf(
				"number of running tasks (%v) exceeds max (%v)", running, max,
			))
		case (completed + running) > total:
			return poll.Error(fmt.Errorf(
				"number of tasks exceeds total (%v), %v running and %v completed",
				total, running, completed,
			))
		case completed == total && running == 0:
			return poll.Success()
		default:
			newRes := fmt.Sprintf(
				"Completed: %2d Running: %v\n\t%v",
				completed, runningSlot, runningID,
			)
			if newRes == previousResult {
			} else {
				previousResult = newRes
			}

			return poll.Continue(
				"Job not yet finished, %v completed and %v running out of %v total",
				completed, running, total,
			)
		}
	}
}

// FailedTasksCount verifies there are `instances` tasks running for `serviceID`
func FailedTasksCount(client client.ServiceAPIClient, serviceID string, instances uint64) func(log poll.LogT) poll.Result {
	return func(log poll.LogT) poll.Result {
		filter := filters.NewArgs()
		filter.Add("service", serviceID)
		tasks, err := client.TaskList(context.Background(), types.TaskListOptions{
			Filters: filter,
		})
		if err != nil {
			return poll.Error(err)
		}

		var failed uint64
		for _, task := range tasks {
			switch task.Status.State {
			case swarmtypes.TaskStateFailed:
				failed++
			}
		}

		switch {
		case failed == instances:
			return poll.Success()
		default:
			return poll.Continue("waiting for %v more tasks to fail.", (instances - failed))
		}
	}
}

// CompletedTasksCount verifies there are `instances` tasks running for `serviceID`
func CompletedTasksCount(client client.ServiceAPIClient, serviceID string, instances uint64) func(log poll.LogT) poll.Result {
	return func(log poll.LogT) poll.Result {
		filter := filters.NewArgs()
		filter.Add("service", serviceID)
		tasks, err := client.TaskList(context.Background(), types.TaskListOptions{
			Filters: filter,
		})
		if err != nil {
			return poll.Error(err)
		}

		var completed uint64
		for _, task := range tasks {
			switch task.Status.State {
			case swarmtypes.TaskStateComplete:
				completed++
			}
		}

		switch {
		case completed == instances:
			return poll.Success()
		default:
			return poll.Continue("waiting for %v more tasks to complete.", (instances - completed))
		}
	}
}
