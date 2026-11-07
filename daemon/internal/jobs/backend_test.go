package jobs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/v2/errdefs"
	containerstatev0 "github.com/moby/moby/v2/extpoints/containerstate/v0"
	"gotest.tools/v3/assert"
)

type exitedStateWatcher struct {
	containerstatev0.ContainerStateWatcher
}

func (exitedStateWatcher) Wait(ctx context.Context, req *containerstatev0.WaitRequest) (*containerstatev0.WaitReply, error) {
	if len(req.Statuses) > 0 {
		// A second wait must not delay delivery of the exit already observed.
		<-ctx.Done()
		return nil, ctx.Err()
	}

	return &containerstatev0.WaitReply{Current: &containerstatev0.ContainerState{
		Status: containerstatev0.StatusExited,
		Exit:   &containerstatev0.Exit{ExitCode: 0},
	}}, nil
}

func TestBackendWaitReturnsExistingExitImmediately(t *testing.T) {
	t.Parallel()

	backend := &engineBackend{state: exitedStateWatcher{}}
	waitC, err := backend.ContainerWait(t.Context(), "exited")
	assert.NilError(t, err)

	select {
	case status := <-waitC:
		assert.Equal(t, status.ExitCode(), 0)
		assert.NilError(t, status.Err())
	default:
		t.Fatal("the existing exit must be ready before arming a timeout")
	}
}

// flakyStateWatcher reports a running container whose first wait for an exit
// fails, as when the wait falls behind the container's status changes.
type flakyStateWatcher struct {
	containerstatev0.ContainerStateWatcher

	mu    sync.Mutex
	waits int
}

func (w *flakyStateWatcher) Wait(_ context.Context, req *containerstatev0.WaitRequest) (*containerstatev0.WaitReply, error) {
	if len(req.Statuses) == 0 {
		return &containerstatev0.WaitReply{Current: &containerstatev0.ContainerState{Status: containerstatev0.StatusRunning}}, nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	w.waits++
	if w.waits == 1 {
		return nil, errdefs.Unavailable(errors.New("container status changes were not consumed fast enough"))
	}

	return &containerstatev0.WaitReply{Current: &containerstatev0.ContainerState{
		Status: containerstatev0.StatusExited,
		Exit:   &containerstatev0.Exit{ExitCode: 3},
	}}, nil
}

func TestBackendWaitRetriesFailedWait(t *testing.T) {
	t.Parallel()

	backend := &engineBackend{state: &flakyStateWatcher{}}
	waitC, err := backend.ContainerWait(t.Context(), "running")
	assert.NilError(t, err)

	select {
	case status := <-waitC:
		// A failed wait is not an exit; the actual exit is delivered.
		assert.Equal(t, status.ExitCode(), 3)
		assert.NilError(t, status.Err())
	case <-time.After(10 * time.Second):
		t.Fatal("no exit delivered")
	}
}
