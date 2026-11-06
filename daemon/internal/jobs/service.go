package jobs

import (
	"context"

	"github.com/containerd/errdefs/pkg/errgrpc"
	jobsv0 "github.com/moby/moby/v2/extpoints/jobs/api/v0"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// service adapts the manager to the Jobs API contract: it resolves the
// active manager, delegates, and translates errdefs classes to gRPC status
// codes with containerd's canonical mapping (invalid parameter to
// InvalidArgument, not found to NotFound, already exists to AlreadyExists,
// failed precondition to FailedPrecondition).
type service struct {
	ext *Extension
}

var _ jobsv0.Jobs = (*service)(nil)

// manager admits a call on the active manager, refusing calls that arrive
// before the daemon activated the extension or after it shut down. The
// returned context is cancelled when shutdown starts draining calls, and
// done must be called once the call no longer uses the manager.
func (s *service) manager(ctx context.Context) (_ context.Context, _ *Manager, done func(), _ error) {
	s.ext.mu.Lock()
	defer s.ext.mu.Unlock()
	if s.ext.manager == nil {
		return nil, nil, nil, status.Error(codes.Unavailable, "the jobs extension is not active")
	}

	s.ext.calls.Add(1)

	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ext.stopCalls, cancel)

	return ctx, s.ext.manager, func() {
		stop()
		cancel()
		s.ext.calls.Done()
	}, nil
}

func (s *service) Create(ctx context.Context, req *jobsv0.CreateRequest) (*jobsv0.CreateReply, error) {
	ctx, m, done, err := s.manager(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	job, created, err := m.Create(ctx, req.Name, req.Spec)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return &jobsv0.CreateReply{Job: job, Created: created}, nil
}

func (s *service) Run(ctx context.Context, req *jobsv0.RunRequest) (*jobsv0.RunReply, error) {
	ctx, m, done, err := s.manager(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	run, err := m.Run(ctx, req.JobRef, req.Reschedule)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return &jobsv0.RunReply{Run: run}, nil
}

func (s *service) CreateAndRun(ctx context.Context, req *jobsv0.CreateAndRunRequest) (*jobsv0.CreateAndRunReply, error) {
	ctx, m, done, err := s.manager(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	job, run, created, err := m.CreateAndRun(ctx, req.Name, req.Spec)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return &jobsv0.CreateAndRunReply{Job: job, Run: run, Created: created}, nil
}

func (s *service) Inspect(ctx context.Context, req *jobsv0.InspectRequest) (*jobsv0.InspectReply, error) {
	ctx, m, done, err := s.manager(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	job, err := m.Inspect(ctx, req.JobRef)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return &jobsv0.InspectReply{Job: job}, nil
}

func (s *service) List(ctx context.Context, req *jobsv0.ListRequest) (*jobsv0.ListReply, error) {
	ctx, m, done, err := s.manager(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	list, err := m.List(ctx, req)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	jobs := make([]jobsv0.Job, len(list))
	for i, job := range list {
		jobs[i] = *job
	}
	return &jobsv0.ListReply{Jobs: jobs}, nil
}

func (s *service) Pause(ctx context.Context, req *jobsv0.PauseRequest) error {
	ctx, m, done, err := s.manager(ctx)
	if err != nil {
		return err
	}
	defer done()

	return errgrpc.ToGRPC(m.Pause(ctx, req.JobRef))
}

func (s *service) Resume(ctx context.Context, req *jobsv0.ResumeRequest) error {
	ctx, m, done, err := s.manager(ctx)
	if err != nil {
		return err
	}
	defer done()

	return errgrpc.ToGRPC(m.Resume(ctx, req.JobRef))
}

func (s *service) Cancel(ctx context.Context, req *jobsv0.CancelRequest) (*jobsv0.CancelReply, error) {
	ctx, m, done, err := s.manager(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	runID, err := m.Cancel(ctx, req.JobRef)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return &jobsv0.CancelReply{RunID: runID}, nil
}

func (s *service) Remove(ctx context.Context, req *jobsv0.RemoveRequest) error {
	ctx, m, done, err := s.manager(ctx)
	if err != nil {
		return err
	}
	defer done()

	return errgrpc.ToGRPC(m.Remove(ctx, req.JobRef, req.RunsRemoval))
}

func (s *service) Prune(ctx context.Context, req *jobsv0.PruneRequest) (*jobsv0.PruneReply, error) {
	ctx, m, done, err := s.manager(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	removed, err := m.Prune(ctx, req.Labels)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return &jobsv0.PruneReply{RemovedJobIDs: removed}, nil
}

func (s *service) ListRuns(ctx context.Context, req *jobsv0.ListRunsRequest) (*jobsv0.ListRunsReply, error) {
	ctx, m, done, err := s.manager(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	page, nextCursor, staleCursor, err := m.RunsPage(ctx, req.JobRef, int(req.Limit), req.Before)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	runs := make([]jobsv0.Run, len(page))
	for i, run := range page {
		runs[i] = *run
	}
	return &jobsv0.ListRunsReply{Runs: runs, NextCursor: nextCursor, CursorStale: staleCursor}, nil
}

func (s *service) InspectRun(ctx context.Context, req *jobsv0.InspectRunRequest) (*jobsv0.InspectRunReply, error) {
	ctx, m, done, err := s.manager(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	run, err := m.InspectRun(ctx, req.JobRef, req.RunRef)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return &jobsv0.InspectRunReply{Run: run}, nil
}

func (s *service) Wait(ctx context.Context, req *jobsv0.WaitRequest) (*jobsv0.WaitReply, error) {
	ctx, m, done, err := s.manager(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	run, err := m.Wait(ctx, req.JobRef, req.RunRef, req.Condition)
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	return &jobsv0.WaitReply{Run: run}, nil
}
