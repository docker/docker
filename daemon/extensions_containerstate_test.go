// SPDX-FileCopyrightText: Copyright The Moby Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/daemon/container"
	containerstatev0 "github.com/moby/moby/v2/extpoints/containerstate/v0"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gotest.tools/v3/assert"
)

func newContainerStateTestDaemon(t *testing.T, state *container.State) (*Daemon, *container.Container) {
	t.Helper()

	ctr := &container.Container{ID: "container-state-test-id", Config: &containertypes.Config{}, State: state}
	store := container.NewMemoryStore()
	store.Add(ctr.ID, ctr)
	replica, err := container.NewViewDB()
	assert.NilError(t, err)
	assert.NilError(t, replica.Save(ctr))

	d := &Daemon{
		containers:        store,
		containersReplica: replica,
		startupDone:       make(chan struct{}),
	}
	close(d.startupDone)
	return d, ctr
}

// setState applies set to ctr's state while holding the container lock, as
// the daemon does.
func setState(ctr *container.Container, set func(*container.State)) {
	ctr.Lock()
	defer ctr.Unlock()

	set(ctr.State)
}

type containerStateWaitResult struct {
	reply *containerstatev0.WaitReply
	err   error
}

func startContainerStateWait(ctx context.Context, d *Daemon, id string, statuses ...string) <-chan containerStateWaitResult {
	waited := make(chan containerStateWaitResult, 1)
	go func() {
		reply, err := containerStateWatcher{daemon: d}.Wait(ctx, &containerstatev0.WaitRequest{
			ContainerID: id,
			Statuses:    statuses,
		})
		waited <- containerStateWaitResult{reply, err}
	}()
	return waited
}

func TestContainerStateWaitSkipsRestart(t *testing.T) {
	t.Parallel()

	d, ctr := newContainerStateTestDaemon(t, &container.State{Running: true, StartedAt: time.Now()})

	// Start waiting while the container is between restart-policy attempts.
	setState(ctr, func(s *container.State) { s.SetRestarting(&container.ExitStatus{ExitCode: 23}) })
	waited := startContainerStateWait(t.Context(), d, ctr.ID, containerstatev0.StatusExited)
	assertWaiting := func() {
		select {
		case <-waited:
			t.Fatal("Wait returned before the final exit")
		case <-time.After(20 * time.Millisecond):
		}
	}
	assertWaiting()

	setState(ctr, func(s *container.State) { s.SetRunning(nil, nil, time.Now()) })
	assertWaiting()

	setState(ctr, func(s *container.State) {
		s.SetStopped(&container.ExitStatus{ExitCode: 42, Error: errors.New("final exit error")})
	})

	select {
	case res := <-waited:
		assert.NilError(t, res.err)
		assert.DeepEqual(t, res.reply.Current, &containerstatev0.ContainerState{
			Status: containerstatev0.StatusExited,
			Exit:   &containerstatev0.Exit{ExitCode: 42, Error: "final exit error"},
		})
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after the final exit")
	}
}

func TestContainerStateWaitEndsAfterRemovalGuardReset(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		d, ctr := newContainerStateTestDaemon(t, &container.State{
			StartedAt:  time.Now().Add(-time.Minute),
			FinishedAt: time.Now(),
			ExitCode:   42,
		})
		ctr.State.SetRemovalInProgress()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		waited := startContainerStateWait(ctx, d, ctr.ID, containerstatev0.StatusExited)

		// Let Wait read "removing" and block before the guard is reset,
		// as happens when link removal finishes without a container exit.
		synctest.Wait()
		ctr.State.ResetRemovalInProgress()

		select {
		case res := <-waited:
			assert.NilError(t, res.err)
			assert.DeepEqual(t, res.reply.Current, &containerstatev0.ContainerState{
				Status: containerstatev0.StatusExited,
				Exit:   &containerstatev0.Exit{ExitCode: 42},
			})
		case <-time.After(time.Second):
			t.Fatal("Wait did not return after the removal guard was reset")
		}
	})
}

