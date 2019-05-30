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
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/api/services/tasks/v1"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/oci"
	"github.com/containerd/containerd/runtime/v2/runc/options"
	"github.com/containerd/typeurl"
	prototypes "github.com/gogo/protobuf/types"
	ver "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

const (
	checkpointImageNameLabel       = "org.opencontainers.image.ref.name"
	checkpointRuntimeNameLabel     = "io.containerd.checkpoint.runtime"
	checkpointSnapshotterNameLabel = "io.containerd.checkpoint.snapshotter"
)

// Container is a metadata object for container resources and task creation
type Container interface {
	// ID identifies the container
	ID() string
	// Info returns the underlying container record type
	Info(context.Context) (containers.Container, error)
	// Delete removes the container
	Delete(context.Context, ...DeleteOpts) error
	// NewTask creates a new task based on the container metadata
	NewTask(context.Context, cio.Creator, ...NewTaskOpts) (Task, error)
	// Spec returns the OCI runtime specification
	Spec(context.Context) (*oci.Spec, error)
	// Task returns the current task for the container
	//
	// If cio.Attach options are passed the client will reattach to the IO for the running
	// task. If no task exists for the container a NotFound error is returned
	//
	// Clients must make sure that only one reader is attached to the task and consuming
	// the output from the task's fifos
	Task(context.Context, cio.Attach) (Task, error)
	// Image returns the image that the container is based on
	Image(context.Context) (Image, error)
	// Labels returns the labels set on the container
	Labels(context.Context) (map[string]string, error)
	// SetLabels sets the provided labels for the container and returns the final label set
	SetLabels(context.Context, map[string]string) (map[string]string, error)
	// Extensions returns the extensions set on the container
	Extensions(context.Context) (map[string]prototypes.Any, error)
	// Update a container
	Update(context.Context, ...UpdateContainerOpts) error
	// Checkpoint creates a checkpoint image of the current container
	Checkpoint(context.Context, string, ...CheckpointOpts) (Image, error)
}

func containerFromRecord(client *Client, c containers.Container) *container {
	return &container{
		client: client,
		id:     c.ID,
	}
}

var _ = (Container)(&container{})

type container struct {
	client *Client
	id     string
}

// ID returns the container's unique id
func (c *container) ID() string {
	return c.id
}

func (c *container) Info(ctx context.Context) (containers.Container, error) {
	return c.get(ctx)
}

func (c *container) Extensions(ctx context.Context) (map[string]prototypes.Any, error) {
	r, err := c.get(ctx)
	if err != nil {
		return nil, err
	}
	return r.Extensions, nil
}

func (c *container) Labels(ctx context.Context) (map[string]string, error) {
	r, err := c.get(ctx)
	if err != nil {
		return nil, err
	}
	return r.Labels, nil
}

func (c *container) SetLabels(ctx context.Context, labels map[string]string) (map[string]string, error) {
	container := containers.Container{
		ID:     c.id,
		Labels: labels,
	}

	var paths []string
	// mask off paths so we only muck with the labels encountered in labels.
	// Labels not in the passed in argument will be left alone.
	for k := range labels {
		paths = append(paths, strings.Join([]string{"labels", k}, "."))
	}

	r, err := c.client.ContainerService().Update(ctx, container, paths...)
	if err != nil {
		return nil, err
	}
	return r.Labels, nil
}

// Spec returns the current OCI specification for the container
func (c *container) Spec(ctx context.Context) (*oci.Spec, error) {
	r, err := c.get(ctx)
	if err != nil {
		return nil, err
	}
	var s oci.Spec
	if err := json.Unmarshal(r.Spec.Value, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Delete deletes an existing container
// an error is returned if the container has running tasks
func (c *container) Delete(ctx context.Context, opts ...DeleteOpts) error {
	if _, err := c.loadTask(ctx, nil); err == nil {
		return errors.Wrapf(errdefs.ErrFailedPrecondition, "cannot delete running task %v", c.id)
	}
	r, err := c.get(ctx)
	if err != nil {
		return err
	}
	for _, o := range opts {
		if err := o(ctx, c.client, r); err != nil {
			return err
		}
	}
	return c.client.ContainerService().Delete(ctx, c.id)
}

func (c *container) Task(ctx context.Context, attach cio.Attach) (Task, error) {
	return c.loadTask(ctx, attach)
}

// Image returns the image that the container is based on
func (c *container) Image(ctx context.Context) (Image, error) {
	r, err := c.get(ctx)
	if err != nil {
		return nil, err
	}
	if r.Image == "" {
		return nil, errors.Wrap(errdefs.ErrNotFound, "container not created from an image")
	}
	i, err := c.client.ImageService().Get(ctx, r.Image)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get image %s for container", r.Image)
	}
	return NewImage(c.client, i), nil
}

func (c *container) NewTask(ctx context.Context, ioCreate cio.Creator, opts ...NewTaskOpts) (_ Task, err error) {
	i, err := ioCreate(c.id)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil && i != nil {
			i.Cancel()
			i.Close()
		}
	}()
	cfg := i.Config()
	request := &tasks.CreateTaskRequest{
		ContainerID: c.id,
		Terminal:    cfg.Terminal,
		Stdin:       cfg.Stdin,
		Stdout:      cfg.Stdout,
		Stderr:      cfg.Stderr,
	}
	r, err := c.get(ctx)
	if err != nil {
		return nil, err
	}
	if r.SnapshotKey != "" {
		if r.Snapshotter == "" {
			return nil, errors.Wrapf(errdefs.ErrInvalidArgument, "unable to resolve rootfs mounts without snapshotter on container")
		}

		// get the rootfs from the snapshotter and add it to the request
		mounts, err := c.client.SnapshotService(r.Snapshotter).Mounts(ctx, r.SnapshotKey)
		if err != nil {
			return nil, err
		}
		for _, m := range mounts {
			request.Rootfs = append(request.Rootfs, &types.Mount{
				Type:    m.Type,
				Source:  m.Source,
				Options: m.Options,
			})
		}
	}
	info := TaskInfo{
		runtime: r.Runtime.Name,
	}
	for _, o := range opts {
		if err := o(ctx, c.client, &info); err != nil {
			return nil, err
		}
	}
	if info.RootFS != nil {
		for _, m := range info.RootFS {
			request.Rootfs = append(request.Rootfs, &types.Mount{
				Type:    m.Type,
				Source:  m.Source,
				Options: m.Options,
			})
		}
	}
	if info.Options != nil {
		any, err := typeurl.MarshalAny(info.Options)
		if err != nil {
			return nil, err
		}
		request.Options = any
	}
	t := &task{
		client: c.client,
		io:     i,
		id:     c.id,
	}
	if info.Checkpoint != nil {
		request.Checkpoint = info.Checkpoint
	}
	response, err := c.client.TaskService().Create(ctx, request)
	if err != nil {
		return nil, errdefs.FromGRPC(err)
	}
	t.pid = response.Pid
	return t, nil
}

