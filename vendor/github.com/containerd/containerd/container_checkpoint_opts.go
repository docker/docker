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

package containerd

import (
	"bytes"
	"context"
	"fmt"
	"runtime"

	tasks "github.com/containerd/containerd/api/services/tasks/v1"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/diff"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/rootfs"
	"github.com/containerd/containerd/runtime/v2/runc/options"
	"github.com/containerd/typeurl"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

var (
	// ErrCheckpointRWUnsupported is returned if the container runtime does not support checkpoint
	ErrCheckpointRWUnsupported = errors.New("rw checkpoint is only supported on v2 runtimes")
	// ErrMediaTypeNotFound returns an error when a media type in the manifest is unknown
	ErrMediaTypeNotFound = errors.New("media type not found")
)

// CheckpointOpts are options to manage the checkpoint operation
type CheckpointOpts func(context.Context, *Client, *containers.Container, *imagespec.Index, *options.CheckpointOptions) error

// WithCheckpointImage includes the container image in the checkpoint
func WithCheckpointImage(ctx context.Context, client *Client, c *containers.Container, index *imagespec.Index, copts *options.CheckpointOptions) error {
	ir, err := client.ImageService().Get(ctx, c.Image)
	if err != nil {
		return err
	}
	index.Manifests = append(index.Manifests, ir.Target)
	return nil
}

// WithCheckpointTask includes the running task
func WithCheckpointTask(ctx context.Context, client *Client, c *containers.Container, index *imagespec.Index, copts *options.CheckpointOptions) error {
	any, err := typeurl.MarshalAny(copts)
	if err != nil {
		return nil
	}
	task, err := client.TaskService().Checkpoint(ctx, &tasks.CheckpointTaskRequest{
		ContainerID: c.ID,
		Options:     any,
	})
	if err != nil {
		return err
	}
	for _, d := range task.Descriptors {
		platformSpec := platforms.DefaultSpec()
		index.Manifests = append(index.Manifests, imagespec.Descriptor{
			MediaType:   d.MediaType,
			Size:        d.Size_,
			Digest:      d.Digest,
			Platform:    &platformSpec,
			Annotations: d.Annotations,
		})
	}
	// save copts
	data, err := any.Marshal()
	if err != nil {
		return err
	}
	r := bytes.NewReader(data)
	desc, err := writeContent(ctx, client.ContentStore(), images.MediaTypeContainerd1CheckpointOptions, c.ID+"-checkpoint-options", r)
	if err != nil {
		return err
	}
	desc.Platform = &imagespec.Platform{
		OS:           runtime.GOOS,
		Architecture: runtime.GOARCH,
	}
	index.Manifests = append(index.Manifests, desc)
	return nil
}

// WithCheckpointRuntime includes the container runtime info
func WithCheckpointRuntime(ctx context.Context, client *Client, c *containers.Container, index *imagespec.Index, copts *options.CheckpointOptions) error {
	if c.Runtime.Options != nil {
		data, err := c.Runtime.Options.Marshal()
		if err != nil {
			return err
		}
		r := bytes.NewReader(data)
		desc, err := writeContent(ctx, client.ContentStore(), images.MediaTypeContainerd1CheckpointRuntimeOptions, c.ID+"-runtime-options", r)
		if err != nil {
			return err
		}
		desc.Platform = &imagespec.Platform{
			OS:           runtime.GOOS,
			Architecture: runtime.GOARCH,
		}
		index.Manifests = append(index.Manifests, desc)
	}
	return nil
}

// WithCheckpointRW includes the rw in the checkpoint
func WithCheckpointRW(ctx context.Context, client *Client, c *containers.Container, index *imagespec.Index, copts *options.CheckpointOptions) error {
	diffOpts := []diff.Opt{
		diff.WithReference(fmt.Sprintf("checkpoint-rw-%s", c.SnapshotKey)),
	}
	rw, err := rootfs.CreateDiff(ctx,
		c.SnapshotKey,
		client.SnapshotService(c.Snapshotter),
		client.DiffService(),
		diffOpts...,
	)
	if err != nil {
		return err

	}
	rw.Platform = &imagespec.Platform{
		OS:           runtime.GOOS,
		Architecture: runtime.GOARCH,
	}
	index.Manifests = append(index.Manifests, rw)
	return nil
}

// WithCheckpointTaskExit causes the task to exit after checkpoint
func WithCheckpointTaskExit(ctx context.Context, client *Client, c *containers.Container, index *imagespec.Index, copts *options.CheckpointOptions) error {
	copts.Exit = true
	return nil
}

// GetIndexByMediaType returns the index in a manifest for the specified media type
func GetIndexByMediaType(index *imagespec.Index, mt string) (*imagespec.Descriptor, error) {
	for _, d := range index.Manifests {
		if d.MediaType == mt {
			return &d, nil
		}
	}
	return nil, ErrMediaTypeNotFound
}
