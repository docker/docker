//go:generate go tool mobyextgen

// Package jobsv0 defines the extension point through which the jobs extension
// exposes its client-facing API on the daemon socket.
//
// A Job is a daemon-managed resource holding a container spec plus a trigger
// declaration; each execution is a Run with a stable identity that survives
// container removal. Jobs fire manually or on a cron schedule; the daemon
// evaluates schedule triggers on its own clock, so scheduled work keeps firing
// while no client is connected.
//
// Wire-format conventions, constrained by the mobyextgen generator (no enums,
// oneof, or well-known types):
//   - enumerations are documented strings (see the *State and policy constants);
//   - timestamps are int64 Unix nanoseconds, zero meaning "not set";
//   - the container definition travels as a JSON payload (see JobSpec.ContainerSpec);
//   - trigger kinds are mutually exclusive fields on a single message, and a
//     trigger declaring no known kind is rejected rather than interpreted
//     (see Trigger).
package jobsv0

import (
	"context"

	"github.com/moby/extensions"
)

// Jobs is the jobs service. All methods are unary; blocking semantics
// (Wait) hold the request open until the condition is met.
//
// Errors are returned as gRPC status codes: InvalidArgument for a spec that
// fails validation, NotFound for an unknown job or run, AlreadyExists for a
// Create whose name is taken by a different spec (the message mentions both
// spec hashes for diagnostics only; the code is the contract), and
// FailedPrecondition for an execution refused by the concurrency policy.
type Jobs interface {
	// Create registers a job without ever starting a run. It is idempotent on
	// the spec hash: re-submitting the same name+spec is a no-op (Created is
	// false), a new name registers the job (Created is true), and an existing
	// name with a different spec fails with AlreadyExists. Registering a
	// schedule job arms its trigger.
	Create(ctx context.Context, req *CreateRequest) (*CreateReply, error)

	// Run executes an existing job, creating the next run. It fails with
	// FailedPrecondition if the job is already running and its concurrency
	// policy forbids overlap.
	Run(ctx context.Context, req *RunRequest) (*RunReply, error)

	// CreateAndRun atomically registers a job if needed and starts a run.
	// At least one of Name and Spec must be set; a missing name is generated
	// by the daemon. Only manual jobs are served: a manual-trigger spec (or
	// the nil-Trigger shorthand) is required, and a Name resolving to an
	// existing schedule job is rejected with InvalidArgument. The name/spec
	// resolution matrix mirrors Create:
	// same spec runs the existing job, a different spec for an existing name
	// fails with AlreadyExists.
	CreateAndRun(ctx context.Context, req *CreateAndRunRequest) (*CreateAndRunReply, error)

	// Inspect returns a job with its latest run summary.
	Inspect(ctx context.Context, req *InspectRequest) (*InspectReply, error)

	// List returns jobs matching all the given filters.
	List(ctx context.Context, req *ListRequest) (*ListReply, error)

	// Pause suppresses a job's schedule trigger and discards a fire deferred
	// by the queue concurrency policy. An in-flight run is not affected, and
	// explicit Run calls still work. Idempotent; a no-op for manual jobs.
	Pause(ctx context.Context, req *PauseRequest) error

	// Resume re-arms a paused job's schedule trigger from the next cron tick.
	// Fires missed while paused are not backfilled. Idempotent.
	Resume(ctx context.Context, req *ResumeRequest) error

	// Cancel stops the job's in-flight run. Triggers keep firing; use Pause
	// to suppress them.
	Cancel(ctx context.Context, req *CancelRequest) (*CancelReply, error)

	// Remove deletes a job, cancelling its in-flight run and disarming its
	// trigger. Run history retention is controlled by RunsRemoval; for
	// retention purposes the run cancelled by Remove itself counts as
	// in-flight, so remove-finished keeps its record.
	// The ID of an already removed job whose runs were kept is accepted with
	// RunsRemoval remove, which purges that history.
	Remove(ctx context.Context, req *RemoveRequest) error

	// Prune removes idle manual jobs. Schedule jobs are never pruned: an
	// idle schedule job is armed, not abandoned.
	Prune(ctx context.Context, req *PruneRequest) (*PruneReply, error)

	// ListRuns returns a job's runs, most recent first.
	ListRuns(ctx context.Context, req *ListRunsRequest) (*ListRunsReply, error)

	// InspectRun returns a single run.
	InspectRun(ctx context.Context, req *InspectRunRequest) (*InspectRunReply, error)

	// Wait blocks until the run satisfies the requested condition and
	// returns the run as it is then. A run that jumps past the condition
	// still satisfies it: waiting for running on a run that went straight
	// from pending to a terminal state returns that terminal run. It returns
	// immediately when the condition is already met. Cancelling the request
	// context detaches the caller without affecting the run.
	Wait(ctx context.Context, req *WaitRequest) (*WaitReply, error)
}

