package jobs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/extensions"
	"github.com/moby/moby/v2/errdefs"
	jobsv0 "github.com/moby/moby/v2/extpoints/jobs/api/v0"
	storagekvv0 "github.com/moby/moby/v2/extpoints/storage/kv/v0"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

// fakeStorage implements the storage point in memory, honoring the provider
// contract the store relies on: Create conflicts on an existing key, Update
// requires one, deletes tolerate absence, and List pages keys in ascending
// byte order. Records are keyed by namespace + NUL + key: the point forbids
// NUL in both, so the composite cannot collide.
type fakeStorage struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newFakeStorage() *fakeStorage {
	return &fakeStorage{data: make(map[string][]byte)}
}

func composeKey(namespace, key string) string {
	return namespace + "\x00" + key
}

func (f *fakeStorage) Get(_ context.Context, req *storagekvv0.RecordRequest) (*storagekvv0.GetResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.data[composeKey(req.Namespace, req.Key)]
	if !ok {
		return nil, errdefs.NotFound(fmt.Errorf("no such record: %s", req.Key))
	}
	return &storagekvv0.GetResponse{Value: slices.Clone(value)}, nil
}

func (f *fakeStorage) Create(_ context.Context, req *storagekvv0.WriteRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := composeKey(req.Namespace, req.Key)
	if _, exists := f.data[key]; exists {
		return errdefs.Conflict(fmt.Errorf("record exists: %s", req.Key))
	}
	f.data[key] = slices.Clone(req.Value)
	return nil
}

func (f *fakeStorage) Update(_ context.Context, req *storagekvv0.WriteRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := composeKey(req.Namespace, req.Key)
	if _, exists := f.data[key]; !exists {
		return errdefs.NotFound(fmt.Errorf("no such record: %s", req.Key))
	}
	f.data[key] = slices.Clone(req.Value)
	return nil
}

func (f *fakeStorage) Delete(_ context.Context, req *storagekvv0.RecordRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data, composeKey(req.Namespace, req.Key))
	return nil
}

func (f *fakeStorage) DeletePrefix(_ context.Context, req *storagekvv0.DeletePrefixRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := composeKey(req.Namespace, req.Prefix)
	for key := range f.data {
		if strings.HasPrefix(key, prefix) {
			delete(f.data, key)
		}
	}
	return nil
}

func (f *fakeStorage) List(_ context.Context, req *storagekvv0.ListRequest) (*storagekvv0.ListResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	limit := int(req.Limit)
	if limit == 0 {
		limit = storagekvv0.DefaultPageSize
	}
	scope := composeKey(req.Namespace, "")
	var keys []string
	for key := range f.data {
		inNamespace, ok := strings.CutPrefix(key, scope)
		if ok && strings.HasPrefix(inNamespace, req.Prefix) && inNamespace > req.Cursor {
			keys = append(keys, inNamespace)
		}
	}
	slices.Sort(keys)
	resp := &storagekvv0.ListResponse{}
	if len(keys) > limit {
		keys = keys[:limit]
		resp.NextCursor = keys[len(keys)-1]
	}
	resp.Keys = keys
	return resp, nil
}

// pointResolver resolves each point to its own single provider; the shared
// static test resolver cannot, as it serves every point the same list.
type pointResolver map[extensions.PointID]any

func (r pointResolver) Provider(point extensions.PointID, _ extensions.ExtensionID) (any, error) {
	if impl, ok := r[point]; ok {
		return impl, nil
	}
	return nil, fmt.Errorf("no provider for point %q", point)
}

func (r pointResolver) Providers(point extensions.PointID) []extensions.ResolvedProvider {
	if impl, ok := r[point]; ok {
		return []extensions.ResolvedProvider{{Impl: impl}}
	}
	return nil
}

// testNamespace is the namespace test stores are bound to.
const testNamespace = "test"

// testKV is the store's KV view, bound to a fresh in-memory provider through
// the real storagekvv0 adapter — the same composition path the extension
// takes — plus direct access to the backing records for the corruption and
// absence checks.
type testKV struct {
	KV
	fake *fakeStorage
}

