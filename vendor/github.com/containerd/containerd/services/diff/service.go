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

package diff

import (
	"context"

	diffapi "github.com/containerd/containerd/api/services/diff/v1"
	"github.com/containerd/containerd/plugin"
	"github.com/containerd/containerd/services"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
)

func init() {
	plugin.Register(&plugin.Registration{
		Type: plugin.GRPCPlugin,
		ID:   "diff",
		Requires: []plugin.Type{
			plugin.ServicePlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			plugins, err := ic.GetByType(plugin.ServicePlugin)
			if err != nil {
				return nil, err
			}
			p, ok := plugins[services.DiffService]
			if !ok {
				return nil, errors.New("diff service not found")
			}
			i, err := p.Instance()
			if err != nil {
				return nil, err
			}
			return &service{local: i.(diffapi.DiffClient)}, nil
		},
	})
}

type service struct {
	local diffapi.DiffClient
}

var _ diffapi.DiffServer = &service{}

func (s *service) Register(gs *grpc.Server) error {
	diffapi.RegisterDiffServer(gs, s)
	return nil
}

func (s *service) Apply(ctx context.Context, er *diffapi.ApplyRequest) (*diffapi.ApplyResponse, error) {
	return s.local.Apply(ctx, er)
}

func (s *service) Diff(ctx context.Context, dr *diffapi.DiffRequest) (*diffapi.DiffResponse, error) {
	return s.local.Diff(ctx, dr)
}