// Point is single-cardinality: the jobs API has one authoritative provider,
// and the host rejects a second one at startup.
var Point = extensions.DefineSinglePoint[Jobs]("org.mobyproject.extension.jobs.api.v0")

// Job states. A job is a trigger declaration: it is idle between fires, even
// when it has never run. Whether the last execution worked lives on the run.
const (
	// JobStateIdle means no run is currently executing.
	JobStateIdle = "idle"
	// JobStateRunning means a run is pending or running.
	JobStateRunning = "running"
)

// Run states. Terminal states (succeeded, failed, timed_out, cancelled) are
// sticky: a terminal run never changes again, re-execution creates a new run.
const (
	// RunStatePending covers the window between run-record creation and
	// container start. A run that fails to create its container goes from
	// pending straight to failed, with Error set and no exit code.
	RunStatePending = "pending"
	// RunStateRunning means the container is executing.
	RunStateRunning = "running"
	// RunStateSucceeded means the container exited zero.
	RunStateSucceeded = "succeeded"
	// RunStateFailed means the container exited nonzero after its restart
	// policy was exhausted, or its creation failed.
	RunStateFailed = "failed"
	// RunStateTimedOut means the run exceeded JobSpec.TimeoutSeconds and was
	// stopped by the daemon.
	RunStateTimedOut = "timed_out"
	// RunStateCancelled means the run was stopped by Cancel or by removing
	// its job.
	RunStateCancelled = "cancelled"
)

// Concurrency policies, applied when a trigger fires while a run is already
// in flight. An empty value defaults to forbid.
const (
	// ConcurrencyForbid drops the new fire.
	ConcurrencyForbid = "forbid"
	// ConcurrencyQueue defers a single fire until the current run ends;
	// further fires while one is queued are dropped. Pausing or removing
	// the job discards the deferred fire.
	ConcurrencyQueue = "queue"
)

// Missed-fire policies, applied when the daemon starts up after schedule
// fires were missed. An empty value defaults to one.
const (
	// MissedFiresOne fires a single catch-up run, then re-arms from the next
	// cron tick.
	MissedFiresOne = "one"
	// MissedFiresSkip drops missed fires and re-arms from the next cron tick.
	MissedFiresSkip = "skip"
)

// Trigger kinds, as recorded on run evidence and used in list filters.
const (
	// TriggerKindManual identifies jobs fired only by explicit Run calls.
	TriggerKindManual = "manual"
	// TriggerKindSchedule identifies jobs fired by the cron scheduler.
	TriggerKindSchedule = "schedule"
)

// Run-history removal modes for Remove. An empty value defaults to keep.
const (
	// RunsKeep retains all run records of the removed job, and their
	// containers.
	RunsKeep = "keep"
	// RunsRemove drops all run records, including terminal ones, and
	// removes their containers; the in-flight run's container is removed
	// once it exits.
	RunsRemove = "remove"
	// RunsRemoveFinished drops terminal run records and their containers,
	// and keeps in-flight ones.
	RunsRemoveFinished = "remove-finished"
)

// Wait conditions. An empty value defaults to terminal.
const (
	// WaitConditionTerminal waits until the run reaches a terminal state.
	WaitConditionTerminal = "terminal"
	// WaitConditionRunning waits until the run leaves pending.
	WaitConditionRunning = "running"
)

