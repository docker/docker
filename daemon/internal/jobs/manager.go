package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/moby/locker"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/v2/daemon/internal/stringid"
	"github.com/moby/moby/v2/daemon/names"
	"github.com/moby/moby/v2/errdefs"
	jobsv0 "github.com/moby/moby/v2/extpoints/jobs/api/v0"
)

// Backend is the container surface the manager drives runs with.
type Backend interface {
	ContainerCreate(ctx context.Context, req ContainerCreateRequest) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, id string) error
	// ContainerStop stops a container using its configured stop signal and
	// timeout.
	ContainerStop(ctx context.Context, id string) error
	ContainerRm(ctx context.Context, id string) error
	// ContainerWait resolves on the container's final exit, after its
	// restart policy is exhausted. It fails only when the container does not
	// exist; other failures to observe it are retried until ctx is done.
	// The returned channel delivers exactly one status unless ctx is done
	// first; it must never be closed without delivering it.
	// If the container has already exited, the status is ready on return.
	ContainerWait(ctx context.Context, id string) (<-chan StateStatus, error)
}

// ContainerCreateRequest describes a run container to create.
type ContainerCreateRequest struct {
	Name             string
	Config           *container.Config
	HostConfig       *container.HostConfig
	NetworkingConfig *network.NetworkingConfig
}

// StateStatus reports a container's final exit, as delivered by
// [Backend.ContainerWait].
type StateStatus interface {
	ExitCode() int
	Err() error
}

// NameGenerator produces a default job name. Retry counts the prior
// generated names that collided, so implementations can vary their answer.
type NameGenerator func(ctx context.Context, retry int64) (string, error)

// Reserved labels applied to every run container, correlating it with its
// job and run records across the container API.
const (
	// LabelJobID is the ID of the job that owns the container.
	LabelJobID = "com.docker.job.id"
	// LabelRunID is the ID of the run the container backs.
	LabelRunID = "com.docker.job.run-id"
)

// nameGenerationRetries bounds the attempts at generating an unused random
// job name, mirroring the daemon's own retry bound for container names.
const nameGenerationRetries = 6

// Manager owns the job lifecycle: registration with spec-hash idempotency,
// run execution through the single tryFireLocked chokepoint, and the queries
// the extension API exposes. Persistence is delegated to the Store, container
// work to the Backend.
type Manager struct {
	store   *Store
	backend Backend
	// generateName produces default job names for unnamed registrations.
	generateName NameGenerator
	// locks serializes state transitions per job ID: every fire, cancel,
	// removal and run-completion transition takes the job's lock, so two
	// triggers racing on the same job cannot both pass the concurrency
	// policy.
	locks *locker.Locker
	// validateCron validates a cron expression at registration time; a
	// field so tests can substitute it.
	validateCron func(expr string) error

	mu sync.Mutex
	// queued holds at most one deferred fire per job (the queue concurrency
	// policy has depth one); fired when the current run completes.
	queued map[string]*jobsv0.TriggerEvidence
	// overrides forces a run's terminal state (cancelled or timed_out)
	// ahead of its container exit; first writer wins.
	overrides map[string]string
	// timers holds the timeout timer of each in-flight run.
	timers map[string]*time.Timer
	// notify carries one-shot subscriptions per job ID, closed on every
	// job or run transition; Wait re-checks its condition on each signal.
	notify map[string][]chan struct{}
	// background tracks every goroutine the manager spawns (run watchers,
	// queued fires, container stops, the scheduler loop) so Shutdown can
	// drain them. Goroutines spawned by a tracked goroutine register before
	// their parent finishes, so the counter cannot reach zero while work is
	// still being spawned.
	background sync.WaitGroup
	// sched fires schedule jobs; its loop runs between Start and Shutdown,
	// but arming works from construction so jobs registered before Start
	// are picked up when the loop begins.
	sched   *scheduler
	started bool
	stopped bool
	// stop is closed by Shutdown so run watchers detach from containers
	// that are not exiting — under live-restore the daemon shuts down while
	// containers deliberately keep running, and waiting on them would stall
	// (or, with an unbounded shutdown timeout, deadlock) the daemon's exit.
	stop chan struct{}
}

