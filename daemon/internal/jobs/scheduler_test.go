package jobs

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	jobsv0 "github.com/moby/moby/v2/extpoints/jobs/api/v0"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

// The scheduler tests run in a synctest bubble, whose clock starts at
// midnight UTC and advances instantly when the bubble sleeps. Tests call tick
// directly instead of starting the scheduler loop, so no test ever waits on
// cron granularity.

func TestSchedulerFiresAndRearms(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		m, fake := newTestManager(t)
		job, _, err := m.Create(t.Context(), "nightly", scheduleSpec(""))
		assert.NilError(t, err)

		// Registration armed the trigger and advertised the next occurrence.
		assert.Check(t, is.Equal(job.NextFireAtNano, start.Add(3*time.Hour).UnixNano()))

		// Before the occurrence, a tick fires nothing.
		m.sched.tick(t.Context())
		_, err = m.store.LatestRun(job.ID)
		assert.Check(t, cerrdefs.IsNotFound(err))

		// Past the occurrence, a tick fires exactly one run with schedule
		// evidence carrying the planned time.
		time.Sleep(3*time.Hour + 10*time.Second)
		m.sched.tick(t.Context())
		run, err := m.Wait(t.Context(), job.ID, "latest", jobsv0.WaitConditionRunning)
		assert.NilError(t, err)
		assert.Check(t, is.Equal(run.Trigger.Kind, jobsv0.TriggerKindSchedule))
		assert.Check(t, is.Equal(run.Trigger.ScheduledAtNano, start.Add(3*time.Hour).UnixNano()))

		// While the run is in flight the record advertises no next fire.
		inFlight, err := m.Inspect(t.Context(), job.ID)
		assert.NilError(t, err)
		assert.Check(t, is.Equal(inFlight.NextFireAtNano, int64(0)))

		fake.exit(run.ContainerID, 0, nil)
		done := waitTerminal(t, m, job.ID, run.ID)
		assert.Check(t, is.Equal(done.State, jobsv0.RunStateSucceeded))

		// After completion the job advertises the re-armed next occurrence.
		rearmed := start.Add(27 * time.Hour).UnixNano()

		waitJobSettled(t, m, job.ID, func(j *jobsv0.Job) bool { return j.NextFireAtNano == rearmed })
	})
}

func TestSchedulerForbidSkipsWhileRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		m, fake := newTestManager(t)
		job, _, err := m.Create(t.Context(), "nightly", scheduleSpec(""))
		assert.NilError(t, err)

		time.Sleep(3*time.Hour + time.Second)
		m.sched.tick(t.Context())
		first, err := m.Wait(t.Context(), job.ID, "latest", jobsv0.WaitConditionRunning)
		assert.NilError(t, err)

		// The next occurrence comes due while the first run is still in
		// flight: forbid (the default) drops the fire. The fire is driven
		// synchronously here — tick's asynchronous dispatch of due entries
		// is covered above — so the count below cannot race the dispatch.
		time.Sleep(24 * time.Hour)
		m.fireScheduled(t.Context(), job.ID, start.Add(27*time.Hour))
		page, _, _, err := m.store.Runs(job.ID, 10, "")
		assert.NilError(t, err)
		assert.Check(t, is.Equal(len(page), 1))

		fake.exit(first.ContainerID, 0, nil)
		waitTerminal(t, m, job.ID, first.ID)
	})
}

