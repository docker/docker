package container

import (
	"context"
	"errors"
	"iter"
	"slices"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/errdefs"
)

// maxPendingStatusChanges is how many status changes a consumer of
// [State.StatusChanges] can fall behind before the iteration ends.
const maxPendingStatusChanges = 64

// StateSnapshot is a copy of a container's state.
type StateSnapshot struct {
	Status     container.ContainerState
	ExitCode   int
	Error      string
	OOMKilled  bool
	FinishedAt time.Time
}

// StatusResult is a container's state yielded by [State.StatusChanges].
type StatusResult struct {
	// Current is the container's state after the status change, or when the
	// iteration started.
	Current StateSnapshot
	// Previous is the state just before the status last changed, and
	// ChangedAt is when it changed.
	// Both are zero if the daemon did not observe a change since it started.
	Previous  StateSnapshot
	ChangedAt time.Time
	// Removed reports whether the container was removed.
	// Unlike after a failed removal, a removed container's state never
	// changes again.
	Removed bool
}

// statusSubscriber is guarded by [State.statusMu].
type statusSubscriber struct {
	pending []StatusResult
	ready   chan struct{}
	err     error
}

// StatusChanges yields the container's state, then its state after each
// status change, in order, and stops after the container is removed.
// It yields an error and stops if ctx is done, or if the consumer falls more
// than [maxPendingStatusChanges] changes behind, instead of skipping changes.
// Breaking out of the loop unsubscribes.
// It takes the state lock only to start, so it must be called without
// holding it; waiting for changes and unsubscribing don't take it.
func (s *State) StatusChanges(ctx context.Context) iter.Seq2[StatusResult, error] {
	return func(yield func(StatusResult, error) bool) {
		sub := &statusSubscriber{ready: make(chan struct{}, 1)}

		s.Lock()

		r := s.currentStatus()
		if !r.Removed {
			s.statusMu.Lock()
			s.statusSubscribers = append(s.statusSubscribers, sub)
			s.statusMu.Unlock()
		}
		s.Unlock()

		defer s.unsubscribe(sub)

		for {
			if !yield(r, nil) || r.Removed {
				return
			}

			var err error
			if r, err = s.nextStatus(ctx, sub); err != nil {
				yield(StatusResult{}, err)
				return
			}
		}
	}
}

// currentStatus returns the container's state and its last status change.
// A removed container keeps its state at removal, because later changes,
// such as resetting RemovalInProgress, are not status changes.
// The caller must hold the state lock.
func (s *State) currentStatus() StatusResult {
	r := s.lastStatus
	if !r.Removed {
		r.Current = s.snapshot()
	}

	return r
}

// nextStatus waits for the next status change queued for sub.
func (s *State) nextStatus(ctx context.Context, sub *statusSubscriber) (StatusResult, error) {
	for {
		if err := ctx.Err(); err != nil {
			return StatusResult{}, err
		}

		s.statusMu.Lock()
		if len(sub.pending) > 0 {
			r := sub.pending[0]
			sub.pending = slices.Delete(sub.pending, 0, 1)
			s.statusMu.Unlock()

			return r, nil
		}

		err := sub.err
		s.statusMu.Unlock()

		if err != nil {
			return StatusResult{}, err
		}

		select {
		case <-ctx.Done():
			return StatusResult{}, ctx.Err()
		case <-sub.ready:
		}
	}
}

func (s *State) unsubscribe(sub *statusSubscriber) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	s.statusSubscribers = slices.DeleteFunc(s.statusSubscribers, func(o *statusSubscriber) bool { return o == sub })
}

// change applies a change to the state, and records it if it changed the
// container's status.
// A removed container's status never changes again.
// The caller must hold the state lock.
func (s *State) change(apply func()) {
	if s.lastStatus.Removed {
		apply()
		return
	}

	prev := s.snapshot()

	apply()

	if cur := s.snapshot(); cur.Status != prev.Status {
		s.recordStatus(StatusResult{Current: cur, Previous: prev})
	}
}

// recordStatus records r as the last status change and queues it for every
// subscriber.
// A subscriber that fell too far behind is dropped with an error instead.
// The caller must hold the state lock, which keeps the queues in the order
// of the changes.
func (s *State) recordStatus(r StatusResult) {
	r.ChangedAt = time.Now().UTC()
	s.lastStatus = r

	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	s.statusSubscribers = slices.DeleteFunc(s.statusSubscribers, func(sub *statusSubscriber) bool {
		behind := len(sub.pending) == maxPendingStatusChanges
		if behind {
			sub.err = errdefs.Unavailable(errors.New("container status changes were not consumed fast enough"))
		} else {
			sub.pending = append(sub.pending, r)
		}

		select {
		case sub.ready <- struct{}{}:
		default:
		}

		return behind
	})
}

// snapshot returns a copy of the container's state.
// The caller must hold the state lock.
func (s *State) snapshot() StateSnapshot {
	return StateSnapshot{
		Status:     s.State(),
		ExitCode:   s.ExitCode,
		Error:      s.ErrorMsg,
		OOMKilled:  s.OOMKilled,
		FinishedAt: s.FinishedAt,
	}
}
