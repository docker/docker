/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package snapshots

import (
	"context"

	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/plugin"
	"github.com/containerd/containerd/services"
	"github.com/containerd/containerd/snapshots"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
)

func init() {
	plugin.Register(&plugin.Registration{
		Type: plugin.GRPCPlugin,
		ID:   "snapshots",
		Requires: []plugin.Type{
			plugin.ServicePlugin,
		},
		InitFn: newService,
	})
}

var empty = &ptypes.Empty{}

type service struct {
	ss map[string]snapshots.Snapshotter
}

func newService(ic *plugin.InitContext) (interface{}, error) {
	plugins, err := ic.GetByType(plugin.ServicePlugin)
	if err != nil {
		return nil, err
	}
	p, ok := plugins[services.SnapshotsService]
	if !ok {
		return nil, errors.New("snapshots service not found")
	}
	i, err := p.Instance()
	if err != nil {
		return nil, err
	}
	ss := i.(map[string]snapshots.Snapshotter)
	return &service{ss: ss}, nil
}

func (s *service) getSnapshotter(name string) (snapshots.Snapshotter, error) {
	if name == "" {
		return nil, errdefs.ToGRPCf(errdefs.ErrInvalidArgument, "snapshotter argument missing")
	}

	sn := s.ss[name]
	if sn == nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrInvalidArgument, "snapshotter not loaded: %s", name)
	}
	return sn, nil
}

func (s *service) Register(gs *grpc.Server) error {
	snapshotsapi.RegisterSnapshotsServer(gs, s)
	return nil
}

func (s *service) Prepare(ctx context.Context, pr *snapshotsapi.PrepareSnapshotRequest) (*snapshotsapi.PrepareSnapshotResponse, error) {
	log.G(ctx).WithField("parent", pr.Parent).WithField("key", pr.Key).Debugf("prepare snapshot")
	sn, err := s.getSnapshotter(pr.Snapshotter)
	if err != nil {
		return nil, err
	}

	var opts []snapshots.Opt
	if pr.Labels != nil {
		opts = append(opts, snapshots.WithLabels(pr.Labels))
	}
	mounts, err := sn.Prepare(ctx, pr.Key, pr.Parent, opts...)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	return &snapshotsapi.PrepareSnapshotResponse{
		Mounts: fromMounts(mounts),
	}, nil
}

func (s *service) View(ctx context.Context, pr *snapshotsapi.ViewSnapshotRequest) (*snapshotsapi.ViewSnapshotResponse, error) {
	log.G(ctx).WithField("parent", pr.Parent).WithField("key", pr.Key).Debugf("prepare view snapshot")
	sn, err := s.getSnapshotter(pr.Snapshotter)
	if err != nil {
		return nil, err
	}
	var opts []snapshots.Opt
	if pr.Labels != nil {
		opts = append(opts, snapshots.WithLabels(pr.Labels))
	}
	mounts, err := sn.View(ctx, pr.Key, pr.Parent, opts...)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	return &snapshotsapi.ViewSnapshotResponse{
		Mounts: fromMounts(mounts),
	}, nil
}

func (s *service) Mounts(ctx context.Context, mr *snapshotsapi.MountsRequest) (*snapshotsapi.MountsResponse, error) {
	log.G(ctx).WithField("key", mr.Key).Debugf("get snapshot mounts")
	sn, err := s.getSnapshotter(mr.Snapshotter)
	if err != nil {
		return nil, err
	}

	mounts, err := sn.Mounts(ctx, mr.Key)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	return &snapshotsapi.MountsResponse{
		Mounts: fromMounts(mounts),
	}, nil
}

func (s *service) Commit(ctx context.Context, cr *snapshotsapi.CommitSnapshotRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithField("key", cr.Key).WithField("name", cr.Name).Debugf("commit snapshot")
	sn, err := s.getSnapshotter(cr.Snapshotter)
	if err != nil {
		return nil, err
	}

	var opts []snapshots.Opt
	if cr.Labels != nil {
		opts = append(opts, snapshots.WithLabels(cr.Labels))
	}
	if err := sn.Commit(ctx, cr.Name, cr.Key, opts...); err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	return empty, nil
}

func (s *service) Remove(ctx context.Context, rr *snapshotsapi.RemoveSnapshotRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithField("key", rr.Key).Debugf("remove snapshot")
	sn, err := s.getSnapshotter(rr.Snapshotter)
	if err != nil {
		return nil, err
	}

	if err := sn.Remove(ctx, rr.Key); err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	return empty, nil
}