func newTestKV(t *testing.T) *testKV {
	t.Helper()
	fake := newFakeStorage()
	kv, err := storagekvv0.GetKV(pointResolver{storagekvv0.Point.ID(): storagekvv0.Storage(fake)}, testNamespace)
	assert.NilError(t, err)
	return &testKV{KV: kv, fake: fake}
}

// set plants a raw record, standing in for a record written by another
// daemon version in the corruption tests.
func (k *testKV) set(key string, value []byte) {
	k.fake.mu.Lock()
	defer k.fake.mu.Unlock()
	k.fake.data[composeKey(testNamespace, key)] = value
}

// raw reads a record's stored bytes without the store in between.
func (k *testKV) raw(t *testing.T, key string) []byte {
	t.Helper()
	k.fake.mu.Lock()
	defer k.fake.mu.Unlock()
	value, ok := k.fake.data[composeKey(testNamespace, key)]
	assert.Assert(t, ok, "no record stored at %s", key)
	return value
}

// has reports whether any record's key starts with prefix.
func (k *testKV) has(prefix string) bool {
	k.fake.mu.Lock()
	defer k.fake.mu.Unlock()
	scoped := composeKey(testNamespace, prefix)
	for key := range k.fake.data {
		if strings.HasPrefix(key, scoped) {
			return true
		}
	}
	return false
}

func newTestStore(t *testing.T) (*Store, *testKV) {
	t.Helper()
	kv := newTestKV(t)
	s, err := NewStore(t.Context(), kv)
	assert.NilError(t, err)
	return s, kv
}

func makeJob(id, name string) *jobsv0.Job {
	return &jobsv0.Job{
		ID:   id,
		Name: name,
		Spec: &jobsv0.JobSpec{
			ContainerSpec: []byte(`{"Image":"busybox"}`),
			Trigger:       &jobsv0.Trigger{Schedule: &jobsv0.ScheduleTrigger{Cron: "0 3 * * *"}},
			Labels:        map[string]string{"com.example.k": "v"},
		},
		SpecHash:      "sha256:abc",
		State:         jobsv0.JobStateIdle,
		CreatedAtNano: 100,
		UpdatedAtNano: 100,
	}
}

func makeRun(jobID, runID, state string) *jobsv0.Run {
	return &jobsv0.Run{
		ID:            runID,
		JobID:         jobID,
		State:         state,
		CreatedAtNano: 200,
		Trigger:       &jobsv0.TriggerEvidence{Kind: jobsv0.TriggerKindManual, FiredAtNano: 200},
	}
}

func TestStoreReload(t *testing.T) {
	ctx := t.Context()
	s, kv := newTestStore(t)

	job := makeJob("job1", "backup")
	assert.NilError(t, s.CreateJob(ctx, job))
	run := makeRun("job1", "run1", jobsv0.RunStateSucceeded)
	run.ExitCode = &jobsv0.ExitCode{Value: 0}
	assert.NilError(t, s.CreateRun(ctx, run))
	assert.Check(t, is.Equal(run.Iteration, uint64(1)))

	reloaded, err := NewStore(ctx, kv)
	assert.NilError(t, err)

	gotJob, err := reloaded.Job("job1")
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(gotJob, job))

	gotRun, err := reloaded.Run("job1", "run1")
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(gotRun, run))

	byName, err := reloaded.JobByName("backup")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(byName.ID, "job1"))

	// The iteration counter must survive the reload: the next run continues
	// the sequence instead of reusing iteration numbers.
	next := makeRun("job1", "run2", jobsv0.RunStatePending)
	assert.NilError(t, reloaded.CreateRun(ctx, next))
	assert.Check(t, is.Equal(next.Iteration, uint64(2)))
}