// shutdownCtx bounds the drain waits used mid-test.
func shutdownCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestSchedulerPauseResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		m, _ := newTestManager(t)
		job, _, err := m.Create(t.Context(), "nightly", scheduleSpec(""))
		assert.NilError(t, err)

		assert.NilError(t, m.Pause(t.Context(), job.ID))
		paused, err := m.Inspect(t.Context(), job.ID)
		assert.NilError(t, err)
		assert.Check(t, is.Equal(paused.NextFireAtNano, int64(0)))

		// The occurrence passes while paused: nothing fires, and resuming
		// re-arms from the next tick without backfilling.
		time.Sleep(4 * time.Hour)
		m.sched.tick(t.Context())
		_, err = m.store.LatestRun(job.ID)
		assert.Check(t, cerrdefs.IsNotFound(err))

		assert.NilError(t, m.Resume(t.Context(), job.ID))
		resumed, err := m.Inspect(t.Context(), job.ID)
		assert.NilError(t, err)
		assert.Check(t, is.Equal(resumed.NextFireAtNano, start.Add(27*time.Hour).UnixNano()))
	})
}

func TestSchedulerMissedFiresOnStart(t *testing.T) {
	for _, tc := range []struct {
		doc           string
		policy        string
		expectCatchUp bool
	}{
		{doc: "one fires a single catch-up", policy: "", expectCatchUp: true},
		{doc: "skip drops missed fires", policy: jobsv0.MissedFiresSkip, expectCatchUp: false},
	} {
		t.Run(tc.doc, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				m, fake := newTestManager(t)
				spec := scheduleSpec("")
				spec.Trigger.Schedule.MissedFires = tc.policy
				job, _, err := m.Create(t.Context(), "nightly", spec)
				assert.NilError(t, err)

				// The daemon was down across the 03:00 occurrence: the
				// clock advances but no tick observed it. Start applies the
				// policy from the persisted next-fire time.
				time.Sleep(5 * time.Hour)
				m.Start(t.Context())

				if tc.expectCatchUp {
					run, err := m.Wait(t.Context(), job.ID, "latest", jobsv0.WaitConditionRunning)
					assert.NilError(t, err)
					assert.Check(t, is.Equal(run.Trigger.ScheduledAtNano, start.Add(3*time.Hour).UnixNano()))
					fake.exit(run.ContainerID, 0, nil)
					waitTerminal(t, m, job.ID, run.ID)
				} else {
					assert.NilError(t, m.Shutdown(shutdownCtx(t)))
					_, err := m.store.LatestRun(job.ID)
					assert.Check(t, cerrdefs.IsNotFound(err))
				}

				// Either way the schedule re-armed from the next occurrence,
				// not from the backlog. The catch-up fire runs concurrently
				// with Start's own arm+persist, so the re-arm write can land
				// an instant after the run turns terminal.
				rearmed := start.Add(27 * time.Hour).UnixNano()

				waitJobSettled(t, m, job.ID, func(j *jobsv0.Job) bool { return j.NextFireAtNano == rearmed })
			})
		})
	}
}

func TestSchedulerReschedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		m, fake := newTestManager(t)
		job, _, err := m.Create(t.Context(), "nightly", scheduleSpec(""))
		assert.NilError(t, err)

		// A manual fire with reschedule stands in for the upcoming
		// occurrence: the schedule skips 03:00 today and re-arms on
		// tomorrow's.
		run, err := m.Run(t.Context(), job.ID, true)
		assert.NilError(t, err)
		fake.exit(run.ContainerID, 0, nil)
		waitTerminal(t, m, job.ID, run.ID)

		rearmed := start.Add(27 * time.Hour).UnixNano()

		waitJobSettled(t, m, job.ID, func(j *jobsv0.Job) bool { return j.NextFireAtNano == rearmed })

		// Today's occurrence passes without a fire.
		time.Sleep(4 * time.Hour)
		m.sched.tick(t.Context())
		assert.NilError(t, m.Shutdown(shutdownCtx(t)))
		page, _, _, err := m.store.Runs(job.ID, 10, "")
		assert.NilError(t, err)
		assert.Check(t, is.Equal(len(page), 1))
	})
}

