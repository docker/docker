// SPDX-FileCopyrightText: Copyright The Moby Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"slices"

	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/moby/extensions/stream"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/errdefs"
	containerstatev0 "github.com/moby/moby/v2/extpoints/containerstate/v0"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// grpcError keeps error classification for in-process callers while also
// carrying a gRPC status for the callback server.
type grpcError struct{ error }

func (e grpcError) Unwrap() error { return e.error }

func (e grpcError) GRPCStatus() *status.Status {
	return status.Convert(errgrpc.ToGRPC(e.error))
}

type containerStateWatcher struct {
	daemon *Daemon
}

func (w containerStateWatcher) Wait(ctx context.Context, req *containerstatev0.WaitRequest) (*containerstatev0.WaitReply, error) {
	if req == nil || req.ContainerID == "" {
		return nil, errdefs.InvalidParameter(status.Error(codes.InvalidArgument, "container ID is required"))
	}

	if len(req.Statuses) > 0 && len(req.NotStatuses) > 0 {
		return nil, errdefs.InvalidParameter(status.Error(codes.InvalidArgument, "only one of Statuses and NotStatuses can be set"))
	}

	for _, s := range slices.Concat(req.Statuses, req.NotStatuses) {
		if s != containerstatev0.StatusRemoved && containertypes.ValidateContainerState(containertypes.ContainerState(s)) != nil {
			return nil, errdefs.InvalidParameter(status.Errorf(codes.InvalidArgument, "invalid status: %q", s))
		}
	}

	ctr, err := w.lookup(req.ContainerID)
	if err != nil {
		return nil, err
	}

	for r, err := range ctr.State.StatusChanges(ctx) {
		if err != nil {
			return nil, statusChangesError(ctx, err)
		}

		reply := waitReply(r)
		cur := reply.Current.Status

		matched := slices.Contains(req.Statuses, cur)
		if len(req.Statuses) == 0 {
			matched = !slices.Contains(req.NotStatuses, cur)
		}
		// A removed container's state never changes again.
		if matched || r.Removed {
			return reply, nil
		}
	}
	// StatusChanges only stops after yielding a removal or an error.
	return nil, errStatusChangesEnded
}

func (w containerStateWatcher) Watch(ctx context.Context, req *containerstatev0.WatchRequest, events stream.Sender[containerstatev0.WatchEvent]) error {
	if req == nil || req.ContainerID == "" {
		return errdefs.InvalidParameter(status.Error(codes.InvalidArgument, "container ID is required"))
	}

	ctr, err := w.lookup(req.ContainerID)
	if err != nil {
		return err
	}

	for r, err := range ctr.State.StatusChanges(ctx) {
		if err != nil {
			return statusChangesError(ctx, err)
		}

		reply := waitReply(r)
		if err := events.Send(&containerstatev0.WatchEvent{
			ChangedAt: reply.ChangedAt,
			Current:   reply.Current,
			Previous:  reply.Previous,
		}); err != nil {
			return err
		}

		if r.Removed {
			return nil
		}
	}

	return errStatusChangesEnded
}

// errStatusChangesEnded is returned if State.StatusChanges stops without
// yielding a removal or an error, which it does not do.
var errStatusChangesEnded = errdefs.Unavailable(status.Error(codes.Unavailable, "container status changes ended"))

// lookup returns the container with the given ID, name, or unique ID prefix
// once daemon startup is complete.
func (w containerStateWatcher) lookup(id string) (*container.Container, error) {
	select {
	case <-w.daemon.startupDone:
	default:
		return nil, errdefs.Unavailable(status.Error(codes.Unavailable, "daemon startup is not complete"))
	}

	ctr, err := w.daemon.GetContainer(id)
	if err != nil {
		return nil, grpcError{err}
	}

	return ctr, nil
}

// statusChangesError returns the error to report for an error yielded by
// State.StatusChanges.
func statusChangesError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	return grpcError{err}
}

// waitReply returns the point's view of r.
func waitReply(r container.StatusResult) *containerstatev0.WaitReply {
	reply := &containerstatev0.WaitReply{Current: containerState(r.Current)}
	if r.Removed {
		reply.Current.Status = containerstatev0.StatusRemoved
	}
	if !r.ChangedAt.IsZero() {
		reply.ChangedAt = r.ChangedAt.UnixNano()
		reply.Previous = containerState(r.Previous)
	}
	return reply
}

// containerState returns the point's view of s.
func containerState(s container.StateSnapshot) *containerstatev0.ContainerState {
	cs := &containerstatev0.ContainerState{Status: string(s.Status)}
	// Only running and paused containers have not exited since they started;
	// a restarting container reports the exit that triggered the restart.
	running := s.Status == containertypes.StateRunning || s.Status == containertypes.StatePaused
	if !running && !s.FinishedAt.IsZero() {
		cs.Exit = &containerstatev0.Exit{
			ExitCode:  int64(s.ExitCode),
			Error:     s.Error,
			OOMKilled: s.OOMKilled,
		}
	}
	return cs
}

var _ containerstatev0.ContainerStateWatcher = containerStateWatcher{}