// JobSpec is the immutable definition of a job. The daemon canonicalizes the
// spec (applying defaults) before hashing it; the resulting hash is the job's
// identity for idempotent re-registration.
type JobSpec struct {
	// ContainerSpec is the JSON-encoded container definition, in the exact
	// format of the container-create API request body (Config, HostConfig,
	// NetworkingConfig). It is decoded and validated by the same path as the
	// container API; unknown fields are rejected. HostConfig.AutoRemove and
	// the always/unless-stopped restart policies are rejected: run outcome
	// capture requires the container to outlive its exit, and a job is
	// expected to terminate.
	ContainerSpec []byte `pb:"1"`
	// Trigger declares what fires the job. A nil Trigger is the manual
	// shorthand; a non-nil Trigger must declare exactly one kind (see
	// Trigger).
	Trigger *Trigger `pb:"2"`
	// Labels are applied to the job itself, not to run containers. Run
	// containers carry the reserved com.docker.job.id and
	// com.docker.job.run-id labels instead.
	Labels map[string]string `pb:"3"`
	// TimeoutSeconds bounds a run's execution; past the deadline the daemon
	// stops the container and the run ends timed_out. Zero means no timeout.
	TimeoutSeconds int64 `pb:"4"`
	// RemoveOnSuccess removes the run container after a successful exit,
	// before the terminal run record is written, so the record reports
	// ContainerGone. Logs are lost with the container; exit code and error
	// are preserved on the run record.
	RemoveOnSuccess bool `pb:"5"`
	// RemoveOnFailure removes the run container after a failed exit. False
	// by default so failed containers are kept for postmortem, until
	// history eviction or Remove drops their run record.
	RemoveOnFailure bool `pb:"6"`
	// RunHistoryLimit caps retained run records, evicting the oldest
	// terminal runs first, along with their containers. The in-flight run
	// is never evicted. Zero means the daemon default of 10000; retaining
	// no history is deliberately not supported.
	RunHistoryLimit uint32 `pb:"7"`
}

// Trigger declares what fires a job. Exactly one field must be set.
//
// A nil Trigger on the JobSpec is the manual shorthand; a non-nil Trigger
// that declares no known kind is rejected with InvalidArgument rather than
// interpreted. This is deliberate: protobuf silently drops fields it does not
// know, so a spec using a trigger kind from a newer contract version must
// fail loudly on an older daemon instead of silently registering as a manual
// job that never fires. Future trigger kinds (events) are added as new
// fields under this rule.
type Trigger struct {
	// Manual declares that the job fires only on explicit Run calls.
	// Mutually exclusive with Schedule.
	Manual bool `pb:"1"`
	// Schedule fires the job on a cron schedule. Mutually exclusive with
	// Manual.
	Schedule *ScheduleTrigger `pb:"2"`
}

// ScheduleTrigger fires a job on the daemon's clock.
type ScheduleTrigger struct {
	// Cron is a strict five-field crontab expression of numbers, lists,
	// ranges, and steps. Names and shortcuts such as @daily are not
	// accepted on the wire; clients expand them.
	Cron string `pb:"1"`
	// Timezone is an IANA timezone name for evaluating the expression.
	// Empty means UTC, never the daemon host's local timezone, so a spec
	// evaluates identically on every host.
	Timezone string `pb:"2"`
	// Concurrency is the policy applied when the schedule fires while a run
	// is in flight. See the Concurrency constants; empty means forbid.
	Concurrency string `pb:"3"`
	// MissedFires is the policy applied to fires missed while the daemon was
	// down. See the MissedFires constants; empty means one.
	MissedFires string `pb:"4"`
}

// Job is a registered job and its current state.
type Job struct {
	// ID is the daemon-generated stable identifier.
	ID string `pb:"1"`
	// Name is unique across all jobs on the daemon.
	Name string `pb:"2"`
	// Spec is the job definition as submitted, before canonicalization.
	Spec *JobSpec `pb:"3"`
	// SpecHash is the canonical hash of the spec after daemon-side
	// canonicalization. Callers can compare hashes to check spec equality
	// before a Create. It is computed when the job is registered; another
	// daemon version may hash the same spec differently, and Create
	// compares specs with the running daemon's canonicalization.
	SpecHash string `pb:"4"`
	// State is the job's execution state. See the JobState constants.
	State string `pb:"5"`
	// Paused reports whether the schedule trigger is suppressed.
	Paused bool `pb:"6"`
	// NextFireAtNano is the next scheduled fire in Unix nanoseconds. Zero
	// when the job is manual, paused, or currently running.
	NextFireAtNano int64 `pb:"7"`
	// CreatedAtNano is the registration time in Unix nanoseconds.
	CreatedAtNano int64 `pb:"8"`
	// UpdatedAtNano is the last state-change time in Unix nanoseconds.
	UpdatedAtNano int64 `pb:"9"`
	// LatestRun is the most recent run, nil if the job never ran.
	LatestRun *Run `pb:"10"`
}

