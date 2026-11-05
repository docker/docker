package jobs

import (
	"errors"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	jobsv0 "github.com/moby/moby/v2/extpoints/jobs/api/v0"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
)

// newRestartableManager builds a manager whose storage is kept so a test
// can simulate a daemon restart against the same persisted records.
func newRestartableManager(t *testing.T) (*Manager, *fakeBackend, *testKV) {
	t.Helper()
	kv := newTestKV(t)
	store, err := NewStore(t.Context(), kv)
	assert.NilError(t, err)
	fake := newFakeBackend()
	m := NewManager(store, fake, testNameGenerator)
	t.Cleanup(func() { assert.Check(t, m.Shutdown(shutdownCtx(t))) })
	return m, fake, kv
}

// restartManager shuts the previous manager down, reloads the store from
// storage — the path a real daemon restart takes — and reconciles.
func restartManager(t *testing.T, prev *Manager, kv *testKV, fake *fakeBackend) *Manager {
	t.Helper()
	assert.NilError(t, prev.Shutdown(shutdownCtx(t)))
	store, err := NewStore(t.Context(), kv)
	assert.NilError(t, err)
	m := NewManager(store, fake, testNameGenerator)
	t.Cleanup(func() { assert.Check(t, m.Shutdown(shutdownCtx(t))) })
	m.Restore(t.Context())
	return m
}

func TestRestoreReattachesRunningContainer(t *testing.T) {
	m1, fake, kv := newRestartableManager(t)
	_, _, err := m1.Create(t.Context(), "backup", manualSpec())
	assert.NilError(t, err)
	run, err := m1.Run(t.Context(), "backup", false)
	assert.NilError(t, err)

	// The container is still running across the restart (live-restore).
	m2 := restartManager(t, m1, kv, fake)
	kept, err := m2.InspectRun(t.Context(), "backup", run.ID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(kept.State, jobsv0.RunStateRunning))

	// The re-attached watcher records the exit like any other run.
	fake.exit(run.ContainerID, 0, nil)
	done := waitTerminal(t, m2, "backup", run.ID)
	assert.Check(t, is.Equal(done.State, jobsv0.RunStateSucceeded))
	waitJobSettled(t, m2, "backup", func(j *jobsv0.Job) bool { return j.State == jobsv0.JobStateIdle })
}

func TestRestoreResolvesContainerExitedDuringDowntime(t *testing.T) {
	m1, fake, kv := newRestartableManager(t)
	_, _, err := m1.Create(t.Context(), "backup", manualSpec())
	assert.NilError(t, err)
	run, err := m1.Run(t.Context(), "backup", false)
	assert.NilError(t, err)

	assert.NilError(t, m1.Shutdown(shutdownCtx(t)))
	// The container exits while the daemon is down; the record still says
	// running when the store reloads.
	fake.exit(run.ContainerID, 3, nil)

	store, err := NewStore(t.Context(), kv)
	assert.NilError(t, err)
	m2 := NewManager(store, fake, testNameGenerator)
	t.Cleanup(func() { assert.Check(t, m2.Shutdown(shutdownCtx(t))) })
	m2.Restore(t.Context())

	done := waitTerminal(t, m2, "backup", run.ID)
	assert.Check(t, is.Equal(done.State, jobsv0.RunStateFailed))
	assert.Assert(t, done.ExitCode != nil)
	assert.Check(t, is.Equal(done.ExitCode.Value, int64(3)))
}

func TestRestoreFailsLostContainer(t *testing.T) {
	m1, _, kv := newRestartableManager(t)
	_, _, err := m1.Create(t.Context(), "backup", manualSpec())
	assert.NilError(t, err)
	run, err := m1.Run(t.Context(), "backup", false)
	assert.NilError(t, err)

	// The container vanished entirely while the daemon was down.
	m2 := restartManager(t, m1, kv, newFakeBackend())
	done := waitTerminal(t, m2, "backup", run.ID)
	assert.Check(t, is.Equal(done.State, jobsv0.RunStateFailed))
	assert.Check(t, is.ErrorContains(errors.New(done.Error), "waiting on run container"))
	waitJobSettled(t, m2, "backup", func(j *jobsv0.Job) bool { return j.State == jobsv0.JobStateIdle })
}

