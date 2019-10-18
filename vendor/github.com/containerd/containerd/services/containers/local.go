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

package containers

import (
	"context"
	"io"

	eventstypes "github.com/containerd/containerd/api/events"
	api "github.com/containerd/containerd/api/services/containers/v1"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/events"
	"github.com/containerd/containerd/metadata"
	"github.com/containerd/containerd/plugin"
	"github.com/containerd/containerd/services"
	ptypes "github.com/gogo/protobuf/types"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcm "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func init() {
	plugin.Register(&plugin.Registration{
		Type: plugin.ServicePlugin,
		ID:   services.ContainersService,
		Requires: []plugin.Type{
			plugin.MetadataPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			m, err := ic.Get(plugin.MetadataPlugin)
			if err != nil {
				return nil, err
			}
			return &local{
				db:        m.(*metadata.DB),
				publisher: ic.Events,
			}, nil
		},
	})
}

type local struct {
	db        *metadata.DB
	publisher events.Publisher
}

var _ api.ContainersClient = &local{}

func (l *local) Get(ctx context.Context, req *api.GetContainerRequest, _ ...grpc.CallOption) (*api.GetContainerResponse, error) {
	var resp api.GetContainerResponse

	return &resp, errdefs.ToGRPC(l.withStoreView(ctx, func(ctx context.Context, store containers.Store) error {
		container, err := store.Get(ctx, req.ID)
		if err != nil {
			return err
		}
		containerpb := containerToProto(&container)
		resp.Container = containerpb

		return nil
	}))
}

func (l *local) List(ctx context.Context, req *api.ListContainersRequest, _ ...grpc.CallOption) (*api.ListContainersResponse, error) {
	var resp api.ListContainersResponse
	return &resp, errdefs.ToGRPC(l.withStoreView(ctx, func(ctx context.Context, store containers.Store) error {
		containers, err := store.List(ctx, req.Filters...)
		if err != nil {
			return err
		}
		resp.Containers = containersToProto(containers)
		return nil
	}))
}

func (l *local) ListStream(ctx context.Context, req *api.ListContainersRequest, _ ...grpc.CallOption) (api.Containers_ListStreamClient, error) {
	stream := &localStream{
		ctx: ctx,
	}
	return stream, errdefs.ToGRPC(l.withStoreView(ctx, func(ctx context.Context, store containers.Store) error {
		containers, err := store.List(ctx, req.Filters...)
		if err != nil {
			return err
		}
		stream.containers = containersToProto(containers)
		return nil
	}))
}

func (l *local) Create(ctx context.Context, req *api.CreateContainerRequest, _ ...grpc.CallOption) (*api.CreateContainerResponse, error) {
	var resp api.CreateContainerResponse

	if err := l.withStoreUpdate(ctx, func(ctx context.Context, store containers.Store) error {
		container := containerFromProto(&req.Container)

		created, err := store.Create(ctx, container)
		if err != nil {
			return err
		}

		resp.Container = containerToProto(&created)

		return nil
	}); err != nil {
		return &resp, errdefs.ToGRPC(err)
	}
	if err := l.publisher.Publish(ctx, "/containers/create", &eventstypes.ContainerCreate{
		ID:    resp.Container.ID,
		Image: resp.Container.Image,
		Runtime: &eventstypes.ContainerCreate_Runtime{
			Name:    resp.Container.Runtime.Name,
			Options: resp.Container.Runtime.Options,
		},
	}); err != nil {
		return &resp, err
	}

	return &resp, nil
}

func (l *local) Update(ctx context.Context, req *api.UpdateContainerRequest, _ ...grpc.CallOption) (*api.UpdateContainerResponse, error) {
	if req.Container.ID == "" {
		return nil, status.Errorf(codes.InvalidArgument, "Container.ID required")
	}
	var (
		resp      api.UpdateContainerResponse
		container = containerFromProto(&req.Container)
	)

	if err := l.withStoreUpdate(ctx, func(ctx context.Context, store containers.Store) error {
		var fieldpaths []string
		if req.UpdateMask != nil && len(req.UpdateMask.Paths) > 0 {
			fieldpaths = append(fieldpaths, req.UpdateMask.Paths...)
		}

		updated, err := store.Update(ctx, container, fieldpaths...)
		if err != nil {
			return err
		}

		resp.Container = containerToProto(&updated)
		return nil
	}); err != nil {
		return &resp, errdefs.ToGRPC(err)
	}

	if err := l.publisher.Publish(ctx, "/containers/update", &eventstypes.ContainerUpdate{
		ID:          resp.Container.ID,
		Image:       resp.Container.Image,
		Labels:      resp.Container.Labels,
		SnapshotKey: resp.Container.SnapshotKey,
	}); err != nil {
		return &resp, err
	}

	return &resp, nil
}

func (l *local) Delete(ctx context.Context, req *api.DeleteContainerRequest, _ ...grpc.CallOption) (*ptypes.Empty, error) {
	if err := l.withStoreUpdate(ctx, func(ctx context.Context, store containers.Store) error {
		return store.Delete(ctx, req.ID)
	}); err != nil {
		return &ptypes.Empty{}, errdefs.ToGRPC(err)
	}

	if err := l.publisher.Publish(ctx, "/containers/delete", &eventstypes.ContainerDelete{
		ID: req.ID,
	}); err != nil {
		return &ptypes.Empty{}, err
	}

	return &ptypes.Empty{}, nil
}

func (l *local) withStore(ctx context.Context, fn func(ctx context.Context, store containers.Store) error) func(tx *bolt.Tx) error {
	return func(tx *bolt.Tx) error { return fn(ctx, metadata.NewContainerStore(tx)) }
}

func (l *local) withStoreView(ctx context.Context, fn func(ctx context.Context, store containers.Store) error) error {
	return l.db.View(l.withStore(ctx, fn))
}

func (l *local) withStoreUpdate(ctx context.Context, fn func(ctx context.Context, store containers.Store) error) error {
	return l.db.Update(l.withStore(ctx, fn))
}

type localStream struct {
	ctx        context.Context
	containers []api.Container
	i          int
}

func (s *localStream) Recv() (*api.ListContainerMessage, error) {
	if s.i >= len(s.containers) {
		return nil, io.EOF
	}
	c := s.containers[s.i]
	s.i++
	return &api.ListContainerMessage{
		Container: &c,
	}, nil
}

func (s *localStream) Context() context.Context {
	return s.ctx
}

func (s *localStream) CloseSend() error {
	return nil
}

func (s *localStream) Header() (grpcm.MD, error) {
	return nil, nil
}

func (s *localStream) Trailer() grpcm.MD {
	return nil
}

func (s *localStream) SendMsg(m interface{}) error {
	return nil
}

func (s *localStream) RecvMsg(m interface{}) error {
	return nil
}
