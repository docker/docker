package jobs

import (
	"container/heap"
	"context"
	"errors"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	jobsv0 "github.com/moby/moby/v2/extpoints/jobs/api/v0"
)

// schedulerIdleWait bounds the loop's sleep when no job is armed, so a
// missed wake signal can only ever delay a fire, not lose it.
const schedulerIdleWait = time.Minute

// schedEntry is one armed schedule job.
type schedEntry struct {
	jobID    string
	schedule *cronSchedule
	loc      *time.Location
	next     time.Time
	index    int // position in the heap, -1 once removed
}

// schedHeap is a min-heap of armed entries ordered by next fire time.
type schedHeap []*schedEntry

func (h schedHeap) Len() int           { return len(h) }
func (h schedHeap) Less(i, j int) bool { return h[i].next.Before(h[j].next) }
func (h schedHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *schedHeap) Push(x any) {
	entry := x.(*schedEntry)
	entry.index = len(*h)
	*h = append(*h, entry)
}

func (h *schedHeap) Pop() any {
	old := *h
	entry := old[len(old)-1]
	old[len(old)-1] = nil
	entry.index = -1
	*h = old[:len(old)-1]
	return entry
}

// scheduler fires schedule jobs on the daemon's clock: a single goroutine
// sleeping until the earliest armed entry is due, dispatching fires through
// the manager's tryFireLocked chokepoint like any other trigger.
//
// Lock order: the scheduler's mutex is taken while holding a job lock (Arm
// and Disarm run under the manager's transitions) but never the other way
// around — tick releases the mutex before dispatching, so fires and
// persistence take job locks lock-free of the scheduler.
type scheduler struct {
	m *Manager

	mu    sync.Mutex
	heap  schedHeap
	byJob map[string]*schedEntry
	// gens holds the generation each job was last armed with. A tick's fire
	// is dropped when its job was disarmed, re-armed or rescheduled since,
	// so a delayed fire cannot run an occurrence of a superseded schedule or
	// one a manual run replaced. An entry that tick exhausts keeps its
	// generation: its last fire still stands.
	gens map[string]uint64
	gen  uint64
	// pending counts each job's occurrences consumed by a tick whose fires
	// have not been claimed yet.
	pending map[string]int

	wake chan struct{} // buffered kick after arm/disarm, so the loop re-plans
	stop chan struct{}
}

func newScheduler(m *Manager) *scheduler {
	return &scheduler{
		m:       m,
		byJob:   make(map[string]*schedEntry),
		gens:    make(map[string]uint64),
		pending: make(map[string]int),
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
	}
}

// arm registers (or re-registers) a job's next occurrence.
func (s *scheduler) arm(jobID string, schedule *cronSchedule, loc *time.Location, next time.Time) {
	s.mu.Lock()

	s.gen++
	s.gens[jobID] = s.gen
	delete(s.pending, jobID)

	if entry, armed := s.byJob[jobID]; armed {
		entry.schedule, entry.loc, entry.next = schedule, loc, next
		heap.Fix(&s.heap, entry.index)
	} else {
		entry := &schedEntry{jobID: jobID, schedule: schedule, loc: loc, next: next}
		heap.Push(&s.heap, entry)
		s.byJob[jobID] = entry
	}
	s.mu.Unlock()
	s.kick()
}

// disarm forgets a job. It is a no-op for unarmed jobs.
func (s *scheduler) disarm(jobID string) {
	s.mu.Lock()
	delete(s.gens, jobID)
	delete(s.pending, jobID)

	if entry, armed := s.byJob[jobID]; armed {
		heap.Remove(&s.heap, entry.index)
		delete(s.byJob, jobID)
	}
	s.mu.Unlock()
	s.kick()
}