func (s *service) Stat(ctx context.Context, sr *snapshotsapi.StatSnapshotRequest) (*snapshotsapi.StatSnapshotResponse, error) {
	log.G(ctx).WithField("key", sr.Key).Debugf("stat snapshot")
	sn, err := s.getSnapshotter(sr.Snapshotter)
	if err != nil {
		return nil, err
	}

	info, err := sn.Stat(ctx, sr.Key)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	return &snapshotsapi.StatSnapshotResponse{Info: fromInfo(info)}, nil
}

func (s *service) Update(ctx context.Context, sr *snapshotsapi.UpdateSnapshotRequest) (*snapshotsapi.UpdateSnapshotResponse, error) {
	log.G(ctx).WithField("key", sr.Info.Name).Debugf("update snapshot")
	sn, err := s.getSnapshotter(sr.Snapshotter)
	if err != nil {
		return nil, err
	}

	info, err := sn.Update(ctx, toInfo(sr.Info), sr.UpdateMask.GetPaths()...)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	return &snapshotsapi.UpdateSnapshotResponse{Info: fromInfo(info)}, nil
}

func (s *service) List(sr *snapshotsapi.ListSnapshotsRequest, ss snapshotsapi.Snapshots_ListServer) error {
	sn, err := s.getSnapshotter(sr.Snapshotter)
	if err != nil {
		return err
	}

	var (
		buffer    []snapshotsapi.Info
		sendBlock = func(block []snapshotsapi.Info) error {
			return ss.Send(&snapshotsapi.ListSnapshotsResponse{
				Info: block,
			})
		}
	)
	err = sn.Walk(ss.Context(), func(ctx context.Context, info snapshots.Info) error {
		buffer = append(buffer, fromInfo(info))

		if len(buffer) >= 100 {
			if err := sendBlock(buffer); err != nil {
				return err
			}

			buffer = buffer[:0]
		}

		return nil
	})
	if err != nil {
		return err
	}
	if len(buffer) > 0 {
		// Send remaining infos
		if err := sendBlock(buffer); err != nil {
			return err
		}
	}

	return nil
}

func (s *service) Usage(ctx context.Context, ur *snapshotsapi.UsageRequest) (*snapshotsapi.UsageResponse, error) {
	sn, err := s.getSnapshotter(ur.Snapshotter)
	if err != nil {
		return nil, err
	}

	usage, err := sn.Usage(ctx, ur.Key)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	return fromUsage(usage), nil
}

func fromKind(kind snapshots.Kind) snapshotsapi.Kind {
	if kind == snapshots.KindActive {
		return snapshotsapi.KindActive
	}
	if kind == snapshots.KindView {
		return snapshotsapi.KindView
	}
	return snapshotsapi.KindCommitted
}

func fromInfo(info snapshots.Info) snapshotsapi.Info {
	return snapshotsapi.Info{
		Name:      info.Name,
		Parent:    info.Parent,
		Kind:      fromKind(info.Kind),
		CreatedAt: info.Created,
		UpdatedAt: info.Updated,
		Labels:    info.Labels,
	}
}

func fromUsage(usage snapshots.Usage) *snapshotsapi.UsageResponse {
	return &snapshotsapi.UsageResponse{
		Inodes: usage.Inodes,
		Size_:  usage.Size,
	}
}

func fromMounts(mounts []mount.Mount) []*types.Mount {
	out := make([]*types.Mount, len(mounts))
	for i, m := range mounts {
		out[i] = &types.Mount{
			Type:    m.Type,
			Source:  m.Source,
			Options: m.Options,
		}
	}
	return out
}

func toInfo(info snapshotsapi.Info) snapshots.Info {
	return snapshots.Info{
		Name:    info.Name,
		Parent:  info.Parent,
		Kind:    toKind(info.Kind),
		Created: info.CreatedAt,
		Updated: info.UpdatedAt,
		Labels:  info.Labels,
	}
}

func toKind(kind snapshotsapi.Kind) snapshots.Kind {
	if kind == snapshotsapi.KindActive {
		return snapshots.KindActive
	}
	if kind == snapshotsapi.KindView {
		return snapshots.KindView
	}
	return snapshots.KindCommitted
}
