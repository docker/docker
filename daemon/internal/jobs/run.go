package jobs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/internal/stringid"
	jobsv0 "github.com/moby/moby/v2/extpoints/jobs/api/v0"
)

// errFireQueued reports that a fire was deferred behind the current run by
// the queue concurrency policy; the deferred fire happens when that run
// completes. Callers (the scheduler) treat it as a skip, not a failure.
var errFireQueued = errors.New("fire queued behind the current run")

// errFireDropped reports that a fire was dropped because a fire is already
// queued (the queue has depth one).
var errFireDropped = errors.New("fire dropped: a fire is already queued")

var errFirePaused = errors.New("scheduled fire dropped: the job is paused")

// terminalOutcome is what a run ends with.
type terminalOutcome struct {
	state    string
	exitCode *int64
	errMsg   string
}

// fireManual fires a run for an explicit Run or CreateAndRun call.
func (m *Manager) fireManual(ctx context.Context, jobID string) (*jobsv0.Run, error) {
	m.locks.Lock(jobID)
	defer m.locks.Unlock(jobID)

	return m.fireManualLocked(ctx, jobID)
}

// fireManualLocked is fireManual for callers already holding the job's lock.
func (m *Manager) fireManualLocked(ctx context.Context, jobID string) (*jobsv0.Run, error) {
	return m.tryFireLocked(ctx, jobID, &jobsv0.TriggerEvidence{
		Kind:        jobsv0.TriggerKindManual,
		FiredAtNano: m.now().UnixNano(),
	})
}

// tryFireLocked is the single chokepoint every fire goes through, whatever
// the trigger: called under the job's lock, it applies the concurrency policy
// against the job's state re-read under that lock, and only then creates the
// run. Two triggers racing on the same job therefore cannot both pass.
func (m *Manager) tryFireLocked(ctx context.Context, jobID string, evidence *jobsv0.TriggerEvidence) (*jobsv0.Run, error) {
	// A fire outlives its trigger. The daemon completes a container start
	// even when the API caller goes away, so a cancelled request must not
	// record a failure and release the job while its container runs.
	ctx = context.WithoutCancel(ctx)

	job, err := m.store.Job(jobID)
	if err != nil {
		return nil, err
	}

	if job.Paused && evidence.Kind == jobsv0.TriggerKindSchedule {
		return nil, errFirePaused
	}

	if job.State == jobsv0.JobStateRunning {
		if evidence.Kind == jobsv0.TriggerKindSchedule && scheduleConcurrency(job) == jobsv0.ConcurrencyQueue {
			m.mu.Lock()
			_, full := m.queued[jobID]
			if !full {
				m.queued[jobID] = evidence
			}
			m.mu.Unlock()
			if full {
				return nil, errFireDropped
			}
			return nil, errFireQueued
		}
		return nil, cerrdefs.ErrFailedPrecondition.WithMessage(fmt.Sprintf("job %s is already running", job.Name))
	}

	now := m.now().UnixNano()
	run := &jobsv0.Run{
		ID:            stringid.GenerateRandomID(),
		JobID:         job.ID,
		State:         jobsv0.RunStatePending,
		CreatedAtNano: now,
		Trigger:       evidence,
	}
	// The run record is persisted BEFORE the container is created. Every
	// crash-safety property depends on this ordering: a daemon crash or a
	// container-create failure leaves a discoverable run record instead of
	// an orphaned container. Do not reorder.
	if err := m.store.CreateRun(ctx, run); err != nil {
		return nil, err
	}
	job.State = jobsv0.JobStateRunning
	job.UpdatedAtNano = now

	if err := m.store.UpdateJob(ctx, job); err != nil {
		m.completeRunLocked(ctx, job, run, terminalOutcome{state: jobsv0.RunStateFailed, errMsg: "recording job state: " + err.Error()})
		return m.store.Run(job.ID, run.ID)
	}
	m.broadcast(job.ID)

	if err := m.startContainer(ctx, job, run); err != nil {
		// The failure is recorded on the run, not returned: the run exists
		// and its record is the outcome, per the API contract.
		m.failUnstartedLocked(ctx, job, run, err.Error())
	}
	return m.store.Run(job.ID, run.ID)
}