func TestStoreCreateJobConflicts(t *testing.T) {
	ctx := t.Context()
	s, _ := newTestStore(t)
	assert.NilError(t, s.CreateJob(ctx, makeJob("job1", "backup")))

	err := s.CreateJob(ctx, makeJob("job1", "other"))
	assert.Check(t, cerrdefs.IsConflict(err), "duplicate ID: %v", err)

	err = s.CreateJob(ctx, makeJob("job2", "backup"))
	assert.Check(t, cerrdefs.IsConflict(err), "duplicate name: %v", err)
}

func TestStoreCreateJobInvalidID(t *testing.T) {
	ctx := t.Context()
	s, _ := newTestStore(t)
	for _, tc := range []struct {
		doc string
		id  string
	}{
		{doc: "empty", id: ""},
		{doc: "separator", id: "a/b"},
		{doc: "nul", id: "a\x00b"},
		{doc: "too-long", id: strings.Repeat("a", 129)},
	} {
		t.Run(tc.doc, func(t *testing.T) {
			err := s.CreateJob(ctx, makeJob(tc.id, "n-"+tc.doc))
			assert.Check(t, cerrdefs.IsInvalidArgument(err), "id %q: %v", tc.id, err)
		})
	}
}

func TestStoreCreateJobRecordTooLarge(t *testing.T) {
	ctx := t.Context()
	s, _ := newTestStore(t)
	// The container spec is bounded upstream; labels are the way a record
	// blows past the cap with every field individually acceptable.
	job := makeJob("job1", "backup")
	job.Spec.Labels["com.example.big"] = strings.Repeat("x", maxJobRecordSize)
	err := s.CreateJob(ctx, job)
	assert.Check(t, cerrdefs.IsInvalidArgument(err), "oversized record: %v", err)
	_, err = s.Job("job1")
	assert.Check(t, cerrdefs.IsNotFound(err), "nothing may be registered on rejection")
}

func TestStoreUpdateJob(t *testing.T) {
	ctx := t.Context()
	s, _ := newTestStore(t)
	job := makeJob("job1", "backup")
	assert.NilError(t, s.CreateJob(ctx, job))

	job.State = jobsv0.JobStateRunning
	job.Paused = true
	job.NextFireAtNano = 42
	assert.NilError(t, s.UpdateJob(ctx, job))
	got, err := s.Job("job1")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(got.State, jobsv0.JobStateRunning))
	assert.Check(t, got.Paused)
	assert.Check(t, is.Equal(got.NextFireAtNano, int64(42)))

	renamed := *job
	renamed.Name = "other"
	assert.Check(t, cerrdefs.IsInvalidArgument(s.UpdateJob(ctx, &renamed)))

	// A spec-hash change is rejected, and a spec mutated without changing
	// the hash is not persisted: the stored spec wins.
	respecced := *job
	respecced.SpecHash = "sha256:other"
	assert.Check(t, cerrdefs.IsInvalidArgument(s.UpdateJob(ctx, &respecced)))
	tampered, err := s.Job("job1")
	assert.NilError(t, err)
	tampered.Spec.Labels["com.example.k"] = "tampered"
	assert.NilError(t, s.UpdateJob(ctx, tampered))
	got, err = s.Job("job1")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(got.Spec.Labels["com.example.k"], "v"))

	missing := makeJob("nope", "nope")
	assert.Check(t, cerrdefs.IsNotFound(s.UpdateJob(ctx, missing)))
}

func TestStoreDeleteJobWithoutRuns(t *testing.T) {
	ctx := t.Context()
	s, kv := newTestStore(t)
	assert.NilError(t, s.CreateJob(ctx, makeJob("job1", "backup")))

	// With no history to keep, deletion leaves no tombstone and no records
	// behind.
	assert.NilError(t, s.DeleteJob(ctx, "job1"))
	assert.Check(t, !kv.has(jobKey("job1")))
	assert.Check(t, !kv.has(runsPrefix("job1")))
	assert.Check(t, cerrdefs.IsNotFound(s.PurgeJob(ctx, "job1")))

	reloaded, err := NewStore(ctx, kv)
	assert.NilError(t, err)
	_, err = reloaded.Job("job1")
	assert.Check(t, cerrdefs.IsNotFound(err))
}

