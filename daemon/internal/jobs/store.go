// Package jobs implements the daemon-side jobs extension: persistence,
// lifecycle management and trigger evaluation behind the
// org.mobyproject.extension.jobs.api.v0 point.
package jobs

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/errdefs"
	jobsv0 "github.com/moby/moby/v2/extpoints/jobs/api/v0"
	storagekvv0 "github.com/moby/moby/v2/extpoints/storage/kv/v0"
)

// storeSchemaVersion is written into every record so that a future schema
// change can be detected and migrated instead of silently misread. A record
// with a newer version than this is quarantined at load time.
const storeSchemaVersion = 1

// DefaultRunHistoryLimit is the run-record cap applied when the job spec
// leaves RunHistoryLimit at zero. Retaining no history is deliberately not
// supported: the latest run is the job's observable outcome.
const DefaultRunHistoryLimit = 10000

// defaultRunsPageSize is the ListRuns page size when the request leaves
// Limit at zero.
const defaultRunsPageSize = 20

// Record keys in the extension's storage namespace. Jobs and runs live under
// distinct prefixes so each kind loads with one prefix listing, and a job's
// whole run history is one prefix deletion.
const (
	jobKeyPrefix = "job/"
	runKeyPrefix = "run/"
)

func jobKey(jobID string) string { return jobKeyPrefix + jobID }

func runKey(jobID, runID string) string { return runKeyPrefix + jobID + "/" + runID }

func runsPrefix(jobID string) string { return runKeyPrefix + jobID + "/" }

// KV is the slice of the storage point's record access the store uses,
// satisfied by *storagekvv0.KV. It is an interface so tests can persist to
// memory without the provider's host wiring.
type KV interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Create(ctx context.Context, key string, value []byte) error
	Update(ctx context.Context, key string, value []byte) error
	Delete(ctx context.Context, key string) error
	DeletePrefix(ctx context.Context, prefix string) error
	List(ctx context.Context, opts storagekvv0.ListOptions) (*storagekvv0.ListResponse, error)
}

// jobRecord is the stored envelope of a job. NextIteration lives here
// rather than on the wire type so that iteration numbers stay monotonic
// even after old run records were evicted from history.
type jobRecord struct {
	SchemaVersion int        `json:"schemaVersion"`
	NextIteration uint64     `json:"nextIteration"`
	Job           jobsv0.Job `json:"job"`
}

// runRecord is the stored envelope of a run.
type runRecord struct {
	SchemaVersion int        `json:"schemaVersion"`
	Run           jobsv0.Run `json:"run"`
}

// storedJob is the in-memory state of a live job.
type storedJob struct {
	job           jobsv0.Job
	nextIteration uint64
}

// Store persists jobs and runs as records in the extension's storage
// namespace: a job under job/<jobID>, each of its runs under
// run/<jobID>/<runID>. Every write replaces a single record atomically, so a
// crash mid-write can damage at most the record being written and never a
// neighbor. All records are also kept in memory (run volumes are bounded by
// RunHistoryLimit), so reads never touch storage.
//
// A job whose record was deleted but whose run history was kept (the default
// removal mode) leaves a tombstone: its runs remain listable by job ID until
// they are purged.
//
// Job.LatestRun is derived state: the authoritative record lives in the runs
// collection. The store neither persists nor returns it; callers compose it
// from LatestRun when they need it.
type Store struct {
	kv KV

	// onRunsDropped, when set, receives the run records removed from storage
	// by history eviction, DeleteRun, or PurgeJob, so the owner can release
	// what they reference. It is called under the store's lock and must not
	// call back into the store.
	onRunsDropped func(ctx context.Context, jobID string, runs []jobsv0.Run)

	mu   sync.RWMutex
	jobs map[string]*storedJob   // job ID -> live job
	ids  map[string]string       // job name -> job ID, live jobs only
	runs map[string][]jobsv0.Run // job ID -> runs, newest first; includes tombstones
}

// NewStore loads the store from kv. A listing or read error fails the load:
// it is the storage backend failing, and a store built from a partial read
// would silently drop jobs.
//
// Record-level damage is tolerated: a record that cannot be decoded, or
// whose schema version is newer than this daemon understands, is skipped
// with a warning instead of failing the whole store. The damaged record is
// left in place for inspection; the affected job is invisible until
// repaired, other jobs are unaffected.
func NewStore(ctx context.Context, kv KV) (*Store, error) {
	s := &Store{
		kv:   kv,
		jobs: make(map[string]*storedJob),
		ids:  make(map[string]string),
		runs: make(map[string][]jobsv0.Run),
	}
	if err := s.loadJobs(ctx); err != nil {
		return nil, err
	}
	if err := s.loadRuns(ctx); err != nil {
		return nil, err
	}
	for _, runs := range s.runs {
		sortRunsNewestFirst(runs)
	}
	return s, nil
}