func TestSchedulerRemoveDisarms(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m, _ := newTestManager(t)
		job, _, err := m.Create(t.Context(), "nightly", scheduleSpec(""))
		assert.NilError(t, err)

		assert.NilError(t, m.Remove(t.Context(), job.ID, ""))
		time.Sleep(4 * time.Hour)
		m.sched.tick(t.Context())
		assert.NilError(t, m.Shutdown(shutdownCtx(t)))
		_, err = m.store.LatestRun(job.ID)
		assert.Check(t, cerrdefs.IsNotFound(err), "a removed job must not fire")
	})
}

func TestSchedulerStartWithUnparseableStoredCron(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// A schedule the validation would never accept can still sit in the
		// store (written by a newer or buggy daemon); Start must leave the
		// job disarmed instead of failing the whole startup.
		store, _ := newTestStore(t)
		job := makeJob("job1", "broken")
		job.Spec.Trigger.Schedule.Cron = "not a cron"
		assert.NilError(t, store.CreateJob(t.Context(), job))

		m := NewManager(store, newFakeBackend(), testNameGenerator)
		m.Start(t.Context())
		t.Cleanup(func() { assert.Check(t, m.Shutdown(shutdownCtx(t))) })

		time.Sleep(24 * time.Hour)
		m.sched.tick(t.Context())
		_, err := m.store.LatestRun("job1")
		assert.Check(t, cerrdefs.IsNotFound(err), "a job with an unparseable schedule must never fire")
	})
}

func TestSchedulerHonorsTimezone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		m, _ := newTestManager(t)
		spec := scheduleSpec("")
		spec.Trigger.Schedule.Timezone = "Europe/Paris"
		job, _, err := m.Create(t.Context(), "paris-nightly", spec)
		assert.NilError(t, err)

		// 03:00 Paris is 02:00 UTC in January.
		paris, err := time.LoadLocation("Europe/Paris")
		assert.NilError(t, err)

		y, mo, d := start.Date()
		assert.Check(t, is.Equal(job.NextFireAtNano, time.Date(y, mo, d, 3, 0, 0, 0, paris).UnixNano()))
		assert.Check(t, is.Equal(job.NextFireAtNano, start.Add(2*time.Hour).UnixNano()))
	})
}

func TestSchedulerCreateRejectsNeverFiringCron(t *testing.T) {
	m, _ := newTestManager(t)
	spec := scheduleSpec("")
	spec.Trigger.Schedule.Cron = "0 0 30 2 *"
	_, _, err := m.Create(t.Context(), "never", spec)
	assert.Check(t, cerrdefs.IsInvalidArgument(err), "want invalid-argument, got %v", err)
}

// TestSchedulerStartKeepsRescheduledOccurrence checks that a restart does not
// replay occurrences replaced by one or more manual runs.
func TestSchedulerStartKeepsRescheduledOccurrence(t *testing.T) {
	for _, tc := range []struct {
		doc  string
		runs int
	}{
		{doc: "one reschedule", runs: 1},
		{doc: "repeated reschedules", runs: 2},
	} {
		t.Run(tc.doc, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				m, fake, kv := newRestartableManager(t)
				job, _, err := m.Create(t.Context(), "nightly", scheduleSpec(""))
				assert.NilError(t, err)

				for range tc.runs {
					run, err := m.Run(t.Context(), job.ID, true)
					assert.NilError(t, err)
					fake.exit(run.ContainerID, 0, nil)
					waitTerminal(t, m, job.ID, run.ID)
					waitJobSettled(t, m, job.ID, func(j *jobsv0.Job) bool { return j.State == jobsv0.JobStateIdle })
				}

				expected := start.Add(3*time.Hour + time.Duration(tc.runs)*24*time.Hour).UnixNano()
				before, err := m.Inspect(t.Context(), job.ID)
				assert.NilError(t, err)
				assert.Equal(t, before.NextFireAtNano, expected)

				restarted := restartManager(t, m, kv, fake)
				restarted.Start(t.Context())
				after, err := restarted.Inspect(t.Context(), job.ID)
				assert.NilError(t, err)
				assert.Equal(t, after.NextFireAtNano, expected)
			})
		})
	}
}

