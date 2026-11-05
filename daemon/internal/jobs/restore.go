package jobs

import (
	"context"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	jobsv0 "github.com/moby/moby/v2/extpoints/jobs/api/v0"
)

// Restore reconciles state left behind by an unclean daemon stop — a crash,
// or a live-restore shutdown that deliberately detached from still-running
// containers. It runs after the daemon's own container restore and before
// Start arms schedule triggers.
//
// For each job's in-flight run (at most one, by the concurrency invariant):
//
//   - a run whose start was never recorded is failed outright: whether its
//     container was never created, created but never started, or started
//     just as the daemon died, the start evidence is lost, and guessing an
//     outcome from a container that never ran would fabricate a success.
//     Any container left behind is stopped before the job is released, so
//     it cannot overlap the job's next run; if stopping fails, the job
//     stays occupied until the container is observed exiting. A container
//     whose ID was never recorded never started; it is found by its name
//     and removed;
//   - a started run is re-attached to the standard exit watcher, which
//     resolves it from the container's actual state — immediately if the
//     container exited during the downtime, or whenever it exits if it is
//     still running. A timeout deadline is re-armed from the original start
//     time, and enforced immediately when it expired while the daemon was
//     down;
//   - a job stuck in the running state with a terminal (or no) run — a
//     crash between the run's terminal write and the job's idle write — is
//     returned to idle.
//
// Runs kept as tombstone history of removed jobs are not reconciled: they
// may stay in flight forever, the documented cost of retaining history past
// a job's removal.
//
// Restore must be called at most once, before Start: a second call would
// attach duplicate watchers to the re-attached runs.
func (m *Manager) Restore(ctx context.Context) {
	// Reconciliation outlives the startup call the same way watchers do.
	ctx = context.WithoutCancel(ctx)
	for _, job := range m.store.Jobs() {
		m.locks.Lock(job.ID)
		m.restoreJobLocked(ctx, job)
		m.locks.Unlock(job.ID)
	}
}

func (m *Manager) restoreJobLocked(ctx context.Context, job *jobsv0.Job) {
	run, err := m.store.LatestRun(job.ID)
	if err != nil || isTerminalRunState(run.State) {
		if job.State == jobsv0.JobStateRunning {
			var runID string
			if err == nil {
				runID = run.ID
			}

			m.releaseJobLocked(ctx, job.ID, runID)
		}
		return
	}

	if run.StartedAtNano == 0 {
		if run.ContainerID == "" {
			// A crash between creating the container and recording its ID
			// leaves a container known only by its name. It never started,
			// so there is nothing to keep it for.
			if err := m.backend.ContainerRm(ctx, runContainerName(job, run)); err != nil && !cerrdefs.IsNotFound(err) {
				log.G(ctx).WithError(err).WithFields(log.Fields{"job": job.ID, "run": run.ID}).Warn("could not remove unrecorded container of unstarted restored run")
			}
		}

		m.failUnstartedLocked(ctx, job, run, "daemon restarted before the run start was recorded")
		return
	}

	m.watch(ctx, job, run)
}