func TestRestoreFailsUnstartedRun(t *testing.T) {
	for _, tc := range []struct {
		doc           string
		withContainer bool
	}{
		{doc: "no container was created"},
		{doc: "a container was left behind", withContainer: true},
	} {
		t.Run(tc.doc, func(t *testing.T) {
			m1, fake, kv := newRestartableManager(t)
			job, _, err := m1.Create(t.Context(), "backup", manualSpec())
			assert.NilError(t, err)

			// Craft the crash shape directly in the store: a pending run
			// whose start was never recorded, on a job stuck running.
			run := &jobsv0.Run{ID: "orphan", JobID: job.ID, State: jobsv0.RunStatePending, CreatedAtNano: 1}
			if tc.withContainer {
				created, err := fake.ContainerCreate(t.Context(), ContainerCreateRequest{Name: "job-backup-orphan"})
				assert.NilError(t, err)
				run.ContainerID = created.ID
			}
			assert.NilError(t, m1.store.CreateRun(t.Context(), run))
			stored, err := m1.store.Job(job.ID)
			assert.NilError(t, err)
			stored.State = jobsv0.JobStateRunning
			assert.NilError(t, m1.store.UpdateJob(t.Context(), stored))

			m2 := restartManager(t, m1, kv, fake)
			done := waitStoreTerminal(t, m2, job.ID, run.ID)
			assert.Check(t, is.Equal(done.State, jobsv0.RunStateFailed))
			assert.Check(t, is.ErrorContains(errors.New(done.Error), "before the run start was recorded"))
			waitJobSettled(t, m2, job.ID, func(j *jobsv0.Job) bool { return j.State == jobsv0.JobStateIdle })
			if tc.withContainer {
				// The container is stopped before the job is released.
				assert.Check(t, is.Contains(fake.stoppedContainers(), run.ContainerID))
			}
		})
	}
}

// TestRestoreUnstoppableUnstartedRunKeepsJobOccupied checks that a container
// left behind by an unstarted run, which may have started just as the daemon
// died, keeps its job occupied while it cannot be stopped.
func TestRestoreUnstoppableUnstartedRunKeepsJobOccupied(t *testing.T) {
	m1, fake, kv := newRestartableManager(t)
	job, _, err := m1.Create(t.Context(), "backup", manualSpec())
	assert.NilError(t, err)

	created, err := fake.ContainerCreate(t.Context(), ContainerCreateRequest{Name: "job-backup-orphan"})
	assert.NilError(t, err)
	assert.NilError(t, fake.ContainerStart(t.Context(), created.ID))

	run := &jobsv0.Run{ID: "orphan", JobID: job.ID, State: jobsv0.RunStatePending, CreatedAtNano: 1, ContainerID: created.ID}
	assert.NilError(t, m1.store.CreateRun(t.Context(), run))
	stored, err := m1.store.Job(job.ID)
	assert.NilError(t, err)

	stored.State = jobsv0.JobStateRunning
	assert.NilError(t, m1.store.UpdateJob(t.Context(), stored))

	fake.stopErr = errors.New("injected stop failure")
	m2 := restartManager(t, m1, kv, fake)
	_, err = m2.Run(t.Context(), job.ID, false)
	assert.Check(t, cerrdefs.IsFailedPrecondition(err), "want failed-precondition, got %v", err)

	fake.exit(run.ContainerID, 0, nil)
	done := waitStoreTerminal(t, m2, job.ID, run.ID)
	assert.Check(t, is.Equal(done.State, jobsv0.RunStateFailed))
	assert.Check(t, is.ErrorContains(errors.New(done.Error), "before the run start was recorded"))
	waitJobSettled(t, m2, job.ID, func(j *jobsv0.Job) bool { return j.State == jobsv0.JobStateIdle })
}