// TestSchedulerDropsSupersededFire checks that a tick's fire delayed past a
// pause neither overwrites the cleared next-fire time nor, after a resume,
// runs the occurrence of the superseded schedule.
func TestSchedulerDropsSupersededFire(t *testing.T) {
	m, _ := newTestManager(t)
	job, _, err := m.Create(t.Context(), "nightly", scheduleSpec(""))
	assert.NilError(t, err)

	m.sched.mu.Lock()
	gen := m.sched.gens[job.ID]
	m.sched.mu.Unlock()

	scheduledAt := time.Unix(0, job.NextFireAtNano)

	assert.NilError(t, m.Pause(t.Context(), job.ID))
	m.fireDue(t.Context(), job.ID, gen, scheduledAt)
	paused, err := m.store.Job(job.ID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(paused.NextFireAtNano, int64(0)))

	assert.NilError(t, m.Resume(t.Context(), job.ID))
	m.fireDue(t.Context(), job.ID, gen, scheduledAt)
	_, err = m.store.LatestRun(job.ID)
	assert.Check(t, cerrdefs.IsNotFound(err), "a superseded fire must not run")
}

// TestSchedulerRescheduleReplacesConsumedOccurrence checks that a reschedule
// landing after a tick consumed the upcoming occurrence, but before its fire
// ran, replaces that occurrence instead of letting it fire and skipping the
// following one too.
func TestSchedulerRescheduleReplacesConsumedOccurrence(t *testing.T) {
	m, fake := newTestManager(t)
	job, _, err := m.Create(t.Context(), "nightly", scheduleSpec(""))
	assert.NilError(t, err)

	fires, _ := m.sched.takeDue(time.Unix(0, job.NextFireAtNano))
	assert.Assert(t, is.Len(fires, 1))

	following, _ := m.sched.nextFor(job.ID)

	run, err := m.Run(t.Context(), job.ID, true)
	assert.NilError(t, err)
	fake.exit(run.ContainerID, 0, nil)
	waitTerminal(t, m, job.ID, run.ID)
	waitJobSettled(t, m, job.ID, func(j *jobsv0.Job) bool { return j.State == jobsv0.JobStateIdle })

	m.fireDue(t.Context(), fires[0].jobID, fires[0].gen, fires[0].scheduledAt)
	page, _, _, err := m.store.Runs(job.ID, 10, "")
	assert.NilError(t, err)
	assert.Check(t, is.Len(page, 1), "the replaced occurrence must not fire")

	next, _ := m.sched.nextFor(job.ID)
	assert.Check(t, is.Equal(next.UnixNano(), following.UnixNano()))
}

// TestSchedulerStartSkipsCatchUpAlreadyFired checks that a restart does not
// catch up an occurrence whose run was recorded before the daemon died, ahead
// of the next fire time.
func TestSchedulerStartSkipsCatchUpAlreadyFired(t *testing.T) {
	store, _ := newTestStore(t)
	missed := time.Now().Add(-time.Hour).UnixNano()
	job := makeJob("job1", "nightly")
	job.NextFireAtNano = missed
	assert.NilError(t, store.CreateJob(t.Context(), job))

	run := makeRun(job.ID, "run1", jobsv0.RunStateSucceeded)
	run.Trigger = &jobsv0.TriggerEvidence{Kind: jobsv0.TriggerKindSchedule, ScheduledAtNano: missed, FiredAtNano: missed}
	assert.NilError(t, store.CreateRun(t.Context(), run))

	m := NewManager(store, newFakeBackend(), testNameGenerator)
	m.Start(t.Context())
	assert.NilError(t, m.Shutdown(shutdownCtx(t)))

	page, _, _, err := m.store.Runs(job.ID, 10, "")
	assert.NilError(t, err)
	assert.Check(t, is.Len(page, 1))
}
