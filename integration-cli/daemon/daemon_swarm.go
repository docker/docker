package daemon // import "github.com/docker/docker/integration-cli/daemon"

import (
	"context"
	"fmt"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/go-check/check"
	"gotest.tools/assert"
)

// CheckServiceTasksInState returns the number of tasks with a matching state,
// and optional message substring.
func (d *Daemon) CheckServiceTasksInState(service string, state swarm.TaskState, message string) func(*check.C) (interface{}, check.CommentInterface) {
	return func(c *check.C) (interface{}, check.CommentInterface) {
		tasks := d.GetServiceTasks(c, service)
		var count int
		for _, task := range tasks {
			if task.Status.State == state {
				if message == "" || strings.Contains(task.Status.Message, message) {
					count++
				}
			}
		}
		return count, nil
	}
}

// CheckServiceTasksInStateWithError returns the number of tasks with a matching state,
// and optional message substring.
func (d *Daemon) CheckServiceTasksInStateWithError(service string, state swarm.TaskState, errorMessage string) func(*check.C) (interface{}, check.CommentInterface) {
	return func(c *check.C) (interface{}, check.CommentInterface) {
		tasks := d.GetServiceTasks(c, service)
		var count int
		for _, task := range tasks {
			if task.Status.State == state {
				if errorMessage == "" || strings.Contains(task.Status.Err, errorMessage) {
					count++
				}
			}
		}
		return count, nil
	}
}

// CheckServiceRunningTasks returns the number of running tasks for the specified service
func (d *Daemon) CheckServiceRunningTasks(service string) func(*check.C) (interface{}, check.CommentInterface) {
	return d.CheckServiceTasksInState(service, swarm.TaskStateRunning, "")
}

// CheckServiceUpdateState returns the current update state for the specified service
func (d *Daemon) CheckServiceUpdateState(service string) func(*check.C) (interface{}, check.CommentInterface) {
	return func(c *check.C) (interface{}, check.CommentInterface) {
		service := d.GetService(c, service)
		if service.UpdateStatus == nil {
			return "", nil
		}
		return service.UpdateStatus.State, nil
	}
}

// CheckPluginRunning returns the runtime state of the plugin
func (d *Daemon) CheckPluginRunning(plugin string) func(c *check.C) (interface{}, check.CommentInterface) {
	return func(c *check.C) (interface{}, check.CommentInterface) {
		apiclient := d.NewClientT(c)
		resp, _, err := apiclient.PluginInspectWithRaw(context.Background(), plugin)
		if client.IsErrNotFound(err) {
			return false, check.Commentf("%v", err)
		}
		assert.NilError(c, err)
		return resp.Enabled, check.Commentf("%+v", resp)
	}
}

// CheckPluginImage returns the runtime state of the plugin
func (d *Daemon) CheckPluginImage(plugin string) func(c *check.C) (interface{}, check.CommentInterface) {
	return func(c *check.C) (interface{}, check.CommentInterface) {
		apiclient := d.NewClientT(c)
		resp, _, err := apiclient.PluginInspectWithRaw(context.Background(), plugin)
		if client.IsErrNotFound(err) {
			return false, check.Commentf("%v", err)
		}
		assert.NilError(c, err)
		return resp.PluginReference, check.Commentf("%+v", resp)
	}
}

// CheckServiceTasks returns the number of tasks for the specified service
func (d *Daemon) CheckServiceTasks(service string) func(*check.C) (interface{}, check.CommentInterface) {
	return func(c *check.C) (interface{}, check.CommentInterface) {
		tasks := d.GetServiceTasks(c, service)
		return len(tasks), nil
	}
}

// CheckRunningTaskNetworks returns the number of times each network is referenced from a task.
func (d *Daemon) CheckRunningTaskNetworks(c *check.C) (interface{}, check.CommentInterface) {
	cli := d.NewClientT(c)
	defer cli.Close()

	filterArgs := filters.NewArgs()
	filterArgs.Add("desired-state", "running")

	options := types.TaskListOptions{
		Filters: filterArgs,
	}

	tasks, err := cli.TaskList(context.Background(), options)
	assert.NilError(c, err)

	result := make(map[string]int)
	for _, task := range tasks {
		for _, network := range task.Spec.Networks {
			result[network.Target]++
		}
	}
	return result, nil
}

// CheckRunningTaskImages returns the times each image is running as a task.
func (d *Daemon) CheckRunningTaskImages(c *check.C) (interface{}, check.CommentInterface) {
	cli := d.NewClientT(c)
	defer cli.Close()

	filterArgs := filters.NewArgs()
	filterArgs.Add("desired-state", "running")

	options := types.TaskListOptions{
		Filters: filterArgs,
	}

	tasks, err := cli.TaskList(context.Background(), options)
	assert.NilError(c, err)

	result := make(map[string]int)
	for _, task := range tasks {
		if task.Status.State == swarm.TaskStateRunning && task.Spec.ContainerSpec != nil {
			result[task.Spec.ContainerSpec.Image]++
		}
	}
	return result, nil
}

// CheckNodeReadyCount returns the number of ready node on the swarm
func (d *Daemon) CheckNodeReadyCount(c *check.C) (interface{}, check.CommentInterface) {
	nodes := d.ListNodes(c)
	var readyCount int
	for _, node := range nodes {
		if node.Status.State == swarm.NodeStateReady {
			readyCount++
		}
	}
	return readyCount, nil
}

// CheckLocalNodeState returns the current swarm node state
func (d *Daemon) CheckLocalNodeState(c *check.C) (interface{}, check.CommentInterface) {
	info := d.SwarmInfo(c)
	return info.LocalNodeState, nil
}

// CheckControlAvailable returns the current swarm control available
func (d *Daemon) CheckControlAvailable(c *check.C) (interface{}, check.CommentInterface) {
	info := d.SwarmInfo(c)
	assert.Equal(c, info.LocalNodeState, swarm.LocalNodeStateActive)
	return info.ControlAvailable, nil
}

// CheckLeader returns whether there is a leader on the swarm or not
func (d *Daemon) CheckLeader(c *check.C) (interface{}, check.CommentInterface) {
	cli := d.NewClientT(c)
	defer cli.Close()

	errList := check.Commentf("could not get node list")

	ls, err := cli.NodeList(context.Background(), types.NodeListOptions{})
	if err != nil {
		return err, errList
	}

	for _, node := range ls {
		if node.ManagerStatus != nil && node.ManagerStatus.Leader {
			return nil, nil
		}
	}
	return fmt.Errorf("no leader"), check.Commentf("could not find leader")
}
