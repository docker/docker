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
	"context"
	"encoding/json"
	"fmt"
	"syscall"

	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/runtime/linux/runctypes"
	"github.com/containerd/containerd/runtime/v2/runc/options"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
)

// NewTaskOpts allows the caller to set options on a new task
type NewTaskOpts func(context.Context, *Client, *TaskInfo) error

// WithRootFS allows a task to be created without a snapshot being allocated to its container
func WithRootFS(mounts []mount.Mount) NewTaskOpts {
	return func(ctx context.Context, c *Client, ti *TaskInfo) error {
		ti.RootFS = mounts
		return nil
	}
}

// WithTaskCheckpoint allows a task to be created with live runtime and memory data from a
// previous checkpoint. Additional software such as CRIU may be required to
// restore a task from a checkpoint
func WithTaskCheckpoint(im Image) NewTaskOpts {
	return func(ctx context.Context, c *Client, info *TaskInfo) error {
		desc := im.Target()
		id := desc.Digest
		index, err := decodeIndex(ctx, c.ContentStore(), desc)
		if err != nil {
			return err
		}
		for _, m := range index.Manifests {
			if m.MediaType == images.MediaTypeContainerd1Checkpoint {
				info.Checkpoint = &types.Descriptor{
					MediaType:   m.MediaType,
					Size_:       m.Size,
					Digest:      m.Digest,
					Annotations: m.Annotations,
				}
				return nil
			}
		}
		return fmt.Errorf("checkpoint not found in index %s", id)
	}
}

func decodeIndex(ctx context.Context, store content.Provider, desc imagespec.Descriptor) (*imagespec.Index, error) {
	var index imagespec.Index
	p, err := content.ReadBlob(ctx, store, desc)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(p, &index); err != nil {
		return nil, err
	}

	return &index, nil
}

// WithCheckpointName sets the image name for the checkpoint
func WithCheckpointName(name string) CheckpointTaskOpts {
	return func(r *CheckpointTaskInfo) error {
		r.Name = name
		return nil
	}
}

// WithCheckpointImagePath sets image path for checkpoint option
func WithCheckpointImagePath(path string) CheckpointTaskOpts {
	return func(r *CheckpointTaskInfo) error {
		if CheckRuntime(r.Runtime(), "io.containerd.runc") {
			if r.Options == nil {
				r.Options = &options.CheckpointOptions{}
			}
			opts, ok := r.Options.(*options.CheckpointOptions)
			if !ok {
				return errors.New("invalid v2 shim checkpoint options format")
			}
			opts.ImagePath = path
		} else {
			if r.Options == nil {
				r.Options = &runctypes.CheckpointOptions{}
			}
			opts, ok := r.Options.(*runctypes.CheckpointOptions)
			if !ok {
				return errors.New("invalid v1 shim checkpoint options format")
			}
			opts.ImagePath = path
		}
		return nil
	}
}

// WithRestoreImagePath sets image path for create option
func WithRestoreImagePath(path string) NewTaskOpts {
	return func(ctx context.Context, c *Client, ti *TaskInfo) error {
		if CheckRuntime(ti.Runtime(), "io.containerd.runc") {
			if ti.Options == nil {
				ti.Options = &options.Options{}
			}
			opts, ok := ti.Options.(*options.Options)
			if !ok {
				return errors.New("invalid v2 shim create options format")
			}
			opts.CriuImagePath = path
		} else {
			if ti.Options == nil {
				ti.Options = &runctypes.CreateOptions{}
			}
			opts, ok := ti.Options.(*runctypes.CreateOptions)
			if !ok {
				return errors.New("invalid v1 shim create options format")
			}
			opts.CriuImagePath = path
		}
		return nil
	}
}

// ProcessDeleteOpts allows the caller to set options for the deletion of a task
type ProcessDeleteOpts func(context.Context, Process) error

// WithProcessKill will forcefully kill and delete a process
func WithProcessKill(ctx context.Context, p Process) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// ignore errors to wait and kill as we are forcefully killing
	// the process and don't care about the exit status
	s, err := p.Wait(ctx)
	if err != nil {
		return err
	}
	if err := p.Kill(ctx, syscall.SIGKILL, WithKillAll); err != nil {
		if errdefs.IsFailedPrecondition(err) || errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	// wait for the process to fully stop before letting the rest of the deletion complete
	<-s
	return nil
}

// KillInfo contains information on how to process a Kill action
type KillInfo struct {
	// All kills all processes inside the task
	// only valid on tasks, ignored on processes
	All bool
	// ExecID is the ID of a process to kill
	ExecID string
}

// KillOpts allows options to be set for the killing of a process
type KillOpts func(context.Context, *KillInfo) error

// WithKillAll kills all processes for a task
func WithKillAll(ctx context.Context, i *KillInfo) error {
	i.All = true
	return nil
}

// WithKillExecID specifies the process ID
func WithKillExecID(execID string) KillOpts {
	return func(ctx context.Context, i *KillInfo) error {
		i.ExecID = execID
		return nil
	}
}

// WithResources sets the provided resources for task updates. Resources must be
// either a *specs.LinuxResources or a *specs.WindowsResources
func WithResources(resources interface{}) UpdateTaskOpts {
	return func(ctx context.Context, client *Client, r *UpdateTaskInfo) error {
		switch resources.(type) {
		case *specs.LinuxResources:
		case *specs.WindowsResources:
		default:
			return errors.New("WithResources requires a *specs.LinuxResources or *specs.WindowsResources")
		}

		r.Resources = resources
		return nil
	}
}