func TestContainerStateWaitNotStatuses(t *testing.T) {
	t.Parallel()

	d, ctr := newContainerStateTestDaemon(t, &container.State{Running: true, StartedAt: time.Now()})
	waited := make(chan containerStateWaitResult, 1)

	go func() {
		reply, err := containerStateWatcher{daemon: d}.Wait(t.Context(), &containerstatev0.WaitRequest{
			ContainerID: ctr.ID,
			NotStatuses: []string{containerstatev0.StatusRunning},
		})
		waited <- containerStateWaitResult{reply, err}
	}()

	select {
	case <-waited:
		t.Fatal("Wait returned while the status was excluded")
	case <-time.After(20 * time.Millisecond):
	}

	// Matching uses the status, so "paused" matches although the process
	// is still running.
	before := time.Now()

	setState(ctr, func(s *container.State) { s.SetPaused(true) })

	after := time.Now()

	select {
	case res := <-waited:
		assert.NilError(t, res.err)
		assert.Equal(t, res.reply.Current.Status, containerstatev0.StatusPaused)
		assert.Assert(t, res.reply.ChangedAt >= before.UnixNano() && res.reply.ChangedAt <= after.UnixNano())
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after the status left the excluded set")
	}
}

func TestContainerStateWaitEndsOnRemoval(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		d, ctr := newContainerStateTestDaemon(t, &container.State{Running: true, StartedAt: time.Now()})

		var waits []<-chan containerStateWaitResult
		for _, statuses := range [][]string{
			// Removal ends a Wait that does not list "removed".
			{containerstatev0.StatusPaused},
			{containerstatev0.StatusRemoved},
		} {
			waits = append(waits, startContainerStateWait(t.Context(), d, ctr.ID, statuses...))
		}
		// Removal must happen after the waits found the container.
		synctest.Wait()
		setState(ctr, func(s *container.State) {
			s.OOMKilled = true
			s.SetStopped(&container.ExitStatus{ExitCode: 7, Error: errors.New("exit error")})
		})
		d.containers.Delete(ctr.ID)
		d.containersReplica.Delete(ctr)
		ctr.State.SetRemoved()

		for _, waited := range waits {
			select {
			case res := <-waited:
				assert.NilError(t, res.err)
				// Removal clears the recorded error.
				assert.DeepEqual(t, res.reply.Current, &containerstatev0.ContainerState{
					Status: containerstatev0.StatusRemoved,
					Exit:   &containerstatev0.Exit{ExitCode: 7, OOMKilled: true},
				})
				// The state at removal keeps it.
				assert.DeepEqual(t, res.reply.Previous, &containerstatev0.ContainerState{
					Status: containerstatev0.StatusExited,
					Exit:   &containerstatev0.Exit{ExitCode: 7, Error: "exit error", OOMKilled: true},
				})
			case <-time.After(3 * time.Second):
				t.Fatal("Wait was not ended by removal")
			}
		}

		_, err := containerStateWatcher{daemon: d}.Wait(t.Context(), &containerstatev0.WaitRequest{ContainerID: ctr.ID})
		assert.Assert(t, cerrdefs.IsNotFound(err))
	})
}

func TestContainerStateWaitReturnsImmediately(t *testing.T) {
	t.Parallel()

	d, ctr := newContainerStateTestDaemon(t, &container.State{Running: true, StartedAt: time.Now()})
	for _, statuses := range [][]string{
		nil,
		{containerstatev0.StatusExited, containerstatev0.StatusRunning},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		reply, err := containerStateWatcher{daemon: d}.Wait(ctx, &containerstatev0.WaitRequest{
			ContainerID: ctr.ID,
			Statuses:    statuses,
		})
		cancel()
		assert.NilError(t, err)
		assert.Equal(t, reply.Current.Status, containerstatev0.StatusRunning)
		// The daemon observed no status change for this container.
		assert.Equal(t, reply.ChangedAt, int64(0))
		assert.Assert(t, reply.Previous == nil)
	}
}

