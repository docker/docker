package libcontainerd

import (
	"context"

	containerd "github.com/containerd/containerd/v2/client"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"

	"github.com/moby/moby/v2/daemon/internal/libcontainerd/types"
)

// ReplaceContainer creates a new container, replacing any existing container
// with the same id if necessary.
func ReplaceContainer(ctx context.Context, client types.Client, id string, spec *specs.Spec, shim string, runtimeOptions any, opts ...containerd.NewContainerOpts) (types.Container, error) {
	newContainer := func() (types.Container, error) {
		return client.NewContainer(ctx, id, spec, shim, runtimeOptions, opts...)
	}
	ctr, err := newContainer()
	if err == nil || !cerrdefs.IsConflict(err) {
		return ctr, err
	}

	log.G(ctx).WithContext(ctx).WithField("container", id).Debug("A container already exists with the same ID. Attempting to clean up the old container.")
	if err := DeleteContainer(ctx, client, id); err != nil {
		return nil, err
	}
	return newContainer()
}

// DeleteContainer deletes a containerd container and any task it still owns.
// It returns no error if the container does not exist.
func DeleteContainer(ctx context.Context, client types.Client, id string) error {
	ctr, err := client.LoadContainer(ctx, id)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil
		}
		return errors.Wrap(err, "could not load containerd container object")
	}
	tsk, err := ctr.Task(ctx)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			goto deleteContainer
		}
		// There is no point in trying to delete the container if we
		// cannot determine whether or not it has a task. The containerd
		// client would just try to load the task itself, get the same
		// error, and give up.
		return errors.Wrap(err, "could not load containerd task object")
	}
	if err := tsk.ForceDelete(ctx); err != nil {
		if !cerrdefs.IsNotFound(err) {
			return errors.Wrap(err, "could not delete containerd task object")
		}
		// The task might have exited on its own. Proceed with
		// attempting to delete the container.
	}
deleteContainer:
	if err := ctr.Delete(ctx); err != nil && !cerrdefs.IsNotFound(err) {
		return errors.Wrap(err, "could not delete containerd container object")
	}
	return nil
}
