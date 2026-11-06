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

package proxy

import (
	"context"

	diffapi "github.com/containerd/containerd/api/services/diff/v1"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/typeurl/v2"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"fmt"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/epoch"
	"github.com/containerd/containerd/v2/pkg/oci"
	ptypes "github.com/containerd/containerd/v2/pkg/protobuf/types"
	"github.com/containerd/errdefs"
)

// NewDiffApplier returns a new comparer and applier which communicates
// over a GRPC connection.
func NewDiffApplier(client diffapi.DiffClient) any {
	return &diffRemote{
		client: client,
	}
}

type diffRemote struct {
	client diffapi.DiffClient
}

func (r *diffRemote) Apply(ctx context.Context, desc ocispec.Descriptor, mounts []mount.Mount, opts ...diff.ApplyOpt) (ocispec.Descriptor, error) {
	var config diff.ApplyConfig
	for _, opt := range opts {
		if err := opt(ctx, desc, &config); err != nil {
			return ocispec.Descriptor{}, err
		}
	}
	if diff.HasIDMapping(config.UIDMap, config.GIDMap) {
		return ocispec.Descriptor{}, fmt.Errorf("ID mapping is not supported by the diff service API: %w", errdefs.ErrNotImplemented)
	}

	payloads := make(map[string]*ptypes.Any)
	for k, v := range config.ProcessorPayloads {
		payloads[k] = typeurl.MarshalProto(v)
	}
	if config.Progress != nil {
		config.Progress(0)
	}
	req := &diffapi.ApplyRequest{
		Diff:     oci.DescriptorToProto(desc),
		Mounts:   mount.ToProto(mounts),
		Payloads: payloads,
		SyncFs:   config.SyncFs,
	}
	resp, err := r.client.Apply(ctx, req)
	if err != nil {
		return ocispec.Descriptor{}, errgrpc.ToNative(err)
	}
	if config.Progress != nil {
		config.Progress(desc.Size)
	}
	return oci.DescriptorFromProto(resp.Applied), nil
}

func (r *diffRemote) Compare(ctx context.Context, a, b []mount.Mount, opts ...diff.Opt) (ocispec.Descriptor, error) {
	var config diff.Config
	for _, opt := range opts {
		if err := opt(&config); err != nil {
			return ocispec.Descriptor{}, err
		}
	}
	if diff.HasIDMapping(config.UIDMap, config.GIDMap) {
		return ocispec.Descriptor{}, fmt.Errorf("ID mapping is not supported by the diff service API: %w", errdefs.ErrNotImplemented)
	}
	if tm := epoch.FromContext(ctx); tm != nil && config.SourceDateEpoch == nil {
		config.SourceDateEpoch = tm
	}
	var sourceDateEpoch *timestamppb.Timestamp
	if config.SourceDateEpoch != nil {
		sourceDateEpoch = timestamppb.New(*config.SourceDateEpoch)
	}
	req := &diffapi.DiffRequest{
		Left:            mount.ToProto(a),
		Right:           mount.ToProto(b),
		MediaType:       config.MediaType,
		Ref:             config.Reference,
		Labels:          config.Labels,
		SourceDateEpoch: sourceDateEpoch,
	}
	resp, err := r.client.Diff(ctx, req)
	if err != nil {
		return ocispec.Descriptor{}, errgrpc.ToNative(err)
	}
	return oci.DescriptorFromProto(resp.Diff), nil
}