// replaceNext implements the reschedule semantics of the run API: a manual
// fire stands in for the job's upcoming occurrence. When a tick already
// consumed that occurrence and its fire is still waiting for the job's lock,
// which the caller holds, the fire is dropped; otherwise the upcoming
// occurrence is skipped. Either way exactly one occurrence is replaced. It
// reports whether the job was armed.
func (s *scheduler) replaceNext(jobID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, armed := s.gens[jobID]; !armed {
		return false
	}

	if s.pending[jobID] > 0 {
		s.gen++
		s.gens[jobID] = s.gen
		delete(s.pending, jobID)

		return true
	}

	entry, armed := s.byJob[jobID]
	if !armed {
		return true
	}

	if next, ok := entry.schedule.next(entry.next, entry.loc); ok {
		entry.next = next
		heap.Fix(&s.heap, entry.index)
	} else {
		heap.Remove(&s.heap, entry.index)
		delete(s.byJob, jobID)
	}

	return true
}

// nextFor reports a job's armed occurrence.
func (s *scheduler) nextFor(jobID string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, armed := s.byJob[jobID]; armed {
		return entry.next, true
	}
	return time.Time{}, false
}

// claimDue claims a fire a tick dispatched, reporting false when its job was
// disarmed, re-armed or rescheduled since.
func (s *scheduler) claimDue(jobID string, gen uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.gens[jobID] != gen {
		return false
	}

	s.pending[jobID]--
	if s.pending[jobID] <= 0 {
		delete(s.pending, jobID)
	}

	return true
}

// kick nudges the loop to re-plan its sleep after the heap changed.
func (s *scheduler) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// dueFire is an occurrence a tick consumed, with the generation its job was
// armed with.
type dueFire struct {
	jobID       string
	gen         uint64
	scheduledAt time.Time
}

// tick fires every due entry and returns how long the loop may sleep before
// the next one. Dispatch happens after the scheduler lock is released: fires
// take job locks, which are held around arm/disarm calls into the scheduler.
func (s *scheduler) tick(ctx context.Context) time.Duration {
	fires, wait := s.takeDue(s.m.now())
	for _, fire := range fires {
		// Firing takes the job's lock, which a run may hold across container
		// operations; doing it here would stall every other schedule.
		s.m.background.Go(func() { s.m.fireDue(ctx, fire.jobID, fire.gen, fire.scheduledAt) })
	}

	return wait
}

// takeDue consumes the occurrences due at now and returns how long the loop
// may sleep before the next one.
func (s *scheduler) takeDue(now time.Time) ([]dueFire, time.Duration) {
	var fires []dueFire

	s.mu.Lock()
	defer s.mu.Unlock()

	// An entry overdue by several occurrences (a long sleep or system
	// suspend) fires once and re-arms from now: the intermediate backlog is
	// deliberately dropped, mirroring how the missed-fires policy caps the
	// catch-up at one run across a daemon restart.
	for len(s.heap) > 0 && !s.heap[0].next.After(now) {
		entry := s.heap[0]
		fires = append(fires, dueFire{jobID: entry.jobID, gen: s.gens[entry.jobID], scheduledAt: entry.next})
		s.pending[entry.jobID]++

		if next, ok := entry.schedule.next(now, entry.loc); ok {
			entry.next = next
			heap.Fix(&s.heap, entry.index)
		} else {
			// The expression has no future occurrence (e.g. a specific
			// date now past): the job stays registered but disarmed.
			heap.Remove(&s.heap, entry.index)
			delete(s.byJob, entry.jobID)
		}
	}
	wait := schedulerIdleWait
	if len(s.heap) > 0 {
		wait = min(s.heap[0].next.Sub(now), schedulerIdleWait)
	}

	return fires, wait
}

// run is the scheduler loop; it exits when stop is closed.
func (s *scheduler) run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-s.wake:
		case <-timer.C:
		}
		if !timer.Stop() {
			// Drain a concurrently fired timer so Reset arms cleanly.
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(s.tick(ctx))
	}
}

