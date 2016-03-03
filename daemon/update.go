package daemon

import (
	"fmt"
	"time"

	"github.com/docker/engine-api/types/container"
)

// ContainerUpdate updates configuration of the container
func (daemon *Daemon) ContainerUpdate(name string, hostConfig *container.HostConfig, labels map[string]string) ([]string, error) {
	var warnings []string

	warnings, err := daemon.verifyContainerSettings(hostConfig, nil, true)
	if err != nil {
		return warnings, err
	}

	if err := daemon.update(name, hostConfig, labels); err != nil {
		return warnings, err
	}

	return warnings, nil
}

// ContainerUpdateCmdOnBuild updates Path and Args for the container with ID cID.
func (daemon *Daemon) ContainerUpdateCmdOnBuild(cID string, cmd []string) error {
	c, err := daemon.GetContainer(cID)
	if err != nil {
		return err
	}
	c.Path = cmd[0]
	c.Args = cmd[1:]
	return nil
}

func (daemon *Daemon) update(name string, hostConfig *container.HostConfig, labels map[string]string) error {
	if hostConfig == nil {
		return nil
	}

	container, err := daemon.GetContainer(name)
	if err != nil {
		return err
	}

	restoreConfig := false
	backupHostConfig := *container.HostConfig
	defer func() {
		if restoreConfig {
			container.Lock()
			container.HostConfig = &backupHostConfig
			container.ToDisk()
			container.Unlock()
		}
	}()

	if container.RemovalInProgress || container.Dead {
		return errCannotUpdate(container.ID, fmt.Errorf("Container is marked for removal and cannot be \"update\"."))
	}

	if container.IsRunning() && hostConfig.KernelMemory != 0 {
		return errCannotUpdate(container.ID, fmt.Errorf("Can not update kernel memory to a running container, please stop it first."))
	}

	if err := container.UpdateContainer(hostConfig, labels); err != nil {
		restoreConfig = true
		return errCannotUpdate(container.ID, err)
	}

	// if Restart Policy changed, we need to update container monitor
	container.UpdateMonitor(hostConfig.RestartPolicy)

	// if container is restarting, wait 5 seconds until it's running
	if container.IsRestarting() {
		container.WaitRunning(5 * time.Second)
	}

	// If container is not running, update hostConfig struct is enough,
	// resources will be updated when the container is started again.
	// If container is running (including paused), we need to update configs
	// to the real world.
	if container.IsRunning() && !container.IsRestarting() {
		if err := daemon.execDriver.Update(container.Command); err != nil {
			restoreConfig = true
			return errCannotUpdate(container.ID, err)
		}
	}

	daemon.LogContainerEvent(container, "update")

	return nil
}

func errCannotUpdate(containerID string, err error) error {
	return fmt.Errorf("Cannot update container %s: %v", containerID, err)
}
