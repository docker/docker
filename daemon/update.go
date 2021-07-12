package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"github.com/pkg/errors"
)

// ContainerUpdate updates configuration of the container
func (daemon *Daemon) ContainerUpdate(name string, hostConfig *container.HostConfig) (container.ContainerUpdateOKBody, error) {
	var warnings []string

	warnings, err := daemon.verifyContainerSettings(hostConfig, nil, true)
	if err != nil {
		return container.ContainerUpdateOKBody{Warnings: warnings}, errdefs.InvalidParameter(err)
	}

	if err := daemon.update(name, hostConfig); err != nil {
		return container.ContainerUpdateOKBody{Warnings: warnings}, err
	}

	return container.ContainerUpdateOKBody{Warnings: warnings}, nil
}

func (daemon *Daemon) update(name string, hostConfig *container.HostConfig) error {
	if hostConfig == nil {
		return nil
	}

	ctr, err := daemon.GetContainer(name)
	if err != nil {
		return err
	}

	restoreConfig := false
	backupHostConfig := *ctr.HostConfig

	defer func() {
		if restoreConfig {
			ctr.Lock()
			if !ctr.RemovalInProgress && !ctr.Dead {
				ctr.HostConfig = &backupHostConfig
				ctr.CheckpointTo(daemon.containersReplica)
			}
			ctr.Unlock()
		}
	}()

	ctr.Lock()

	if ctr.RemovalInProgress || ctr.Dead {
		ctr.Unlock()
		return errCannotUpdate(ctr.ID, fmt.Errorf("container is marked for removal and cannot be \"update\""))
	}

	if err := ctr.UpdateContainer(hostConfig); err != nil {
		restoreConfig = true
		ctr.Unlock()
		return errCannotUpdate(ctr.ID, err)
	}
	if err := ctr.CheckpointTo(daemon.containersReplica); err != nil {
		restoreConfig = true
		ctr.Unlock()
		return errCannotUpdate(ctr.ID, err)
	}

	ctr.Unlock()

	// if Restart Policy changed, we need to update container monitor
	if hostConfig.RestartPolicy.Name != "" {
		ctr.UpdateMonitor(hostConfig.RestartPolicy)
	}

	// If container is not running, update hostConfig struct is enough,
	// resources will be updated when the container is started again.
	// If container is running (including paused), we need to update configs
	// to the real world.
	if ctr.IsRunning() && !ctr.IsRestarting() {
		if err := daemon.containerd.UpdateResources(context.Background(), ctr.ID, toContainerdResources(hostConfig.Resources)); err != nil {
			restoreConfig = true
			// TODO: it would be nice if containerd responded with better errors here so we can classify this better.
			return errCannotUpdate(ctr.ID, errdefs.System(err))
		}
	}

	daemon.LogContainerEvent(ctr, "update")

	return nil
}

func errCannotUpdate(containerID string, err error) error {
	return errors.Wrap(err, "Cannot update container "+containerID)
}
