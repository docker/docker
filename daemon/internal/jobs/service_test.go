package jobs

import (
	"context"
	"errors"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/moby/extensions"
	containernamegeneratorv0 "github.com/moby/moby/v2/extpoints/containernamegenerator/v0"
	containerstatev0 "github.com/moby/moby/v2/extpoints/containerstate/v0"
	daemonapiv0 "github.com/moby/moby/v2/extpoints/daemonapi/v0"
	jobsv0 "github.com/moby/moby/v2/extpoints/jobs/api/v0"
	jobspb "github.com/moby/moby/v2/extpoints/jobs/api/v0/protogen"
	storagekvv0 "github.com/moby/moby/v2/extpoints/storage/kv/v0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
)

// newTestService serves the jobs extension the way the daemon publishes its
// offered point and returns a generated client dialing it, so the tests cover
// the exact path an API consumer takes.
func newTestService(t *testing.T) (jobspb.JobsClient, *Extension, *testKV, *fakeBackend) {
	t.Helper()
	ext := NewExtension()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		assert.Check(t, ext.shutdown(ctx))
	})

	return serveJobsClient(t, ext), ext, newTestKV(t), newFakeBackend()
}

// serveJobsClient serves ext's Jobs provider through the generated
// ServerPoint adapter, as the host publishes an offered point, over a
// loopback listener and returns a generated client dialing it. TCP rather
// than a unix socket so the tests run on Windows too.
func serveJobsClient(t *testing.T, ext *Extension) jobspb.JobsClient {
	t.Helper()
	providers := ext.Declaration().Providers
	i := slices.IndexFunc(providers, func(p extensions.Provider) bool { return p.Point == jobsv0.Point.ID() })
	assert.Assert(t, i >= 0, "the extension provides no Jobs point")

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	assert.NilError(t, err)
	gs := grpc.NewServer()
	jobspb.ServerPoint.Register(gs, providers[i].Impl)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	assert.NilError(t, err)
	t.Cleanup(func() { conn.Close() })
	return jobspb.NewJobsClient(conn)
}

// activateExtension activates ext on backend the way init's background
// goroutine does once the daemon API is ready.
func activateExtension(ctx context.Context, ext *Extension, kv KV, backend Backend) error {
	manager, err := ext.buildManager(ctx, kv, backend, testNameGenerator)
	if err != nil {
		return err
	}
	return ext.activate(ctx, manager)
}

func TestServiceUnavailableBeforeActivation(t *testing.T) {
	// No fixture: an extension that was never activated needs no cleanup.
	ext := NewExtension()
	client := serveJobsClient(t, ext)
	_, err := client.List(t.Context(), &jobspb.ListRequest{})
	assert.Check(t, is.Equal(status.Code(err), codes.Unavailable))
}