// NewManager builds a manager on top of a loaded store, generating default
// job names with generateName. The constructor does not reconcile runs
// orphaned by an unclean stop — callers run Restore before Start, as the
// extension's activation does.
func NewManager(store *Store, backend Backend, generateName NameGenerator) *Manager {
	m := &Manager{
		store:        store,
		backend:      backend,
		generateName: generateName,
		locks:        locker.New(),
		validateCron: func(expr string) error {
			_, err := parseCron(expr)
			return err
		},
		queued:    make(map[string]*jobsv0.TriggerEvidence),
		overrides: make(map[string]string),
		timers:    make(map[string]*time.Timer),
		notify:    make(map[string][]chan struct{}),
		stop:      make(chan struct{}),
	}
	m.sched = newScheduler(m)
	store.onRunsDropped = m.removeRunContainers

	return m
}

// Start arms every registered schedule job and starts the scheduler loop.
// Occurrences missed while the daemon was down — the persisted next-fire
// time is in the past — follow each job's missed-fires policy: one fires a
// single catch-up run, skip drops them; either way the schedule then re-arms
// from the next occurrence, never replaying the backlog.
// A persisted occurrence that a reschedule moved past the upcoming one is
// kept.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.mu.Unlock()

	now := time.Now()

	for _, job := range m.store.Jobs() {
		if triggerKind(job.Spec.Trigger) != jobsv0.TriggerKindSchedule || job.Paused {
			continue
		}
		trigger := job.Spec.Trigger.Schedule
		schedule, loc, err := parseScheduleTrigger(trigger)
		if err != nil {
			// Specs are validated at registration; only a store record from
			// a buggy or newer daemon gets here. Leave the job disarmed
			// rather than fail the whole startup.
			log.G(ctx).WithError(err).WithFields(log.Fields{"job": job.ID}).Warn("leaving job with unparseable schedule disarmed")
			// The record must not keep advertising an occurrence that will
			// never fire.
			m.persistNextFire(ctx, job.ID)

			continue
		}
		// A job still running has its run re-attached by reconciliation,
		// which precedes Start: its catch-up goes through the concurrency
		// policy like any other fire.
		missed := job.NextFireAtNano
		catchUp := missed > 0 && !time.Unix(0, missed).After(now) &&
			(trigger.MissedFires == "" || trigger.MissedFires == jobsv0.MissedFiresOne) &&
			!m.firedFor(job.ID, missed)

		if next, ok := schedule.next(now, loc); ok {
			// A reschedule persisted the occurrence after the upcoming one;
			// keep it rather than fire the occurrence it skipped. Only that
			// exact occurrence is kept, so a clock stepped back cannot
			// postpone the job any further.
			if job.NextFireAtNano > next.UnixNano() {
				if after, ok := schedule.next(next, loc); ok && after.UnixNano() == job.NextFireAtNano {
					next = after
				}
			}

			m.sched.arm(job.ID, schedule, loc, next)
		}

		if catchUp {
			// The catch-up records the next occurrence once it fired, so a
			// crash before then leaves it to the next startup.
			m.background.Go(func() { m.fireScheduled(ctx, job.ID, time.Unix(0, missed)) })

			continue
		}

		m.persistNextFire(ctx, job.ID)
	}
	m.background.Go(func() { m.sched.run(ctx) })
}

// firedFor reports whether the job's latest run was fired for the scheduled
// occurrence: a fire is recorded before the next occurrence is, so a crash in
// between leaves a missed occurrence that already has its run.
func (m *Manager) firedFor(jobID string, scheduledAtNano int64) bool {
	run, err := m.store.LatestRun(jobID)
	return err == nil && run.Trigger != nil && run.Trigger.ScheduledAtNano == scheduledAtNano
}

// parseScheduleTrigger resolves a validated schedule trigger into its parsed
// expression and timezone (empty means UTC).
func parseScheduleTrigger(trigger *jobsv0.ScheduleTrigger) (*cronSchedule, *time.Location, error) {
	schedule, err := parseCron(trigger.Cron)
	if err != nil {
		return nil, nil, err
	}
	loc := time.UTC
	if trigger.Timezone != "" {
		if loc, err = time.LoadLocation(trigger.Timezone); err != nil {
			return nil, nil, err
		}
	}
	return schedule, loc, nil
}

