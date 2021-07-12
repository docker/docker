package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"runtime"
	"time"

	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/container"
	"github.com/docker/docker/errdefs"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// ContainerStart starts a container.
func (daemon *Daemon) ContainerStart(name string, hostConfig *containertypes.HostConfig, checkpoint string, checkpointDir string) error {
	if checkpoint != "" && !daemon.HasExperimental() {
		return errdefs.InvalidParameter(errors.New("checkpoint is only supported in experimental mode"))
	}

	ctr, err := daemon.GetContainer(name)
	if err != nil {
		return err
	}

	validateState := func() error {
		ctr.Lock()
		defer ctr.Unlock()

		if ctr.Paused {
			return errdefs.Conflict(errors.New("cannot start a paused container, try unpause instead"))
		}

		if ctr.Running {
			return containerNotModifiedError{running: true}
		}

		if ctr.RemovalInProgress || ctr.Dead {
			return errdefs.Conflict(errors.New("container is marked for removal and cannot be started"))
		}
		return nil
	}

	if err := validateState(); err != nil {
		return err
	}

	// Windows does not have the backwards compatibility issue here.
	if runtime.GOOS != "windows" {
		// This is kept for backward compatibility - hostconfig should be passed when
		// creating a container, not during start.
		if hostConfig != nil {
			logrus.Warn("DEPRECATED: Setting host configuration options when the container starts is deprecated and has been removed in Docker 1.12")
			oldNetworkMode := ctr.HostConfig.NetworkMode
			if err := daemon.setSecurityOptions(ctr, hostConfig); err != nil {
				return errdefs.InvalidParameter(err)
			}
			if err := daemon.mergeAndVerifyLogConfig(&hostConfig.LogConfig); err != nil {
				return errdefs.InvalidParameter(err)
			}
			if err := daemon.setHostConfig(ctr, hostConfig); err != nil {
				return errdefs.InvalidParameter(err)
			}
			newNetworkMode := ctr.HostConfig.NetworkMode
			if string(oldNetworkMode) != string(newNetworkMode) {
				// if user has change the network mode on starting, clean up the
				// old networks. It is a deprecated feature and has been removed in Docker 1.12
				ctr.NetworkSettings.Networks = nil
				if err := ctr.CheckpointTo(daemon.containersReplica); err != nil {
					return errdefs.System(err)
				}
			}
			ctr.InitDNSHostConfig()
		}
	} else {
		if hostConfig != nil {
			return errdefs.InvalidParameter(errors.New("Supplying a hostconfig on start is not supported. It should be supplied on create"))
		}
	}

	// check if hostConfig is in line with the current system settings.
	// It may happen cgroups are umounted or the like.
	if _, err = daemon.verifyContainerSettings(ctr.HostConfig, nil, false); err != nil {
		return errdefs.InvalidParameter(err)
	}
	// Adapt for old containers in case we have updates in this function and
	// old containers never have chance to call the new function in create stage.
	if hostConfig != nil {
		if err := daemon.adaptContainerSettings(ctr.HostConfig, false); err != nil {
			return errdefs.InvalidParameter(err)
		}
	}
	return daemon.containerStart(ctr, checkpoint, checkpointDir, true)
}