// fireDue fires the occurrence a tick consumed and records the job's next
// one, unless the job was disarmed, re-armed or rescheduled since the tick: a
// pause, resume, removal or reschedule taking the job's lock first
// supersedes the fire.
func (m *Manager) fireDue(ctx context.Context, jobID string, gen uint64, scheduledAt time.Time) {
	m.locks.Lock(jobID)
	defer m.locks.Unlock(jobID)

	if !m.sched.claimDue(jobID, gen) {
		return
	}

	m.fireScheduledLocked(ctx, jobID, scheduledAt)
}

// fireScheduled routes a cron fire through the tryFireLocked chokepoint and
// turns its refusals into skip logs: for a scheduler, a fire refused by the
// concurrency policy is normal operation, not an error. The job's next
// occurrence is recorded only after the fire, so a crash in between leaves
// the occurrence to the restart's catch-up instead of losing it. A fire
// deferred by the queue policy leaves the occurrence recorded until the
// deferred fire runs, for the same reason.
func (m *Manager) fireScheduled(ctx context.Context, jobID string, scheduledAt time.Time) {
	m.locks.Lock(jobID)
	defer m.locks.Unlock(jobID)

	m.fireScheduledLocked(ctx, jobID, scheduledAt)
}

// fireScheduledLocked is fireScheduled for callers already holding the job's
// lock.
func (m *Manager) fireScheduledLocked(ctx context.Context, jobID string, scheduledAt time.Time) {
	evidence := &jobsv0.TriggerEvidence{
		Kind:            jobsv0.TriggerKindSchedule,
		ScheduledAtNano: scheduledAt.UnixNano(),
		FiredAtNano:     m.now().UnixNano(),
	}

	_, err := m.tryFireLocked(ctx, jobID, evidence)
	switch {
	case err == nil:
	case errors.Is(err, errFirePaused):
		log.G(ctx).WithFields(log.Fields{"job": jobID}).Debug("scheduled fire skipped: the job is paused")
	case errors.Is(err, errFireQueued):
		log.G(ctx).WithFields(log.Fields{"job": jobID}).Debug("scheduled fire queued behind the current run")

		return
	case errors.Is(err, errFireDropped):
		log.G(ctx).WithFields(log.Fields{"job": jobID}).Warn("scheduled fire dropped: a fire is already queued")

		return
	case cerrdefs.IsFailedPrecondition(err):
		log.G(ctx).WithFields(log.Fields{"job": jobID}).Warn("scheduled fire skipped: the previous run is still in flight")
	case cerrdefs.IsNotFound(err):
		// The job was removed while the fire was in flight; the removal
		// already disarmed it.
	default:
		log.G(ctx).WithError(err).WithFields(log.Fields{"job": jobID}).Warn("scheduled fire failed")
	}

	m.persistNextFireLocked(ctx, jobID)
}

// persistNextFire records a job's armed occurrence (zero for none) on its
// stored record, where inspect and list read it from, and where Start finds
// the fires missed while the daemon was down.
func (m *Manager) persistNextFire(ctx context.Context, jobID string) {
	m.locks.Lock(jobID)
	defer m.locks.Unlock(jobID)

	m.persistNextFireLocked(ctx, jobID)
}

// persistNextFireLocked is persistNextFire for callers already holding the
// job's lock. It reads the occurrence from the scheduler under that lock
// instead of taking it from the caller, so a delayed write cannot replace a
// newer one.
func (m *Manager) persistNextFireLocked(ctx context.Context, jobID string) {
	var atNano int64
	if next, armed := m.sched.nextFor(jobID); armed {
		atNano = next.UnixNano()
	}

	job, err := m.store.Job(jobID)
	if err != nil {
		return // removed meanwhile; nothing to record on
	}
	if job.NextFireAtNano == atNano {
		return
	}

	job.NextFireAtNano = atNano
	job.UpdatedAtNano = m.now().UnixNano()
	if err := m.store.UpdateJob(ctx, job); err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{"job": jobID}).Warn("could not record next fire time")
	}
}