func TestStoreRunLifecycle(t *testing.T) {
	ctx := t.Context()
	s, _ := newTestStore(t)
	assert.NilError(t, s.CreateJob(ctx, makeJob("job1", "backup")))

	run := makeRun("job1", "run1", jobsv0.RunStatePending)
	assert.NilError(t, s.CreateRun(ctx, run))

	assert.Check(t, cerrdefs.IsConflict(s.CreateRun(ctx, makeRun("job1", "run1", jobsv0.RunStatePending))), "duplicate run ID")
	assert.Check(t, cerrdefs.IsNotFound(s.CreateRun(ctx, makeRun("nope", "run2", jobsv0.RunStatePending))), "unknown job")

	// A non-terminal run can be updated, but its iteration is immutable.
	run.State = jobsv0.RunStateRunning
	run.ContainerID = "ctr1"
	assert.NilError(t, s.UpdateRun(ctx, run))
	tampered := *run
	tampered.Iteration = 99
	assert.Check(t, cerrdefs.IsInvalidArgument(s.UpdateRun(ctx, &tampered)))

	// A non-terminal run cannot be deleted.
	assert.Check(t, cerrdefs.IsConflict(s.DeleteRun(ctx, "job1", "run1")))

	// Once terminal, a run is immutable.
	run.State = jobsv0.RunStateSucceeded
	run.ExitCode = &jobsv0.ExitCode{Value: 0}
	assert.NilError(t, s.UpdateRun(ctx, run))
	run.State = jobsv0.RunStateFailed
	assert.Check(t, cerrdefs.IsConflict(s.UpdateRun(ctx, run)))

	latest, err := s.LatestRun("job1")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(latest.ID, "run1"))
	assert.Check(t, is.Equal(latest.State, jobsv0.RunStateSucceeded))

	assert.NilError(t, s.DeleteRun(ctx, "job1", "run1"))
	_, err = s.Run("job1", "run1")
	assert.Check(t, cerrdefs.IsNotFound(err))
}

func TestStoreRunEviction(t *testing.T) {
	ctx := t.Context()
	s, kv := newTestStore(t)
	job := makeJob("job1", "backup")
	job.Spec.RunHistoryLimit = 3
	assert.NilError(t, s.CreateJob(ctx, job))

	// One active run amid terminal ones: it must survive eviction even when
	// it is the oldest record and the cap is exceeded.
	active := makeRun("job1", "run-active", jobsv0.RunStateRunning)
	assert.NilError(t, s.CreateRun(ctx, active))
	for i := range 5 {
		assert.NilError(t, s.CreateRun(ctx, makeRun("job1", fmt.Sprintf("run%d", i), jobsv0.RunStateSucceeded)))
	}

	page, _, _, err := s.Runs("job1", 10, "")
	assert.NilError(t, err)
	ids := make([]string, len(page))
	for i, r := range page {
		ids[i] = r.ID
	}
	// Cap is 3: the two newest terminal runs plus the protected active run
	// remain; the oldest terminal runs (run0..run2) were evicted first.
	assert.Check(t, is.DeepEqual(ids, []string{"run4", "run3", "run-active"}))

	// Eviction removed the stored records too.
	assert.Check(t, !kv.has(runKey("job1", "run0")))

	// Iteration numbering keeps counting past evicted records.
	next := makeRun("job1", "run-next", jobsv0.RunStatePending)
	assert.NilError(t, s.CreateRun(ctx, next))
	assert.Check(t, is.Equal(next.Iteration, uint64(7)))
}

