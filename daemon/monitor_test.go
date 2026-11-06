package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	containerdcli "github.com/containerd/containerd/v2/client"
	"github.com/moby/moby/v2/daemon/container"
	libcontainerdtypes "github.com/moby/moby/v2/daemon/internal/libcontainerd/types"
	"gotest.tools/v3/assert"
)

// monitorMockTask implements the task methods used by the exit-event check.
type monitorMockTask struct {
	libcontainerdtypes.Task
	status    containerdcli.ProcessStatus
	exitCode  uint32
	statusErr error
}

func (t *monitorMockTask) Pid() uint32 {
	return 42
}

func (t *monitorMockTask) Status(context.Context) (containerdcli.Status, error) {
	if t.statusErr != nil {
		return containerdcli.Status{}, t.statusErr
	}
	return containerdcli.Status{Status: t.status, ExitStatus: t.exitCode}, nil
}

func TestShouldIgnoreExitEventWithLock(t *testing.T) {
	currentContainerID := strings.Repeat("a", dockerContainerIDLength)

	runningState := func(sequence uint64, tsk libcontainerdtypes.Task) *container.State {
		state := &container.State{}
		state.SetRunning(nil, tsk, time.Now())
		state.RunID = sequence
		return state
	}
	currentRun := &container.Container{
		ID:    currentContainerID,
		State: runningState(1, &monitorMockTask{}),
	}
	currentRunID := currentRun.C8dContainerID()

	tests := []struct {
		name       string
		state      *container.State
		event      libcontainerdtypes.EventInfo
		wantIgnore bool
	}{
		{
			name:       "exit from current run is processed",
			state:      runningState(1, &monitorMockTask{}),
			event:      libcontainerdtypes.EventInfo{ContainerID: currentRunID},
			wantIgnore: false,
		},
		{
			name:       "exit from previous run is ignored",
			state:      runningState(1, &monitorMockTask{}),
			event:      libcontainerdtypes.EventInfo{ContainerID: "previous-run"},
			wantIgnore: true,
		},
		{
			name: "exit from previous run is ignored while paused",
			state: func() *container.State {
				state := runningState(1, &monitorMockTask{})
				state.Paused = true
				return state
			}(),
			event:      libcontainerdtypes.EventInfo{ContainerID: "previous-run"},
			wantIgnore: true,
		},
		{
			// For a legacy run without a per-run ID, task status decides.
			name:       "exit is ignored while the task is still running",
			state:      runningState(0, &monitorMockTask{status: containerdcli.Running}),
			event:      libcontainerdtypes.EventInfo{ContainerID: currentContainerID},
			wantIgnore: true,
		},
		{
			name:       "exit of a stopped task is processed",
			state:      runningState(0, &monitorMockTask{status: containerdcli.Stopped}),
			event:      libcontainerdtypes.EventInfo{ContainerID: currentContainerID},
			wantIgnore: false,
		},
		{
			name:       "exit with a different code than the stopped task is ignored",
			state:      runningState(0, &monitorMockTask{status: containerdcli.Stopped, exitCode: 1}),
			event:      libcontainerdtypes.EventInfo{ContainerID: currentContainerID},
			wantIgnore: true,
		},
		{
			name:       "exit is processed when the task status is unavailable",
			state:      runningState(0, &monitorMockTask{statusErr: errors.New("no such task")}),
			event:      libcontainerdtypes.EventInfo{ContainerID: currentContainerID},
			wantIgnore: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			daemon := &Daemon{}
			ctr := &container.Container{
				ID:    currentContainerID,
				State: tc.state,
			}

			ctr.Lock()
			got := daemon.shouldIgnoreExitEventWithLock(ctr, &tc.event)
			ctr.Unlock()

			assert.Equal(t, got, tc.wantIgnore)
		})
	}
}