// Shutdown waits for in-flight run watchers to finish recording their
// outcomes, or for the context to expire. Callers must stop issuing API
// calls before invoking Shutdown: a call racing the drain could register
// new background work while the wait group is being awaited, which
// sync.WaitGroup forbids. When the context expires first, the internal
// waiter goroutine lingers until the background work eventually drains.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	if !m.stopped {
		m.stopped = true
		close(m.stop)
		if m.started {
			close(m.sched.stop)
		}
		// Disarm run timeouts: a deadline landing in the shutdown window
		// would stop a container that live-restore deliberately keeps
		// running.
		for runID, timer := range m.timers {
			timer.Stop()
			delete(m.timers, runID)
		}
	}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		m.background.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Create registers a job without starting a run. It is idempotent on the
// canonical spec hash: re-submitting an identical name+spec returns the
// existing job with created=false, and a name collision with a different
// spec is a conflict carrying both hashes.
func (m *Manager) Create(ctx context.Context, name string, spec *jobsv0.JobSpec) (_ *jobsv0.Job, created bool, _ error) {
	return m.create(ctx, name, spec, nil)
}

// create is Create, calling onCreated with a newly registered job's ID
// before releasing the job's lock.
func (m *Manager) create(ctx context.Context, name string, spec *jobsv0.JobSpec, onCreated func(jobID string)) (_ *jobsv0.Job, created bool, _ error) {
	if err := validateJobName(name); err != nil {
		return nil, false, errdefs.InvalidParameter(err)
	}
	decoded, err := m.validateSpec(spec)
	if err != nil {
		return nil, false, err
	}
	hash, err := specHash(spec, decoded)
	if err != nil {
		return nil, false, err
	}

	// Registering a schedule job arms its trigger; the next occurrence is
	// computed up front so the returned job already advertises it, and an
	// expression with no future occurrence is rejected rather than silently
	// registering a job that would never fire.
	var (
		schedule *cronSchedule
		loc      *time.Location
		next     time.Time
	)
	if spec.Trigger != nil && spec.Trigger.Schedule != nil {
		if schedule, loc, err = parseScheduleTrigger(spec.Trigger.Schedule); err != nil {
			return nil, false, errdefs.InvalidParameter(err) // unreachable after validateSpec, kept for defense
		}
		var ok bool
		if next, ok = schedule.next(time.Now(), loc); !ok {
			return nil, false, errdefs.InvalidParameter(fmt.Errorf("cron expression %q has no future occurrence", spec.Trigger.Schedule.Cron))
		}
	}

	job := &jobsv0.Job{
		ID:            stringid.GenerateRandomID(),
		Name:          name,
		Spec:          spec,
		SpecHash:      hash,
		State:         jobsv0.JobStateIdle,
		CreatedAtNano: time.Now().UnixNano(),
		UpdatedAtNano: time.Now().UnixNano(),
	}
	if schedule != nil {
		job.NextFireAtNano = next.UnixNano()
	}
	// Hold the new job's lock until its trigger is armed: a removal by name
	// landing between the store write and arming would otherwise leave the
	// trigger armed for a deleted job.
	m.locks.Lock(job.ID)
	defer m.locks.Unlock(job.ID)

	if err := m.store.CreateJob(ctx, job); err != nil {
		// A name conflict is idempotent when the stored spec matches;
		// re-read under the store's authority rather than trusting a
		// pre-flight lookup, so concurrent creates converge.
		if cerrdefs.IsConflict(err) {
			existing, lookupErr := m.store.JobByName(name)
			if lookupErr != nil {
				return nil, false, err
			}

			existingHash := currentSpecHash(existing)
			if existingHash == hash {
				return m.composeJob(existing), false, nil
			}

			return nil, false, cerrdefs.ErrAlreadyExists.WithMessage(fmt.Sprintf("job name %s is already in use by a different spec (stored %s, submitted %s)", name, existingHash, hash))
		}
		return nil, false, err
	}
	if schedule != nil {
		m.sched.arm(job.ID, schedule, loc, next)
	}

	if onCreated != nil {
		onCreated(job.ID)
	}

	return m.composeJob(job), true, nil
}