func TestStoreRunsPagination(t *testing.T) {
	ctx := t.Context()
	s, _ := newTestStore(t)
	assert.NilError(t, s.CreateJob(ctx, makeJob("job1", "backup")))
	for i := range 5 {
		assert.NilError(t, s.CreateRun(ctx, makeRun("job1", fmt.Sprintf("run%d", i), jobsv0.RunStateSucceeded)))
	}

	page, cursor, stale, err := s.Runs("job1", 2, "")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(page), 2))
	assert.Check(t, is.Equal(page[0].ID, "run4"))
	assert.Check(t, is.Equal(cursor, "run3"))
	assert.Check(t, !stale)

	page, cursor, stale, err = s.Runs("job1", 2, cursor)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(page[0].ID, "run2"))
	assert.Check(t, is.Equal(cursor, "run1"))
	assert.Check(t, !stale)

	// Final page: no next cursor.
	page, cursor, _, err = s.Runs("job1", 2, cursor)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(page), 1))
	assert.Check(t, is.Equal(page[0].ID, "run0"))
	assert.Check(t, is.Equal(cursor, ""))

	// A cursor naming an unknown (evicted) run restarts from the newest run
	// and reports staleness instead of failing.
	page, _, stale, err = s.Runs("job1", 2, "evicted")
	assert.NilError(t, err)
	assert.Check(t, stale)
	assert.Check(t, is.Equal(page[0].ID, "run4"))

	page, cursor, stale, err = s.Runs("job1", -1, "")
	assert.Check(t, cerrdefs.IsInvalidArgument(err))
	assert.Check(t, is.Nil(page))
	assert.Check(t, is.Equal(cursor, ""))
	assert.Check(t, !stale)
}

func TestStoreDeleteJobKeepsTombstone(t *testing.T) {
	ctx := t.Context()
	s, kv := newTestStore(t)
	assert.NilError(t, s.CreateJob(ctx, makeJob("job1", "backup")))
	assert.NilError(t, s.CreateRun(ctx, makeRun("job1", "run1", jobsv0.RunStateSucceeded)))

	assert.NilError(t, s.DeleteJob(ctx, "job1"))
	_, err := s.Job("job1")
	assert.Check(t, cerrdefs.IsNotFound(err))

	// History outlives the job record, both in memory and across reload.
	latest, err := s.LatestRun("job1")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(latest.ID, "run1"))
	reloaded, err := NewStore(ctx, kv)
	assert.NilError(t, err)
	_, err = reloaded.LatestRun("job1")
	assert.NilError(t, err)

	// The name is freed for reuse by a new job.
	assert.NilError(t, s.CreateJob(ctx, makeJob("job2", "backup")))

	// PurgeJob erases the tombstone.
	assert.NilError(t, s.PurgeJob(ctx, "job1"))
	_, err = s.LatestRun("job1")
	assert.Check(t, cerrdefs.IsNotFound(err))
	assert.Check(t, !kv.has(jobKey("job1")))
	assert.Check(t, !kv.has(runsPrefix("job1")))
}

// failDeletePrefixKV fails history removal, after the job record removal
// already succeeded.
type failDeletePrefixKV struct {
	KV
}

func (failDeletePrefixKV) DeletePrefix(context.Context, string) error {
	return errors.New("injected delete-prefix failure")
}

func TestStorePurgeJobHistoryFailureLeavesTombstone(t *testing.T) {
	ctx := t.Context()
	s, kv := newTestStore(t)
	assert.NilError(t, s.CreateJob(ctx, makeJob("job1", "backup")))
	assert.NilError(t, s.CreateRun(ctx, makeRun("job1", "run1", jobsv0.RunStateSucceeded)))

	s.kv = failDeletePrefixKV{KV: kv}
	assert.Check(t, is.ErrorContains(s.PurgeJob(ctx, "job1"), "injected delete-prefix failure"))

	// The job record is gone, so the live job must be too; its history
	// stays as a tombstone another purge can remove.
	_, err := s.Job("job1")
	assert.Check(t, cerrdefs.IsNotFound(err))
	_, err = s.JobByName("backup")
	assert.Check(t, cerrdefs.IsNotFound(err))
	_, err = s.LatestRun("job1")
	assert.NilError(t, err)

	s.kv = kv
	assert.NilError(t, s.PurgeJob(ctx, "job1"))
	assert.Check(t, !kv.has(runsPrefix("job1")))
}

