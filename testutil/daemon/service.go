package daemon

import (
	"context"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/testutil"
	"gotest.tools/assert"
)

// ServiceConstructor defines a swarm service constructor function
type ServiceConstructor func(*swarm.Service)

func (d *Daemon) createServiceWithOptions(t assert.TestingT, opts types.ServiceCreateOptions, f ...ServiceConstructor) string {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	var service swarm.Service
	for _, fn := range f {
		fn(&service)
	}

	cli := d.NewClientT(t)
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := cli.ServiceCreate(ctx, service.Spec, opts)
	assert.NilError(t, err)
	return res.ID
}

// CreateService creates a swarm service given the specified service constructor
func (d *Daemon) CreateService(t assert.TestingT, f ...ServiceConstructor) string {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	return d.createServiceWithOptions(t, types.ServiceCreateOptions{}, f...)
}

// GetService returns the swarm service corresponding to the specified id
func (d *Daemon) GetService(t assert.TestingT, id string) *swarm.Service {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	cli := d.NewClientT(t)
	defer cli.Close()

	service, _, err := cli.ServiceInspectWithRaw(context.Background(), id, types.ServiceInspectOptions{})
	assert.NilError(t, err)
	return &service
}

// GetServiceTasks returns the swarm tasks for the specified service
func (d *Daemon) GetServiceTasks(t assert.TestingT, service string) []swarm.Task {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	cli := d.NewClientT(t)
	defer cli.Close()

	filterArgs := filters.NewArgs()
	filterArgs.Add("desired-state", "running")
	filterArgs.Add("service", service)

	options := types.TaskListOptions{
		Filters: filterArgs,
	}

	tasks, err := cli.TaskList(context.Background(), options)
	assert.NilError(t, err)
	return tasks
}

// UpdateService updates a swarm service with the specified service constructor
func (d *Daemon) UpdateService(t assert.TestingT, service *swarm.Service, f ...ServiceConstructor) {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	cli := d.NewClientT(t)
	defer cli.Close()

	for _, fn := range f {
		fn(service)
	}

	_, err := cli.ServiceUpdate(context.Background(), service.ID, service.Version, service.Spec, types.ServiceUpdateOptions{})
	assert.NilError(t, err)
}

// RemoveService removes the specified service
func (d *Daemon) RemoveService(t assert.TestingT, id string) {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	cli := d.NewClientT(t)
	defer cli.Close()

	err := cli.ServiceRemove(context.Background(), id)
	assert.NilError(t, err)
}

// ListServices returns the list of the current swarm services
func (d *Daemon) ListServices(t assert.TestingT) []swarm.Service {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	cli := d.NewClientT(t)
	defer cli.Close()

	services, err := cli.ServiceList(context.Background(), types.ServiceListOptions{})
	assert.NilError(t, err)
	return services
}

// GetTask returns the swarm task identified by the specified id
func (d *Daemon) GetTask(t assert.TestingT, id string) swarm.Task {
	if ht, ok := t.(testutil.HelperT); ok {
		ht.Helper()
	}
	cli := d.NewClientT(t)
	defer cli.Close()

	task, _, err := cli.TaskInspectWithRaw(context.Background(), id)
	assert.NilError(t, err)
	return task
}