// CreateAndRun atomically resolves or registers a manual job and fires a
// run, implementing the name/spec resolution matrix of the API contract.
func (m *Manager) CreateAndRun(ctx context.Context, name string, spec *jobsv0.JobSpec) (_ *jobsv0.Job, _ *jobsv0.Run, created bool, _ error) {
	if name == "" && spec == nil {
		return nil, nil, false, errdefs.InvalidParameter(errors.New("at least one of name and spec is required"))
	}
	if spec != nil && triggerKind(spec.Trigger) != jobsv0.TriggerKindManual {
		return nil, nil, false, errdefs.InvalidParameter(errors.New("create-and-run serves manual jobs only; register schedule jobs with create"))
	}

	var (
		job    *jobsv0.Job
		run    *jobsv0.Run
		runErr error
	)
	// A newly registered job's first run fires before the job's lock is
	// released, so nothing can run or remove the job in between.
	fire := func(jobID string) { run, runErr = m.fireManualLocked(ctx, jobID) }

	switch {
	case name == "":
		// No name: always a fresh job under a generated name.
		var err error
		if job, err = m.createNamed(ctx, spec, fire); err != nil {
			return nil, nil, false, err
		}
		created = true
	case spec == nil:
		// The request field is a name, strictly: the create branches treat
		// it as one, so resolving IDs here would be an observable asymmetry.
		existing, err := m.store.JobByName(name)
		if err != nil {
			return nil, nil, false, err
		}
		if triggerKind(existing.Spec.Trigger) != jobsv0.TriggerKindManual {
			return nil, nil, false, errdefs.InvalidParameter(fmt.Errorf("job %s is not a manual job", name))
		}
		job = existing
	default:
		var err error
		if job, created, err = m.create(ctx, name, spec, fire); err != nil {
			return nil, nil, false, err
		}
	}

	if run == nil && runErr == nil {
		run, runErr = m.fireManual(ctx, job.ID)
	}

	if runErr != nil {
		return nil, nil, false, runErr
	}

	job, err := m.Inspect(ctx, job.ID)
	if err != nil {
		return nil, nil, false, err
	}
	return job, run, created, nil
}

// createNamed registers spec under a generated name, retrying on collisions
// like the daemon does for container names. A random name landing on an
// existing job with an identical spec also counts as a collision: adopting
// that job would silently hand out foreign history.
func (m *Manager) createNamed(ctx context.Context, spec *jobsv0.JobSpec, onCreated func(jobID string)) (*jobsv0.Job, error) {
	var lastErr error
	for i := range nameGenerationRetries {
		name, err := m.generateName(ctx, int64(i))
		if err != nil {
			return nil, fmt.Errorf("generating a job name: %w", err)
		}

		job, created, err := m.create(ctx, name, spec, onCreated)
		switch {
		case err == nil && created:
			return job, nil
		case err == nil:
			lastErr = fmt.Errorf("generated name %s is already in use", name)
		case cerrdefs.IsAlreadyExists(err) || cerrdefs.IsConflict(err):
			lastErr = err
		default:
			return nil, err
		}
	}
	return nil, fmt.Errorf("generating a unique job name: %w", lastErr)
}

// resolveJob resolves a job reference, trying IDs first and names second,
// like the container API does.
func (m *Manager) resolveJob(ref string) (*jobsv0.Job, error) {
	if ref == "" {
		return nil, errdefs.InvalidParameter(errors.New("job reference must not be empty"))
	}
	job, err := m.store.Job(ref)
	if err == nil {
		return job, nil
	}
	return m.store.JobByName(ref)
}

// composeJob attaches the derived LatestRun to a stored job. A running job
// advertises no next fire. Its record keeps the scheduler's cursor, which
// Start needs to catch up on fires missed while the daemon was down.
func (m *Manager) composeJob(job *jobsv0.Job) *jobsv0.Job {
	if latest, err := m.store.LatestRun(job.ID); err == nil {
		job.LatestRun = latest
	}

	if job.State == jobsv0.JobStateRunning {
		job.NextFireAtNano = 0
	}

	return job
}

// maxContainerSpecSize bounds the raw container definition, the dominant
// term of the job record's size against the storage point's value limit
// (storagekvv0.MaxValueSize, 1 MiB): the spec is base64-encoded inside the
// record's JSON (4/3 growth). Rejecting it here names the offending field;
// the store caps the whole encoded record, covering the remaining
// user-controlled fields.
const maxContainerSpecSize = 512 * 1024