// containerStart prepares the container to run by setting up everything the
// container needs, such as storage and networking, as well as links
// between containers. The container is left waiting for a signal to
// begin running.
func (daemon *Daemon) containerStart(container *container.Container, checkpoint string, checkpointDir string, resetRestartManager bool) (err error) {
	start := time.Now()
	container.Lock()
	defer container.Unlock()

	if resetRestartManager && container.Running { // skip this check if already in restarting step and resetRestartManager==false
		return nil
	}

	if container.RemovalInProgress || container.Dead {
		return errdefs.Conflict(errors.New("container is marked for removal and cannot be started"))
	}

	if checkpointDir != "" {
		// TODO(mlaventure): how would we support that?
		return errdefs.Forbidden(errors.New("custom checkpointdir is not supported"))
	}

	// if we encounter an error during start we need to ensure that any other
	// setup has been cleaned up properly
	defer func() {
		if err != nil {
			container.SetError(err)
			// if no one else has set it, make sure we don't leave it at zero
			if container.ExitCode() == 0 {
				container.SetExitCode(128)
			}
			if err := container.CheckpointTo(daemon.containersReplica); err != nil {
				logrus.Errorf("%s: failed saving state on start failure: %v", container.ID, err)
			}
			container.Reset(false)

			daemon.Cleanup(container)
			// if containers AutoRemove flag is set, remove it after clean up
			if container.HostConfig.AutoRemove {
				container.Unlock()
				if err := daemon.ContainerRm(container.ID, &types.ContainerRmConfig{ForceRemove: true, RemoveVolume: true}); err != nil {
					logrus.Errorf("can't remove container %s: %v", container.ID, err)
				}
				container.Lock()
			}
		}
	}()

	if err := daemon.conditionalMountOnStart(container); err != nil {
		return err
	}

	if err := daemon.initializeNetworking(container); err != nil {
		return err
	}

	spec, err := daemon.createSpec(container)
	if err != nil {
		return errdefs.System(err)
	}

	if resetRestartManager {
		container.ResetRestartManager(true)
		container.HasBeenManuallyStopped = false
	}

	if err := daemon.saveAppArmorConfig(container); err != nil {
		return err
	}

	if checkpoint != "" {
		checkpointDir, err = getCheckpointDir(checkpointDir, checkpoint, container.Name, container.ID, container.CheckpointDir(), false)
		if err != nil {
			return err
		}
	}

	shim, createOptions, err := daemon.getLibcontainerdCreateOptions(container)
	if err != nil {
		return err
	}

	ctx := context.TODO()

	err = daemon.containerd.Create(ctx, container.ID, spec, shim, createOptions)
	if err != nil {
		if errdefs.IsConflict(err) {
			logrus.WithError(err).WithField("container", container.ID).Error("Container not cleaned up from containerd from previous run")
			// best effort to clean up old container object
			daemon.containerd.DeleteTask(ctx, container.ID)
			if err := daemon.containerd.Delete(ctx, container.ID); err != nil && !errdefs.IsNotFound(err) {
				logrus.WithError(err).WithField("container", container.ID).Error("Error cleaning up stale containerd container object")
			}
			err = daemon.containerd.Create(ctx, container.ID, spec, shim, createOptions)
		}
		if err != nil {
			return translateContainerdStartErr(container.Path, container.SetExitCode, err)
		}
	}

	// TODO(mlaventure): we need to specify checkpoint options here
	pid, err := daemon.containerd.Start(context.Background(), container.ID, checkpointDir,
		container.StreamConfig.Stdin() != nil || container.Config.Tty,
		container.InitializeStdio)
	if err != nil {
		if err := daemon.containerd.Delete(context.Background(), container.ID); err != nil {
			logrus.WithError(err).WithField("container", container.ID).
				Error("failed to delete failed start container")
		}
		return translateContainerdStartErr(container.Path, container.SetExitCode, err)
	}

	container.SetRunning(pid, true)
	container.HasBeenStartedBefore = true
	daemon.setStateCounter(container)

	daemon.initHealthMonitor(container)

	if err := container.CheckpointTo(daemon.containersReplica); err != nil {
		logrus.WithError(err).WithField("container", container.ID).
			Errorf("failed to store container")
	}

	daemon.LogContainerEvent(container, "start")
	containerActions.WithValues("start").UpdateSince(start)

	return nil
}

// Cleanup releases any network resources allocated to the container along with any rules
// around how containers are linked together.  It also unmounts the container's root filesystem.
func (daemon *Daemon) Cleanup(container *container.Container) {
	daemon.releaseNetwork(container)

	if err := container.UnmountIpcMount(); err != nil {
		logrus.Warnf("%s cleanup: failed to unmount IPC: %s", container.ID, err)
	}

	if err := daemon.conditionalUnmountOnCleanup(container); err != nil {
		// FIXME: remove once reference counting for graphdrivers has been refactored
		// Ensure that all the mounts are gone
		if mountid, err := daemon.imageService.GetLayerMountID(container.ID); err == nil {
			daemon.cleanupMountsByID(mountid)
		}
	}

	if err := container.UnmountSecrets(); err != nil {
		logrus.Warnf("%s cleanup: failed to unmount secrets: %s", container.ID, err)
	}

	if err := recursiveUnmount(container.Root); err != nil {
		logrus.WithError(err).WithField("container", container.ID).Warn("Error while cleaning up container resource mounts.")
	}

	for _, eConfig := range container.ExecCommands.Commands() {
		daemon.unregisterExecCommand(container, eConfig)
	}

	if container.BaseFS != nil && container.BaseFS.Path() != "" {
		if err := container.UnmountVolumes(daemon.LogVolumeEvent); err != nil {
			logrus.Warnf("%s cleanup: Failed to umount volumes: %v", container.ID, err)
		}
	}

	container.CancelAttachContext()

	if err := daemon.containerd.Delete(context.Background(), container.ID); err != nil {
		logrus.Errorf("%s cleanup: failed to delete container from containerd: %v", container.ID, err)
	}
}
