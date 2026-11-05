package jobs

import (
	"context"
	"errors"
	"slices"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	containerstatev0 "github.com/moby/moby/v2/extpoints/containerstate/v0"
)

// engineBackend runs job containers through the Engine API and observes
// their exits through the container state point.
type engineBackend struct {
	api   *client.Client
	state containerstatev0.ContainerStateWatcher
}

func (b *engineBackend) ContainerCreate(ctx context.Context, req ContainerCreateRequest) (container.CreateResponse, error) {
	res, err := b.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:             req.Name,
		Config:           req.Config,
		HostConfig:       req.HostConfig,
		NetworkingConfig: req.NetworkingConfig,
	})
	if err != nil {
		return container.CreateResponse{}, err
	}

	return container.CreateResponse{ID: res.ID, Warnings: res.Warnings}, nil
}

func (b *engineBackend) ContainerStart(ctx context.Context, id string) error {
	_, err := b.api.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

func (b *engineBackend) ContainerStop(ctx context.Context, id string) error {
	_, err := b.api.ContainerStop(ctx, id, client.ContainerStopOptions{})
	return err
}

func (b *engineBackend) ContainerRm(ctx context.Context, id string) error {
	// Like docker run --rm, anonymous volumes go with the container; named
	// volumes are kept.
	_, err := b.api.ContainerRemove(ctx, id, client.ContainerRemoveOptions{RemoveVolumes: true})
	return err
}

// finalStatuses are the statuses a container settles in once its restart
// policy is exhausted; between attempts it is restarting, not exited.
var finalStatuses = []string{
	containerstatev0.StatusExited,
	containerstatev0.StatusDead,
	containerstatev0.StatusRemoved,
}

func (b *engineBackend) ContainerWait(ctx context.Context, id string) (<-chan StateStatus, error) {
	waitC := make(chan StateStatus, 1)

	// Resolve the container first, so a missing one fails the call rather
	// than the delivered status.
	current, err := b.state.Wait(ctx, &containerstatev0.WaitRequest{ContainerID: id})
	switch {
	case err == nil && slices.Contains(finalStatuses, current.Current.Status):
		waitC <- exitStatusOf(current)
		return waitC, nil
	case err != nil && (cerrdefs.IsNotFound(err) || ctx.Err() != nil):
		return nil, err
	}

	go b.waitFinal(ctx, id, waitC)

	return waitC, nil
}

// waitRetryInterval is how long waitFinal pauses after a failed wait before
// observing the container again.
const waitRetryInterval = 100 * time.Millisecond

// errContainerRemoved reports a container removed before it exited.
var errContainerRemoved = errors.New("container was removed before it exited")

// waitFinal delivers the container's final exit. A failed wait does not mean
// the container exited, as when the wait falls behind its status changes, so
// the container is observed again until it settles, is gone, or ctx is done.
func (b *engineBackend) waitFinal(ctx context.Context, id string, waitC chan<- StateStatus) {
	for {
		reply, err := b.state.Wait(ctx, &containerstatev0.WaitRequest{ContainerID: id, Statuses: finalStatuses})

		switch {
		case err == nil:
			waitC <- exitStatusOf(reply)
			return
		case ctx.Err() != nil:
			return
		case cerrdefs.IsNotFound(err):
			// Removed between two waits.
			waitC <- exitStatus{code: -1, err: errContainerRemoved}
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(waitRetryInterval):
		}
	}
}

// exitStatus is a container's final exit as reported by the container state
// point.
type exitStatus struct {
	code int
	err  error
}

func (s exitStatus) ExitCode() int { return s.code }
func (s exitStatus) Err() error    { return s.err }

// exitStatusOf converts a final container state into an exit status.
// Like the daemon's own wait, an exit that was not recorded reports exit
// code -1.
func exitStatusOf(reply *containerstatev0.WaitReply) exitStatus {
	exit := reply.Current.Exit
	// Removal clears the error from the current state; the state at removal
	// keeps it.
	if reply.Current.Status == containerstatev0.StatusRemoved && reply.Previous != nil && reply.Previous.Exit != nil {
		exit = reply.Previous.Exit
	}

	if exit == nil {
		if reply.Current.Status == containerstatev0.StatusRemoved {
			return exitStatus{code: -1, err: errContainerRemoved}
		}

		return exitStatus{code: -1, err: errors.New("container stopped without a recorded exit")}
	}

	var exitErr error
	if exit.Error != "" {
		exitErr = errors.New(exit.Error)
	}

	return exitStatus{code: int(exit.ExitCode), err: exitErr}
}

// apiPollInterval is how often activation checks whether the daemon serves
// its API yet.
const apiPollInterval = 100 * time.Millisecond

// waitForAPI blocks until the daemon serves API requests.
// Extensions initialize while the daemon is still starting, and the API
// answers 503 Service Unavailable until container restore completes.
func waitForAPI(ctx context.Context, api *client.Client) error {
	for {
		if _, err := api.Ping(ctx, client.PingOptions{}); err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(apiPollInterval):
		}
	}
}
