package daemon

import (
	"fmt"
	"os"
	"path"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/volume/local"
)

type ContainerRmConfig struct {
	ForceRemove, RemoveVolume, RemoveLink bool
}

func (daemon *Daemon) ContainerRm(name string, config *ContainerRmConfig) error {
	container, err := daemon.Get(name)
	if err != nil {
		return err
	}

	if config.RemoveLink {
		name, err := GetFullContainerName(name)
		if err != nil {
			return err
		}
		parent, n := path.Split(name)
		if parent == "/" {
			return fmt.Errorf("Conflict, cannot remove the default name of the container")
		}
		pe := daemon.ContainerGraph().Get(parent)
		if pe == nil {
			return fmt.Errorf("Cannot get parent %s for name %s", parent, name)
		}
		parentContainer, _ := daemon.Get(pe.ID())

		if err := daemon.ContainerGraph().Delete(name); err != nil {
			return err
		}

		if parentContainer != nil {
			parentContainer.DisableLink(n)
		}

		return nil
	}

	if err := daemon.rm(container, config.ForceRemove); err != nil {
		return fmt.Errorf("Cannot destroy container %s: %v", name, err)
	}

	container.LogEvent("destroy")

	for _, m := range container.MountPoints {
		if m.Volume == nil {
			continue
		}
		if _, ok := m.Volume.(*local.Volume); !ok || config.RemoveVolume {
			if err := removeVolume(m.Volume); err != nil {
				return err
			}
		}
	}
	return nil
}

// Destroy unregisters a container from the daemon and cleanly removes its contents from the filesystem.
func (daemon *Daemon) rm(container *Container, forceRemove bool) (err error) {
	// stop collection of stats for the container regardless
	// if stats are currently getting collected.
	daemon.statsCollector.stopCollection(container)

	if container.IsRunning() {
		if !forceRemove {
			return fmt.Errorf("Conflict, You cannot remove a running container. Stop the container before attempting removal or use -f")
		}
		if err := container.Kill(); err != nil {
			return fmt.Errorf("Could not kill running container, cannot remove - %v", err)
		}
	}

	element := daemon.containers.Get(container.ID)
	if element == nil {
		return fmt.Errorf("Container %v not found - maybe it was already destroyed?", container.ID)
	}

	// Container state RemovalInProgress should be used to avoid races.
	if err = container.SetRemovalInProgress(); err != nil {
		return fmt.Errorf("Failed to set container state to RemovalInProgress: %s", err)
	}

	defer container.ResetRemovalInProgress()

	if err = container.Stop(3); err != nil {
		return err
	}

	// Mark container dead. We don't want anybody to be restarting it.
	container.SetDead()

	// Save container state to disk. So that if error happens before
	// container meta file got removed from disk, then a restart of
	// docker should not make a dead container alive.
	container.ToDisk()

	// If force removal is required, delete container from various
	// indexes even if removal failed.
	defer func() {
		if err != nil && forceRemove {
			daemon.idIndex.Delete(container.ID)
			daemon.containers.Delete(container.ID)
			os.RemoveAll(container.root)
		}
	}()

	if _, err := daemon.containerGraph.Purge(container.ID); err != nil {
		logrus.Debugf("Unable to remove container from link graph: %s", err)
	}

	if err = daemon.driver.Remove(container.ID); err != nil {
		return fmt.Errorf("Driver %s failed to remove root filesystem %s: %s", daemon.driver, container.ID, err)
	}

	initID := fmt.Sprintf("%s-init", container.ID)
	if err := daemon.driver.Remove(initID); err != nil {
		return fmt.Errorf("Driver %s failed to remove init filesystem %s: %s", daemon.driver, initID, err)
	}

	if err = os.RemoveAll(container.root); err != nil {
		return fmt.Errorf("Unable to remove filesystem for %v: %v", container.ID, err)
	}

	if err = daemon.execDriver.Clean(container.ID); err != nil {
		return fmt.Errorf("Unable to remove execdriver data for %s: %s", container.ID, err)
	}

	selinuxFreeLxcContexts(container.ProcessLabel)
	daemon.idIndex.Delete(container.ID)
	daemon.containers.Delete(container.ID)

	return nil
}
