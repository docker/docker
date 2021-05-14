package agent

import (
	"context"
	"reflect"
	"sync"

	"github.com/docker/swarmkit/api"
	"github.com/docker/swarmkit/log"
)

// StatusReporter receives updates to task status. Method may be called
// concurrently, so implementations should be goroutine-safe.
type StatusReporter interface {
	UpdateTaskStatus(ctx context.Context, taskID string, status *api.TaskStatus) error
}

// Reporter recieves update to both task and volume status.
type Reporter interface {
	StatusReporter
	ReportVolumeUnpublished(ctx context.Context, volumeID string) error
}

type statusReporterFunc func(ctx context.Context, taskID string, status *api.TaskStatus) error

func (fn statusReporterFunc) UpdateTaskStatus(ctx context.Context, taskID string, status *api.TaskStatus) error {
	return fn(ctx, taskID, status)
}

type volumeReporterFunc func(ctx context.Context, volumeID string) error

func (fn volumeReporterFunc) ReportVolumeUnpublished(ctx context.Context, volumeID string) error {
	return fn(ctx, volumeID)
}

type statusReporterCombined struct {
	statusReporterFunc
	volumeReporterFunc
}

// statusReporter creates a reliable StatusReporter that will always succeed.
// It handles several tasks at once, ensuring all statuses are reported.
//
// The reporter will continue reporting the current status until it succeeds.
type statusReporter struct {
	reporter Reporter
	statuses map[string]*api.TaskStatus
	// volumes is a set of volumes which are to be reported unpublished.
	volumes map[string]struct{}
	mu      sync.Mutex
	cond    sync.Cond
	closed  bool
}

func newStatusReporter(ctx context.Context, upstream Reporter) *statusReporter {
	r := &statusReporter{
		reporter: upstream,
		statuses: make(map[string]*api.TaskStatus),
		volumes:  make(map[string]struct{}),
	}

	r.cond.L = &r.mu

	go r.run(ctx)
	return r
}

func (sr *statusReporter) UpdateTaskStatus(ctx context.Context, taskID string, status *api.TaskStatus) error {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	current, ok := sr.statuses[taskID]
	if ok {
		if reflect.DeepEqual(current, status) {
			return nil
		}

		if current.State > status.State {
			return nil // ignore old updates
		}
	}
	sr.statuses[taskID] = status
	sr.cond.Signal()

	return nil
}

func (sr *statusReporter) ReportVolumeUnpublished(ctx context.Context, volumeID string) error {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	sr.volumes[volumeID] = struct{}{}
	sr.cond.Signal()

	return nil
}

func (sr *statusReporter) Close() error {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	sr.closed = true
	sr.cond.Signal()

	return nil
}

func (sr *statusReporter) run(ctx context.Context) {
	done := make(chan struct{})
	defer close(done)

	sr.mu.Lock() // released during wait, below.
	defer sr.mu.Unlock()

	go func() {
		select {
		case <-ctx.Done():
			sr.Close()
		case <-done:
			return
		}
	}()

	for {
		if len(sr.statuses) == 0 && len(sr.volumes) == 0 {
			sr.cond.Wait()
		}

		if sr.closed {
			// TODO(stevvooe): Add support here for waiting until all
			// statuses are flushed before shutting down.
			return
		}

		for taskID, status := range sr.statuses {
			delete(sr.statuses, taskID) // delete the entry, while trying to send.

			sr.mu.Unlock()
			err := sr.reporter.UpdateTaskStatus(ctx, taskID, status)
			sr.mu.Lock()

			// reporter might be closed during UpdateTaskStatus call
			if sr.closed {
				return
			}

			if err != nil {
				log.G(ctx).WithError(err).Error("status reporter failed to report status to agent")

				// place it back in the map, if not there, allowing us to pick
				// the value if a new one came in when we were sending the last
				// update.
				if _, ok := sr.statuses[taskID]; !ok {
					sr.statuses[taskID] = status
				}
			}
		}

		for volumeID := range sr.volumes {
			delete(sr.volumes, volumeID)

			sr.mu.Unlock()
			err := sr.reporter.ReportVolumeUnpublished(ctx, volumeID)
			sr.mu.Lock()

			// reporter might be closed during ReportVolumeUnpublished call
			if sr.closed {
				return
			}

			if err != nil {
				log.G(ctx).WithError(err).Error("status reporter failed to report volume status to agent")
				sr.volumes[volumeID] = struct{}{}
			}
		}
	}
}