// Run is a single execution attempt of a job.
type Run struct {
	// ID is the daemon-generated stable identifier, valid across container
	// removal and daemon restarts.
	ID string `pb:"1"`
	// JobID identifies the owning job.
	JobID string `pb:"2"`
	// Iteration is the 1-indexed position of this run in the job's history.
	Iteration uint64 `pb:"3"`
	// ContainerID is the container backing this run. Consumers read run logs
	// from the standard container logs API using this ID; the jobs service
	// does not proxy logs.
	ContainerID string `pb:"4"`
	// ContainerGone reports that the container was removed while the run
	// record was kept; its logs are no longer available.
	ContainerGone bool `pb:"5"`
	// State is the run's execution state. See the RunState constants.
	State string `pb:"6"`
	// CreatedAtNano is the run-record creation time in Unix nanoseconds. The
	// record is written before the container is created, so a run exists
	// even when container creation fails.
	CreatedAtNano int64 `pb:"7"`
	// StartedAtNano is the container start time in Unix nanoseconds, zero if
	// the container never started.
	StartedAtNano int64 `pb:"8"`
	// FinishedAtNano is the terminal-transition time in Unix nanoseconds,
	// zero while the run is in flight.
	FinishedAtNano int64 `pb:"9"`
	// ExitCode is the container's exit code, nil while in flight or when the
	// container never ran.
	ExitCode *ExitCode `pb:"10"`
	// Error describes why a run failed outside the container's own exit,
	// such as a container-create failure or a container lost across a daemon
	// restart.
	Error string `pb:"11"`
	// Trigger records what fired this run.
	Trigger *TriggerEvidence `pb:"12"`
}

// ExitCode wraps an exit code so that absence (run never exited) is
// distinguishable from zero (run exited successfully).
type ExitCode struct {
	// Value is the container's exit code.
	Value int64 `pb:"1"`
}

// TriggerEvidence records what fired a run.
type TriggerEvidence struct {
	// Kind is the trigger kind. See the TriggerKind constants.
	Kind string `pb:"1"`
	// ScheduledAtNano is the cron time the fire was due, in Unix
	// nanoseconds. Zero for manual fires.
	ScheduledAtNano int64 `pb:"2"`
	// FiredAtNano is when the daemon actually fired the run, in Unix
	// nanoseconds.
	FiredAtNano int64 `pb:"3"`
}

// CreateRequest registers a job.
type CreateRequest struct {
	// Name is the job name, unique across the daemon. Required.
	Name string `pb:"1"`
	// Spec is the job definition. Required.
	Spec *JobSpec `pb:"2"`
}

// CreateReply reports the registered job.
type CreateReply struct {
	// Job is the registered (or pre-existing identical) job.
	Job *Job `pb:"1"`
	// Created is false when an identical job already existed and the call
	// was a no-op.
	Created bool `pb:"2"`
}

// RunRequest executes an existing job.
type RunRequest struct {
	// JobRef is the job's ID or name.
	JobRef string `pb:"1"`
	// Reschedule makes this fire stand in for the job's next scheduled
	// occurrence, which is skipped; later occurrences keep the cron
	// alignment. Rejected for jobs without a schedule trigger.
	Reschedule bool `pb:"2"`
}

// RunReply reports the created run.
type RunReply struct {
	// Run is the newly created run. ContainerID is set once the container
	// is created.
	Run *Run `pb:"1"`
}

// CreateAndRunRequest atomically registers a job if needed and runs it.
type CreateAndRunRequest struct {
	// Name is the job name. Optional when Spec is set; the daemon then
	// generates one.
	Name string `pb:"1"`
	// Spec is the job definition. Optional when Name refers to an existing
	// job.
	Spec *JobSpec `pb:"2"`
}

// CreateAndRunReply reports the resolved job and its new run.
type CreateAndRunReply struct {
	// Job is the resolved job, carrying the daemon-generated name when the
	// request had none.
	Job *Job `pb:"1"`
	// Run is the newly created run.
	Run *Run `pb:"2"`
	// Created is false when the job already existed.
	Created bool `pb:"3"`
}

// InspectRequest fetches one job.
type InspectRequest struct {
	// JobRef is the job's ID or name.
	JobRef string `pb:"1"`
}

// InspectReply carries the inspected job.
type InspectReply struct {
	// Job is the inspected job.
	Job *Job `pb:"1"`
}

