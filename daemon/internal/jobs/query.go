package jobs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/moby/moby/v2/errdefs"
	jobsv0 "github.com/moby/moby/v2/extpoints/jobs/api/v0"
)

// Inspect returns a job with its latest run composed in.
func (m *Manager) Inspect(ctx context.Context, jobRef string) (*jobsv0.Job, error) {
	job, err := m.resolveJob(jobRef)
	if err != nil {
		return nil, err
	}
	return m.composeJob(job), nil
}

// List returns the jobs matching every filter of the request, latest runs
// composed in.
func (m *Manager) List(ctx context.Context, req *jobsv0.ListRequest) ([]*jobsv0.Job, error) {
	if req == nil {
		req = &jobsv0.ListRequest{}
	}
	switch req.Paused {
	case "", "true", "false":
	default:
		return nil, errdefs.InvalidParameter(fmt.Errorf("invalid paused filter %q: must be true or false", req.Paused))
	}
	for _, state := range req.States {
		if state != jobsv0.JobStateIdle && state != jobsv0.JobStateRunning {
			return nil, errdefs.InvalidParameter(fmt.Errorf("invalid state filter %q", state))
		}
	}
	for _, kind := range req.TriggerKinds {
		if kind != jobsv0.TriggerKindManual && kind != jobsv0.TriggerKindSchedule {
			return nil, errdefs.InvalidParameter(fmt.Errorf("invalid trigger filter %q", kind))
		}
	}
	for _, state := range req.LatestRunStates {
		if !isTerminalRunState(state) && state != jobsv0.RunStatePending && state != jobsv0.RunStateRunning {
			return nil, errdefs.InvalidParameter(fmt.Errorf("invalid latest-run-state filter %q", state))
		}
	}

	var jobs []*jobsv0.Job
	for _, job := range m.store.Jobs() {
		if len(req.Names) > 0 && !slices.Contains(req.Names, job.Name) {
			continue
		}
		if len(req.States) > 0 && !slices.Contains(req.States, job.State) {
			continue
		}
		if len(req.TriggerKinds) > 0 && !slices.Contains(req.TriggerKinds, triggerKind(job.Spec.Trigger)) {
			continue
		}
		if (req.Paused == "true" && !job.Paused) || (req.Paused == "false" && job.Paused) {
			continue
		}
		if !matchesLabels(job.Spec.Labels, req.Labels) {
			continue
		}
		job = m.composeJob(job)
		if len(req.LatestRunStates) > 0 && (job.LatestRun == nil || !slices.Contains(req.LatestRunStates, job.LatestRun.State)) {
			continue
		}
		jobs = append(jobs, job)
	}
	slices.SortFunc(jobs, func(a, b *jobsv0.Job) int { return strings.Compare(a.Name, b.Name) })
	return jobs, nil
}

// Pause suppresses a job's schedule trigger. An in-flight run is not
// affected; explicit runs still work. Idempotent, a no-op for manual jobs.
func (m *Manager) Pause(ctx context.Context, jobRef string) error {
	return m.setPaused(ctx, jobRef, true)
}

// Resume re-arms a paused job's schedule trigger from the next tick.
// Idempotent, a no-op for manual jobs.
func (m *Manager) Resume(ctx context.Context, jobRef string) error {
	return m.setPaused(ctx, jobRef, false)
}

func (m *Manager) setPaused(ctx context.Context, jobRef string, paused bool) error {
	job, err := m.resolveJob(jobRef)
	if err != nil {
		return err
	}
	if triggerKind(job.Spec.Trigger) == jobsv0.TriggerKindManual {
		return nil
	}
	m.locks.Lock(job.ID)
	defer m.locks.Unlock(job.ID)
	job, err = m.store.Job(job.ID)
	if err != nil {
		return err
	}
	if job.Paused == paused {
		return nil
	}
	// The scheduler and the queued fire change only once the new state is
	// persisted, so a failed write leaves the job as it was.
	job.Paused = paused

	var (
		schedule *cronSchedule
		loc      *time.Location
		next     time.Time
		rearm    bool
	)

	if paused {
		job.NextFireAtNano = 0
	} else {
		// Re-arm from the next occurrence; fires missed while paused are
		// not backfilled.
		if schedule, loc, err = parseScheduleTrigger(job.Spec.Trigger.Schedule); err != nil {
			return err
		}

		if next, rearm = schedule.next(time.Now(), loc); rearm {
			job.NextFireAtNano = next.UnixNano()
		}
	}

	job.UpdatedAtNano = time.Now().UnixNano()
	if err := m.store.UpdateJob(ctx, job); err != nil {
		return err
	}

	if paused {
		m.sched.disarm(job.ID)
		// A deferred fire must not survive a pause/resume cycle.
		m.mu.Lock()
		delete(m.queued, job.ID)
		m.mu.Unlock()
	} else if rearm {
		m.sched.arm(job.ID, schedule, loc, next)
	}

	return nil
}