// TestRestoreCatchesUpFireMissedDuringRun checks that a schedule occurrence
// missed while the daemon was down still gets its catch-up run when the
// daemon stopped during a run.
func TestRestoreCatchesUpFireMissedDuringRun(t *testing.T) {
	m1, fake, kv := newRestartableManager(t)
	clk := &fakeClock{t: jan15}
	m1.now = clk.Now
	job, _, err := m1.Create(t.Context(), "nightly", scheduleSpec(""))
	assert.NilError(t, err)

	clk.Advance(3*time.Hour + time.Second)
	m1.sched.tick(t.Context())
	run, err := m1.Wait(t.Context(), job.ID, "latest", jobsv0.WaitConditionRunning)
	assert.NilError(t, err)

	// The run finishes and the next occurrence passes while the daemon is
	// down.
	assert.NilError(t, m1.Shutdown(shutdownCtx(t)))
	fake.exit(run.ContainerID, 0, nil)
	clk.Advance(48 * time.Hour)

	store, err := NewStore(t.Context(), kv)
	assert.NilError(t, err)

	m2 := NewManager(store, fake, testNameGenerator)
	m2.now = clk.Now

	t.Cleanup(func() { assert.Check(t, m2.Shutdown(shutdownCtx(t))) })
	m2.Restore(t.Context())
	m2.Start(t.Context())

	var catchUp *jobsv0.Run

	poll.WaitOn(t, func(poll.LogT) poll.Result {
		latest, err := m2.store.LatestRun(job.ID)
		if err != nil {
			return poll.Error(err)
		}

		if latest.ID == run.ID || latest.State != jobsv0.RunStateRunning {
			return poll.Continue("no catch-up run yet")
		}

		catchUp = latest

		return poll.Success()
	})
	assert.Check(t, is.Equal(catchUp.Trigger.ScheduledAtNano, jan15.Add(27*time.Hour).UnixNano()))
	fake.exit(catchUp.ContainerID, 0, nil)
	waitTerminal(t, m2, job.ID, catchUp.ID)
}

func TestRestoreIdlesStuckJob(t *testing.T) {
	m1, fake, kv := newRestartableManager(t)
	_, _, err := m1.Create(t.Context(), "backup", manualSpec())
	assert.NilError(t, err)
	run, err := m1.Run(t.Context(), "backup", false)
	assert.NilError(t, err)
	fake.exit(run.ContainerID, 0, nil)
	waitTerminal(t, m1, "backup", run.ID)

	// Crash shape: the run's terminal write landed, the job's idle write
	// did not.
	stored, err := m1.store.Job(run.JobID)
	assert.NilError(t, err)
	stored.State = jobsv0.JobStateRunning
	assert.NilError(t, m1.store.UpdateJob(t.Context(), stored))

	m2 := restartManager(t, m1, kv, fake)
	job, err := m2.Inspect(t.Context(), "backup")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(job.State, jobsv0.JobStateIdle))
}

func TestRestoreTimeouts(t *testing.T) {
	t.Run("completed run keeps its outcome past the deadline", func(t *testing.T) {
		t.Parallel()

		m1, fake, kv := newRestartableManager(t)
		clk := &fakeClock{t: jan15}
		m1.now = clk.Now
		spec := manualSpec()
		spec.TimeoutSeconds = 60
		job, _, err := m1.Create(t.Context(), "quick", spec)
		assert.NilError(t, err)
		run, err := m1.Run(t.Context(), job.ID, false)
		assert.NilError(t, err)
		assert.NilError(t, m1.Shutdown(shutdownCtx(t)))

		// The container exits within its timeout, but startup happens later.
		fake.exit(run.ContainerID, 0, nil)
		clk.Advance(time.Hour)

		store, err := NewStore(t.Context(), kv)
		assert.NilError(t, err)

		m2 := NewManager(store, fake, testNameGenerator)
		m2.now = clk.Now

		t.Cleanup(func() { assert.Check(t, m2.Shutdown(shutdownCtx(t))) })
		m2.Restore(t.Context())

		done := waitTerminal(t, m2, job.ID, run.ID)
		assert.Equal(t, done.State, jobsv0.RunStateSucceeded)
		assert.Assert(t, done.ExitCode != nil)
		assert.Equal(t, done.ExitCode.Value, int64(0))
		assert.Equal(t, len(fake.stoppedContainers()), 0)
	})

	t.Run("expired deadline is enforced on restore", func(t *testing.T) {
		m1, fake, kv := newRestartableManager(t)
		clk := &fakeClock{t: jan15}
		m1.now = clk.Now
		spec := manualSpec()
		spec.TimeoutSeconds = 60
		_, _, err := m1.Create(t.Context(), "slow", spec)
		assert.NilError(t, err)
		run, err := m1.Run(t.Context(), "slow", false)
		assert.NilError(t, err)

		assert.NilError(t, m1.Shutdown(shutdownCtx(t)))
		store, err := NewStore(t.Context(), kv)
		assert.NilError(t, err)
		m2 := NewManager(store, fake, testNameGenerator)
		t.Cleanup(func() { assert.Check(t, m2.Shutdown(shutdownCtx(t))) })
		// The daemon comes back long past the run's deadline.
		clk.Advance(time.Hour)
		m2.now = clk.Now
		m2.Restore(t.Context())

		done := waitTerminal(t, m2, "slow", run.ID)
		assert.Check(t, is.Equal(done.State, jobsv0.RunStateTimedOut))
		assert.Check(t, is.Contains(fake.stoppedContainers(), run.ContainerID))
	})

	t.Run("remaining deadline is re-armed", func(t *testing.T) {
		m1, fake, kv := newRestartableManager(t)
		spec := manualSpec()
		spec.TimeoutSeconds = 3600
		_, _, err := m1.Create(t.Context(), "slow", spec)
		assert.NilError(t, err)
		run, err := m1.Run(t.Context(), "slow", false)
		assert.NilError(t, err)

		m2 := restartManager(t, m1, kv, fake)
		m2.mu.Lock()
		_, armed := m2.timers[run.ID]
		m2.mu.Unlock()
		assert.Check(t, armed, "the surviving deadline must be re-armed")
		fake.exit(run.ContainerID, 0, nil)
		waitTerminal(t, m2, "slow", run.ID)
	})
}