// startContainer creates and starts the run's container, arms the timeout,
// and hands off to the exit watcher. Called under the job's lock.
func (m *Manager) startContainer(ctx context.Context, job *jobsv0.Job, run *jobsv0.Run) error {
	req, err := decodeContainerSpec(job.Spec.ContainerSpec, false)
	if err != nil {
		// Validated at registration; only a corrupted store record gets here.
		return fmt.Errorf("decoding container spec: %w", err)
	}
	if req.Config.Labels == nil {
		req.Config.Labels = make(map[string]string, 2)
	}
	req.Config.Labels[LabelJobID] = job.ID
	req.Config.Labels[LabelRunID] = run.ID

	created, err := m.backend.ContainerCreate(ctx, ContainerCreateRequest{
		Name:             runContainerName(job, run),
		Config:           req.Config,
		HostConfig:       req.HostConfig,
		NetworkingConfig: req.NetworkingConfig,
	})
	if err != nil {
		return fmt.Errorf("creating run container: %w", err)
	}
	run.ContainerID = created.ID
	if err := m.store.UpdateRun(ctx, run); err != nil {
		return fmt.Errorf("recording run container: %w", err)
	}

	if err := m.backend.ContainerStart(ctx, created.ID); err != nil {
		return fmt.Errorf("starting run container: %w", err)
	}
	run.State = jobsv0.RunStateRunning
	run.StartedAtNano = m.now().UnixNano()

	if err := m.store.UpdateRun(ctx, run); err != nil {
		// The container is already running. Keep observing it until exit,
		// even if stopping fails, rather than free the job for another run.
		run.Error = "recording run start: " + err.Error()

		m.background.Go(func() {
			if err := m.backend.ContainerStop(ctx, run.ContainerID); err != nil {
				log.G(ctx).WithError(err).WithFields(log.Fields{"job": job.ID, "run": run.ID}).Warn("could not stop run container after recording its start failed")
			}
		})
	} else {
		m.broadcast(job.ID)
	}

	m.watch(ctx, job, run)

	return nil
}

// runContainerName names a run's container. The run-ID suffix keeps the name
// unique across job generations: a removed job leaves its kept containers
// behind, and a re-created job under the same name restarts its iterations
// at one.
func runContainerName(job *jobsv0.Job, run *jobsv0.Run) string {
	return fmt.Sprintf("job-%s-%d-%s", job.Name, run.Iteration, stringid.TruncateID(run.ID))
}

// failUnstartedLocked fails a run whose start is unknown: a failed start
// request may still have started the container. A container left behind is
// stopped before the job is released, so it cannot overlap the job's next
// run; if stopping fails, the job stays occupied until the container is
// observed exiting. Called under the job's lock.
func (m *Manager) failUnstartedLocked(ctx context.Context, job *jobsv0.Job, run *jobsv0.Run, errMsg string) {
	run.Error = errMsg
	if run.ContainerID != "" {
		if err := m.backend.ContainerStop(ctx, run.ContainerID); err != nil && !cerrdefs.IsNotFound(err) {
			log.G(ctx).WithError(err).WithFields(log.Fields{"job": job.ID, "run": run.ID}).Warn("could not stop container of unstarted run; watching it until it exits")
			m.watch(ctx, job, run)

			return
		}
	}

	m.completeRunLocked(ctx, job, run, terminalOutcome{state: jobsv0.RunStateFailed, errMsg: errMsg})
}

// watch records an existing exit or arms the timeout and observes a future
// exit in the background. Called under the job's lock.
func (m *Manager) watch(ctx context.Context, job *jobsv0.Job, run *jobsv0.Run) {
	// Observation ends with the manager: under live-restore the container
	// keeps running past shutdown.
	waitCtx, cancelWait := context.WithCancel(ctx)

	waitC, err := m.backend.ContainerWait(waitCtx, run.ContainerID)
	if err != nil {
		cancelWait()
		// Per the Backend contract, the container no longer exists.
		m.completeRunLocked(ctx, job, run, terminalOutcome{state: jobsv0.RunStateFailed, errMsg: "waiting on run container: " + err.Error()})
		return
	}
	// Reconcile an exit before enforcing a deadline: the container may have
	// finished within its timeout while the daemon was down.
	select {
	case status := <-waitC:
		cancelWait()
		m.completeRunStatusLocked(ctx, job, run, status)
		return
	default:
	}

	// A run whose start was never recorded has no deadline to enforce.
	if job.Spec.TimeoutSeconds > 0 && run.StartedAtNano != 0 {
		deadline := time.Unix(0, run.StartedAtNano).Add(time.Duration(job.Spec.TimeoutSeconds) * time.Second)
		m.armTimeout(run.ID, run.ContainerID, deadline.Sub(m.now()))
	}

	m.background.Go(func() {
		defer cancelWait()

		select {
		case status := <-waitC:
			m.locks.Lock(job.ID)
			defer m.locks.Unlock(job.ID)

			m.completeRunStatusLocked(ctx, job, run, status)
		case <-m.stop:
			// Live-restore keeps the container running. Leave the run in
			// flight on disk for reconciliation on the next startup.
		}
	})
}

