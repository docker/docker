package daemon

import (
	"context"
	"fmt"

	"github.com/containerd/log"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/v2/daemon/container"
)

// ContainerUnpause unpauses a container
func (daemon *Daemon) ContainerUnpause(name string) error {
	ctr, err := daemon.GetContainer(name)
	if err != nil {
		return err
	}
	return daemon.containerUnpause(ctr)
}

// containerUnpause resumes the container execution after the container is paused.
func (daemon *Daemon) containerUnpause(ctr *container.Container) error {
	ctr.Lock()
	defer ctr.Unlock()

	// We cannot unpause the container which is not paused
	if !ctr.State.Paused {
		return fmt.Errorf("Container %s is not paused", ctr.ID)
	}
	tsk, err := ctr.GetRunningTask()
	if err != nil {
		return err
	}

	if err := tsk.Resume(context.Background()); err != nil {
		return fmt.Errorf("Cannot unpause container %s: %s", ctr.ID, err)
	}

	ctr.State.SetPaused(false)
	daemon.setStateCounter(ctr)
	daemon.updateHealthMonitor(ctr)
	daemon.LogContainerEvent(ctr, events.ActionUnPause)

	if err := ctr.CheckpointTo(context.WithoutCancel(context.TODO()), daemon.containersReplica); err != nil {
		log.G(context.TODO()).WithError(err).Warn("could not save container to disk")
	}

	return nil
}
