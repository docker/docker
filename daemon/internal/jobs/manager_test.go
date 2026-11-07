package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	jobsv0 "github.com/moby/moby/v2/extpoints/jobs/api/v0"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
)

// fakeStatus is a container exit observed by a fake wait.
type fakeStatus struct {
	code int
	err  error
}

func (s fakeStatus) ExitCode() int { return s.code }
func (s fakeStatus) Err() error    { return s.err }

// fakeContainer is one container tracked by the fake backend.
type fakeContainer struct {
	config  ContainerCreateRequest
	started bool
	removed bool
	exited  *fakeStatus
	waiters []chan StateStatus
}

// fakeBackend implements Backend in memory. Tests drive container exits
// explicitly with exit(), and stops behave like a kill: the container exits
// with code 137.
type fakeBackend struct {
	mu         sync.Mutex
	seq        int
	containers map[string]*fakeContainer
	createErr  error
	startErr   error
	stopErr    error
	waitErr    error
	stopped    []string
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{containers: make(map[string]*fakeContainer)}
}

func (b *fakeBackend) ContainerCreate(ctx context.Context, config ContainerCreateRequest) (container.CreateResponse, error) {
	// Like the Engine API client, fail requests whose context is done.
	if err := ctx.Err(); err != nil {
		return container.CreateResponse{}, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.createErr != nil {
		return container.CreateResponse{}, b.createErr
	}
	b.seq++
	id := fmt.Sprintf("ctr%d", b.seq)
	b.containers[id] = &fakeContainer{config: config}
	return container.CreateResponse{ID: id}, nil
}

func (b *fakeBackend) ContainerStart(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.startErr != nil {
		return b.startErr
	}
	ctr, exists := b.containers[name]
	if !exists {
		return errors.New("no such container")
	}
	ctr.started = true
	return nil
}

func (b *fakeBackend) ContainerStop(_ context.Context, name string) error {
	b.mu.Lock()
	b.stopped = append(b.stopped, name)
	err := b.stopErr
	b.mu.Unlock()

	if err != nil {
		return err
	}

	b.exit(name, 137, nil)
	return nil
}

func (b *fakeBackend) ContainerRm(_ context.Context, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Like the Engine API, accept a container name as well as an ID.
	for id, ctr := range b.containers {
		if id == name || ctr.config.Name == name {
			ctr.removed = true
			return nil
		}
	}

	return errors.New("no such container")
}

func (b *fakeBackend) ContainerWait(_ context.Context, name string) (<-chan StateStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.waitErr != nil {
		return nil, b.waitErr
	}
	ctr, exists := b.containers[name]
	if !exists {
		return nil, errors.New("no such container")
	}
	waitC := make(chan StateStatus, 1)
	if ctr.exited != nil {
		waitC <- *ctr.exited
		return waitC, nil
	}
	ctr.waiters = append(ctr.waiters, waitC)
	return waitC, nil
}

// exit delivers a container's final exit to its waiters; later waits observe
// it immediately. A second exit for the same container is ignored.
func (b *fakeBackend) exit(name string, code int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ctr, exists := b.containers[name]
	if !exists || ctr.exited != nil {
		return
	}
	ctr.exited = &fakeStatus{code: code, err: err}
	for _, waitC := range ctr.waiters {
		waitC <- *ctr.exited
	}
	ctr.waiters = nil
}

func (b *fakeBackend) container(t *testing.T, id string) fakeContainer {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	ctr, exists := b.containers[id]
	assert.Assert(t, exists, "no container %s in fake backend", id)
	return *ctr
}

func (b *fakeBackend) createCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.containers)
}

// stoppedContainers returns a copy of the stop log, for assertions that are
// not ordered after the stop by a happens-before chain.
func (b *fakeBackend) stoppedContainers() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.stopped)
}

// testNameGenerator stands in for the daemon's name generation:
// deterministic, distinct per retry.
func testNameGenerator(_ context.Context, retry int64) (string, error) {
	return fmt.Sprintf("generated-job-%d", retry), nil
}

func newTestManager(t *testing.T) (*Manager, *fakeBackend) {
	t.Helper()
	store, _ := newTestStore(t)
	fake := newFakeBackend()
	m := NewManager(store, fake, testNameGenerator)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		assert.Check(t, m.Shutdown(ctx))
	})
	return m, fake
}

// tryFire fires through the tryFireLocked chokepoint, taking the job's lock.
func (m *Manager) tryFire(ctx context.Context, jobID string, evidence *jobsv0.TriggerEvidence) (*jobsv0.Run, error) {
	m.locks.Lock(jobID)
	defer m.locks.Unlock(jobID)

	return m.tryFireLocked(ctx, jobID, evidence)
}

func manualSpec() *jobsv0.JobSpec {
	return &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"busybox","Cmd":["/backup.sh"]}`)}
}

func scheduleSpec(concurrency string) *jobsv0.JobSpec {
	return &jobsv0.JobSpec{
		ContainerSpec: []byte(`{"Image":"busybox"}`),
		Trigger: &jobsv0.Trigger{Schedule: &jobsv0.ScheduleTrigger{
			Cron:        "0 3 * * *",
			Concurrency: concurrency,
		}},
	}
}

// waitTerminal blocks until the run reaches a terminal state.
func waitTerminal(t *testing.T, m *Manager, jobRef, runRef string) *jobsv0.Run {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	run, err := m.Wait(ctx, jobRef, runRef, jobsv0.WaitConditionTerminal)
	assert.NilError(t, err)
	return run
}

// waitJobSettled polls Inspect until settled reports true, and returns the
// settled job. Wait's contract covers the run record only: the completion
// transition writes the run's terminal state first (a crash between the two
// must leave the run resolved), then returns the job record to idle — so
// assertions on the job record after a wait must poll that second write.
func waitJobSettled(t *testing.T, m *Manager, jobRef string, settled func(*jobsv0.Job) bool) *jobsv0.Job {
	t.Helper()
	var job *jobsv0.Job
	poll.WaitOn(t, func(poll.LogT) poll.Result {
		var err error
		job, err = m.Inspect(t.Context(), jobRef)
		if err != nil {
			return poll.Error(err)
		}
		if settled(job) {
			return poll.Success()
		}
		return poll.Continue("job not settled yet (state %s, next fire %d)", job.State, job.NextFireAtNano)
	})
	return job
}

// waitStoreTerminal observes a run in the store until it turns terminal,
// for runs whose job record is gone (tombstone history) and which the
// API-level Wait therefore correctly refuses to resolve.
func waitStoreTerminal(t *testing.T, m *Manager, jobID, runID string) *jobsv0.Run {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		run, err := m.store.Run(jobID, runID)
		assert.NilError(t, err)
		if isTerminalRunState(run.State) {
			return run
		}
		sub := m.subscribe(jobID)
		select {
		case <-ctx.Done():
			t.Fatal("run never turned terminal")
		case <-sub:
		}
	}
}

func TestManagerCreateIdempotency(t *testing.T) {
	m, fake := newTestManager(t)

	job, created, err := m.Create(t.Context(), "backup", manualSpec())
	assert.NilError(t, err)
	assert.Check(t, created)

	// Same spec, different JSON formatting: same identity, no-op.
	respaced := &jobsv0.JobSpec{ContainerSpec: []byte(`{ "Cmd": ["/backup.sh"], "Image": "busybox" }`)}
	again, created, err := m.Create(t.Context(), "backup", respaced)
	assert.NilError(t, err)
	assert.Check(t, !created)
	assert.Check(t, is.Equal(again.ID, job.ID))
	assert.Check(t, is.Equal(again.SpecHash, job.SpecHash))

	// Different spec under the same name: already-exists carrying both
	// hashes, so a gRPC adapter can map it to codes.AlreadyExists.
	other := &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"alpine"}`)}
	_, _, err = m.Create(t.Context(), "backup", other)
	assert.Check(t, cerrdefs.IsAlreadyExists(err), "want already-exists, got %v", err)
	assert.Check(t, is.ErrorContains(err, job.SpecHash))

	// Registration never creates containers, whatever the trigger.
	_, _, err = m.Create(t.Context(), "nightly", scheduleSpec(""))
	assert.NilError(t, err)
	assert.Check(t, is.Equal(fake.createCount(), 0))
}