func (m *Manager) completeRunStatusLocked(ctx context.Context, job *jobsv0.Job, run *jobsv0.Run, status StateStatus) {
	if status == nil {
		// The Backend contract forbids closing without a status.
		m.completeRunLocked(ctx, job, run, terminalOutcome{state: jobsv0.RunStateFailed, errMsg: "waiting on run container: wait channel closed without a status"})
		return
	}

	code := int64(status.ExitCode())

	outcome := terminalOutcome{exitCode: &code, errMsg: run.Error}
	if waitErr := status.Err(); waitErr != nil {
		outcome.errMsg = waitErr.Error()
	}
	switch override := m.peekOverride(run.ID); {
	case override != "":
		outcome.state = override
	case outcome.errMsg != "":
		outcome.state = jobsv0.RunStateFailed
	case code == 0:
		outcome.state = jobsv0.RunStateSucceeded
	default:
		outcome.state = jobsv0.RunStateFailed
	}
	m.completeRunLocked(ctx, job, run, outcome)
}

// completeRunLocked writes the run's terminal record, removes its container
// when the spec asks for it, returns the job to idle, and fires any queued
// trigger. Called under the job's lock; job is the snapshot taken at fire
// time (its spec is immutable, its runtime state is re-read).
func (m *Manager) completeRunLocked(ctx context.Context, job *jobsv0.Job, run *jobsv0.Run, outcome terminalOutcome) {
	// A terminal outcome must be recorded even when the triggering context
	// was cancelled: dropping the write would leave the job and run stuck
	// in running until restart reconciliation.
	ctx = context.WithoutCancel(ctx)
	m.disarmTimeout(run.ID)
	m.clearOverride(run.ID)

	run.State = outcome.state
	run.ExitCode = nil
	if outcome.exitCode != nil {
		run.ExitCode = &jobsv0.ExitCode{Value: *outcome.exitCode}
	}
	run.Error = outcome.errMsg
	run.FinishedAtNano = m.now().UnixNano()
	m.finishRunLocked(ctx, job, run)
}

// recordRetryInterval is how long a failed write of a run's terminal record,
// or of its job's return to idle, waits before it is retried.
const recordRetryInterval = 5 * time.Second

// finishRunLocked persists a terminal run record and releases the job.
// Called under the job's lock.
func (m *Manager) finishRunLocked(ctx context.Context, job *jobsv0.Job, run *jobsv0.Run) {
	switch err := m.store.UpdateRun(ctx, run); {
	case err == nil:
		// The outcome is recorded before the container is removed, so a
		// crash in between cannot lose it.
		// Timed-out and cancelled runs always keep their container for
		// postmortem inspection.
		remove := (run.State == jobsv0.RunStateSucceeded && job.Spec.RemoveOnSuccess) ||
			(run.State == jobsv0.RunStateFailed && job.Spec.RemoveOnFailure)
		if !remove || run.ContainerID == "" {
			break
		}

		if err := m.backend.ContainerRm(ctx, run.ContainerID); err != nil {
			log.G(ctx).WithError(err).WithFields(log.Fields{"job": job.ID, "run": run.ID}).Warn("keeping run container that could not be auto-removed")
			break
		}

		if err := m.store.MarkContainerGone(ctx, job.ID, run.ID); err != nil {
			log.G(ctx).WithError(err).WithFields(log.Fields{"job": job.ID, "run": run.ID}).Warn("could not record run container removal")
		}
	case cerrdefs.IsNotFound(err):
		// The job was purged while the run was in flight, which left its
		// still-running container behind; it goes with the record.
		m.removeRunContainers(ctx, job.ID, []jobsv0.Run{*run})
	default:
		// Releasing the job would leave the run in flight with nothing left
		// to finish it, and a later run would hide it from restart
		// reconciliation. Keep the job occupied and retry; a shutdown
		// leaves the run in flight for that reconciliation.
		log.G(ctx).WithError(err).WithFields(log.Fields{"job": job.ID, "run": run.ID}).Warn("could not record run outcome; retrying")
		m.retryLater(job.ID, func() { m.finishRunLocked(ctx, job, run) })

		return
	}

	m.releaseJobLocked(ctx, job.ID, run.ID)
}

