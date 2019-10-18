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

package leases

import (
	"context"

	"google.golang.org/grpc"

	api "github.com/containerd/containerd/api/services/leases/v1"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/plugin"
	"github.com/containerd/containerd/services"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/pkg/errors"
)

func init() {
	plugin.Register(&plugin.Registration{
		Type: plugin.GRPCPlugin,
		ID:   "leases",
		Requires: []plugin.Type{
			plugin.ServicePlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			plugins, err := ic.GetByType(plugin.ServicePlugin)
			if err != nil {
				return nil, err
			}
			p, ok := plugins[services.LeasesService]
			if !ok {
				return nil, errors.New("leases service not found")
			}
			i, err := p.Instance()
			if err != nil {
				return nil, err
			}
			return &service{lm: i.(leases.Manager)}, nil
		},
	})
}

type service struct {
	lm leases.Manager
}

func (s *service) Register(server *grpc.Server) error {
	api.RegisterLeasesServer(server, s)
	return nil
}

func (s *service) Create(ctx context.Context, r *api.CreateRequest) (*api.CreateResponse, error) {
	opts := []leases.Opt{
		leases.WithLabels(r.Labels),
	}
	if r.ID == "" {
		opts = append(opts, leases.WithRandomID())
	} else {
		opts = append(opts, leases.WithID(r.ID))
	}

	l, err := s.lm.Create(ctx, opts...)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	return &api.CreateResponse{
		Lease: leaseToGRPC(l),
	}, nil
}

func (s *service) Delete(ctx context.Context, r *api.DeleteRequest) (*ptypes.Empty, error) {
	var opts []leases.DeleteOpt
	if r.Sync {
		opts = append(opts, leases.SynchronousDelete)
	}
	if err := s.lm.Delete(ctx, leases.Lease{
		ID: r.ID,
	}, opts...); err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	return &ptypes.Empty{}, nil
}

func (s *service) List(ctx context.Context, r *api.ListRequest) (*api.ListResponse, error) {
	l, err := s.lm.List(ctx, r.Filters...)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	apileases := make([]*api.Lease, len(l))
	for i := range l {
		apileases[i] = leaseToGRPC(l[i])
	}

	return &api.ListResponse{
		Leases: apileases,
	}, nil
}

func leaseToGRPC(l leases.Lease) *api.Lease {
	return &api.Lease{
		ID:        l.ID,
		Labels:    l.Labels,
		CreatedAt: l.CreatedAt,
	}
}