func TestServiceJourney(t *testing.T) {
	client, ext, kv, fake := newTestService(t)
	assert.NilError(t, activateExtension(t.Context(), ext, kv, fake))

	// Register a manual job; re-applying is a no-op, a different spec under
	// the same name is a conflict surfaced as AlreadyExists.
	spec := &jobspb.JobSpec{ContainerSpec: []byte(`{"Image":"busybox"}`)}
	created, err := client.Create(t.Context(), &jobspb.CreateRequest{Name: "backup", Spec: spec})
	assert.NilError(t, err)
	assert.Check(t, created.GetCreated())
	again, err := client.Create(t.Context(), &jobspb.CreateRequest{Name: "backup", Spec: spec})
	assert.NilError(t, err)
	assert.Check(t, !again.GetCreated())
	_, err = client.Create(t.Context(), &jobspb.CreateRequest{Name: "backup", Spec: &jobspb.JobSpec{ContainerSpec: []byte(`{"Image":"alpine"}`)}})
	assert.Check(t, is.Equal(status.Code(err), codes.AlreadyExists))

	// Validation failures surface as InvalidArgument, unknown jobs as
	// NotFound.
	_, err = client.Create(t.Context(), &jobspb.CreateRequest{Name: "bad", Spec: &jobspb.JobSpec{ContainerSpec: []byte(`{}`)}})
	assert.Check(t, is.Equal(status.Code(err), codes.InvalidArgument))
	_, err = client.Inspect(t.Context(), &jobspb.InspectRequest{JobRef: "ghost"})
	assert.Check(t, is.Equal(status.Code(err), codes.NotFound))

	// Execute; a second run while the first is in flight is refused with
	// FailedPrecondition.
	runReply, err := client.Run(t.Context(), &jobspb.RunRequest{JobRef: "backup"})
	assert.NilError(t, err)
	run := runReply.GetRun()
	assert.Check(t, is.Equal(run.GetState(), "running"))
	_, err = client.Run(t.Context(), &jobspb.RunRequest{JobRef: "backup"})
	assert.Check(t, is.Equal(status.Code(err), codes.FailedPrecondition))

	// Complete the run and observe it through Wait.
	fake.exit(run.GetContainerId(), 0, nil)
	waited, err := client.Wait(t.Context(), &jobspb.WaitRequest{JobRef: "backup", RunRef: run.GetId()})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(waited.GetRun().GetState(), "succeeded"))
	assert.Assert(t, waited.GetRun().GetExitCode() != nil)
	assert.Check(t, is.Equal(waited.GetRun().GetExitCode().GetValue(), int64(0)))

	// List, run history, and removal round out the journey.
	list, err := client.List(t.Context(), &jobspb.ListRequest{})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(list.GetJobs()), 1))
	runs, err := client.ListRuns(t.Context(), &jobspb.ListRunsRequest{JobRef: "backup"})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(len(runs.GetRuns()), 1))
	_, err = client.Remove(t.Context(), &jobspb.RemoveRequest{JobRef: "backup"})
	assert.NilError(t, err)
	_, err = client.Inspect(t.Context(), &jobspb.InspectRequest{JobRef: "backup"})
	assert.Check(t, is.Equal(status.Code(err), codes.NotFound))
	_, err = client.ListRuns(t.Context(), &jobspb.ListRunsRequest{JobRef: "backup"})
	assert.Equal(t, status.Code(err), codes.NotFound)

	// Reusing a name must not hide or adopt the previous job's history.
	replacement, err := client.Create(t.Context(), &jobspb.CreateRequest{Name: "backup", Spec: spec})
	assert.NilError(t, err)
	assert.Check(t, replacement.GetJob().GetId() != created.GetJob().GetId())
	runs, err = client.ListRuns(t.Context(), &jobspb.ListRunsRequest{JobRef: "backup"})
	assert.NilError(t, err)
	assert.Equal(t, len(runs.GetRuns()), 0)

	oldID := created.GetJob().GetId()
	runs, err = client.ListRuns(t.Context(), &jobspb.ListRunsRequest{JobRef: oldID})
	assert.NilError(t, err)
	assert.Assert(t, len(runs.GetRuns()) == 1)
	assert.Equal(t, runs.GetRuns()[0].GetId(), run.GetId())
	kept, err := client.InspectRun(t.Context(), &jobspb.InspectRunRequest{JobRef: oldID, RunRef: run.GetId()})
	assert.NilError(t, err)
	assert.Equal(t, kept.GetRun().GetState(), "succeeded")

	_, err = client.Remove(t.Context(), &jobspb.RemoveRequest{JobRef: oldID, RunsRemoval: "remove"})
	assert.NilError(t, err)
	_, err = client.ListRuns(t.Context(), &jobspb.ListRunsRequest{JobRef: oldID})
	assert.Equal(t, status.Code(err), codes.NotFound)
	_, err = client.InspectRun(t.Context(), &jobspb.InspectRunRequest{JobRef: oldID, RunRef: run.GetId()})
	assert.Equal(t, status.Code(err), codes.NotFound)
	live, err := client.Inspect(t.Context(), &jobspb.InspectRequest{JobRef: "backup"})
	assert.NilError(t, err)
	assert.Equal(t, live.GetJob().GetId(), replacement.GetJob().GetId())
}

// TestExtensionShutdownDrainsCalls checks that shutdown releases an API call
// blocked in the manager and drains it before stopping the manager.
func TestExtensionShutdownDrainsCalls(t *testing.T) {
	client, ext, kv, fake := newTestService(t)
	assert.NilError(t, activateExtension(t.Context(), ext, kv, fake))
	ext.mu.Lock()
	manager := ext.manager
	ext.mu.Unlock()

	created, err := client.Create(t.Context(), &jobspb.CreateRequest{Name: "backup", Spec: &jobspb.JobSpec{ContainerSpec: []byte(`{"Image":"busybox"}`)}})
	assert.NilError(t, err)

	// Waiting for the latest run of a job that never ran blocks.
	waited := make(chan error, 1)

	go func() {
		_, err := client.Wait(t.Context(), &jobspb.WaitRequest{JobRef: "backup"})
		waited <- err
	}()

	poll.WaitOn(t, func(poll.LogT) poll.Result {
		manager.mu.Lock()
		defer manager.mu.Unlock()

		if len(manager.notify[created.GetJob().GetId()]) > 0 {
			return poll.Success()
		}

		return poll.Continue("wait not blocked yet")
	})

	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
	defer cancel()

	assert.NilError(t, ext.shutdown(ctx))
	assert.Check(t, is.Equal(status.Code(<-waited), codes.Canceled))
}

