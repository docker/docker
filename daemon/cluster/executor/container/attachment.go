package container // import "github.com/docker/docker/daemon/cluster/executor/container"

import (
	"context"

	executorpkg "github.com/docker/docker/daemon/cluster/executor"
	"github.com/docker/swarmkit/agent/exec"
	"github.com/docker/swarmkit/api"
)

// networkAttacherController implements agent.Controller against docker's API.
//
// networkAttacherController manages the lifecycle of network
// attachment of a docker unmanaged container managed as a task from
// agent point of view. It provides network attachment information to
// the unmanaged container for it to attach to the network and run.
type networkAttacherController struct {
	backend executorpkg.Backend
	task    *api.Task
	adapter *containerAdapter
	closed  chan struct{}
}

func newNetworkAttacherController(b executorpkg.Backend, i executorpkg.ImageBackend, v executorpkg.VolumeBackend, task *api.Task, node *api.NodeDescription, dependencies exec.DependencyGetter) (*networkAttacherController, error) {
	adapter, err := newContainerAdapter(b, i, v, task, node, dependencies)
	if err != nil {
		return nil, err
	}

	return &networkAttacherController{
		backend: b,
		task:    task,
		adapter: adapter,
		closed:  make(chan struct{}),
	}, nil
}

func (nc *networkAttacherController) Update(ctx context.Context, t *api.Task) error {
	return nil
}

func (nc *networkAttacherController) Prepare(ctx context.Context) error {
	// Make sure all the networks that the task needs are created.
	return nc.adapter.createNetworks(ctx)
}

func (nc *networkAttacherController) Start(ctx context.Context) error {
	return nc.adapter.networkAttach(ctx)
}

func (nc *networkAttacherController) Wait(pctx context.Context) error {
	ctx, cancel := context.WithCancel(pctx)
	defer cancel()

	return nc.adapter.waitForDetach(ctx)
}

func (nc *networkAttacherController) Shutdown(ctx context.Context) error {
	return nil
}

func (nc *networkAttacherController) Terminate(ctx context.Context) error {
	return nil
}

func (nc *networkAttacherController) Remove(ctx context.Context) error {
	return nil
}

func (nc *networkAttacherController) Close() error {
	return nil
}
