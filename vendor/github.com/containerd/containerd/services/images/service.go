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

package images

import (
	"context"

	imagesapi "github.com/containerd/containerd/api/services/images/v1"
	"github.com/containerd/containerd/plugin"
	"github.com/containerd/containerd/services"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
)

func init() {
	plugin.Register(&plugin.Registration{
		Type: plugin.GRPCPlugin,
		ID:   "images",
		Requires: []plugin.Type{
			plugin.ServicePlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			plugins, err := ic.GetByType(plugin.ServicePlugin)
			if err != nil {
				return nil, err
			}
			p, ok := plugins[services.ImagesService]
			if !ok {
				return nil, errors.New("images service not found")
			}
			i, err := p.Instance()
			if err != nil {
				return nil, err
			}
			return &service{local: i.(imagesapi.ImagesClient)}, nil
		},
	})
}

type service struct {
	local imagesapi.ImagesClient
}

var _ imagesapi.ImagesServer = &service{}

func (s *service) Register(server *grpc.Server) error {
	imagesapi.RegisterImagesServer(server, s)
	return nil
}

func (s *service) Get(ctx context.Context, req *imagesapi.GetImageRequest) (*imagesapi.GetImageResponse, error) {
	return s.local.Get(ctx, req)
}

func (s *service) List(ctx context.Context, req *imagesapi.ListImagesRequest) (*imagesapi.ListImagesResponse, error) {
	return s.local.List(ctx, req)
}

func (s *service) Create(ctx context.Context, req *imagesapi.CreateImageRequest) (*imagesapi.CreateImageResponse, error) {
	return s.local.Create(ctx, req)
}

func (s *service) Update(ctx context.Context, req *imagesapi.UpdateImageRequest) (*imagesapi.UpdateImageResponse, error) {
	return s.local.Update(ctx, req)
}

func (s *service) Delete(ctx context.Context, req *imagesapi.DeleteImageRequest) (*ptypes.Empty, error) {
	return s.local.Delete(ctx, req)
}
