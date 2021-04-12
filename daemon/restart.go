package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"fmt"

	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/container"
	"github.com/sirupsen/logrus"
)

// ContainerRestart stops and starts a container. It attempts to
// gracefully stop the container within the given timeout, forcefully
// stopping it if the timeout is exceeded. If given a negative
// timeout, ContainerRestart will wait forever until a graceful
// stop. Returns an error if the container cannot be found, or if
// there is an underlying error at any stage of the restart.
func (daemon *Daemon) ContainerRestart(ctx context.Context, name string, seconds *int) error {
	ctr, err := daemon.GetContainer(name)
	if err != nil {
		return err
	}
	if seconds == nil {
		stopTimeout := ctr.StopTimeout()
		seconds = &stopTimeout
	}
	if err := daemon.containerRestart(ctx, ctr, *seconds); err != nil {
		return fmt.Errorf("Cannot restart container %s: %v", name, err)
	}
	return nil

}

// containerRestart attempts to gracefully stop and then start the
// container. When stopping, wait for the given duration in seconds to
// gracefully stop, before forcefully terminating the container. If
// given a negative duration, wait forever for a graceful stop.
func (daemon *Daemon) containerRestart(ctx context.Context, container *container.Container, seconds int) error {

	// Determine isolation. If not specified in the hostconfig, use daemon default.
	actualIsolation := container.HostConfig.Isolation
	if containertypes.Isolation.IsDefault(actualIsolation) {
		actualIsolation = daemon.defaultIsolation
	}

	// Avoid unnecessarily unmounting and then directly mounting
	// the container when the container stops and then starts
	// again. We do not do this for Hyper-V isolated containers
	// (implying also on Windows) as the HCS must have exclusive
	// access to mount the containers filesystem inside the utility
	// VM.
	if !containertypes.Isolation.IsHyperV(actualIsolation) {
		if err := daemon.Mount(container); err == nil {
			defer daemon.Unmount(container)
		}
	}

	if container.IsRunning() {
		// set AutoRemove flag to false before stop so the container won't be
		// removed during restart process
		autoRemove := container.HostConfig.AutoRemove

		container.HostConfig.AutoRemove = false
		err := daemon.containerStop(ctx, container, seconds)
		// restore AutoRemove irrespective of whether the stop worked or not
		container.HostConfig.AutoRemove = autoRemove
		// containerStop will write HostConfig to disk, we shall restore AutoRemove
		// in disk too
		if toDiskErr := daemon.checkpointAndSave(container); toDiskErr != nil {
			logrus.Errorf("Write container to disk error: %v", toDiskErr)
		}

		if err != nil {
			return err
		}
	}

	if err := daemon.containerStart(ctx, container, "", "", true); err != nil {
		return err
	}

	daemon.LogContainerEvent(container, "restart")
	return nil
}