// releaseJobLocked returns a job whose run is terminal to idle, and fires any
// queued trigger. A failed write is retried on its own, as the terminal run
// record cannot be written again; until it lands the job stays running, so
// no other run starts. Called under the job's lock.
func (m *Manager) releaseJobLocked(ctx context.Context, jobID, runID string) {
	// Only the job's latest run releases it. A run whose completion was
	// retried can be overtaken when marking its job running failed: the job
	// stayed idle on disk, and a newer run took it meanwhile.
	if latest, err := m.store.LatestRun(jobID); err == nil && latest.ID != runID {
		m.broadcast(jobID)
		return
	}

	// The job may have been removed while the run was in flight; its runs
	// are then a tombstone and there is no state to restore.
	if current, err := m.store.Job(jobID); err == nil {
		current.State = jobsv0.JobStateIdle
		current.UpdatedAtNano = m.now().UnixNano()

		if err := m.store.UpdateJob(ctx, current); err != nil {
			log.G(ctx).WithError(err).WithFields(log.Fields{"job": jobID}).Warn("could not return job to idle; retrying")
			m.retryLater(jobID, func() { m.releaseJobLocked(ctx, jobID, runID) })

			return
		}
	}

	m.broadcast(jobID)

	m.mu.Lock()
	_, queued := m.queued[jobID]
	m.mu.Unlock()

	if queued {
		// Fire outside the held job lock.
		m.background.Go(func() { m.fireQueued(ctx, jobID) })
	}
}

// retryLater runs retry under the job's lock after recordRetryInterval,
// unless the manager shuts down first.
func (m *Manager) retryLater(jobID string, retry func()) {
	m.background.Go(func() {
		select {
		case <-m.stop:
			return
		case <-time.After(recordRetryInterval):
		}

		m.locks.Lock(jobID)
		defer m.locks.Unlock(jobID)

		retry()
	})
}

// fireQueued fires a job's deferred fire. The fire is claimed under the job's
// lock, so a pause or removal that takes the lock first discards it for good.
func (m *Manager) fireQueued(ctx context.Context, jobID string) {
	m.locks.Lock(jobID)
	defer m.locks.Unlock(jobID)

	m.mu.Lock()
	evidence := m.queued[jobID]
	delete(m.queued, jobID)
	m.mu.Unlock()

	if evidence == nil {
		return
	}
	// A failure to fire is the queued trigger's outcome, reported like a
	// scheduler skip.
	if _, err := m.tryFireLocked(ctx, jobID, evidence); err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{"job": jobID}).Warn("queued fire was not started")
	}
	// The deferred occurrence stayed recorded until now.
	m.persistNextFireLocked(ctx, jobID)
}

// removeRunContainers removes the containers of dropped run records in the
// background: a run container lives as long as its record, so neither history
// eviction nor removing a job's runs leaves containers behind. In-flight runs
// are skipped, as their containers cannot be removed while running; their
// completion removes them instead.
func (m *Manager) removeRunContainers(ctx context.Context, jobID string, runs []jobsv0.Run) {
	var ids []string

	for _, run := range runs {
		if isTerminalRunState(run.State) && run.ContainerID != "" && !run.ContainerGone {
			ids = append(ids, run.ContainerID)
		}
	}

	if len(ids) == 0 {
		return
	}

	ctx = context.WithoutCancel(ctx)

	m.background.Go(func() {
		for _, id := range ids {
			if err := m.backend.ContainerRm(ctx, id); err != nil && !cerrdefs.IsNotFound(err) {
				log.G(ctx).WithError(err).WithFields(log.Fields{"job": jobID, "container": id}).Warn("could not remove container of dropped run")
			}
		}
	})
}

