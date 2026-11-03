package daemon // import "github.com/docker/docker/integration-cli/daemon"

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/errdefs"
	"gotest.tools/v3/assert"
)

// CheckServiceTasksInState returns the number of tasks with a matching state,
// and optional message substring.
func (d *Daemon) CheckServiceTasksInState(ctx context.Context, service string, state swarm.TaskState, message string) func(*testing.T) (interface{}, string) {
	return func(c *testing.T) (interface{}, string) {
		tasks := d.GetServiceTasks(ctx, c, service)
		var count int
		for _, task := range tasks {
			if task.Status.State == state {
				if message == "" || strings.Contains(task.Status.Message, message) {
					count++
				}
			}
		}
		return count, ""
	}
}

// CheckServiceTasksInStateWithError returns the number of tasks with a matching state,
// and optional message substring.
func (d *Daemon) CheckServiceTasksInStateWithError(ctx context.Context, service string, state swarm.TaskState, errorMessage string) func(*testing.T) (interface{}, string) {
	return func(c *testing.T) (interface{}, string) {
		tasks := d.GetServiceTasks(ctx, c, service)
		var count int
		for _, task := range tasks {
			if task.Status.State == state {
				if errorMessage == "" || strings.Contains(task.Status.Err, errorMessage) {
					count++
				}
			}
		}
		return count, ""
	}
}

// CheckServiceRunningTasks returns the number of running tasks for the specified service
func (d *Daemon) CheckServiceRunningTasks(ctx context.Context, service string) func(*testing.T) (interface{}, string) {
	return d.CheckServiceTasksInState(ctx, service, swarm.TaskStateRunning, "")
}

// CheckServiceUpdateState returns the current update state for the specified service
func (d *Daemon) CheckServiceUpdateState(ctx context.Context, service string) func(*testing.T) (interface{}, string) {
	return func(c *testing.T) (interface{}, string) {
		service := d.GetService(ctx, c, service)
		if service.UpdateStatus == nil {
			return "", ""
		}
		return service.UpdateStatus.State, ""
	}
}

// CheckPluginRunning returns the runtime state of the plugin
func (d *Daemon) CheckPluginRunning(ctx context.Context, plugin string) func(c *testing.T) (interface{}, string) {
	return func(c *testing.T) (interface{}, string) {
		apiclient := d.NewClientT(c)
		resp, _, err := apiclient.PluginInspectWithRaw(ctx, plugin)
		if errdefs.IsNotFound(err) {
			return false, fmt.Sprintf("%v", err)
		}
		assert.NilError(c, err)
		return resp.Enabled, fmt.Sprintf("%+v", resp)
	}
}

// CheckPluginImage returns the runtime state of the plugin
func (d *Daemon) CheckPluginImage(ctx context.Context, plugin string) func(c *testing.T) (interface{}, string) {
	return func(c *testing.T) (interface{}, string) {
		apiclient := d.NewClientT(c)
		resp, _, err := apiclient.PluginInspectWithRaw(ctx, plugin)
		if errdefs.IsNotFound(err) {
			return false, fmt.Sprintf("%v", err)
		}
		assert.NilError(c, err)
		return resp.PluginReference, fmt.Sprintf("%+v", resp)
	}
}

// CheckServiceTasks returns the number of tasks for the specified service
func (d *Daemon) CheckServiceTasks(ctx context.Context, service string) func(*testing.T) (interface{}, string) {
	return func(c *testing.T) (interface{}, string) {
		tasks := d.GetServiceTasks(ctx, c, service)
		return len(tasks), ""
	}
}

// CheckRunningTaskNetworks returns the number of times each network is referenced from a task.
func (d *Daemon) CheckRunningTaskNetworks(ctx context.Context) func(t *testing.T) (interface{}, string) {
	return func(t *testing.T) (interface{}, string) {
		cli := d.NewClientT(t)
		defer cli.Close()

		tasks, err := cli.TaskList(ctx, types.TaskListOptions{
			Filters: filters.NewArgs(filters.Arg("desired-state", "running")),
		})
		assert.NilError(t, err)

		result := make(map[string]int)
		for _, task := range tasks {
			for _, network := range task.Spec.Networks {
				result[network.Target]++
			}
		}
		return result, ""
	}
}

// CheckRunningTaskImages returns the times each image is running as a task.
func (d *Daemon) CheckRunningTaskImages(ctx context.Context) func(t *testing.T) (interface{}, string) {
	return func(t *testing.T) (interface{}, string) {
		cli := d.NewClientT(t)
		defer cli.Close()

		tasks, err := cli.TaskList(ctx, types.TaskListOptions{
			Filters: filters.NewArgs(filters.Arg("desired-state", "running")),
		})
		assert.NilError(t, err)

		result := make(map[string]int)
		for _, task := range tasks {
			if task.Status.State == swarm.TaskStateRunning && task.Spec.ContainerSpec != nil {
				result[task.Spec.ContainerSpec.Image]++
			}
		}
		return result, ""
	}
}

// CheckNodeReadyCount returns the number of ready node on the swarm
func (d *Daemon) CheckNodeReadyCount(ctx context.Context) func(t *testing.T) (interface{}, string) {
	return func(t *testing.T) (interface{}, string) {
		nodes := d.ListNodes(ctx, t)
		var readyCount int
		for _, node := range nodes {
			if node.Status.State == swarm.NodeStateReady {
				readyCount++
			}
		}
		return readyCount, ""
	}
}

// CheckLocalNodeState returns the current swarm node state
func (d *Daemon) CheckLocalNodeState(ctx context.Context) func(t *testing.T) (interface{}, string) {
	return func(t *testing.T) (interface{}, string) {
		info := d.SwarmInfo(ctx, t)
		return info.LocalNodeState, fmt.Sprintf("local node state is %q", info.LocalNodeState)
	}
}

// CheckControlAvailable returns the current swarm control available
func (d *Daemon) CheckControlAvailable(ctx context.Context) func(t *testing.T) (interface{}, string) {
	return func(t *testing.T) (interface{}, string) {
		info := d.SwarmInfo(ctx, t)
		assert.Equal(t, info.LocalNodeState, swarm.LocalNodeStateActive)
		return info.ControlAvailable, ""
	}
}

// CheckLeader returns whether there is a leader on the swarm or not
func (d *Daemon) CheckLeader(ctx context.Context) func(t *testing.T) (interface{}, string) {
	return func(t *testing.T) (interface{}, string) {
		cli := d.NewClientT(t)
		defer cli.Close()

		errList := "could not get node list"

		ls, err := cli.NodeList(ctx, types.NodeListOptions{})
		if err != nil {
			return err, errList
		}

		for _, node := range ls {
			if node.ManagerStatus != nil && node.ManagerStatus.Leader {
				return nil, ""
			}
		}
		return fmt.Errorf("no leader"), "could not find leader"
	}
}

// CmdRetryOutOfSequence tries the specified command against the current daemon
// up to 10 times, retrying if it encounters an "update out of sequence" error.
func (d *Daemon) CmdRetryOutOfSequence(args ...string) (string, error) {
	var (
		output string
		err    error
	)

	for i := 0; i < 10; i++ {
		output, err = d.Cmd(args...)
		// error, no error, whatever. if we don't have "update out of
		// sequence", we don't retry, we just return.
		if !strings.Contains(output, "update out of sequence") {
			return output, err
		}
	}

	// otherwise, once all of our attempts have been exhausted, just return
	// whatever the last values were.
	return output, err
}