func TestManagerSpecHashCanonicalization(t *testing.T) {
	hash := func(t *testing.T, spec *jobsv0.JobSpec) string {
		t.Helper()
		m, _ := newTestManager(t)
		decoded, err := m.validateSpec(spec)
		assert.NilError(t, err)
		h, err := specHash(spec, decoded)
		assert.NilError(t, err)
		return h
	}

	base := scheduleSpec("")
	assert.Check(t, is.Equal(hash(t, base), hash(t, scheduleSpec(jobsv0.ConcurrencyForbid))),
		"an empty concurrency policy must hash like the explicit default")

	utc := scheduleSpec("")
	utc.Trigger.Schedule.Timezone = "UTC"
	assert.Check(t, is.Equal(hash(t, base), hash(t, utc)),
		"an empty timezone must hash like explicit UTC")

	capped := manualSpec()
	capped.RunHistoryLimit = DefaultRunHistoryLimit
	assert.Check(t, is.Equal(hash(t, manualSpec()), hash(t, capped)),
		"a zero history limit must hash like the explicit default")

	reordered := manualSpec()
	reordered.ContainerSpec = []byte(`{"Cmd":["/backup.sh"],"Image":"busybox"}`)
	assert.Check(t, is.Equal(hash(t, manualSpec()), hash(t, reordered)),
		"JSON key order must not change a job's identity")

	swapped := manualSpec()
	swapped.ContainerSpec = []byte(`{"Image":"busybox","Cmd":["/other.sh"]}`)
	assert.Check(t, hash(t, manualSpec()) != hash(t, swapped), "Cmd is order-sensitive identity")
}

// TestManagerJobsFromOtherDaemonVersions covers jobs registered by another
// daemon version: their persisted hash came from that daemon's
// canonicalization, and their container spec may carry a field this daemon's
// API types no longer have.
func TestManagerJobsFromOtherDaemonVersions(t *testing.T) {
	store, _ := newTestStore(t)
	replayed := makeJob("job1", "replayed")
	assert.NilError(t, store.CreateJob(t.Context(), replayed))

	legacy := makeJob("job2", "legacy")
	legacy.Spec.ContainerSpec = []byte(`{"Image":"busybox","RemovedField":true}`)
	assert.NilError(t, store.CreateJob(t.Context(), legacy))

	fake := newFakeBackend()
	m := NewManager(store, fake, testNameGenerator)

	t.Cleanup(func() { assert.Check(t, m.Shutdown(shutdownCtx(t))) })

	job, created, err := m.Create(t.Context(), "replayed", replayed.Spec)
	assert.NilError(t, err, "replaying a stored spec must stay idempotent")
	assert.Check(t, !created)
	assert.Check(t, is.Equal(job.ID, "job1"))

	run, err := m.Run(t.Context(), "legacy", false)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(run.State, jobsv0.RunStateRunning), "run error: %q", run.Error)
	fake.exit(run.ContainerID, 0, nil)
	waitTerminal(t, m, "legacy", run.ID)
}

func TestManagerValidation(t *testing.T) {
	m, _ := newTestManager(t)
	for _, tc := range []struct {
		doc  string
		name string
		spec *jobsv0.JobSpec
	}{
		{doc: "empty name", name: "", spec: manualSpec()},
		{doc: "invalid name", name: "no spaces allowed", spec: manualSpec()},
		{doc: "nil spec", name: "j", spec: nil},
		{doc: "empty container spec", name: "j", spec: &jobsv0.JobSpec{}},
		{doc: "no image", name: "j", spec: &jobsv0.JobSpec{ContainerSpec: []byte(`{}`)}},
		{doc: "unknown container field", name: "j", spec: &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"busybox","Imag":"typo"}`)}},
		{doc: "auto remove", name: "j", spec: &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"busybox","HostConfig":{"AutoRemove":true}}`)}},
		{doc: "restart always", name: "j", spec: &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"busybox","HostConfig":{"RestartPolicy":{"Name":"always"}}}`)}},
		{doc: "restart unless-stopped", name: "j", spec: &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"busybox","HostConfig":{"RestartPolicy":{"Name":"unless-stopped"}}}`)}},
		{doc: "reserved job label", name: "j", spec: &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"busybox"}`), Labels: map[string]string{"com.docker.job.id": "x"}}},
		{doc: "reserved container label", name: "j", spec: &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"busybox","Labels":{"com.docker.job.run-id":"x"}}`)}},
		{doc: "negative timeout", name: "j", spec: &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"busybox"}`), TimeoutSeconds: -1}},
		{doc: "timeout overflowing a duration", name: "j", spec: &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"busybox"}`), TimeoutSeconds: math.MaxInt64/int64(time.Second) + 1}},
		{doc: "infinite stop timeout", name: "j", spec: &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"busybox","StopTimeout":-1}`)}},
		{doc: "oversized container spec", name: "j", spec: &jobsv0.JobSpec{ContainerSpec: append([]byte(`{"Image":"busybox","Env":["`), append(bytes.Repeat([]byte{'x'}, maxContainerSpecSize), []byte(`"]}`)...)...)}},
		{doc: "empty trigger declares no kind", name: "j", spec: &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"busybox"}`), Trigger: &jobsv0.Trigger{}}},
		{doc: "two trigger kinds", name: "j", spec: &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"busybox"}`), Trigger: &jobsv0.Trigger{Manual: true, Schedule: &jobsv0.ScheduleTrigger{Cron: "* * * * *"}}}},
		{doc: "empty cron", name: "j", spec: &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"busybox"}`), Trigger: &jobsv0.Trigger{Schedule: &jobsv0.ScheduleTrigger{Cron: "  "}}}},
		{doc: "unknown timezone", name: "j", spec: &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"busybox"}`), Trigger: &jobsv0.Trigger{Schedule: &jobsv0.ScheduleTrigger{Cron: "* * * * *", Timezone: "Mars/Olympus"}}}},
		{doc: "host-local timezone", name: "j", spec: &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"busybox"}`), Trigger: &jobsv0.Trigger{Schedule: &jobsv0.ScheduleTrigger{Cron: "* * * * *", Timezone: "Local"}}}},
		{doc: "unknown concurrency", name: "j", spec: &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"busybox"}`), Trigger: &jobsv0.Trigger{Schedule: &jobsv0.ScheduleTrigger{Cron: "* * * * *", Concurrency: "replace"}}}},
		{doc: "unknown missed fires", name: "j", spec: &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"busybox"}`), Trigger: &jobsv0.Trigger{Schedule: &jobsv0.ScheduleTrigger{Cron: "* * * * *", MissedFires: "all"}}}},
	} {
		t.Run(tc.doc, func(t *testing.T) {
			_, _, err := m.Create(t.Context(), tc.name, tc.spec)
			assert.Check(t, cerrdefs.IsInvalidArgument(err), "want invalid-argument, got %v", err)
		})
	}
}

