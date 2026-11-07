package container

import (
	"context"
	"errors"
	"iter"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	containertypes "github.com/moby/moby/api/types/container"
	"gotest.tools/v3/assert"
)

func TestStateStatusChanges(t *testing.T) {
	t.Parallel()

	s := &State{}
	locked := func(change func()) {
		s.Lock()
		defer s.Unlock()

		change()
	}
	subscribers := func() int {
		s.statusMu.Lock()
		defer s.statusMu.Unlock()

		return len(s.statusSubscribers)
	}
	must := func(r StatusResult, err error, ok bool) StatusResult {
		t.Helper()
		assert.Assert(t, ok)
		assert.NilError(t, err)

		return r
	}

	locked(func() { s.SetRunning(nil, nil, time.Now()) })

	// Every status change is yielded in order, even if several happen before
	// the consumer reads them, and a change that keeps the status is not
	// yielded.
	next, stop := iter.Pull2(s.StatusChanges(t.Context()))
	assert.Equal(t, must(next()).Current.Status, containertypes.StateRunning)
	locked(func() {
		s.SetStopped(&ExitStatus{ExitCode: 3, Error: errors.New("exit error")})
		s.SetStopped(&ExitStatus{ExitCode: 3})
		s.SetRunning(nil, nil, time.Now())
		s.SetPaused(true)
	})

	r := must(next())
	assert.Equal(t, r.Current.Status, containertypes.StateExited)
	assert.Equal(t, r.Current.ExitCode, 3)
	assert.Equal(t, r.Current.Error, "exit error")
	// The exit error is applied with the status change, not before it.
	assert.Equal(t, r.Previous.Status, containertypes.StateRunning)
	assert.Equal(t, r.Previous.Error, "")
	assert.Equal(t, must(next()).Current.Status, containertypes.StateRunning)
	assert.Equal(t, must(next()).Current.Status, containertypes.StatePaused)
	stop()
	assert.Equal(t, subscribers(), 0)

	// A consumer that falls too far behind gets an error instead of skipped
	// changes, and is unsubscribed.
	next, stop = iter.Pull2(s.StatusChanges(t.Context()))
	defer stop()

	must(next())

	for i := range maxPendingStatusChanges + 1 {
		locked(func() { s.SetPaused(i%2 == 1) })
	}

	for range maxPendingStatusChanges {
		must(next())
	}

	_, err, _ := next()
	assert.Assert(t, cerrdefs.IsUnavailable(err))

	_, _, ok := next()
	assert.Assert(t, !ok)
	assert.Equal(t, subscribers(), 0)

	// Removal is the last change.
	next, stop = iter.Pull2(s.StatusChanges(t.Context()))
	defer stop()

	must(next())
	// Marking a running container for removal doesn't change its status, so
	// its exit is what changes it to removing.
	assert.Assert(t, !s.SetRemovalInProgress())
	locked(func() { s.SetStopped(&ExitStatus{ExitCode: 7, Error: errors.New("exit error")}) })
	s.SetRemoved()

	assert.Equal(t, must(next()).Current.Status, containertypes.StateRemoving)
	removed := must(next())
	assert.Assert(t, removed.Removed)
	// Removal clears the error, but the state before it keeps it.
	assert.Equal(t, removed.Previous.Error, "exit error")
	assert.Equal(t, removed.Current.Error, "")

	_, _, ok = next()
	assert.Assert(t, !ok)

	// Changes after removal, such as resetting RemovalInProgress, are not
	// status changes.
	s.ResetRemovalInProgress()

	for r, err := range s.StatusChanges(t.Context()) {
		assert.NilError(t, err)
		assert.DeepEqual(t, r, removed)
	}
}

func TestStateStatusChangesWhileLocked(t *testing.T) {
	t.Parallel()

	s := &State{Running: true}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	next, stop := iter.Pull2(s.StatusChanges(ctx))
	_, err, _ := next()
	assert.NilError(t, err)

	// While the state lock is held, as during a slow start or exit, the
	// consumer still reads a queued change, sees cancellation before the
	// remaining queued change, and unsubscribes.
	s.Lock()
	defer s.Unlock()

	s.SetPaused(true)
	s.SetPaused(false)

	var (
		got              StatusResult
		gotErr, afterErr error
	)

	done := make(chan struct{})
	go func() {
		defer close(done)

		got, gotErr, _ = next()

		cancel()

		_, afterErr, _ = next()

		stop()
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("consumer waited for the state lock")
	}

	assert.NilError(t, gotErr)
	assert.Equal(t, got.Current.Status, containertypes.StatePaused)
	assert.ErrorIs(t, afterErr, context.Canceled)

	s.statusMu.Lock()
	subscribers := len(s.statusSubscribers)
	s.statusMu.Unlock()
	assert.Equal(t, subscribers, 0)
}
