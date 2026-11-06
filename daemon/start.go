package daemon

import (
	"context"
	"math"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/log"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/v2/daemon/container"
	mobyc8dstore "github.com/moby/moby/v2/daemon/containerd"
	"github.com/moby/moby/v2/daemon/internal/libcontainerd"
	"github.com/moby/moby/v2/daemon/internal/metrics"
	"github.com/moby/moby/v2/daemon/internal/otelutil"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/errdefs"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// validateState verifies if the container is in a non-conflicting state.
func validateState(ctr *container.Container) error {
	ctr.Lock()
	defer ctr.Unlock()

	// Intentionally checking paused first, because a container can be
	// BOTH running AND paused. To start a paused (but running) container,
	// it must be thawed ("un-paused").
	if ctr.State.Paused {
		return errdefs.Conflict(errors.New("cannot start a paused container, try unpause instead"))
	} else if ctr.State.Running {
		// This is not an actual error, but produces a 304 "not modified"
		// when returned through the API to indicates the container is
		// already in the desired state. It's implemented as an error
		// to make the code calling this function terminate early (as
		// no further processing is needed).
		return errdefs.NotModified(errors.New("container is already running"))
	}
	if ctr.State.RemovalInProgress || ctr.State.Dead {
		return errdefs.Conflict(errors.New("container is marked for removal and cannot be started"))
	}
	return nil
}

// ContainerStart starts a container.
func (daemon *Daemon) ContainerStart(ctx context.Context, name string, checkpoint string, checkpointDir string) error {
	if checkpoint != "" {
		if err := validateCheckpointID(checkpoint); err != nil {
			return err
		}
	}

	daemonCfg := daemon.config()
	if checkpoint != "" && !daemonCfg.Experimental {
		return errdefs.InvalidParameter(errors.New("checkpoint is only supported in experimental mode"))
	}

	ctr, err := daemon.GetContainer(name)
	if err != nil {
		return err
	}
	if err := validateState(ctr); err != nil {
		return err
	}

	// check if hostConfig is in line with the current system settings.
	// It may happen cgroups are unmounted or the like.
	if _, err = daemon.verifyContainerSettings(daemonCfg, ctr.HostConfig, nil, false); err != nil {
		return errdefs.InvalidParameter(err)
	}

	return daemon.containerStart(ctx, daemonCfg, ctr, checkpoint, checkpointDir, true)
}

// containerStart prepares the container to run by setting up everything the
// container needs, such as storage and networking, as well as links
// between containers. The container is left waiting for a signal to
// begin running.
func (daemon *Daemon) containerStart(ctx context.Context, daemonCfg *configStore, container *container.Container, checkpoint string, checkpointDir string, resetRestartManager bool) (retErr error) {
	ctx, span := otel.Tracer("").Start(ctx, "daemon.containerStart", trace.WithAttributes(append(
		labelsAsOTelAttributes(container.Config.Labels),
		attribute.String("container.ID", container.ID),
		attribute.String("container.Name", container.Name),
	)...))
	defer func() {
		otelutil.RecordStatus(span, retErr)
		span.End()
	}()

	if checkpointDir != "" {
		// TODO(mlaventure): how would we support that?
		return errdefs.Forbidden(errors.New("custom checkpointdir is not supported"))
	}

	start := time.Now()
	container.Lock()
	defer container.Unlock()

	if resetRestartManager && container.State.Running { // skip this check if already in restarting step and resetRestartManager==false
		return nil
	}

	if container.State.RemovalInProgress || container.State.Dead {
		return errdefs.Conflict(errors.New("container is marked for removal and cannot be started"))
	}

	// if we encounter an error during start we need to ensure that any other
	// setup has been cleaned up properly
	defer func() {
		if retErr != nil {
			container.State.SetError(retErr)
			// if no one else has set it, make sure we don't leave it at zero
			if container.State.ExitCode == 0 {
				container.State.SetExitCode(exitUnknown)
			}
			if err := container.CheckpointTo(context.WithoutCancel(ctx), daemon.containersReplica); err != nil {
				log.G(ctx).Errorf("%s: failed saving state on start failure: %v", container.ID, err)
			}
			container.Reset()

			daemon.Cleanup(context.WithoutCancel(ctx), container)
			// if containers AutoRemove flag is set, remove it after clean up
			if container.HostConfig.AutoRemove {
				container.Unlock()
				if err := daemon.containerRm(&daemonCfg.Config, container.ID, &backend.ContainerRmConfig{ForceRemove: true, RemoveVolume: true}); err != nil {
					log.G(ctx).Errorf("can't remove container %s: %v", container.ID, err)
				}
				container.Lock()
			}
		}
	}()

	if err := daemon.conditionalMountOnStart(container); err != nil {
		return err
	}

	newSandbox, err := daemon.initializeNetworking(ctx, &daemonCfg.Config, container)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil && newSandbox != nil {
			if err := newSandbox.Delete(ctx); err != nil {
				log.G(ctx).WithFields(log.Fields{
					"error":     err,
					"container": container.ID,
				}).Warn("After failure in networking initialisation, failed to remove sandbox")
			}
		}
	}()

	mnts, err := daemon.setupContainerDirs(container)
	if err != nil {
		return err
	}

	m, cleanup, err := daemon.setupMounts(ctx, container)
	if err != nil {
		return err
	}
	mnts = append(mnts, m...)
	defer cleanup(context.WithoutCancel(ctx))

	spec, err := daemon.createSpec(ctx, daemonCfg, container, mnts)
	if err != nil {
		// Any error that occurs while creating the spec, even if it's the
		// result of an invalid container config, must be considered a System
		// error (internal server error), as it's not an error with the request
		// to start the container.
		//
		// Invalid configuration in the config itself must be validated when
		// creating the container (creating its config), but some errors are
		// dependent on the current state, for example when starting a container
		// that shares a namespace with another container, and that container
		// is not running (or missing).
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

	shim, createOptions, err := daemon.getLibcontainerdCreateOptions(daemonCfg, container)
	if err != nil {
		return err
	}

	// Remove the previous run before recording a new sequence.
	if err := libcontainerd.DeleteContainer(ctx, daemon.containerd, container.C8dContainerID()); err != nil {
		return err
	}
	if container.State.RunID == math.MaxUint64 {
		container.State.RunID = 1
	} else {
		container.State.RunID++
	}
	c8dContainerID := container.C8dContainerID()
	// Save the sequence first so the next start can clean up after a crash.
	if err := container.CheckpointTo(context.WithoutCancel(ctx), daemon.containersReplica); err != nil {
		return errors.Wrap(err, "persisting container run sequence")
	}

	ctr, err := libcontainerd.ReplaceContainer(ctx, daemon.containerd, c8dContainerID, spec, shim, createOptions, func(ctx context.Context, client *containerd.Client, c *containers.Container) error {
		// Only set the image if we are using containerd for image storage.
		// This is for metadata purposes only.
		// Other lower-level components may make use of this information.
		is, ok := daemon.imageService.(*mobyc8dstore.ImageService)
		if !ok {
			return nil
		}
		img, err := is.ResolveImage(ctx, container.Config.Image)
		if err != nil {
			log.G(ctx).WithError(err).WithField("container", container.ID).Warn("Failed to resolve containerd image reference")
			return nil
		}
		c.Image = img.Name
		return nil
	})
	if err != nil {
		return setExitCodeFromError(container.State.SetExitCode, err)
	}
	defer func() {
		if retErr != nil {
			if err := ctr.Delete(context.WithoutCancel(ctx)); err != nil {
				log.G(ctx).WithError(err).WithField("container", container.ID).
					Error("failed to delete failed start container")
			}
		}
	}()

	startupTime := time.Now()
	// TODO(mlaventure): we need to specify checkpoint options here
	tsk, err := ctr.NewTask(context.WithoutCancel(ctx), // passing a cancelable ctx caused integration tests to be stuck in the cleanup phase
		checkpointDir, container.StreamConfig.Stdin() != nil || container.Config.Tty,
		container.InitializeStdio)
	if err != nil {
		return setExitCodeFromError(container.State.SetExitCode, err)
	}
	defer func() {
		if retErr != nil {
			if err := tsk.ForceDelete(context.WithoutCancel(ctx)); err != nil {
				log.G(ctx).WithError(err).WithField("container", container.ID).
					Error("failed to delete task after fail start")
			}
		}
	}()

	if err := daemon.initializeCreatedTask(ctx, &daemonCfg.Config, tsk, container, spec); err != nil {
		return err
	}

	if err := tsk.Start(context.WithoutCancel(ctx)); err != nil { // passing a cancelable ctx caused integration tests to be stuck in the cleanup phase
		return setExitCodeFromError(container.State.SetExitCode, err)
	}

	container.HasBeenManuallyRestarted = false
	container.State.SetRunning(ctr, tsk, startupTime)
	container.HasBeenStartedBefore = true
	daemon.setStateCounter(container)

	daemon.initHealthMonitor(container)

	if err := container.CheckpointTo(context.WithoutCancel(ctx), daemon.containersReplica); err != nil {
		log.G(ctx).WithError(err).WithField("container", container.ID).
			Errorf("failed to store container")
	}

	daemon.LogContainerEvent(container, events.ActionStart)
	metrics.ContainerActions.WithValues("start").UpdateSince(start)

	return nil
}

// Cleanup releases any network resources allocated to the container along with any rules
// around how containers are linked together.  It also unmounts the container's root filesystem.
func (daemon *Daemon) Cleanup(ctx context.Context, container *container.Container) {
	// Microsoft HCS containers get in a bad state if host resources are
	// released while the container still exists.
	if ctr, ok := container.State.C8dContainer(); ok {
		if err := ctr.Delete(context.Background()); err != nil {
			log.G(ctx).Errorf("%s cleanup: failed to delete container from containerd: %v", container.ID, err)
		}
	}

	daemon.releaseNetwork(ctx, container)

	if err := container.UnmountIpcMount(); err != nil {
		log.G(ctx).Warnf("%s cleanup: failed to unmount IPC: %s", container.ID, err)
	}

	if err := daemon.conditionalUnmountOnCleanup(container); err != nil {
		// FIXME: remove once reference counting for graphdrivers has been refactored
		// Ensure that all the mounts are gone
		if mountid, err := daemon.imageService.GetLayerMountID(container.ID); err == nil {
			daemon.cleanupMountsByID(mountid)
		}
	}

	if err := container.UnmountSecrets(); err != nil {
		log.G(ctx).Warnf("%s cleanup: failed to unmount secrets: %s", container.ID, err)
	}

	if err := recursiveUnmount(container.Root); err != nil {
		log.G(ctx).WithError(err).WithField("container", container.ID).Warn("Error while cleaning up container resource mounts.")
	}

	for _, eConfig := range container.ExecCommands.Commands() {
		daemon.unregisterExecCommand(container, eConfig)
	}

	if container.BaseFS != "" {
		if err := container.UnmountVolumes(ctx, daemon.LogVolumeEvent); err != nil {
			log.G(ctx).Warnf("%s cleanup: Failed to umount volumes: %v", container.ID, err)
		}
	}

	container.CancelAttachContext()
}
