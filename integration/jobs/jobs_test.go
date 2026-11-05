package jobs

import (
	"context"
	"testing"

	extensionclient "github.com/moby/extensions/client"
	"github.com/moby/moby/client"
	jobsv0 "github.com/moby/moby/v2/extpoints/jobs/api/v0"
	jobspb "github.com/moby/moby/v2/extpoints/jobs/api/v0/protogen"
	"github.com/moby/moby/v2/internal/testutil"
	"github.com/moby/moby/v2/internal/testutil/daemon"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
	"gotest.tools/v3/skip"
)

// startJobsDaemon starts a daemon with the given extra arguments and returns
// a Jobs API client resolved through the extensions client over its socket.
// Callers opt into the jobs extension with --extension-enable.
func startJobsDaemon(ctx context.Context, t *testing.T, args ...string) (*daemon.Daemon, jobsv0.Jobs) {
	t.Helper()
	d := daemon.New(t)
	d.StartWithBusybox(ctx, t, append([]string{"--iptables=false", "--ip6tables=false"}, args...)...)
	t.Cleanup(func() { d.Stop(t) })

	engine := d.NewClientT(t)
	extensions, err := extensionclient.New(engine, extensionclient.WithGRPCPoint(jobspb.ClientPoint))
	assert.NilError(t, err)
	t.Cleanup(func() { assert.NilError(t, extensions.Close()) })
	jobs, err := extensionclient.Resolve(extensions, jobsv0.Point)
	assert.NilError(t, err)
	waitJobsActive(ctx, t, jobs)
	return d, jobs
}

// waitJobsActive waits until the Jobs API stops answering Unavailable: the
// extension activates in the background once the daemon serves its API.
func waitJobsActive(ctx context.Context, t *testing.T, jobs jobsv0.Jobs) {
	t.Helper()
	poll.WaitOn(t, func(poll.LogT) poll.Result {
		_, err := jobs.List(ctx, &jobsv0.ListRequest{})
		if status.Code(err) == codes.Unavailable {
			return poll.Continue("jobs extension not active yet: %v", err)
		}
		return poll.Success()
	})
}

// TestJobLifecycle drives a manual job end to end against a real daemon:
// idempotent registration, a run executing a real container, the terminal
// state observed through Wait, the run container carrying the correlation
// labels, and removal.
func TestJobLifecycle(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon, "cannot start a local daemon on a remote host")
	skip.If(t, testEnv.DaemonInfo.OSType != "linux", "the jobs extension test image is linux-only")
	ctx := testutil.StartSpan(baseContext, t)

	d, jobs := startJobsDaemon(ctx, t, "--extension-enable", "org.mobyproject.jobs.v0")

	spec := &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"busybox","Cmd":["/bin/sh","-c","echo hello-from-job"]}`)}
	created, err := jobs.Create(ctx, &jobsv0.CreateRequest{Name: "greet", Spec: spec})
	assert.NilError(t, err)
	assert.Check(t, created.Created)

	// Re-applying the same spec is a no-op: the idempotency contract that
	// makes registration safe to replay.
	again, err := jobs.Create(ctx, &jobsv0.CreateRequest{Name: "greet", Spec: spec})
	assert.NilError(t, err)
	assert.Check(t, !again.Created)
	assert.Check(t, is.Equal(again.Job.ID, created.Job.ID))

	runReply, err := jobs.Run(ctx, &jobsv0.RunRequest{JobRef: "greet"})
	assert.NilError(t, err)
	waited, err := jobs.Wait(ctx, &jobsv0.WaitRequest{JobRef: "greet", RunRef: runReply.Run.ID})
	assert.NilError(t, err)
	run := waited.Run
	assert.Check(t, is.Equal(run.State, jobsv0.RunStateSucceeded), "run error: %q", run.Error)
	assert.Assert(t, run.ExitCode != nil)
	assert.Check(t, is.Equal(run.ExitCode.Value, int64(0)))

	// The run container exists, correlated through the reserved labels.
	apiClient := d.NewClientT(t)
	inspected, err := apiClient.ContainerInspect(ctx, run.ContainerID, client.ContainerInspectOptions{})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(inspected.Container.Config.Labels["com.docker.job.id"], created.Job.ID))
	assert.Check(t, is.Equal(inspected.Container.Config.Labels["com.docker.job.run-id"], run.ID))

	err = jobs.Remove(ctx, &jobsv0.RemoveRequest{JobRef: "greet", RunsRemoval: jobsv0.RunsRemove})
	assert.NilError(t, err)
	_, err = jobs.Inspect(ctx, &jobsv0.InspectRequest{JobRef: "greet"})
	assert.Check(t, is.Equal(status.Code(err), codes.NotFound))
}

// TestJobsReconcileAfterUncleanShutdown kills the daemon under an in-flight
// run and verifies reconciliation resolves it after restart: the run is
// either re-attached (container survived — then cancellable like any run)
// or recorded terminal from the container's actual fate; either way it ends
// terminal and the job returns to idle instead of being stuck running.
func TestJobsReconcileAfterUncleanShutdown(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon, "cannot start a local daemon on a remote host")
	skip.If(t, testEnv.DaemonInfo.OSType != "linux", "the jobs extension test image is linux-only")
	ctx := testutil.StartSpan(baseContext, t)

	d, jobs := startJobsDaemon(ctx, t, "--extension-enable", "org.mobyproject.jobs.v0")

	spec := &jobsv0.JobSpec{ContainerSpec: []byte(`{"Image":"busybox","Cmd":["sleep","300"]}`)}
	started, err := jobs.CreateAndRun(ctx, &jobsv0.CreateAndRunRequest{Name: "sleeper", Spec: spec})
	assert.NilError(t, err)

	assert.NilError(t, d.Kill())
	d.Start(t, "--extension-enable", "org.mobyproject.jobs.v0", "--iptables=false", "--ip6tables=false")
	waitJobsActive(ctx, t, jobs)

	inspected, err := jobs.InspectRun(ctx, &jobsv0.InspectRunRequest{JobRef: "sleeper", RunRef: started.Run.ID})
	assert.NilError(t, err)
	if inspected.Run.State == jobsv0.RunStateRunning {
		// Re-attached to the surviving container: cancel to finish the test
		// without waiting out the sleep.
		_, err := jobs.Cancel(ctx, &jobsv0.CancelRequest{JobRef: "sleeper"})
		assert.NilError(t, err)
	}
	waited, err := jobs.Wait(ctx, &jobsv0.WaitRequest{JobRef: "sleeper", RunRef: started.Run.ID})
	assert.NilError(t, err)
	assert.Check(t, is.Contains([]string{jobsv0.RunStateCancelled, jobsv0.RunStateFailed}, waited.Run.State))

	job, err := jobs.Inspect(ctx, &jobsv0.InspectRequest{JobRef: "sleeper"})
	assert.NilError(t, err)
	assert.Check(t, is.Equal(job.Job.State, jobsv0.JobStateIdle))
}

// TestJobsExtensionDisabled proves the gate: without the extension enabled,
// the daemon does not expose the Jobs service at all.
func TestJobsExtensionDisabled(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon, "cannot start a local daemon on a remote host")
	skip.If(t, testEnv.DaemonInfo.OSType != "linux", "the jobs extension test image is linux-only")
	ctx := testutil.StartSpan(baseContext, t)

	_, jobs := startJobsDaemon(ctx, t)

	_, err := jobs.List(ctx, &jobsv0.ListRequest{})
	assert.Check(t, is.Equal(status.Code(err), codes.Unimplemented))
}
