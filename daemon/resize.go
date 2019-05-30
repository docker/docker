package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"fmt"
	"time"

	libcontainerdtypes "github.com/docker/docker/libcontainerd/types"
)

// ContainerResize changes the size of the TTY of the process running
// in the container with the given name to the given height and width.
func (daemon *Daemon) ContainerResize(name string, height, width int) error {
	container, err := daemon.GetContainer(name)
	if err != nil {
		return err
	}

	if !container.IsRunning() {
		return errNotRunning(container.ID)
	}

	if err = daemon.containerd.ResizeTerminal(context.Background(), container.ID, libcontainerdtypes.InitProcessName, width, height); err == nil {
		attributes := map[string]string{
			"height": fmt.Sprintf("%d", height),
			"width":  fmt.Sprintf("%d", width),
		}
		daemon.LogContainerEventWithAttributes(container, "resize", attributes)
	}
	return err
}

// ContainerExecResize changes the size of the TTY of the process
// running in the exec with the given name to the given height and
// width.
func (daemon *Daemon) ContainerExecResize(name string, height, width int) error {
	ec, err := daemon.getExecConfig(name)
	if err != nil {
		return err
	}
	// TODO: the timeout is hardcoded here, it would be more flexible to make it
	// a parameter in resize request context, which would need API changes.
	timeout := 10 * time.Second
	select {
	case <-ec.Started:
		return daemon.containerd.ResizeTerminal(context.Background(), ec.ContainerID, ec.ID, width, height)
	case <-time.After(timeout):
		return fmt.Errorf("timeout waiting for exec session ready")
	}
}