// removeRunsPageSize is the page size Remove drains terminal runs with; a
// smaller page than the store default keeps each critical section short.
const removeRunsPageSize = 200

// Remove deletes a job, cancelling its in-flight run and applying the
// requested run-history retention mode.
func (m *Manager) Remove(ctx context.Context, jobRef, runsRemoval string) error {
	switch runsRemoval {
	case "", jobsv0.RunsKeep, jobsv0.RunsRemove, jobsv0.RunsRemoveFinished:
	default:
		return errdefs.InvalidParameter(fmt.Errorf("invalid runs-removal mode %q", runsRemoval))
	}

	jobID, err := m.resolveHistoryJobID(jobRef)
	if err != nil {
		return err
	}

	m.locks.Lock(jobID)
	defer m.locks.Unlock(jobID)

	job, err := m.store.Job(jobID)
	if err != nil {
		if cerrdefs.IsNotFound(err) && runsRemoval == jobsv0.RunsRemove {
			return m.store.PurgeJob(ctx, jobID)
		}

		return err
	}

	// Cancel the in-flight run. Its record stays non-terminal until the
	// watcher observes the container exit, which makes the remove-finished
	// retention rule hold naturally: the run being cancelled by this very
	// removal still counts as in-flight below.
	if run, err := m.store.LatestRun(job.ID); err == nil && !isTerminalRunState(run.State) {
		// Like Cancel, stop even when the terminal decision was claimed
		// already, as an earlier stop may have failed.
		m.setOverride(run.ID, jobsv0.RunStateCancelled)
		containerID := run.ContainerID
		stopCtx := context.WithoutCancel(ctx)

		m.background.Go(func() {
			if err := m.backend.ContainerStop(stopCtx, containerID); err != nil {
				log.G(stopCtx).WithError(err).WithFields(log.Fields{"job": job.ID, "run": run.ID}).Warn("could not stop run container of removed job")
			}
		})
	}

	var removeErr error
	switch runsRemoval {
	case jobsv0.RunsRemove:
		removeErr = m.store.PurgeJob(ctx, job.ID)
	case jobsv0.RunsRemoveFinished:
		for {
			page, cursor, _, err := m.store.Runs(job.ID, removeRunsPageSize, "")
			if err != nil {
				return err
			}
			deleted := false
			for _, run := range page {
				if isTerminalRunState(run.State) {
					if err := m.store.DeleteRun(ctx, job.ID, run.ID); err != nil {
						return err
					}
					deleted = true
				}
			}
			// At most one run is ever non-terminal (the in-flight one), so
			// a page with nothing left to delete means only that run
			// remains; the guard keeps the loop finite even if that
			// invariant is ever broken.
			if cursor == "" || !deleted {
				break
			}
		}
		removeErr = m.store.DeleteJob(ctx, job.ID)
	default:
		removeErr = m.store.DeleteJob(ctx, job.ID)
	}
	// Removing the history can fail after the job record is gone; the job
	// must not stay armed or keep its waiters blocked then.
	if _, err := m.store.Job(job.ID); cerrdefs.IsNotFound(err) {
		m.sched.disarm(job.ID)
		m.mu.Lock()
		delete(m.queued, job.ID)
		m.mu.Unlock()
		// Wake waiters so a Wait on the removed job re-resolves to
		// not-found instead of blocking until its context expires.
		m.broadcast(job.ID)
	}
	return removeErr
}

// Prune removes idle manual jobs matching the label filters, keeping their
// run history. Schedule jobs are never pruned: idle means armed, not
// abandoned.
func (m *Manager) Prune(ctx context.Context, labels []string) ([]string, error) {
	var removed []string
	for _, job := range m.store.Jobs() {
		if triggerKind(job.Spec.Trigger) != jobsv0.TriggerKindManual {
			continue
		}
		if !matchesLabels(job.Spec.Labels, labels) {
			continue
		}
		m.locks.Lock(job.ID)
		// Re-read under the lock: the job may have started running or been
		// removed since the snapshot.
		current, err := m.store.Job(job.ID)
		if err == nil && current.State == jobsv0.JobStateIdle {
			if err := m.store.DeleteJob(ctx, job.ID); err == nil {
				removed = append(removed, job.ID)
				m.broadcast(job.ID)
			}
		}
		m.locks.Unlock(job.ID)
	}
	slices.Sort(removed)
	return removed, nil
}

// RunsPage returns one page of a job's runs, newest first.
func (m *Manager) RunsPage(ctx context.Context, jobRef string, limit int, before string) (page []*jobsv0.Run, nextCursor string, staleCursor bool, err error) {
	jobID, err := m.resolveHistoryJobID(jobRef)
	if err != nil {
		return nil, "", false, err
	}

	return m.store.Runs(jobID, limit, before)
}

// InspectRun returns one run; runRef accepts an ID, "latest", or empty for
// latest.
func (m *Manager) InspectRun(ctx context.Context, jobRef, runRef string) (*jobsv0.Run, error) {
	jobID, err := m.resolveHistoryJobID(jobRef)
	if err != nil {
		return nil, err
	}

	return m.resolveRun(jobID, runRef)
}