// startingDaemonAPI reports the daemon API as not available yet.
type startingDaemonAPI struct{}

func (startingDaemonAPI) Get(context.Context, *daemonapiv0.GetRequest) (*daemonapiv0.GetResponse, error) {
	return nil, errors.New("daemon is starting")
}

// unusedWatcher satisfies the container state point for tests that never
// reach a container.
type unusedWatcher struct {
	containerstatev0.ContainerStateWatcher
}

// testContainerNames provides the name-generator point with
// testNameGenerator.
type testContainerNames struct{}

func (testContainerNames) GenerateContainerName(ctx context.Context, req *containernamegeneratorv0.GenerateContainerNameRequest) (*containernamegeneratorv0.GenerateContainerNameReply, error) {
	name, err := testNameGenerator(ctx, req.Retry)
	if err != nil {
		return nil, err
	}

	return &containernamegeneratorv0.GenerateContainerNameReply{Name: name}, nil
}

// TestExtensionShutdownReleasesPendingActivation shuts the extension down
// while its activation is still waiting for the daemon API: the wait must be
// released and no manager may start afterwards.
func TestExtensionShutdownReleasesPendingActivation(t *testing.T) {
	ext := NewExtension()
	assert.NilError(t, ext.init(t.Context(), nil, pointResolver{
		daemonapiv0.Point.ID():              daemonapiv0.DaemonAPI(startingDaemonAPI{}),
		containerstatev0.Point.ID():         containerstatev0.ContainerStateWatcher(unusedWatcher{}),
		storagekvv0.Point.ID():              storagekvv0.Storage(newFakeStorage()),
		containernamegeneratorv0.Point.ID(): containernamegeneratorv0.ContainerNameGenerator(testContainerNames{}),
	}))

	done := make(chan error, 1)
	go func() { done <- ext.shutdown(t.Context()) }()
	select {
	case err := <-done:
		assert.Check(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown still waiting on the pending activation")
	}
	ext.mu.Lock()
	manager := ext.manager
	ext.mu.Unlock()
	assert.Check(t, is.Nil(manager))
}

// blockingStopBackend blocks container stops until release is closed, like a
// container ignoring its stop signal, and reports each stop on stopping.
type blockingStopBackend struct {
	*fakeBackend
	stopping chan struct{}
	release  chan struct{}
}

func (b blockingStopBackend) ContainerStop(ctx context.Context, name string) error {
	b.stopping <- struct{}{}

	<-b.release

	return b.fakeBackend.ContainerStop(ctx, name)
}

// TestExtensionShutdownHonorsDeadlineDuringActivation checks that a shutdown
// does not wait past its deadline for an activation stuck reconciling, and
// that the activation does not publish its manager once it finishes.
func TestExtensionShutdownHonorsDeadlineDuringActivation(t *testing.T) {
	kv := newTestKV(t)
	backend := blockingStopBackend{fakeBackend: newFakeBackend(), stopping: make(chan struct{}, 1), release: make(chan struct{})}

	// Leave a run whose start was never recorded, with a container
	// reconciliation must stop.
	store, err := NewStore(t.Context(), kv)
	assert.NilError(t, err)

	job := makeJob("job1", "backup")
	job.State = jobsv0.JobStateRunning
	assert.NilError(t, store.CreateJob(t.Context(), job))
	created, err := backend.ContainerCreate(t.Context(), ContainerCreateRequest{Name: "job-backup-orphan"})
	assert.NilError(t, err)
	assert.NilError(t, store.CreateRun(t.Context(), &jobsv0.Run{ID: "orphan", JobID: job.ID, State: jobsv0.RunStatePending, CreatedAtNano: 1, ContainerID: created.ID}))

	ext := NewExtension()
	activated := make(chan error, 1)

	// Tracked like init's background activation.
	ext.activation.Go(func() { activated <- activateExtension(t.Context(), ext, kv, backend) })

	<-backend.stopping

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- ext.shutdown(ctx) }()

	select {
	case err := <-done:
		assert.Check(t, is.ErrorIs(err, context.DeadlineExceeded))
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown still waiting on the activation")
	}

	close(backend.release)
	assert.Check(t, is.ErrorContains(<-activated, "shut down"))
	ext.mu.Lock()
	manager := ext.manager
	ext.mu.Unlock()
	assert.Check(t, is.Nil(manager))
}