// Cancel stops the job's in-flight run. It returns the in-flight run's ID,
// empty when no run was in flight; triggers keep firing. When a timeout
// claims the run's terminal decision first, the returned run is recorded
// timed_out: the run's recorded terminal state is authoritative, not this
// reply.
func (m *Manager) Cancel(ctx context.Context, jobRef string) (string, error) {
	job, err := m.resolveJob(jobRef)
	if err != nil {
		return "", err
	}
	m.locks.Lock(job.ID)
	defer m.locks.Unlock(job.ID)

	run, err := m.store.LatestRun(job.ID)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	if isTerminalRunState(run.State) {
		return "", nil
	}
	// A timeout or an earlier cancel may have claimed the terminal decision
	// already; the container is stopped regardless, as an earlier stop may
	// have failed.
	m.setOverride(run.ID, jobsv0.RunStateCancelled)
	// A pending run without a container cannot be observed here: container
	// creation happens synchronously under the job lock this method holds.
	containerID := run.ContainerID
	stopCtx := context.WithoutCancel(ctx)
	m.background.Go(func() {
		// Stopping can take the stop grace period; do not hold the job lock
		// for it. The watcher records the terminal state.
		if err := m.backend.ContainerStop(stopCtx, containerID); err != nil {
			log.G(stopCtx).WithError(err).WithFields(log.Fields{"job": job.ID, "run": run.ID}).Warn("could not stop cancelled run container")
		}
	})
	return run.ID, nil
}

// armTimeout schedules the run's deadline: past it, the container is
// stopped and the run ends timed_out (unless already cancelled).
func (m *Manager) armTimeout(runID, containerID string, timeout time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Shutdown disarmed the timers already; a timer armed after it would
	// stop a container that live-restore keeps running.
	if m.stopped {
		return
	}

	m.timers[runID] = time.AfterFunc(timeout, func() {
		// Claim the timer entry and the override in one critical section:
		// a completion racing this firing has already disarmed the entry,
		// and claiming the override past that point would leak it forever
		// (nothing clears overrides of completed runs) and stop a fresh
		// container.
		m.mu.Lock()
		if _, armed := m.timers[runID]; !armed {
			m.mu.Unlock()
			return
		}
		delete(m.timers, runID)
		// A cancel may have claimed the terminal decision already; the
		// container is stopped regardless, as the cancel's stop may have
		// failed.
		if _, claimed := m.overrides[runID]; !claimed {
			m.overrides[runID] = jobsv0.RunStateTimedOut
		}
		// Register the stop while still holding the mutex: Shutdown
		// disarms timers under it before draining background work, so the
		// drain cannot miss the stop or close the backend under it.
		m.background.Go(func() {
			if err := m.backend.ContainerStop(context.Background(), containerID); err != nil {
				log.G(context.Background()).WithError(err).WithFields(log.Fields{"run": runID}).Warn("could not stop timed-out run container")
			}
		})
		m.mu.Unlock()
	})
}

// disarmTimeout stops and forgets the run's timeout timer.
func (m *Manager) disarmTimeout(runID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if timer, armed := m.timers[runID]; armed {
		timer.Stop()
		delete(m.timers, runID)
	}
}

// setOverride claims the run's terminal decision; the first claimant wins.
func (m *Manager) setOverride(runID, state string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, claimed := m.overrides[runID]; !claimed {
		m.overrides[runID] = state
	}
}

// peekOverride reads the run's forced terminal state without consuming it;
// clearOverride forgets it once the terminal record is written.
func (m *Manager) peekOverride(runID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.overrides[runID]
}

func (m *Manager) clearOverride(runID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.overrides, runID)
}

// subscribe returns a channel closed on the job's next state transition.
// Callers that return without being woken must unsubscribe, or the entry
// lingers until the job's next transition, which may never come.
func (m *Manager) subscribe(jobID string) chan struct{} {
	ch := make(chan struct{})
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notify[jobID] = append(m.notify[jobID], ch)
	return ch
}

// unsubscribe withdraws a subscription that was not woken. A channel already
// consumed by broadcast is simply no longer found.
func (m *Manager) unsubscribe(jobID string, ch chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	subs := m.notify[jobID]
	for i, sub := range subs {
		if sub == ch {
			m.notify[jobID] = slices.Delete(subs, i, i+1)
			break
		}
	}
	if len(m.notify[jobID]) == 0 {
		delete(m.notify, jobID)
	}
}

// broadcast wakes every waiter subscribed to the job.
func (m *Manager) broadcast(jobID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ch := range m.notify[jobID] {
		close(ch)
	}
	delete(m.notify, jobID)
}

// scheduleConcurrency returns the job's effective concurrency policy.
func scheduleConcurrency(job *jobsv0.Job) string {
	if job.Spec != nil && job.Spec.Trigger != nil && job.Spec.Trigger.Schedule != nil && job.Spec.Trigger.Schedule.Concurrency != "" {
		return job.Spec.Trigger.Schedule.Concurrency
	}
	return jobsv0.ConcurrencyForbid
}