func TestStoreEvictionOnTerminalTransition(t *testing.T) {
	ctx := t.Context()
	s, _ := newTestStore(t)
	job := makeJob("job1", "backup")
	job.Spec.RunHistoryLimit = 1
	assert.NilError(t, s.CreateJob(ctx, job))

	// Two active runs hold the history over its cap of one; nothing is
	// evictable until one of them turns terminal.
	first := makeRun("job1", "run1", jobsv0.RunStateRunning)
	assert.NilError(t, s.CreateRun(ctx, first))
	assert.NilError(t, s.CreateRun(ctx, makeRun("job1", "run2", jobsv0.RunStateRunning)))

	first.State = jobsv0.RunStateSucceeded
	assert.NilError(t, s.UpdateRun(ctx, first))

	// The terminal transition triggered eviction without waiting for the
	// next CreateRun.
	_, err := s.Run("job1", "run1")
	assert.Check(t, cerrdefs.IsNotFound(err))
	_, err = s.Run("job1", "run2")
	assert.NilError(t, err)
}

func TestStoreReloadSortsRuns(t *testing.T) {
	ctx := t.Context()
	s, kv := newTestStore(t)
	assert.NilError(t, s.CreateJob(ctx, makeJob("job1", "backup")))
	// Run IDs whose lexical order inverts creation order, so the reload
	// order cannot come for free from the listing's sorted keys.
	for _, id := range []string{"z-first", "m-second", "a-third"} {
		assert.NilError(t, s.CreateRun(ctx, makeRun("job1", id, jobsv0.RunStateSucceeded)))
	}

	reloaded, err := NewStore(ctx, kv)
	assert.NilError(t, err)
	page, _, _, err := reloaded.Runs("job1", 10, "")
	assert.NilError(t, err)
	ids := make([]string, len(page))
	for i, r := range page {
		ids[i] = r.ID
	}
	assert.Check(t, is.DeepEqual(ids, []string{"a-third", "m-second", "z-first"}))
}

func TestStoreQuarantinesCorruptRecords(t *testing.T) {
	ctx := t.Context()
	s, kv := newTestStore(t)
	assert.NilError(t, s.CreateJob(ctx, makeJob("good", "good-job")))
	assert.NilError(t, s.CreateRun(ctx, makeRun("good", "run-ok", jobsv0.RunStateSucceeded)))
	assert.NilError(t, s.CreateJob(ctx, makeJob("corrupt", "corrupt-job")))
	assert.NilError(t, s.CreateJob(ctx, makeJob("future", "future-job")))
	assert.NilError(t, s.CreateJob(ctx, makeJob("mismatch", "mismatch-job")))
	assert.NilError(t, s.CreateJob(ctx, makeJob("nospec", "nospec-job")))
	assert.NilError(t, s.CreateJob(ctx, makeJob("z-dup", "good-job-dup")))

	// Damage one job record, give another a schema version from the future,
	// make one claim an ID other than its key, strip one of its spec, give
	// one a name already taken by an earlier key, and add three bad run
	// records to the healthy job: undecodable, key/ID mismatch, and wrong job.
	kv.set(jobKey("corrupt"), []byte("{not json"))
	kv.set(jobKey("future"), []byte(`{"schemaVersion":99,"job":{"ID":"future"}}`))
	kv.set(jobKey("mismatch"), []byte(`{"schemaVersion":1,"job":{"ID":"other"}}`))
	kv.set(jobKey("nospec"), []byte(`{"schemaVersion":1,"job":{"ID":"nospec","Name":"nospec-job","Spec":null}}`))
	kv.set(jobKey("z-dup"), []byte(`{"schemaVersion":1,"job":{"ID":"z-dup","Name":"good-job"}}`))
	kv.set(runKey("good", "run-bad"), []byte("{not json"))
	kv.set(runKey("good", "run-renamed"), []byte(`{"schemaVersion":1,"run":{"ID":"run-other","JobID":"good"}}`))
	kv.set(runKey("good", "run-stray"), []byte(`{"schemaVersion":1,"run":{"ID":"run-stray","JobID":"another-job"}}`))

	reloaded, err := NewStore(ctx, kv)
	assert.NilError(t, err)

	// The healthy job and its healthy run survive; damaged records are
	// invisible without failing the load.
	_, err = reloaded.Job("good")
	assert.NilError(t, err)
	_, err = reloaded.Run("good", "run-ok")
	assert.NilError(t, err)
	_, err = reloaded.Run("good", "run-bad")
	assert.Check(t, cerrdefs.IsNotFound(err))
	_, err = reloaded.Run("good", "run-other")
	assert.Check(t, cerrdefs.IsNotFound(err))
	_, err = reloaded.Run("good", "run-stray")
	assert.Check(t, cerrdefs.IsNotFound(err))
	_, err = reloaded.Job("corrupt")
	assert.Check(t, cerrdefs.IsNotFound(err))
	_, err = reloaded.Job("future")
	assert.Check(t, cerrdefs.IsNotFound(err))
	_, err = reloaded.Job("mismatch")
	assert.Check(t, cerrdefs.IsNotFound(err))
	_, err = reloaded.Job("nospec")
	assert.Check(t, cerrdefs.IsNotFound(err))
	// The duplicate-name record was quarantined; the first loaded holder of
	// the name (lexically earlier key) kept it.
	_, err = reloaded.Job("z-dup")
	assert.Check(t, cerrdefs.IsNotFound(err))
	byName, err := reloaded.JobByName("good-job")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(byName.ID, "good"))

	// The damaged records were left in place for inspection.
	assert.Check(t, is.Equal(string(kv.raw(t, jobKey("corrupt"))), "{not json"))
}

