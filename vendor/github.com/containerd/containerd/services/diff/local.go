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
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/diff"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/plugin"
	"github.com/containerd/containerd/services"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
)

type config struct {
	// Order is the order of preference in which to try diff algorithms, the
	// first differ which is supported is used.
	// Note when multiple differs may be supported, this order will be
	// respected for which is chosen. Each differ should return the same
	// correct output, allowing any ordering to be used to prefer
	// more optimimal implementations.
	Order []string `toml:"default"`
}

type differ interface {
	diff.Comparer
	diff.Applier
}

func init() {
	plugin.Register(&plugin.Registration{
		Type: plugin.ServicePlugin,
		ID:   services.DiffService,
		Requires: []plugin.Type{
			plugin.DiffPlugin,
		},
		Config: defaultDifferConfig,
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			differs, err := ic.GetByType(plugin.DiffPlugin)
			if err != nil {
				return nil, err
			}

			orderedNames := ic.Config.(*config).Order
			ordered := make([]differ, len(orderedNames))
			for i, n := range orderedNames {
				differp, ok := differs[n]
				if !ok {
					return nil, errors.Errorf("needed differ not loaded: %s", n)
				}
				d, err := differp.Instance()
				if err != nil {
					return nil, errors.Wrapf(err, "could not load required differ due plugin init error: %s", n)
				}

				ordered[i], ok = d.(differ)
				if !ok {
					return nil, errors.Errorf("differ does not implement Comparer and Applier interface: %s", n)
				}
			}

			return &local{
				differs: ordered,
			}, nil
		},
	})
}

type local struct {
	differs []differ
}

var _ diffapi.DiffClient = &local{}

func (l *local) Apply(ctx context.Context, er *diffapi.ApplyRequest, _ ...grpc.CallOption) (*diffapi.ApplyResponse, error) {
	var (
		ocidesc ocispec.Descriptor
		err     error
		desc    = toDescriptor(er.Diff)
		mounts  = toMounts(er.Mounts)
	)

	for _, differ := range l.differs {
		ocidesc, err = differ.Apply(ctx, desc, mounts)
		if !errdefs.IsNotImplemented(err) {
			break
		}
	}

	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	return &diffapi.ApplyResponse{
		Applied: fromDescriptor(ocidesc),
	}, nil

}

func (l *local) Diff(ctx context.Context, dr *diffapi.DiffRequest, _ ...grpc.CallOption) (*diffapi.DiffResponse, error) {
	var (
		ocidesc ocispec.Descriptor
		err     error
		aMounts = toMounts(dr.Left)
		bMounts = toMounts(dr.Right)
	)

	var opts []diff.Opt
	if dr.MediaType != "" {
		opts = append(opts, diff.WithMediaType(dr.MediaType))
	}
	if dr.Ref != "" {
		opts = append(opts, diff.WithReference(dr.Ref))
	}
	if dr.Labels != nil {
		opts = append(opts, diff.WithLabels(dr.Labels))
	}

	for _, d := range l.differs {
		ocidesc, err = d.Compare(ctx, aMounts, bMounts, opts...)
		if !errdefs.IsNotImplemented(err) {
			break
		}
	}
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	return &diffapi.DiffResponse{
		Diff: fromDescriptor(ocidesc),
	}, nil
}

func toMounts(apim []*types.Mount) []mount.Mount {
	mounts := make([]mount.Mount, len(apim))
	for i, m := range apim {
		mounts[i] = mount.Mount{
			Type:    m.Type,
			Source:  m.Source,
			Options: m.Options,
		}
	}
	return mounts
}

func toDescriptor(d *types.Descriptor) ocispec.Descriptor {
	return ocispec.Descriptor{
		MediaType: d.MediaType,
		Digest:    d.Digest,
		Size:      d.Size_,
	}
}

func fromDescriptor(d ocispec.Descriptor) *types.Descriptor {
	return &types.Descriptor{
		MediaType: d.MediaType,
		Digest:    d.Digest,
		Size_:     d.Size,
	}
}