// resolveHistoryJobID includes retained history in ID resolution, before
// trying live job names. Removed job names may belong to a new job now.
func (m *Manager) resolveHistoryJobID(ref string) (string, error) {
	if _, err := m.store.LatestRun(ref); err == nil {
		return ref, nil
	}

	job, err := m.resolveJob(ref)
	if err != nil {
		return "", err
	}

	return job.ID, nil
}

func (m *Manager) resolveRun(jobID, runRef string) (*jobsv0.Run, error) {
	if runRef == "" || runRef == "latest" {
		return m.store.LatestRun(jobID)
	}
	return m.store.Run(jobID, runRef)
}

// Wait blocks until the referenced run satisfies the condition and returns
// it. Waiting for "latest" on a job with no runs blocks until one is fired,
// and follows the first run it observes, not later ones. The job is
// resolved once, so a job re-created under the same name is not followed
// either. A run that jumps past the condition satisfies it: waiting for
// running on a run that went straight to a terminal state returns that
// terminal run.
func (m *Manager) Wait(ctx context.Context, jobRef, runRef, condition string) (*jobsv0.Run, error) {
	switch condition {
	case "", jobsv0.WaitConditionTerminal, jobsv0.WaitConditionRunning:
	default:
		return nil, errdefs.InvalidParameter(fmt.Errorf("invalid wait condition %q", condition))
	}

	job, err := m.resolveJob(jobRef)
	if err != nil {
		return nil, err
	}
	// check reports the run once it meets the condition, pinning runRef to
	// the first run observed.
	check := func() (_ *jobsv0.Run, met bool, _ error) {
		run, err := m.resolveRun(job.ID, runRef)
		if err != nil {
			if runRef == "" || runRef == "latest" {
				// No run fired yet.
				return nil, false, nil
			}
			// An explicit run ID either exists or never will.
			return nil, false, err
		}

		runRef = run.ID

		return run, waitConditionMet(run, condition), nil
	}
	for {
		if run, met, err := check(); err != nil || met {
			return run, err
		}
		// Subscribe, then re-check both the job and the run: a transition —
		// or the job's removal, which broadcasts exactly once — between the
		// checks above and the subscription would otherwise be missed
		// forever. Returns that were not woken withdraw their subscription
		// so entries do not accumulate on jobs that stopped transitioning.
		ch := m.subscribe(job.ID)
		if _, err := m.store.Job(job.ID); err != nil {
			m.unsubscribe(job.ID, ch)
			return nil, err
		}

		if run, met, err := check(); err != nil || met {
			m.unsubscribe(job.ID, ch)
			return run, err
		}
		select {
		case <-ctx.Done():
			m.unsubscribe(job.ID, ch)
			return nil, ctx.Err()
		case <-ch:
		}
	}
}

// waitConditionMet reports whether a run satisfies a wait condition. The
// zero condition means terminal.
func waitConditionMet(run *jobsv0.Run, condition string) bool {
	if condition == jobsv0.WaitConditionRunning {
		return run.State != jobsv0.RunStatePending
	}
	return isTerminalRunState(run.State)
}

// matchesLabels reports whether labels satisfy every filter entry, each
// either "key" (presence) or "key=value" (equality), matching the container
// API's label-filter semantics.
func matchesLabels(labels map[string]string, filterEntries []string) bool {
	for _, entry := range filterEntries {
		key, value, hasValue := strings.Cut(entry, "=")
		stored, present := labels[key]
		if !present || (hasValue && stored != value) {
			return false
		}
	}
	return true
}

// Run executes an existing job, creating its next run. The returned run may
// already be terminal when the container could not be created or started;
// the failure is recorded on the run record, per the API contract.
//
// With reschedule, the manual fire stands in for the job's upcoming
// scheduled occurrence: the schedule re-arms on the occurrence after it,
// instead of keeping the original cadence. An occurrence a tick already
// consumed but has not fired yet counts as the upcoming one.
func (m *Manager) Run(ctx context.Context, jobRef string, reschedule bool) (*jobsv0.Run, error) {
	job, err := m.resolveJob(jobRef)
	if err != nil {
		return nil, err
	}
	if reschedule && triggerKind(job.Spec.Trigger) != jobsv0.TriggerKindSchedule {
		return nil, errdefs.InvalidParameter(errors.New("reschedule requires a schedule trigger"))
	}

	// Holding the job's lock across the fire and the replacement keeps a
	// tick's fire for the replaced occurrence waiting until it is dropped.
	m.locks.Lock(job.ID)
	defer m.locks.Unlock(job.ID)

	run, err := m.fireManualLocked(ctx, job.ID)
	if err != nil {
		return nil, err
	}

	if reschedule && m.sched.replaceNext(job.ID) {
		m.persistNextFireLocked(ctx, job.ID)
	}
	return run, nil
}