func TestStoreReturnsCopies(t *testing.T) {
	ctx := t.Context()
	s, _ := newTestStore(t)
	assert.NilError(t, s.CreateJob(ctx, makeJob("job1", "backup")))
	assert.NilError(t, s.CreateRun(ctx, makeRun("job1", "run1", jobsv0.RunStatePending)))

	got, err := s.Job("job1")
	assert.NilError(t, err)
	got.Spec.Labels["com.example.k"] = "tampered"
	got.Spec.Trigger.Schedule.Cron = "tampered"
	got.Spec.ContainerSpec[0] = 'X'

	fresh, err := s.Job("job1")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(fresh.Spec.Labels["com.example.k"], "v"))
	assert.Check(t, is.Equal(fresh.Spec.Trigger.Schedule.Cron, "0 3 * * *"))
	assert.Check(t, is.Equal(string(fresh.Spec.ContainerSpec[0]), "{"))

	run, err := s.Run("job1", "run1")
	assert.NilError(t, err)
	run.Trigger.Kind = "tampered"
	freshRun, err := s.Run("job1", "run1")
	assert.NilError(t, err)
	assert.Check(t, is.Equal(freshRun.Trigger.Kind, jobsv0.TriggerKindManual))
}

func TestStoreConcurrentRunCreation(t *testing.T) {
	ctx := t.Context()
	s, _ := newTestStore(t)
	assert.NilError(t, s.CreateJob(ctx, makeJob("job1", "backup")))

	const workers = 16
	runs := make([]*jobsv0.Run, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() {
			runs[i] = makeRun("job1", fmt.Sprintf("run%d", i), jobsv0.RunStateSucceeded)
			assert.Check(t, s.CreateRun(ctx, runs[i]))
		})
	}
	wg.Wait()

	// Every run got a distinct iteration in 1..workers.
	seen := make(map[uint64]bool)
	for _, run := range runs {
		assert.Check(t, run.Iteration >= 1 && run.Iteration <= workers, "iteration %d out of range", run.Iteration)
		assert.Check(t, !seen[run.Iteration], "iteration %d assigned twice", run.Iteration)
		seen[run.Iteration] = true
	}
}