// validateSpec checks everything the contract rejects at registration time
// and returns the decoded container definition.
func (m *Manager) validateSpec(spec *jobsv0.JobSpec) (*container.CreateRequest, error) {
	if spec == nil {
		return nil, errdefs.InvalidParameter(errors.New("a job spec is required"))
	}
	if len(spec.ContainerSpec) > maxContainerSpecSize {
		return nil, errdefs.InvalidParameter(fmt.Errorf("container spec: definition exceeds the maximum size of %d bytes", maxContainerSpecSize))
	}
	if err := validateTrigger(spec.Trigger, m.validateCron); err != nil {
		return nil, errdefs.InvalidParameter(err)
	}
	if spec.TimeoutSeconds < 0 {
		return nil, errdefs.InvalidParameter(errors.New("timeout must not be negative"))
	}

	if spec.TimeoutSeconds > int64(math.MaxInt64/time.Second) {
		return nil, errdefs.InvalidParameter(fmt.Errorf("timeout must not exceed %d seconds", int64(math.MaxInt64/time.Second)))
	}

	for label := range spec.Labels {
		if strings.HasPrefix(label, "com.docker.job.") {
			return nil, errdefs.InvalidParameter(fmt.Errorf("label %s is reserved", label))
		}
	}

	decoded, err := decodeContainerSpec(spec.ContainerSpec, true)
	if err != nil {
		return nil, errdefs.InvalidParameter(fmt.Errorf("container spec: %w", err))
	}
	if decoded.Image == "" {
		return nil, errdefs.InvalidParameter(errors.New("container spec: an image is required"))
	}
	for label := range decoded.Labels {
		if strings.HasPrefix(label, "com.docker.job.") {
			return nil, errdefs.InvalidParameter(fmt.Errorf("container spec: label %s is reserved", label))
		}
	}
	// Cancels, timeouts and reconciliation wait for the container to stop;
	// a stop that never escalates to a kill would hold the job forever.
	if decoded.StopTimeout != nil && *decoded.StopTimeout < 0 {
		return nil, errdefs.InvalidParameter(errors.New("container spec: a negative StopTimeout is not supported for jobs; jobs must be stoppable"))
	}

	if hc := decoded.HostConfig; hc != nil {
		// AutoRemove races the exit-code capture the run record depends on;
		// RemoveOnSuccess/RemoveOnFailure are the job-level equivalents.
		if hc.AutoRemove {
			return nil, errdefs.InvalidParameter(errors.New("container spec: AutoRemove is not supported for jobs; use RemoveOnSuccess or RemoveOnFailure"))
		}
		// A job is expected to terminate; keep-alive restart policies would
		// make the run never reach an outcome. The same goes for on-failure
		// without a retry cap, which Docker treats as retry-forever.
		switch hc.RestartPolicy.Name {
		case "", container.RestartPolicyDisabled:
		case container.RestartPolicyOnFailure:
			if hc.RestartPolicy.MaximumRetryCount <= 0 {
				return nil, errdefs.InvalidParameter(errors.New("container spec: on-failure requires a maximum retry count for jobs; unbounded retries would keep the run from ever completing"))
			}
		default:
			return nil, errdefs.InvalidParameter(fmt.Errorf("container spec: restart policy %s is not supported for jobs; use no or on-failure", hc.RestartPolicy.Name))
		}
	}
	return decoded, nil
}

// validateTrigger enforces the trigger sum-type rules of the API contract.
func validateTrigger(trigger *jobsv0.Trigger, validateCron func(string) error) error {
	if trigger == nil {
		return nil // the manual shorthand
	}
	switch {
	case trigger.Manual && trigger.Schedule != nil:
		return errors.New("trigger declares more than one kind")
	case trigger.Schedule != nil:
		sched := trigger.Schedule
		if err := validateCron(sched.Cron); err != nil {
			return fmt.Errorf("schedule: %w", err)
		}
		if sched.Timezone != "" {
			// Local resolves to the daemon host's timezone, which a spec must
			// not depend on.
			if sched.Timezone == "Local" {
				return errors.New("schedule: timezone Local is not supported; use an IANA timezone name")
			}

			if _, err := time.LoadLocation(sched.Timezone); err != nil {
				return fmt.Errorf("schedule: unknown timezone %s", sched.Timezone)
			}
		}
		switch sched.Concurrency {
		case "", jobsv0.ConcurrencyForbid, jobsv0.ConcurrencyQueue:
		default:
			return fmt.Errorf("schedule: unsupported concurrency policy %s", sched.Concurrency)
		}
		switch sched.MissedFires {
		case "", jobsv0.MissedFiresOne, jobsv0.MissedFiresSkip:
		default:
			return fmt.Errorf("schedule: unsupported missed-fires policy %s", sched.MissedFires)
		}
		return nil
	case trigger.Manual:
		return nil
	default:
		// Deliberate forward-compatibility failure: a trigger kind from a
		// newer contract version reaches an older daemon as an empty
		// message, because protobuf drops unknown fields. Registering it
		// as anything would silently disarm it.
		return errors.New("trigger declares no known kind; the daemon may be too old for this trigger type")
	}
}