// listKeys collects every key under prefix, page by page. Values then load
// with one Get per key: the point offers no bulk read, and the cost is paid
// once, at daemon startup, on a volume bounded by job count and run history
// caps.
func (s *Store) listKeys(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	var cursor string
	for {
		page, err := s.kv.List(ctx, storagekvv0.ListOptions{Prefix: prefix, Limit: storagekvv0.MaxPageSize, Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("listing %s records: %w", prefix, err)
		}
		keys = append(keys, page.Keys...)
		if page.NextCursor == "" {
			return keys, nil
		}
		cursor = page.NextCursor
	}
}

// loadJobs loads every job record into memory, quarantining damaged records.
// Keys list in ascending byte order, so when two records claim the same name
// the lexically earlier job ID keeps it, deterministically.
func (s *Store) loadJobs(ctx context.Context) error {
	keys, err := s.listKeys(ctx, jobKeyPrefix)
	if err != nil {
		return err
	}
	for _, key := range keys {
		id := strings.TrimPrefix(key, jobKeyPrefix)
		logger := log.G(ctx).WithFields(log.Fields{"job": id})
		data, err := s.kv.Get(ctx, key)
		if err != nil {
			// A read error is the storage backend failing, not one damaged
			// record; proceeding would silently drop jobs.
			return fmt.Errorf("reading job record %s: %w", id, err)
		}
		var record jobRecord
		if err := json.Unmarshal(data, &record); err != nil {
			logger.WithError(err).Warn("quarantining undecodable job record")
			continue
		}
		if record.SchemaVersion > storeSchemaVersion {
			logger.WithFields(log.Fields{"schemaVersion": record.SchemaVersion}).Warn("quarantining job record with unsupported schema version")
			continue
		}
		if record.Job.ID != id {
			logger.WithFields(log.Fields{"recordID": record.Job.ID}).Warn("quarantining job record whose ID does not match its key")
			continue
		}
		if prev, taken := s.ids[record.Job.Name]; taken {
			logger.WithFields(log.Fields{"name": record.Job.Name, "conflictingJob": prev}).Warn("quarantining job record with duplicate name")
			continue
		}
		if record.Job.Spec == nil {
			logger.Warn("quarantining job record without a spec")
			continue
		}
		// LatestRun is derived state; whatever a record carries is stale by
		// definition and must not survive the load.
		record.Job.LatestRun = nil
		s.jobs[id] = &storedJob{job: record.Job, nextIteration: record.NextIteration}
		s.ids[record.Job.Name] = id
	}
	return nil
}

// loadRuns loads every run record into memory, quarantining damaged records.
func (s *Store) loadRuns(ctx context.Context) error {
	keys, err := s.listKeys(ctx, runKeyPrefix)
	if err != nil {
		return err
	}
	for _, key := range keys {
		jobID, runID, ok := strings.Cut(strings.TrimPrefix(key, runKeyPrefix), "/")
		if !ok {
			log.G(ctx).WithFields(log.Fields{"key": key}).Warn("skipping run record with a malformed key")
			continue
		}
		logger := log.G(ctx).WithFields(log.Fields{"job": jobID, "run": runID})
		data, err := s.kv.Get(ctx, key)
		if err != nil {
			// A read error is the storage backend failing, not one damaged
			// record; proceeding would silently drop run history.
			return fmt.Errorf("reading run record %s/%s: %w", jobID, runID, err)
		}
		var record runRecord
		if err := json.Unmarshal(data, &record); err != nil {
			logger.WithError(err).Warn("skipping undecodable run record")
			continue
		}
		if record.SchemaVersion > storeSchemaVersion {
			logger.WithFields(log.Fields{"schemaVersion": record.SchemaVersion}).Warn("skipping run record with unsupported schema version")
			continue
		}
		// A record whose identity does not match its key would be removed
		// at the wrong key later (silently, as remove tolerates absence)
		// and resurrect at every load; quarantine it instead.
		if record.Run.JobID != jobID || record.Run.ID != runID {
			logger.WithFields(log.Fields{"recordRun": record.Run.ID, "recordJob": record.Run.JobID}).Warn("skipping run record whose identity does not match its key")
			continue
		}
		s.runs[jobID] = append(s.runs[jobID], record.Run)
	}
	return nil
}

// maxJobRecordSize caps a job record at creation, with headroom under the
// storage point's value limit for the runtime fields that grow by a few
// bytes on later updates (state, timestamps, the iteration counter): a
// record accepted here must never become too big to update. Checked on the
// encoded record, it bounds every user-controlled field — spec, labels,
// name — without enumerating them.
const maxJobRecordSize = storagekvv0.MaxValueSize - 64*1024

// CreateJob registers a new job. The job's ID and name must both be unused.
func (s *Store) CreateJob(ctx context.Context, job *jobsv0.Job) error {
	if err := validateKeyComponent(job.ID); err != nil {
		return errdefs.InvalidParameter(fmt.Errorf("job ID: %w", err))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.jobs[job.ID]; exists {
		return errdefs.Conflict(fmt.Errorf("job ID %s already exists", job.ID))
	}
	if id, taken := s.ids[job.Name]; taken {
		return errdefs.Conflict(fmt.Errorf("job name %s already in use by %s", job.Name, id))
	}
	stored := &storedJob{job: *cloneJob(job), nextIteration: 1}
	stored.job.LatestRun = nil
	data, err := json.Marshal(jobRecord{
		SchemaVersion: storeSchemaVersion,
		NextIteration: stored.nextIteration,
		Job:           stored.job,
	})
	if err != nil {
		return fmt.Errorf("encoding job record: %w", err)
	}
	if len(data) > maxJobRecordSize {
		return errdefs.InvalidParameter(fmt.Errorf("job definition too large: its record is %d bytes encoded, the maximum is %d", len(data), maxJobRecordSize))
	}
	if err := s.kv.Create(ctx, jobKey(job.ID), data); err != nil {
		return fmt.Errorf("writing job record: %w", err)
	}
	s.jobs[job.ID] = stored
	s.ids[job.Name] = job.ID
	return nil
}

// UpdateJob persists new runtime state (State, Paused, NextFireAtNano,
// UpdatedAtNano) for an existing job. The name, spec, and creation time are
// immutable: a name or spec-hash change is rejected, and the stored spec and
// creation time are preserved even when the caller's copy diverges.
func (s *Store) UpdateJob(ctx context.Context, job *jobsv0.Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, exists := s.jobs[job.ID]
	if !exists {
		return errdefs.NotFound(fmt.Errorf("no such job: %s", job.ID))
	}
	if job.Name != stored.job.Name {
		return errdefs.InvalidParameter(errors.New("job names are immutable"))
	}
	if job.SpecHash != stored.job.SpecHash {
		return errdefs.InvalidParameter(errors.New("job specs are immutable"))
	}
	next := &storedJob{job: *cloneJob(job), nextIteration: stored.nextIteration}
	next.job.LatestRun = nil
	next.job.Spec = cloneSpec(stored.job.Spec)
	next.job.CreatedAtNano = stored.job.CreatedAtNano
	if err := s.writeJobRecord(ctx, next, s.kv.Update); err != nil {
		return err
	}
	s.jobs[job.ID] = next
	return nil
}

// DeleteJob removes a job's record while keeping its run history as a
// tombstone. Use PurgeJob to drop the history too.
func (s *Store) DeleteJob(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, exists := s.jobs[id]
	if !exists {
		return errdefs.NotFound(fmt.Errorf("no such job: %s", id))
	}
	if err := s.kv.Delete(ctx, jobKey(id)); err != nil {
		return fmt.Errorf("removing job record: %w", err)
	}
	delete(s.ids, stored.job.Name)
	delete(s.jobs, id)
	return nil
}

// PurgeJob removes every trace of a job: its record and its run history. It
// also erases tombstones left by DeleteJob. The record goes first: a crash
// in between leaves a tombstone, which stays reachable for another purge,
// rather than a live job that silently lost its history. For the same
// reason, a failure to remove the history still removes the live job and
// leaves its history as a tombstone.
func (s *Store) PurgeJob(ctx context.Context, id string) error {
	if err := validateKeyComponent(id); err != nil {
		return errdefs.InvalidParameter(fmt.Errorf("job ID: %w", err))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, live := s.jobs[id]
	_, tombstone := s.runs[id]
	if !live && !tombstone {
		return errdefs.NotFound(fmt.Errorf("no such job: %s", id))
	}
	if err := s.kv.Delete(ctx, jobKey(id)); err != nil {
		return fmt.Errorf("removing job record: %w", err)
	}

	if live {
		delete(s.ids, stored.job.Name)
		delete(s.jobs, id)
	}

	if err := s.kv.DeletePrefix(ctx, runsPrefix(id)); err != nil {
		return fmt.Errorf("removing run records: %w", err)
	}

	s.runsDropped(ctx, id, s.runs[id])
	delete(s.runs, id)
	return nil
}

// Job returns a copy of the job with the given ID.
func (s *Store) Job(id string) (*jobsv0.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stored, exists := s.jobs[id]
	if !exists {
		return nil, errdefs.NotFound(fmt.Errorf("no such job: %s", id))
	}
	return cloneJob(&stored.job), nil
}

// JobByName returns a copy of the job with the given name.
func (s *Store) JobByName(name string) (*jobsv0.Job, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, exists := s.ids[name]
	if !exists {
		return nil, errdefs.NotFound(fmt.Errorf("no such job: %s", name))
	}
	return cloneJob(&s.jobs[id].job), nil
}

// Jobs returns a copy of every live job, in no particular order.
func (s *Store) Jobs() []*jobsv0.Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	jobs := make([]*jobsv0.Job, 0, len(s.jobs))
	for _, stored := range s.jobs {
		jobs = append(jobs, cloneJob(&stored.job))
	}
	return jobs
}

// CreateRun persists a new run for a live job, assigning run.Iteration from
// the job's persisted counter (the passed run is mutated). Creating the run
// evicts the oldest terminal runs beyond the job's RunHistoryLimit; a
// non-terminal run is never evicted, even above the cap.
func (s *Store) CreateRun(ctx context.Context, run *jobsv0.Run) error {
	if err := validateKeyComponent(run.ID); err != nil {
		return errdefs.InvalidParameter(fmt.Errorf("run ID: %w", err))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, exists := s.jobs[run.JobID]
	if !exists {
		return errdefs.NotFound(fmt.Errorf("no such job: %s", run.JobID))
	}
	if slices.ContainsFunc(s.runs[run.JobID], func(r jobsv0.Run) bool { return r.ID == run.ID }) {
		return errdefs.Conflict(fmt.Errorf("run ID %s already exists", run.ID))
	}

	clone := cloneRun(run)
	clone.Iteration = stored.nextIteration
	next := &storedJob{job: stored.job, nextIteration: stored.nextIteration + 1}
	// The counter is persisted before the run record: re-persisting the
	// counter after a crash costs one skipped iteration number, while a run
	// record with a reused iteration would corrupt history ordering.
	if err := s.writeJobRecord(ctx, next, s.kv.Update); err != nil {
		return err
	}
	if err := s.writeRunRecord(ctx, clone, s.kv.Create); err != nil {
		return err
	}
	s.jobs[run.JobID] = next
	s.runs[run.JobID] = append([]jobsv0.Run{*clone}, s.runs[run.JobID]...)
	s.evictLocked(ctx, run.JobID, stored.job.Spec)
	run.Iteration = clone.Iteration
	return nil
}

// UpdateRun persists new state for an in-flight run. Terminal runs are
// immutable, except for [Store.MarkContainerGone]: re-execution creates a
// new run instead. A transition to a terminal state makes the run eligible
// for history eviction, which may collect it immediately when the job is
// over its cap.
func (s *Store) UpdateRun(ctx context.Context, run *jobsv0.Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	runs := s.runs[run.JobID]
	i := slices.IndexFunc(runs, func(r jobsv0.Run) bool { return r.ID == run.ID })
	if i < 0 {
		return errdefs.NotFound(fmt.Errorf("no such run: %s", run.ID))
	}
	if isTerminalRunState(runs[i].State) {
		return errdefs.Conflict(fmt.Errorf("run %s is terminal and immutable", run.ID))
	}
	if run.Iteration != runs[i].Iteration {
		return errdefs.InvalidParameter(errors.New("run iterations are immutable"))
	}
	if err := s.writeRunRecord(ctx, run, s.kv.Update); err != nil {
		return err
	}
	runs[i] = *cloneRun(run)
	if isTerminalRunState(run.State) {
		if stored, live := s.jobs[run.JobID]; live {
			s.evictLocked(ctx, run.JobID, stored.job.Spec)
		}
	}
	return nil
}

// MarkContainerGone records that a terminal run's container was removed.
// It is the one change a terminal run accepts: cleanup metadata, not part
// of the outcome. The outcome is recorded first, so a crash before the
// removal cannot lose it.
func (s *Store) MarkContainerGone(ctx context.Context, jobID, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	runs := s.runs[jobID]

	i := slices.IndexFunc(runs, func(r jobsv0.Run) bool { return r.ID == runID })
	if i < 0 {
		return errdefs.NotFound(fmt.Errorf("no such run: %s", runID))
	}

	if !isTerminalRunState(runs[i].State) {
		return errdefs.Conflict(fmt.Errorf("run %s is not terminal", runID))
	}

	run := cloneRun(&runs[i])

	run.ContainerGone = true
	if err := s.writeRunRecord(ctx, run, s.kv.Update); err != nil {
		return err
	}

	runs[i] = *run

	return nil
}

// DeleteRun removes a single terminal run record.
func (s *Store) DeleteRun(ctx context.Context, jobID, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	runs := s.runs[jobID]
	i := slices.IndexFunc(runs, func(r jobsv0.Run) bool { return r.ID == runID })
	if i < 0 {
		return errdefs.NotFound(fmt.Errorf("no such run: %s", runID))
	}
	if !isTerminalRunState(runs[i].State) {
		return errdefs.Conflict(fmt.Errorf("run %s is not terminal", runID))
	}
	if err := s.removeRunLocked(ctx, jobID, runID); err != nil {
		return err
	}

	s.runsDropped(ctx, jobID, runs[i:i+1])
	s.runs[jobID] = slices.Delete(runs, i, i+1)
	return nil
}

// Run returns a copy of one run.
func (s *Store) Run(jobID, runID string) (*jobsv0.Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	runs := s.runs[jobID]
	i := slices.IndexFunc(runs, func(r jobsv0.Run) bool { return r.ID == runID })
	if i < 0 {
		return nil, errdefs.NotFound(fmt.Errorf("no such run: %s", runID))
	}
	return cloneRun(&runs[i]), nil
}

// LatestRun returns a copy of the job's most recent run. The not-found
// error covers both a job that never ran and a job ID that never existed:
// tombstones make the two indistinguishable at this layer, so callers that
// care resolve the job first.
func (s *Store) LatestRun(jobID string) (*jobsv0.Run, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	runs := s.runs[jobID]
	if len(runs) == 0 {
		return nil, errdefs.NotFound(fmt.Errorf("job %s has no runs", jobID))
	}
	return cloneRun(&runs[0]), nil
}

// Runs returns one page of a job's runs, newest first. A zero limit applies
// the default page size. An empty before starts from the newest run; a
// before naming an evicted run restarts from the newest run and reports the
// cursor as stale instead of failing, so a slow reader survives concurrent
// eviction.
//
// Job existence is not checked: a job ID that never existed yields an empty
// page, indistinguishable from a job with no history. Callers that care
// resolve the job first.
func (s *Store) Runs(jobID string, limit int, before string) (page []*jobsv0.Run, nextCursor string, staleCursor bool, err error) {
	if limit < 0 {
		return nil, "", false, errdefs.InvalidParameter(errors.New("limit must not be negative"))
	}
	if limit == 0 {
		limit = defaultRunsPageSize
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	runs := s.runs[jobID]
	start := 0
	if before != "" {
		if i := slices.IndexFunc(runs, func(r jobsv0.Run) bool { return r.ID == before }); i >= 0 {
			start = i + 1
		} else {
			staleCursor = true
		}
	}
	for i := start; i < len(runs) && len(page) < limit; i++ {
		page = append(page, cloneRun(&runs[i]))
	}
	if last := start + len(page); len(page) > 0 && last < len(runs) {
		nextCursor = page[len(page)-1].ID
	}
	return page, nextCursor, staleCursor, nil
}

// evictLocked drops the oldest terminal runs beyond the job's history limit.
// The caller holds the write lock.
func (s *Store) evictLocked(ctx context.Context, jobID string, spec *jobsv0.JobSpec) {
	limit := DefaultRunHistoryLimit
	if spec != nil && spec.RunHistoryLimit > 0 {
		limit = int(spec.RunHistoryLimit)
	}
	runs := s.runs[jobID]

	var dropped []jobsv0.Run

	for i := len(runs) - 1; i >= 0 && len(runs) > limit; i-- {
		if !isTerminalRunState(runs[i].State) {
			continue
		}
		// Eviction is best-effort: a record that cannot be removed stays
		// both in storage and in history rather than diverging the two.
		if err := s.removeRunLocked(ctx, jobID, runs[i].ID); err != nil {
			continue
		}

		dropped = append(dropped, runs[i])
		runs = slices.Delete(runs, i, i+1)
	}
	s.runs[jobID] = runs
	s.runsDropped(ctx, jobID, dropped)
}

func (s *Store) runsDropped(ctx context.Context, jobID string, runs []jobsv0.Run) {
	if s.onRunsDropped != nil && len(runs) > 0 {
		s.onRunsDropped(ctx, jobID, runs)
	}
}

// writeJobRecord persists a job record with write, the Create or Update
// operation matching the caller's intent.
func (s *Store) writeJobRecord(ctx context.Context, stored *storedJob, write func(context.Context, string, []byte) error) error {
	data, err := json.Marshal(jobRecord{
		SchemaVersion: storeSchemaVersion,
		NextIteration: stored.nextIteration,
		Job:           stored.job,
	})
	if err != nil {
		return fmt.Errorf("encoding job record: %w", err)
	}
	if err := write(ctx, jobKey(stored.job.ID), data); err != nil {
		return fmt.Errorf("writing job record: %w", err)
	}
	return nil
}

// writeRunRecord persists a run record with write, the Create or Update
// operation matching the caller's intent.
func (s *Store) writeRunRecord(ctx context.Context, run *jobsv0.Run, write func(context.Context, string, []byte) error) error {
	data, err := json.Marshal(runRecord{SchemaVersion: storeSchemaVersion, Run: *run})
	if err != nil {
		return fmt.Errorf("encoding run record: %w", err)
	}
	if err := write(ctx, runKey(run.JobID, run.ID), data); err != nil {
		return fmt.Errorf("writing run record: %w", err)
	}
	return nil
}

func (s *Store) removeRunLocked(ctx context.Context, jobID, runID string) error {
	// Deleting an absent record succeeds, per the point's contract.
	if err := s.kv.Delete(ctx, runKey(jobID, runID)); err != nil {
		return fmt.Errorf("removing run record: %w", err)
	}
	return nil
}

// sortRunsNewestFirst orders runs by descending iteration.
func sortRunsNewestFirst(runs []jobsv0.Run) {
	slices.SortFunc(runs, func(a, b jobsv0.Run) int {
		return cmp.Compare(b.Iteration, a.Iteration)
	})
}

// isTerminalRunState reports whether a run in this state is finished for
// good. Terminal runs are immutable and eligible for history eviction.
func isTerminalRunState(state string) bool {
	switch state {
	case jobsv0.RunStateSucceeded, jobsv0.RunStateFailed, jobsv0.RunStateTimedOut, jobsv0.RunStateCancelled:
		return true
	}
	return false
}

// validateKeyComponent guards IDs that become components of record keys. IDs
// are daemon-generated hex, so anything else indicates a bug rather than bad
// user input. A separator would corrupt the key structure; NUL and the
// length cap are the storage point's key constraints an ID could otherwise
// break (hex IDs cannot produce the invalid UTF-8 it also rejects).
func validateKeyComponent(id string) error {
	switch {
	case id == "":
		return errors.New("must not be empty")
	case len(id) > 128:
		return errors.New("must not exceed 128 characters")
	}
	for _, r := range id {
		if r == '/' || r == 0 {
			return fmt.Errorf("must not contain %q", r)
		}
	}
	return nil
}

// cloneJob deep-copies a job so store state never aliases caller memory.
func cloneJob(job *jobsv0.Job) *jobsv0.Job {
	c := *job
	c.Spec = cloneSpec(job.Spec)
	if job.LatestRun != nil {
		c.LatestRun = cloneRun(job.LatestRun)
	}
	return &c
}

// cloneSpec deep-copies a job spec.
func cloneSpec(spec *jobsv0.JobSpec) *jobsv0.JobSpec {
	if spec == nil {
		return nil
	}
	c := *spec
	c.ContainerSpec = slices.Clone(spec.ContainerSpec)
	c.Labels = maps.Clone(spec.Labels)
	if spec.Trigger != nil {
		trigger := *spec.Trigger
		if spec.Trigger.Schedule != nil {
			schedule := *spec.Trigger.Schedule
			trigger.Schedule = &schedule
		}
		c.Trigger = &trigger
	}
	return &c
}

// cloneRun deep-copies a run so store state never aliases caller memory.
func cloneRun(run *jobsv0.Run) *jobsv0.Run {
	c := *run
	if run.ExitCode != nil {
		code := *run.ExitCode
		c.ExitCode = &code
	}
	if run.Trigger != nil {
		evidence := *run.Trigger
		c.Trigger = &evidence
	}
	return &c
}