func TestManagerRunLifecycle(t *testing.T) {
	m, fake := newTestManager(t)
	job, _, err := m.Create(t.Context(), "backup", manualSpec())
	assert.NilError(t, err)

	run, err := m.Run(t.Context(), "backup", false)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(run.State, jobsv0.RunStateRunning))
	assert.Check(t, is.Equal(run.Iteration, uint64(1)))

	// The run container carries the correlation labels and the derived
	// name; the run-ID suffix keeps names unique across job generations.
	ctr := fake.container(t, run.ContainerID)
	assert.Check(t, strings.HasPrefix(ctr.config.Name, "job-backup-1-"), "container name %q", ctr.config.Name)
	assert.Check(t, is.Equal(ctr.config.Config.Labels[LabelJobID], job.ID))
	assert.Check(t, is.Equal(ctr.config.Config.Labels[LabelRunID], run.ID))
	assert.Check(t, ctr.started)

	// While running, the job reports running and a second run is refused.
	inspected, err := m.Inspect(t.Context(), "backup")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(inspected.State, jobsv0.JobStateRunning))
	_, err = m.Run(t.Context(), "backup", false)
	assert.Check(t, cerrdefs.IsFailedPrecondition(err), "want failed-precondition, got %v", err)

	fake.exit(run.ContainerID, 0, nil)
	done := waitTerminal(t, m, "backup", run.ID)
	assert.Check(t, is.Equal(done.State, jobsv0.RunStateSucceeded))
	assert.Assert(t, done.ExitCode != nil)
	assert.Check(t, is.Equal(done.ExitCode.Value, int64(0)))

	inspected = waitJobSettled(t, m, "backup", func(j *jobsv0.Job) bool { return j.State == jobsv0.JobStateIdle })
	assert.Check(t, is.Equal(inspected.LatestRun.ID, run.ID))

	// The next run continues the iteration sequence.
	second, err := m.Run(t.Context(), "backup", false)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(second.Iteration, uint64(2)))
	fake.exit(second.ContainerID, 3, nil)
	done = waitTerminal(t, m, "backup", second.ID)
	assert.Check(t, is.Equal(done.State, jobsv0.RunStateFailed))
	assert.Check(t, is.Equal(done.ExitCode.Value, int64(3)))
}

func TestManagerRunRecordsCreateFailure(t *testing.T) {
	m, fake := newTestManager(t)
	_, _, err := m.Create(t.Context(), "backup", manualSpec())
	assert.NilError(t, err)
	fake.createErr = errors.New("no such image: busybox")

	// The failure is an outcome, not an error: the run record exists (it is
	// written before the container is created) and carries the failure.
	run, err := m.Run(t.Context(), "backup", false)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(run.State, jobsv0.RunStateFailed))
	assert.Check(t, is.ErrorContains(errors.New(run.Error), "no such image"))
	assert.Check(t, is.Nil(run.ExitCode))
	assert.Check(t, is.Equal(run.ContainerID, ""))

	job, err := m.Inspect(t.Context(), "backup")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(job.State, jobsv0.JobStateIdle))
}

// A caller going away mid-fire must not fail the run: the daemon completes a
// container start regardless, and a failed run would release the job while
// its container keeps running unwatched.
func TestManagerRunOutlivesCancelledCaller(t *testing.T) {
	m, fake := newTestManager(t)
	_, _, err := m.Create(t.Context(), "backup", manualSpec())
	assert.NilError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	run, err := m.Run(ctx, "backup", false)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(run.State, jobsv0.RunStateRunning), "run error: %q", run.Error)

	fake.exit(run.ContainerID, 0, nil)
	done := waitTerminal(t, m, "backup", run.ID)
	assert.Check(t, is.Equal(done.State, jobsv0.RunStateSucceeded))
}

type failRunStartKV struct {
	KV
}

func (k failRunStartKV) Update(ctx context.Context, key string, value []byte) error {
	if strings.HasPrefix(key, runKeyPrefix) {
		var record runRecord
		if err := json.Unmarshal(value, &record); err != nil {
			return err
		}

		if record.Run.State == jobsv0.RunStateRunning {
			return errors.New("injected start-record write failure")
		}
	}

	return k.KV.Update(ctx, key, value)
}

func TestManagerRunStartRecordFailure(t *testing.T) {
	for _, stopFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("stop failure=%t", stopFails), func(t *testing.T) {
			t.Parallel()

			m, fake := newTestManager(t)
			spec := manualSpec()
			spec.TimeoutSeconds = 3600
			job, _, err := m.Create(t.Context(), "backup", spec)
			assert.NilError(t, err)

			m.store.kv = failRunStartKV{KV: m.store.kv}

			if stopFails {
				fake.stopErr = errors.New("injected stop failure")
			}

			run, err := m.Run(t.Context(), job.ID, false)
			assert.NilError(t, err)
			poll.WaitOn(t, func(poll.LogT) poll.Result {
				if slices.Contains(fake.stoppedContainers(), run.ContainerID) {
					return poll.Success()
				}

				return poll.Continue("waiting for container stop")
			})

			if stopFails {
				_, err := m.Run(t.Context(), job.ID, false)
				assert.Check(t, cerrdefs.IsFailedPrecondition(err))
				m.mu.Lock()
				_, armed := m.timers[run.ID]
				m.mu.Unlock()
				assert.Check(t, armed)
				fake.exit(run.ContainerID, 0, nil)
			}

			done := waitTerminal(t, m, job.ID, run.ID)
			assert.Equal(t, done.State, jobsv0.RunStateFailed)
			assert.Check(t, is.Contains(done.Error, "injected start-record write failure"))
			assert.Assert(t, fake.container(t, run.ContainerID).exited != nil)
			waitJobSettled(t, m, job.ID, func(j *jobsv0.Job) bool { return j.State == jobsv0.JobStateIdle })
		})
	}
}