func TestContainerStateWaitErrors(t *testing.T) {
	t.Parallel()

	d, _ := newContainerStateTestDaemon(t, &container.State{})
	_, err := containerStateWatcher{daemon: d}.Wait(t.Context(), &containerstatev0.WaitRequest{ContainerID: "unknown-container"})
	assert.Assert(t, cerrdefs.IsNotFound(err))
	assert.Equal(t, status.Code(err), codes.NotFound)

	_, err = containerStateWatcher{daemon: d}.Wait(t.Context(), &containerstatev0.WaitRequest{ContainerID: "any", Statuses: []string{"exitted"}})
	assert.Assert(t, cerrdefs.IsInvalidArgument(err))
	assert.Equal(t, status.Code(err), codes.InvalidArgument)

	_, err = containerStateWatcher{daemon: d}.Wait(t.Context(), &containerstatev0.WaitRequest{
		ContainerID: "any",
		Statuses:    []string{containerstatev0.StatusExited},
		NotStatuses: []string{containerstatev0.StatusRunning},
	})
	assert.Assert(t, cerrdefs.IsInvalidArgument(err))
	assert.Equal(t, status.Code(err), codes.InvalidArgument)

	starting := &Daemon{containers: container.NewMemoryStore(), startupDone: make(chan struct{})}
	_, err = containerStateWatcher{daemon: starting}.Wait(t.Context(), &containerstatev0.WaitRequest{ContainerID: "any"})
	assert.Assert(t, cerrdefs.IsUnavailable(err))
	assert.Equal(t, status.Code(err), codes.Unavailable)
}

// watchEvents is a [stream.Sender] that queues sent events.
type watchEvents chan *containerstatev0.WatchEvent

func (c watchEvents) Send(e *containerstatev0.WatchEvent) error {
	c <- e
	return nil
}

func TestContainerStateWatch(t *testing.T) {
	t.Parallel()

	d, ctr := newContainerStateTestDaemon(t, &container.State{Running: true, StartedAt: time.Now()})
	events := make(watchEvents, 8)
	watched := make(chan error, 1)

	go func() {
		watched <- containerStateWatcher{daemon: d}.Watch(t.Context(), &containerstatev0.WatchRequest{ContainerID: ctr.ID}, events)
	}()

	next := func() *containerstatev0.WatchEvent {
		t.Helper()

		select {
		case e := <-events:
			return e
		case <-time.After(3 * time.Second):
			t.Fatal("Watch did not send an event")
			return nil
		}
	}

	// The first event is the current state.
	e := next()
	assert.Equal(t, e.Current.Status, containerstatev0.StatusRunning)
	assert.Equal(t, e.ChangedAt, int64(0))
	assert.Assert(t, e.Previous == nil)

	setState(ctr, func(s *container.State) { s.SetPaused(true) })
	setState(ctr, func(s *container.State) { s.SetPaused(false) })
	setState(ctr, func(s *container.State) { s.SetStopped(&container.ExitStatus{ExitCode: 3}) })
	d.containers.Delete(ctr.ID)
	d.containersReplica.Delete(ctr)
	ctr.State.SetRemoved()

	// Every change is sent in order, with the state before it.
	for _, want := range []struct{ previous, current string }{
		{containerstatev0.StatusRunning, containerstatev0.StatusPaused},
		{containerstatev0.StatusPaused, containerstatev0.StatusRunning},
		{containerstatev0.StatusRunning, containerstatev0.StatusExited},
		{containerstatev0.StatusExited, containerstatev0.StatusRemoved},
	} {
		e := next()
		assert.Equal(t, e.Previous.Status, want.previous)
		assert.Equal(t, e.Current.Status, want.current)
		assert.Assert(t, e.ChangedAt != 0)
	}

	select {
	case err := <-watched:
		assert.NilError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Watch did not return after the removal")
	}
}