// TestRestoreQueuesCatchUpBehindReattachedRun checks that an occurrence
// missed while the daemon was down and a live-restored run kept going is
// queued behind that run under the queue concurrency policy.
func TestRestoreQueuesCatchUpBehindReattachedRun(t *testing.T) {
	m1, fake, kv := newRestartableManager(t)
	job, _, err := m1.Create(t.Context(), "nightly", scheduleSpec(jobsv0.ConcurrencyQueue))
	assert.NilError(t, err)
	run, err := m1.Run(t.Context(), job.ID, false)
	assert.NilError(t, err)

	missed := time.Now().Add(-time.Hour).UnixNano()
	stored, err := m1.store.Job(job.ID)
	assert.NilError(t, err)

	stored.NextFireAtNano = missed
	assert.NilError(t, m1.store.UpdateJob(t.Context(), stored))

	m2 := restartManager(t, m1, kv, fake)
	m2.Start(t.Context())
	poll.WaitOn(t, func(poll.LogT) poll.Result {
		m2.mu.Lock()
		defer m2.mu.Unlock()

		if m2.queued[job.ID] != nil {
			return poll.Success()
		}

		return poll.Continue("catch-up not queued yet")
	})

	// Until the deferred catch-up runs, the record keeps the missed
	// occurrence, so another restart would catch it up again.
	queued, err := m2.store.Job(job.ID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(queued.NextFireAtNano, missed))

	fake.exit(run.ContainerID, 0, nil)

	var catchUp *jobsv0.Run

	poll.WaitOn(t, func(poll.LogT) poll.Result {
		latest, err := m2.store.LatestRun(job.ID)
		if err != nil {
			return poll.Error(err)
		}

		if latest.ID == run.ID || latest.State != jobsv0.RunStateRunning {
			return poll.Continue("no catch-up run yet")
		}

		catchUp = latest

		return poll.Success()
	})
	assert.Check(t, is.Equal(catchUp.Trigger.ScheduledAtNano, missed))
	fake.exit(catchUp.ContainerID, 0, nil)
	waitTerminal(t, m2, job.ID, catchUp.ID)
}

// TestRestoreRemovesUnrecordedContainer checks that a container created just
// before the daemon died, whose ID its run never recorded, is found by its
// name and removed.
func TestRestoreRemovesUnrecordedContainer(t *testing.T) {
	m1, fake, kv := newRestartableManager(t)
	job, _, err := m1.Create(t.Context(), "backup", manualSpec())
	assert.NilError(t, err)

	run := &jobsv0.Run{ID: "orphan", JobID: job.ID, State: jobsv0.RunStatePending, CreatedAtNano: 1}
	assert.NilError(t, m1.store.CreateRun(t.Context(), run))
	created, err := fake.ContainerCreate(t.Context(), ContainerCreateRequest{Name: runContainerName(job, run)})
	assert.NilError(t, err)

	stored, err := m1.store.Job(job.ID)
	assert.NilError(t, err)

	stored.State = jobsv0.JobStateRunning
	assert.NilError(t, m1.store.UpdateJob(t.Context(), stored))

	m2 := restartManager(t, m1, kv, fake)
	done := waitStoreTerminal(t, m2, job.ID, run.ID)
	assert.Check(t, is.Equal(done.State, jobsv0.RunStateFailed))
	assert.Check(t, fake.container(t, created.ID).removed)
}