func TestManagerConcurrencyPolicies(t *testing.T) {
	t.Run("forbid drops the fire", func(t *testing.T) {
		m, fake := newTestManager(t)
		job, _, err := m.Create(t.Context(), "nightly", scheduleSpec(""))
		assert.NilError(t, err)
		first, err := m.tryFire(t.Context(), job.ID, &jobsv0.TriggerEvidence{Kind: jobsv0.TriggerKindSchedule})
		assert.NilError(t, err)

		_, err = m.tryFire(t.Context(), job.ID, &jobsv0.TriggerEvidence{Kind: jobsv0.TriggerKindSchedule})
		assert.Check(t, cerrdefs.IsFailedPrecondition(err), "want failed-precondition, got %v", err)
		fake.exit(first.ContainerID, 0, nil)
		waitTerminal(t, m, job.ID, first.ID)
	})

	t.Run("queue defers one fire", func(t *testing.T) {
		m, fake := newTestManager(t)
		job, _, err := m.Create(t.Context(), "nightly", scheduleSpec(jobsv0.ConcurrencyQueue))
		assert.NilError(t, err)
		first, err := m.tryFire(t.Context(), job.ID, &jobsv0.TriggerEvidence{Kind: jobsv0.TriggerKindSchedule})
		assert.NilError(t, err)

		_, err = m.tryFire(t.Context(), job.ID, &jobsv0.TriggerEvidence{Kind: jobsv0.TriggerKindSchedule})
		assert.Check(t, is.ErrorIs(err, errFireQueued))
		_, err = m.tryFire(t.Context(), job.ID, &jobsv0.TriggerEvidence{Kind: jobsv0.TriggerKindSchedule})
		assert.Check(t, is.ErrorIs(err, errFireDropped))

		// Completing the first run fires the queued one automatically. Wait
		// for the second run to be RUNNING before making it exit: its record
		// exists before its container does, so exiting on sight of the
		// record alone would race container creation.
		fake.exit(first.ContainerID, 0, nil)
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		for {
			second, err := m.InspectRun(ctx, job.ID, "latest")
			assert.NilError(t, err)
			if second.Iteration == 2 && second.State == jobsv0.RunStateRunning {
				fake.exit(second.ContainerID, 0, nil)
				waitTerminal(t, m, job.ID, second.ID)
				return
			}
			sub := m.subscribe(job.ID)
			select {
			case <-ctx.Done():
				t.Fatal("queued fire never started")
			case <-sub:
			}
		}
	})
}

func TestManagerCancel(t *testing.T) {
	m, fake := newTestManager(t)
	_, _, err := m.Create(t.Context(), "backup", manualSpec())
	assert.NilError(t, err)

	// Cancelling an idle job is a no-op reporting no run.
	runID, err := m.Cancel(t.Context(), "backup")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(runID, ""))

	run, err := m.Run(t.Context(), "backup", false)
	assert.NilError(t, err)
	runID, err = m.Cancel(t.Context(), "backup")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(runID, run.ID))

	// The stop makes the container exit nonzero, but the cancel override
	// wins over the exit code.
	done := waitTerminal(t, m, "backup", run.ID)
	assert.Check(t, is.Equal(done.State, jobsv0.RunStateCancelled))
	assert.Check(t, is.Contains(fake.stopped, run.ContainerID))
}

func TestManagerTimeout(t *testing.T) {
	m, fake := newTestManager(t)
	spec := manualSpec()
	spec.TimeoutSeconds = 1
	_, _, err := m.Create(t.Context(), "slow", spec)
	assert.NilError(t, err)

	run, err := m.Run(t.Context(), "slow", false)
	assert.NilError(t, err)

	// The container never exits by itself; the deadline stops it and the
	// timed_out override wins over the kill exit code.
	done := waitTerminal(t, m, "slow", run.ID)
	assert.Check(t, is.Equal(done.State, jobsv0.RunStateTimedOut))
	assert.Check(t, is.Contains(fake.stopped, run.ContainerID))
}

func TestManagerAutoRemoval(t *testing.T) {
	m, fake := newTestManager(t)
	spec := manualSpec()
	spec.RemoveOnSuccess = true
	_, _, err := m.Create(t.Context(), "tidy", spec)
	assert.NilError(t, err)

	run, err := m.Run(t.Context(), "tidy", false)
	assert.NilError(t, err)
	fake.exit(run.ContainerID, 0, nil)
	done := waitTerminal(t, m, "tidy", run.ID)
	assert.Check(t, is.Equal(done.State, jobsv0.RunStateSucceeded))
	// The outcome is recorded before the container is removed, so the
	// removal lands an instant after the run turns terminal.
	poll.WaitOn(t, func(poll.LogT) poll.Result {
		gone, err := m.store.Run(run.JobID, run.ID)
		if err != nil {
			return poll.Error(err)
		}

		if gone.ContainerGone {
			return poll.Success()
		}

		return poll.Continue("container removal not recorded yet")
	})
	assert.Check(t, fake.container(t, run.ContainerID).removed)

	// A failed run keeps its container when only RemoveOnSuccess is set.
	waitJobSettled(t, m, "tidy", func(j *jobsv0.Job) bool { return j.State == jobsv0.JobStateIdle })
	second, err := m.Run(t.Context(), "tidy", false)
	assert.NilError(t, err)
	fake.exit(second.ContainerID, 1, nil)
	done = waitTerminal(t, m, "tidy", second.ID)
	assert.Check(t, is.Equal(done.State, jobsv0.RunStateFailed))
	assert.Check(t, !done.ContainerGone)
	assert.Check(t, !fake.container(t, second.ContainerID).removed)
}