func (c *container) Update(ctx context.Context, opts ...UpdateContainerOpts) error {
	// fetch the current container config before updating it
	r, err := c.get(ctx)
	if err != nil {
		return err
	}
	for _, o := range opts {
		if err := o(ctx, c.client, &r); err != nil {
			return err
		}
	}
	if _, err := c.client.ContainerService().Update(ctx, r); err != nil {
		return errdefs.FromGRPC(err)
	}
	return nil
}

func (c *container) Checkpoint(ctx context.Context, ref string, opts ...CheckpointOpts) (Image, error) {
	index := &ocispec.Index{
		Versioned: ver.Versioned{
			SchemaVersion: 2,
		},
		Annotations: make(map[string]string),
	}
	copts := &options.CheckpointOptions{
		Exit:                false,
		OpenTcp:             false,
		ExternalUnixSockets: false,
		Terminal:            false,
		FileLocks:           true,
		EmptyNamespaces:     nil,
	}
	info, err := c.Info(ctx)
	if err != nil {
		return nil, err
	}

	img, err := c.Image(ctx)
	if err != nil {
		return nil, err
	}

	ctx, done, err := c.client.WithLease(ctx)
	if err != nil {
		return nil, err
	}
	defer done(ctx)

	// add image name to manifest
	index.Annotations[checkpointImageNameLabel] = img.Name()
	// add runtime info to index
	index.Annotations[checkpointRuntimeNameLabel] = info.Runtime.Name
	// add snapshotter info to index
	index.Annotations[checkpointSnapshotterNameLabel] = info.Snapshotter

	// process remaining opts
	for _, o := range opts {
		if err := o(ctx, c.client, &info, index, copts); err != nil {
			err = errdefs.FromGRPC(err)
			if !errdefs.IsAlreadyExists(err) {
				return nil, err
			}
		}
	}

	desc, err := writeIndex(ctx, index, c.client, c.ID()+"index")
	if err != nil {
		return nil, err
	}
	i := images.Image{
		Name:   ref,
		Target: desc,
	}
	checkpoint, err := c.client.ImageService().Create(ctx, i)
	if err != nil {
		return nil, err
	}

	return NewImage(c.client, checkpoint), nil
}

func (c *container) loadTask(ctx context.Context, ioAttach cio.Attach) (Task, error) {
	response, err := c.client.TaskService().Get(ctx, &tasks.GetRequest{
		ContainerID: c.id,
	})
	if err != nil {
		err = errdefs.FromGRPC(err)
		if errdefs.IsNotFound(err) {
			return nil, errors.Wrapf(err, "no running task found")
		}
		return nil, err
	}
	var i cio.IO
	if ioAttach != nil {
		if i, err = attachExistingIO(response, ioAttach); err != nil {
			return nil, err
		}
	}
	t := &task{
		client: c.client,
		io:     i,
		id:     response.Process.ID,
		pid:    response.Process.Pid,
	}
	return t, nil
}

func (c *container) get(ctx context.Context) (containers.Container, error) {
	return c.client.ContainerService().Get(ctx, c.id)
}

// get the existing fifo paths from the task information stored by the daemon
func attachExistingIO(response *tasks.GetResponse, ioAttach cio.Attach) (cio.IO, error) {
	fifoSet := loadFifos(response)
	return ioAttach(fifoSet)
}

// loadFifos loads the containers fifos
func loadFifos(response *tasks.GetResponse) *cio.FIFOSet {
	path := getFifoDir([]string{
		response.Process.Stdin,
		response.Process.Stdout,
		response.Process.Stderr,
	})
	closer := func() error {
		return os.RemoveAll(path)
	}
	return cio.NewFIFOSet(cio.Config{
		Stdin:    response.Process.Stdin,
		Stdout:   response.Process.Stdout,
		Stderr:   response.Process.Stderr,
		Terminal: response.Process.Terminal,
	}, closer)
}

// getFifoDir looks for any non-empty path for a stdio fifo
// and returns the dir for where it is located
func getFifoDir(paths []string) string {
	for _, p := range paths {
		if p != "" {
			return filepath.Dir(p)
		}
	}
	return ""
}
