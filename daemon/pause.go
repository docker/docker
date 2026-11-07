package daemon

import (
	"context"
	"fmt"

	"github.com/containerd/log"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/errdefs"
)

// ContainerPause pauses a container
func (daemon *Daemon) ContainerPause(name string) error {
	ctr, err := daemon.GetContainer(name)
	if err != nil {
		return err
	}
	return daemon.containerPause(ctr)
}

// containerPause pauses the container execution without stopping the process.
// The execution can be resumed by calling containerUnpause.
func (daemon *Daemon) containerPause(container *container.Container) error {
	container.Lock()
	defer container.Unlock()

	// We cannot Pause the container which is not running
	tsk, err := container.GetRunningTask()
	if err != nil {
		return err
	}

	// We cannot Pause the container which is already paused
	if container.State.Paused {
		return errdefs.Conflict(fmt.Errorf("container %s is already paused", container.ID))
	}

	// We cannot Pause the container which is restarting
	if container.State.Restarting {
		return errContainerIsRestarting(container.ID)
	}

	if err := tsk.Pause(context.Background()); err != nil {
		return fmt.Errorf("cannot pause container %s: %s", container.ID, err)
	}

	container.State.SetPaused(true)
	daemon.setStateCounter(container)
	daemon.updateHealthMonitor(container)
	daemon.LogContainerEvent(container, events.ActionPause)

	if err := container.CheckpointTo(context.WithoutCancel(context.TODO()), daemon.containersReplica); err != nil {
		log.G(context.TODO()).WithError(err).Warn("could not save container to disk")
	}

	return nil
}