func TestManagerCreateAndRunMatrix(t *testing.T) {
	newManagerWithJob := func(t *testing.T) (*Manager, *fakeBackend, *jobsv0.Job) {
		t.Helper()
		m, fake := newTestManager(t)
		job, _, err := m.Create(t.Context(), "existing", manualSpec())
		assert.NilError(t, err)
		return m, fake, job
	}
	finish := func(t *testing.T, m *Manager, fake *fakeBackend, run *jobsv0.Run) {
		t.Helper()
		fake.exit(run.ContainerID, 0, nil)
		waitTerminal(t, m, run.JobID, run.ID)
	}

	t.Run("no name no spec", func(t *testing.T) {
		m, _ := newTestManager(t)
		_, _, _, err := m.CreateAndRun(t.Context(), "", nil)
		assert.Check(t, cerrdefs.IsInvalidArgument(err))
	})
	t.Run("spec only generates a name", func(t *testing.T) {
		m, fake := newTestManager(t)
		job, run, created, err := m.CreateAndRun(t.Context(), "", manualSpec())
		assert.NilError(t, err)
		assert.Check(t, created)
		assert.Check(t, job.Name != "")
		finish(t, m, fake, run)
	})
	t.Run("name only runs the existing job", func(t *testing.T) {
		m, fake, job := newManagerWithJob(t)
		got, run, created, err := m.CreateAndRun(t.Context(), "existing", nil)
		assert.NilError(t, err)
		assert.Check(t, !created)
		assert.Check(t, is.Equal(got.ID, job.ID))
		finish(t, m, fake, run)
	})
	t.Run("name only unknown job", func(t *testing.T) {
		m, _ := newTestManager(t)
		_, _, _, err := m.CreateAndRun(t.Context(), "ghost", nil)
		assert.Check(t, cerrdefs.IsNotFound(err))
	})
	t.Run("name and matching spec run the existing job", func(t *testing.T) {
		m, fake, job := newManagerWithJob(t)
		got, run, created, err := m.CreateAndRun(t.Context(), "existing", manualSpec())
		assert.NilError(t, err)
		assert.Check(t, !created)
		assert.Check(t, is.Equal(got.ID, job.ID))
		finish(t, m, fake, run)
	})
	t.Run("name and different spec conflict", func(t *testing.T) {
		m, _, _ := newManagerWithJob(t)
		_, _, _, err := m.CreateAndRun(t.Context(), "existing", &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"alpine"}`)})
		assert.Check(t, cerrdefs.IsAlreadyExists(err), "want already-exists, got %v", err)
	})
	t.Run("name and spec register and run", func(t *testing.T) {
		m, fake := newTestManager(t)
		job, run, created, err := m.CreateAndRun(t.Context(), "fresh", manualSpec())
		assert.NilError(t, err)
		assert.Check(t, created)
		assert.Check(t, is.Equal(job.Name, "fresh"))
		finish(t, m, fake, run)
	})
	t.Run("schedule spec rejected", func(t *testing.T) {
		m, _ := newTestManager(t)
		_, _, _, err := m.CreateAndRun(t.Context(), "nightly", scheduleSpec(""))
		assert.Check(t, cerrdefs.IsInvalidArgument(err))
	})
	t.Run("name resolving a schedule job rejected", func(t *testing.T) {
		m, _ := newTestManager(t)
		_, _, err := m.Create(t.Context(), "nightly", scheduleSpec(""))
		assert.NilError(t, err)
		_, _, _, err = m.CreateAndRun(t.Context(), "nightly", nil)
		assert.Check(t, cerrdefs.IsInvalidArgument(err))
	})
}

func TestManagerRemoveModes(t *testing.T) {
	setup := func(t *testing.T) (*Manager, *fakeBackend, *jobsv0.Run) {
		t.Helper()
		m, fake := newTestManager(t)
		_, _, err := m.Create(t.Context(), "backup", manualSpec())
		assert.NilError(t, err)
		run, err := m.Run(t.Context(), "backup", false)
		assert.NilError(t, err)
		fake.exit(run.ContainerID, 0, nil)
		return m, fake, waitTerminal(t, m, "backup", run.ID)
	}

	t.Run("default keeps history", func(t *testing.T) {
		m, fake, run := setup(t)
		assert.NilError(t, m.Remove(t.Context(), "backup", ""))
		_, err := m.Inspect(t.Context(), "backup")
		assert.Check(t, cerrdefs.IsNotFound(err))
		kept, err := m.InspectRun(t.Context(), run.JobID, run.ID)
		assert.NilError(t, err)
		assert.Check(t, is.Equal(kept.State, jobsv0.RunStateSucceeded))
		assert.Check(t, !fake.container(t, run.ContainerID).removed)
	})
	t.Run("remove drops history", func(t *testing.T) {
		m, fake, run := setup(t)
		assert.NilError(t, m.Remove(t.Context(), "backup", jobsv0.RunsRemove))
		_, err := m.store.Run(run.JobID, run.ID)
		assert.Check(t, cerrdefs.IsNotFound(err))
		waitContainerRemoved(t, fake, run.ContainerID)
	})
	t.Run("remove drops the in-flight run's container once it exits", func(t *testing.T) {
		m, fake, _ := setup(t)
		inflight, err := m.Run(t.Context(), "backup", false)
		assert.NilError(t, err)

		// The removal stops the container; the fake stop exits it, and the
		// watcher then finds the run record purged.
		assert.NilError(t, m.Remove(t.Context(), "backup", jobsv0.RunsRemove))
		waitContainerRemoved(t, fake, inflight.ContainerID)
	})
	t.Run("remove-finished keeps the run cancelled by the removal", func(t *testing.T) {
		m, fake, terminal := setup(t)
		inflight, err := m.Run(t.Context(), "backup", false)
		assert.NilError(t, err)

		assert.NilError(t, m.Remove(t.Context(), "backup", jobsv0.RunsRemoveFinished))
		// The already-terminal run is dropped; the run cancelled by this
		// removal was still in flight at removal time, so it is kept.
		_, err = m.store.Run(terminal.JobID, terminal.ID)
		assert.Check(t, cerrdefs.IsNotFound(err))
		waitContainerRemoved(t, fake, terminal.ContainerID)

		kept, err := m.store.Run(inflight.JobID, inflight.ID)
		assert.NilError(t, err)
		assert.Check(t, !isTerminalRunState(kept.State) || kept.State == jobsv0.RunStateCancelled)

		// The watcher records the cancellation on the tombstone history. The
		// job record is gone, so the API-level Wait correctly refuses the
		// reference; observe the store directly instead.
		fake.exit(inflight.ContainerID, 137, nil)
		done := waitStoreTerminal(t, m, inflight.JobID, inflight.ID)
		assert.Check(t, is.Equal(done.State, jobsv0.RunStateCancelled))
		assert.Check(t, !fake.container(t, inflight.ContainerID).removed)
	})
	t.Run("invalid mode", func(t *testing.T) {
		m, _, _ := setup(t)
		assert.Check(t, cerrdefs.IsInvalidArgument(m.Remove(t.Context(), "backup", "bogus")))
	})
}

// TestManagerEvictionRemovesContainers checks that history eviction removes
// the evicted runs' containers, so a frequently firing job does not
// accumulate stopped containers.
func TestManagerEvictionRemovesContainers(t *testing.T) {
	m, fake := newTestManager(t)
	spec := manualSpec()
	spec.RunHistoryLimit = 1
	_, _, err := m.Create(t.Context(), "backup", spec)
	assert.NilError(t, err)

	first, err := m.Run(t.Context(), "backup", false)
	assert.NilError(t, err)
	fake.exit(first.ContainerID, 0, nil)
	waitTerminal(t, m, "backup", first.ID)
	waitJobSettled(t, m, "backup", func(j *jobsv0.Job) bool { return j.State == jobsv0.JobStateIdle })

	second, err := m.Run(t.Context(), "backup", false)
	assert.NilError(t, err)
	_, err = m.store.Run(first.JobID, first.ID)
	assert.Check(t, cerrdefs.IsNotFound(err))
	waitContainerRemoved(t, fake, first.ContainerID)
	assert.Check(t, !fake.container(t, second.ContainerID).removed)
}

// waitContainerRemoved polls until the fake backend removed the container;
// dropped runs have their containers removed in the background.
func waitContainerRemoved(t *testing.T, fake *fakeBackend, id string) {
	t.Helper()
	poll.WaitOn(t, func(poll.LogT) poll.Result {
		if fake.container(t, id).removed {
			return poll.Success()
		}

		return poll.Continue("container %s not removed yet", id)
	})
}

func TestManagerPrune(t *testing.T) {
	m, fake := newTestManager(t)
	idle, _, err := m.Create(t.Context(), "idle-manual", manualSpec())
	assert.NilError(t, err)
	labelled := manualSpec()
	labelled.Labels = map[string]string{"com.example.project": "demo"}
	_, _, err = m.Create(t.Context(), "labelled-manual", labelled)
	assert.NilError(t, err)
	_, _, err = m.Create(t.Context(), "nightly", scheduleSpec(""))
	assert.NilError(t, err)
	_, _, err = m.Create(t.Context(), "busy", manualSpec())
	assert.NilError(t, err)
	busyRun, err := m.Run(t.Context(), "busy", false)
	assert.NilError(t, err)

	// Label-filtered prune touches only matching jobs.
	removed, err := m.Prune(t.Context(), []string{"com.example.project=demo"})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(removed), 1))

	// An unfiltered prune removes idle manual jobs, never schedule jobs
	// (armed, not abandoned) nor running ones.
	removed, err = m.Prune(t.Context(), nil)
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(removed, []string{idle.ID}))
	_, err = m.Inspect(t.Context(), "nightly")
	assert.NilError(t, err)
	_, err = m.Inspect(t.Context(), "busy")
	assert.NilError(t, err)

	fake.exit(busyRun.ContainerID, 0, nil)
	waitTerminal(t, m, "busy", busyRun.ID)
}

func TestManagerWait(t *testing.T) {
	t.Run("latest blocks until the first run", func(t *testing.T) {
		m, fake := newTestManager(t)
		_, _, err := m.Create(t.Context(), "backup", manualSpec())
		assert.NilError(t, err)

		type result struct {
			run *jobsv0.Run
			err error
		}
		waited := make(chan result, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
			defer cancel()
			run, err := m.Wait(ctx, "backup", "latest", "")
			waited <- result{run, err}
		}()

		run, err := m.Run(t.Context(), "backup", false)
		assert.NilError(t, err)
		fake.exit(run.ContainerID, 0, nil)
		got := <-waited
		assert.NilError(t, got.err)
		assert.Check(t, is.Equal(got.run.ID, run.ID))
		assert.Check(t, is.Equal(got.run.State, jobsv0.RunStateSucceeded))
	})
	t.Run("running condition satisfied by a terminal jump", func(t *testing.T) {
		m, fake := newTestManager(t)
		_, _, err := m.Create(t.Context(), "backup", manualSpec())
		assert.NilError(t, err)
		fake.createErr = errors.New("boom")
		run, err := m.Run(t.Context(), "backup", false)
		assert.NilError(t, err)

		got, err := m.Wait(t.Context(), "backup", run.ID, jobsv0.WaitConditionRunning)
		assert.NilError(t, err)
		assert.Check(t, is.Equal(got.State, jobsv0.RunStateFailed))
	})
	t.Run("explicit unknown run", func(t *testing.T) {
		m, _ := newTestManager(t)
		_, _, err := m.Create(t.Context(), "backup", manualSpec())
		assert.NilError(t, err)
		_, err = m.Wait(t.Context(), "backup", "ghost", "")
		assert.Check(t, cerrdefs.IsNotFound(err))
	})
	t.Run("invalid condition", func(t *testing.T) {
		m, _ := newTestManager(t)
		_, _, err := m.Create(t.Context(), "backup", manualSpec())
		assert.NilError(t, err)
		_, err = m.Wait(t.Context(), "backup", "latest", "bogus")
		assert.Check(t, cerrdefs.IsInvalidArgument(err))
	})
	t.Run("caller detaches on context cancellation", func(t *testing.T) {
		m, _ := newTestManager(t)
		_, _, err := m.Create(t.Context(), "backup", manualSpec())
		assert.NilError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err = m.Wait(ctx, "backup", "latest", "")
		assert.Check(t, is.ErrorIs(err, context.Canceled))
	})
}

func TestManagerPauseResume(t *testing.T) {
	m, _ := newTestManager(t)
	_, _, err := m.Create(t.Context(), "nightly", scheduleSpec(""))
	assert.NilError(t, err)
	_, _, err = m.Create(t.Context(), "manual", manualSpec())
	assert.NilError(t, err)

	assert.NilError(t, m.Pause(t.Context(), "nightly"))
	assert.NilError(t, m.Pause(t.Context(), "nightly")) // idempotent
	job, err := m.Inspect(t.Context(), "nightly")
	assert.NilError(t, err)
	assert.Check(t, job.Paused)

	assert.NilError(t, m.Resume(t.Context(), "nightly"))
	job, err = m.Inspect(t.Context(), "nightly")
	assert.NilError(t, err)
	assert.Check(t, !job.Paused)

	// A no-op for manual jobs, not an error.
	assert.NilError(t, m.Pause(t.Context(), "manual"))
	job, err = m.Inspect(t.Context(), "manual")
	assert.NilError(t, err)
	assert.Check(t, !job.Paused)
}

func TestManagerPauseDropsScheduledFires(t *testing.T) {
	t.Parallel()

	m, fake := newTestManager(t)
	job, _, err := m.Create(t.Context(), "nightly", scheduleSpec(jobsv0.ConcurrencyQueue))
	assert.NilError(t, err)
	run, err := m.Run(t.Context(), job.ID, false)
	assert.NilError(t, err)

	evidence := &jobsv0.TriggerEvidence{Kind: jobsv0.TriggerKindSchedule}
	_, err = m.tryFire(t.Context(), job.ID, evidence)
	assert.ErrorIs(t, err, errFireQueued)

	assert.NilError(t, m.Pause(t.Context(), job.ID))
	m.mu.Lock()
	_, queued := m.queued[job.ID]
	m.mu.Unlock()
	assert.Check(t, !queued)

	// A fire dispatched before Pause can acquire the job lock afterward.
	_, err = m.tryFire(t.Context(), job.ID, evidence)
	assert.ErrorIs(t, err, errFirePaused)
	fake.exit(run.ContainerID, 0, nil)
	waitTerminal(t, m, job.ID, run.ID)
	waitJobSettled(t, m, job.ID, func(j *jobsv0.Job) bool { return j.State == jobsv0.JobStateIdle })
	_, err = m.tryFire(t.Context(), job.ID, evidence)
	assert.ErrorIs(t, err, errFirePaused)
	assert.Equal(t, fake.createCount(), 1)

	// Only scheduled fires are suppressed; explicit runs remain available.
	run, err = m.Run(t.Context(), job.ID, false)
	assert.NilError(t, err)
	fake.exit(run.ContainerID, 0, nil)
	waitTerminal(t, m, job.ID, run.ID)
}

// failJobUpdateKV fails every job record update.
type failJobUpdateKV struct {
	KV
}

func (k failJobUpdateKV) Update(ctx context.Context, key string, value []byte) error {
	if strings.HasPrefix(key, jobKeyPrefix) {
		return errors.New("injected job-record write failure")
	}

	return k.KV.Update(ctx, key, value)
}

// TestManagerPauseWriteFailureKeepsSchedule checks that a pause whose record
// write fails leaves the schedule armed, matching the unpaused record.
func TestManagerPauseWriteFailureKeepsSchedule(t *testing.T) {
	t.Parallel()

	m, _ := newTestManager(t)
	job, _, err := m.Create(t.Context(), "nightly", scheduleSpec(""))
	assert.NilError(t, err)

	m.store.kv = failJobUpdateKV{KV: m.store.kv}
	assert.Check(t, is.ErrorContains(m.Pause(t.Context(), job.ID), "injected job-record write failure"))

	_, armed := m.sched.nextFor(job.ID)
	assert.Check(t, armed)

	stored, err := m.store.Job(job.ID)
	assert.NilError(t, err)
	assert.Check(t, !stored.Paused)
}

func TestManagerList(t *testing.T) {
	m, fake := newTestManager(t)
	labelled := manualSpec()
	labelled.Labels = map[string]string{"com.example.project": "demo"}
	_, _, err := m.Create(t.Context(), "alpha", labelled)
	assert.NilError(t, err)
	_, _, err = m.Create(t.Context(), "nightly", scheduleSpec(""))
	assert.NilError(t, err)
	assert.NilError(t, m.Pause(t.Context(), "nightly"))
	_, _, err = m.Create(t.Context(), "worker", manualSpec())
	assert.NilError(t, err)
	run, err := m.Run(t.Context(), "worker", false)
	assert.NilError(t, err)
	fake.exit(run.ContainerID, 2, nil)
	waitTerminal(t, m, "worker", run.ID)

	names := func(jobs []*jobsv0.Job) []string {
		out := make([]string, len(jobs))
		for i, job := range jobs {
			out[i] = job.Name
		}
		return out
	}

	all, err := m.List(t.Context(), nil)
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(names(all), []string{"alpha", "nightly", "worker"}))

	byName, err := m.List(t.Context(), &jobsv0.ListRequest{Names: []string{"alpha", "worker"}})
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(names(byName), []string{"alpha", "worker"}))

	byLabel, err := m.List(t.Context(), &jobsv0.ListRequest{Labels: []string{"com.example.project"}})
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(names(byLabel), []string{"alpha"}))

	byKind, err := m.List(t.Context(), &jobsv0.ListRequest{TriggerKinds: []string{jobsv0.TriggerKindSchedule}})
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(names(byKind), []string{"nightly"}))

	paused, err := m.List(t.Context(), &jobsv0.ListRequest{Paused: "true"})
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(names(paused), []string{"nightly"}))

	failed, err := m.List(t.Context(), &jobsv0.ListRequest{LatestRunStates: []string{jobsv0.RunStateFailed}})
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(names(failed), []string{"worker"}))

	_, err = m.List(t.Context(), &jobsv0.ListRequest{Paused: "maybe"})
	assert.Check(t, cerrdefs.IsInvalidArgument(err))
	_, err = m.List(t.Context(), &jobsv0.ListRequest{States: []string{"bogus"}})
	assert.Check(t, cerrdefs.IsInvalidArgument(err))
}

func TestManagerGeneratedNamesRetryOnCollision(t *testing.T) {
	m, fake := newTestManager(t)

	// Exhaust a good part of the namespace cheaply: register a job, then
	// verify a nameless create-and-run picks a fresh name rather than
	// erroring on the first collision. Determinism comes from retrying, not
	// from the generator, so assert on success and name difference only.
	first, run1, _, err := m.CreateAndRun(t.Context(), "", manualSpec())
	assert.NilError(t, err)
	fake.exit(run1.ContainerID, 0, nil)
	waitTerminal(t, m, first.ID, run1.ID)

	second, run2, _, err := m.CreateAndRun(t.Context(), "", manualSpec())
	assert.NilError(t, err)
	assert.Check(t, first.Name != second.Name)
	fake.exit(run2.ContainerID, 0, nil)
	waitTerminal(t, m, second.ID, run2.ID)
}

func TestManagerRescheduleRequiresSchedule(t *testing.T) {
	m, _ := newTestManager(t)
	_, _, err := m.Create(t.Context(), "adhoc", manualSpec())
	assert.NilError(t, err)
	// Rebasing a cadence only means something for a schedule trigger.
	_, err = m.Run(t.Context(), "adhoc", true)
	assert.Check(t, cerrdefs.IsInvalidArgument(err), "want invalid-argument, got %v", err)
}

func TestManagerWatchErrorFailsRun(t *testing.T) {
	m, fake := newTestManager(t)
	_, _, err := m.Create(t.Context(), "backup", manualSpec())
	assert.NilError(t, err)

	fake.waitErr = errors.New("no such container")

	run, err := m.Run(t.Context(), "backup", false)
	assert.NilError(t, err)

	// A failed wait means the container is gone; the watcher records the
	// failure on the run and releases the job.
	done := waitTerminal(t, m, "backup", run.ID)
	assert.Check(t, is.Equal(done.State, jobsv0.RunStateFailed))
	assert.Check(t, is.ErrorContains(errors.New(done.Error), "waiting on run container"))
	waitJobSettled(t, m, "backup", func(j *jobsv0.Job) bool { return j.State == jobsv0.JobStateIdle })
}

func TestManagerInspectRunRefs(t *testing.T) {
	m, fake := newTestManager(t)
	_, _, err := m.Create(t.Context(), "backup", manualSpec())
	assert.NilError(t, err)
	run, err := m.Run(t.Context(), "backup", false)
	assert.NilError(t, err)
	fake.exit(run.ContainerID, 0, nil)
	waitTerminal(t, m, "backup", run.ID)

	for _, tc := range []struct{ doc, ref string }{
		{doc: "empty means latest", ref: ""},
		{doc: "latest keyword", ref: "latest"},
		{doc: "explicit ID", ref: run.ID},
	} {
		t.Run(tc.doc, func(t *testing.T) {
			got, err := m.InspectRun(t.Context(), "backup", tc.ref)
			assert.NilError(t, err)
			assert.Check(t, is.Equal(got.ID, run.ID))
		})
	}
	_, err = m.InspectRun(t.Context(), "backup", "ghost")
	assert.Check(t, cerrdefs.IsNotFound(err))
}

func TestManagerShutdownDetachesFromRunningContainers(t *testing.T) {
	m, _ := newTestManager(t)
	_, _, err := m.Create(t.Context(), "backup", manualSpec())
	assert.NilError(t, err)
	run, err := m.Run(t.Context(), "backup", false)
	assert.NilError(t, err)

	// The container never exits — the live-restore shape, where the daemon
	// shuts down while containers deliberately keep running. Shutdown must
	// detach the watcher and return promptly instead of stalling until the
	// context expires.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	assert.NilError(t, m.Shutdown(ctx))

	// The run record is deliberately left in flight: the crash-leftover
	// shape restart reconciliation resolves on the next startup.
	kept, err := m.store.Run(run.JobID, run.ID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(kept.State, jobsv0.RunStateRunning))
}

// TestManagerRunStartFailure checks that a failed start request, which may
// still have started the container, stops the container before releasing the
// job, and keeps the job occupied while it cannot be stopped.
func TestManagerRunStartFailure(t *testing.T) {
	for _, stopFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("stop failure=%t", stopFails), func(t *testing.T) {
			t.Parallel()

			m, fake := newTestManager(t)
			job, _, err := m.Create(t.Context(), "backup", manualSpec())
			assert.NilError(t, err)

			fake.startErr = errors.New("injected start failure")
			if stopFails {
				fake.stopErr = errors.New("injected stop failure")
			}

			run, err := m.Run(t.Context(), job.ID, false)
			assert.NilError(t, err)
			assert.Check(t, is.Contains(fake.stoppedContainers(), run.ContainerID))

			if stopFails {
				_, err := m.Run(t.Context(), job.ID, false)
				assert.Check(t, cerrdefs.IsFailedPrecondition(err), "want failed-precondition, got %v", err)
				fake.exit(run.ContainerID, 0, nil)
				run = waitTerminal(t, m, job.ID, run.ID)
			}

			assert.Check(t, is.Equal(run.State, jobsv0.RunStateFailed))
			assert.Check(t, is.Contains(run.Error, "injected start failure"))
			waitJobSettled(t, m, job.ID, func(j *jobsv0.Job) bool { return j.State == jobsv0.JobStateIdle })
		})
	}
}

// TestManagerCancelRetriesFailedStop checks that a cancel whose stop failed
// can be retried: the first cancel decides the terminal state, and a later
// cancel stops the container again.
func TestManagerCancelRetriesFailedStop(t *testing.T) {
	m, fake := newTestManager(t)
	job, _, err := m.Create(t.Context(), "backup", manualSpec())
	assert.NilError(t, err)
	run, err := m.Run(t.Context(), job.ID, false)
	assert.NilError(t, err)

	fake.mu.Lock()
	fake.stopErr = errors.New("injected stop failure")
	fake.mu.Unlock()

	_, err = m.Cancel(t.Context(), job.ID)
	assert.NilError(t, err)
	poll.WaitOn(t, func(poll.LogT) poll.Result {
		if len(fake.stoppedContainers()) == 1 {
			return poll.Success()
		}

		return poll.Continue("waiting for the failed stop")
	})

	fake.mu.Lock()
	fake.stopErr = nil
	fake.mu.Unlock()

	runID, err := m.Cancel(t.Context(), job.ID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(runID, run.ID))

	done := waitTerminal(t, m, job.ID, run.ID)
	assert.Check(t, is.Equal(done.State, jobsv0.RunStateCancelled))
	assert.Check(t, is.DeepEqual(fake.stoppedContainers(), []string{run.ContainerID, run.ContainerID}))
}

// failFirstStopBackend fails the first container stop.
type failFirstStopBackend struct {
	*fakeBackend
	failed atomic.Bool
}

func (b *failFirstStopBackend) ContainerStop(ctx context.Context, name string) error {
	if b.failed.CompareAndSwap(false, true) {
		return errors.New("injected stop failure")
	}

	return b.fakeBackend.ContainerStop(ctx, name)
}

// TestManagerTimeoutStopsAfterFailedCancel checks that the deadline still
// stops a container whose cancel failed to stop it, keeping the cancel's
// outcome.
func TestManagerTimeoutStopsAfterFailedCancel(t *testing.T) {
	store, _ := newTestStore(t)
	backend := &failFirstStopBackend{fakeBackend: newFakeBackend()}
	m := NewManager(store, backend, testNameGenerator)

	t.Cleanup(func() { assert.Check(t, m.Shutdown(context.WithoutCancel(t.Context()))) })

	spec := manualSpec()
	spec.TimeoutSeconds = 1
	job, _, err := m.Create(t.Context(), "slow", spec)
	assert.NilError(t, err)
	run, err := m.Run(t.Context(), job.ID, false)
	assert.NilError(t, err)

	_, err = m.Cancel(t.Context(), job.ID)
	assert.NilError(t, err)

	done := waitTerminal(t, m, job.ID, run.ID)
	assert.Check(t, is.Equal(done.State, jobsv0.RunStateCancelled))
	assert.Check(t, is.DeepEqual(backend.stoppedContainers(), []string{run.ContainerID}))
}

// TestManagerReleaseRequiresLatestRun checks that a run overtaken by a newer
// one, as when its completion was retried, does not return the job to idle.
func TestManagerReleaseRequiresLatestRun(t *testing.T) {
	m, fake := newTestManager(t)
	job, _, err := m.Create(t.Context(), "backup", manualSpec())
	assert.NilError(t, err)

	stale := &jobsv0.Run{ID: "stale", JobID: job.ID, State: jobsv0.RunStateFailed, CreatedAtNano: 1}
	assert.NilError(t, m.store.CreateRun(t.Context(), stale))
	run, err := m.Run(t.Context(), job.ID, false)
	assert.NilError(t, err)

	m.locks.Lock(job.ID)
	m.releaseJobLocked(t.Context(), job.ID, stale.ID)
	m.locks.Unlock(job.ID)

	stored, err := m.store.Job(job.ID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(stored.State, jobsv0.JobStateRunning))

	fake.exit(run.ContainerID, 0, nil)
	waitTerminal(t, m, job.ID, run.ID)
}

// TestManagerQueuedFireKeepsNextFireTime checks that a fire deferred by the
// queue policy leaves the job's recorded next-fire time in place until the
// deferred fire runs, so a restart in between catches it up.
func TestManagerQueuedFireKeepsNextFireTime(t *testing.T) {
	m, fake := newTestManager(t)
	job, _, err := m.Create(t.Context(), "nightly", scheduleSpec(jobsv0.ConcurrencyQueue))
	assert.NilError(t, err)
	first, err := m.Run(t.Context(), job.ID, false)
	assert.NilError(t, err)

	fires, _ := m.sched.takeDue(time.Unix(0, job.NextFireAtNano))
	assert.Assert(t, is.Len(fires, 1))
	m.fireDue(t.Context(), fires[0].jobID, fires[0].gen, fires[0].scheduledAt)

	stored, err := m.store.Job(job.ID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(stored.NextFireAtNano, job.NextFireAtNano))

	following, _ := m.sched.nextFor(job.ID)

	fake.exit(first.ContainerID, 0, nil)
	poll.WaitOn(t, func(poll.LogT) poll.Result {
		stored, err := m.store.Job(job.ID)
		if err != nil {
			return poll.Error(err)
		}

		if stored.NextFireAtNano == following.UnixNano() {
			return poll.Success()
		}

		return poll.Continue("next fire time not advanced yet")
	})

	deferred, err := m.store.LatestRun(job.ID)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(deferred.Trigger.ScheduledAtNano, job.NextFireAtNano))
	fake.exit(deferred.ContainerID, 0, nil)
	waitTerminal(t, m, job.ID, deferred.ID)
}