// validateJobName applies the container-name character rules to job names.
func validateJobName(name string) error {
	if name == "" {
		return errors.New("a job name is required")
	}
	if !names.RestrictedNamePattern.MatchString(name) {
		return fmt.Errorf("invalid job name %q, only %s are allowed", name, names.RestrictedNameChars)
	}
	return nil
}

// decodeContainerSpec decodes the JSON container definition, in the exact
// format of the container-create API request body.
// Registration passes strict to reject unknown fields, so a typo fails
// instead of silently altering the container.
// Stored specs decode leniently: a field accepted by the daemon that
// registered the job may have been removed from the API types since.
func decodeContainerSpec(raw []byte, strict bool) (*container.CreateRequest, error) {
	if len(raw) == 0 {
		return nil, errors.New("a container definition is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if strict {
		decoder.DisallowUnknownFields()
	}

	req := &container.CreateRequest{}
	if err := decoder.Decode(req); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errors.New("unexpected trailing data after the container definition")
	}
	if req.Config == nil {
		req.Config = &container.Config{}
	}
	return req, nil
}

// canonicalSpec is the hashing form of a job spec: daemon defaults applied,
// container definition re-encoded from its decoded form so JSON formatting
// differences (whitespace, key order — encoding/json sorts map keys) do not
// change a job's identity. Structural presence still matters: an explicit
// empty HostConfig decodes to a non-nil pointer and hashes differently from
// an absent one, a conservative trade-off over normalizing every substruct.
type canonicalSpec struct {
	ContainerSpec   *container.CreateRequest
	TriggerKind     string
	Schedule        *jobsv0.ScheduleTrigger `json:",omitempty"`
	Labels          map[string]string       `json:",omitempty"`
	TimeoutSeconds  int64
	RemoveOnSuccess bool
	RemoveOnFailure bool
	RunHistoryLimit uint32
}

// specHash computes the canonical identity hash of a validated spec.
func specHash(spec *jobsv0.JobSpec, decoded *container.CreateRequest) (string, error) {
	canonical := canonicalSpec{
		ContainerSpec:   decoded,
		TriggerKind:     triggerKind(spec.Trigger),
		Labels:          spec.Labels,
		TimeoutSeconds:  spec.TimeoutSeconds,
		RemoveOnSuccess: spec.RemoveOnSuccess,
		RemoveOnFailure: spec.RemoveOnFailure,
		RunHistoryLimit: spec.RunHistoryLimit,
	}
	if canonical.RunHistoryLimit == 0 {
		canonical.RunHistoryLimit = DefaultRunHistoryLimit
	}
	if spec.Trigger != nil && spec.Trigger.Schedule != nil {
		sched := *spec.Trigger.Schedule
		if sched.Timezone == "" {
			sched.Timezone = "UTC"
		}
		if sched.Concurrency == "" {
			sched.Concurrency = jobsv0.ConcurrencyForbid
		}
		if sched.MissedFires == "" {
			sched.MissedFires = jobsv0.MissedFiresOne
		}
		canonical.Schedule = &sched
	}
	data, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encoding canonical spec: %w", err)
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data)), nil
}

// currentSpecHash computes a stored job's spec hash with this daemon's
// canonicalization.
// The persisted hash comes from the daemon that registered the job, and the
// canonical encoding changes whenever the container API types gain fields.
func currentSpecHash(job *jobsv0.Job) string {
	decoded, err := decodeContainerSpec(job.Spec.ContainerSpec, false)
	if err != nil {
		return job.SpecHash
	}

	hash, err := specHash(job.Spec, decoded)
	if err != nil {
		return job.SpecHash
	}

	return hash
}

// triggerKind names a validated trigger's kind; a nil trigger is the manual
// shorthand.
func triggerKind(trigger *jobsv0.Trigger) string {
	if trigger != nil && trigger.Schedule != nil {
		return jobsv0.TriggerKindSchedule
	}
	return jobsv0.TriggerKindManual
}