// ListRequest filters jobs. All filters are conjunctive; within one filter,
// values are disjunctive, matching the filter semantics of the container API.
type ListRequest struct {
	// Names filters on exact job names.
	Names []string `pb:"1"`
	// Labels filters on job labels, each entry either "key" or "key=value".
	Labels []string `pb:"2"`
	// States filters on job states. See the JobState constants.
	States []string `pb:"3"`
	// TriggerKinds filters on trigger kinds. See the TriggerKind constants.
	TriggerKinds []string `pb:"4"`
	// Paused filters on the paused flag: "true", "false", or empty for both.
	Paused string `pb:"5"`
	// LatestRunStates filters on the state of each job's latest run. See the
	// RunState constants.
	LatestRunStates []string `pb:"6"`
}

// ListReply carries the matching jobs.
type ListReply struct {
	// Jobs are the matching jobs. LatestRun is not trimmed: each entry
	// carries the same fields as Inspect.
	Jobs []Job `pb:"1"`
}

// PauseRequest suppresses a job's schedule trigger.
type PauseRequest struct {
	// JobRef is the job's ID or name.
	JobRef string `pb:"1"`
}

// ResumeRequest re-arms a paused job's schedule trigger.
type ResumeRequest struct {
	// JobRef is the job's ID or name.
	JobRef string `pb:"1"`
}

// CancelRequest stops a job's in-flight run.
type CancelRequest struct {
	// JobRef is the job's ID or name.
	JobRef string `pb:"1"`
}

// CancelReply reports the cancelled run.
type CancelReply struct {
	// RunID is the ID of the run that was cancelled, empty if no run was in
	// flight.
	RunID string `pb:"1"`
}

// RemoveRequest deletes a job.
type RemoveRequest struct {
	// JobRef is the job's ID or name, or the ID of a removed job whose runs
	// were kept (see Jobs.Remove).
	JobRef string `pb:"1"`
	// RunsRemoval controls run-history retention. See the Runs constants;
	// empty means keep.
	RunsRemoval string `pb:"2"`
}

// PruneRequest removes idle manual jobs.
type PruneRequest struct {
	// Labels restricts pruning to jobs matching all entries, each either
	// "key" or "key=value".
	Labels []string `pb:"1"`
}

// PruneReply reports what was pruned.
type PruneReply struct {
	// RemovedJobIDs are the IDs of the removed jobs.
	RemovedJobIDs []string `pb:"1"`
}

// ListRunsRequest pages through a job's runs, most recent first.
type ListRunsRequest struct {
	// JobRef is the job's ID or name, or the ID of a removed job whose runs
	// were kept. Names resolve only to live jobs.
	JobRef string `pb:"1"`
	// Limit caps the page size. Zero means the daemon default of 20; a
	// negative value is rejected with InvalidArgument.
	Limit int32 `pb:"2"`
	// Before restricts the page to runs older than the given cursor; pass
	// the previous reply's NextCursor to fetch the next page.
	Before string `pb:"3"`
}

// ListRunsReply carries one page of runs.
type ListRunsReply struct {
	// Runs are the page's runs, most recent first.
	Runs []Run `pb:"1"`
	// NextCursor resumes the listing on the next call, empty on the last
	// page.
	NextCursor string `pb:"2"`
	// CursorStale reports that the requested Before cursor was evicted from
	// history; the listing restarted from the most recent run.
	CursorStale bool `pb:"3"`
}

// InspectRunRequest fetches one run.
type InspectRunRequest struct {
	// JobRef is the job's ID or name, or the ID of a removed job whose runs
	// were kept. Names resolve only to live jobs.
	JobRef string `pb:"1"`
	// RunRef is the run's ID, or "latest" for the most recent run.
	RunRef string `pb:"2"`
}

// InspectRunReply carries the inspected run.
type InspectRunReply struct {
	// Run is the inspected run.
	Run *Run `pb:"1"`
}

// WaitRequest blocks until a run reaches a condition.
type WaitRequest struct {
	// JobRef is the job's ID or name.
	JobRef string `pb:"1"`
	// RunRef is the run's ID, or "latest" (also the default when empty).
	// For a schedule job with no runs yet, "latest" resolves to the next
	// run to fire, so the call may block until the next cron tick.
	RunRef string `pb:"2"`
	// Condition is the state to wait for. See the WaitCondition constants;
	// empty means terminal.
	Condition string `pb:"3"`
}

// WaitReply carries the run in its awaited state.
type WaitReply struct {
	// Run is the run that reached the condition.
	Run *Run `pb:"1"`
}
