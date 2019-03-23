// +build linux

package daemon

import (
	"context"
	"testing"

	"github.com/docker/docker/container"
	"github.com/docker/docker/daemon/exec"
	"github.com/docker/docker/pkg/signal"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
)

type mockContainerd struct {
	MockContainerdClient
	calledCtx         *context.Context
	calledContainerID *string
	calledID          *string
	calledSig         *int
}

func (cd *mockContainerd) SignalProcess(ctx context.Context, containerID, id string, sig int) error {
	cd.calledCtx = &ctx
	cd.calledContainerID = &containerID
	cd.calledID = &id
	cd.calledSig = &sig
	return nil
}

func TestContainerExecKillNoSuchExec(t *testing.T) {
	mock := mockContainerd{}
	ctx := context.Background()
	d := &Daemon{
		execCommands: exec.NewStore(),
		containerd:   &mock,
	}

	err := d.ContainerExecKill(ctx, "nil", uint64(signal.SignalMap["TERM"]))
	assert.ErrorContains(t, err, "No such exec instance")
	assert.Assert(t, is.Nil(mock.calledCtx))
	assert.Assert(t, is.Nil(mock.calledContainerID))
	assert.Assert(t, is.Nil(mock.calledID))
	assert.Assert(t, is.Nil(mock.calledSig))
}

func TestContainerExecKill(t *testing.T) {
	mock := mockContainerd{}
	ctx := context.Background()
	c := &container.Container{
		ExecCommands: exec.NewStore(),
		State:        &container.State{Running: true},
	}
	ec := &exec.Config{
		ID:          "exec",
		ContainerID: "container",
		Started:     make(chan struct{}),
	}
	d := &Daemon{
		execCommands: exec.NewStore(),
		containers:   container.NewMemoryStore(),
		containerd:   &mock,
	}
	d.containers.Add("container", c)
	d.registerExecCommand(c, ec)

	err := d.ContainerExecKill(ctx, "exec", uint64(signal.SignalMap["TERM"]))
	assert.NilError(t, err)
	assert.Equal(t, *mock.calledCtx, ctx)
	assert.Equal(t, *mock.calledContainerID, "container")
	assert.Equal(t, *mock.calledID, "exec")
	assert.Equal(t, *mock.calledSig, int(signal.SignalMap["TERM"]))
}
