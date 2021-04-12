package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"time"

	containerpkg "github.com/docker/docker/container"
	"github.com/docker/docker/errdefs"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// ContainerStop looks for the given container and stops it.
// In case the container fails to stop gracefully within a time duration
// specified by the timeout argument, in seconds, it is forcefully
// terminated (killed).
//
// If the timeout is nil, the container's StopTimeout value is used, if set,
// otherwise the engine default. A negative timeout value can be specified,
// meaning no timeout, i.e. no forceful termination is performed.
func (daemon *Daemon) ContainerStop(ctx context.Context, name string, timeout *int) error {
	container, err := daemon.GetContainer(name)
	if err != nil {
		return err
	}
	if !container.IsRunning() {
		return containerNotModifiedError{running: false}
	}
	if timeout == nil {
		stopTimeout := container.StopTimeout()
		timeout = &stopTimeout
	}
	if err := daemon.containerStop(ctx, container, *timeout); err != nil {
		return errdefs.System(errors.Wrapf(err, "cannot stop container: %s", name))
	}
	return nil
}

// containerStop sends a stop signal, waits, sends a kill signal.
func (daemon *Daemon) containerStop(ctx context.Context, container *containerpkg.Container, seconds int) error {
	if !container.IsRunning() {
		return nil
	}

	stopSignal := container.StopSignal()
	// 1. Send a stop signal
	if err := daemon.killPossiblyDeadProcess(ctx, container, stopSignal); err != nil {
		// While normally we might "return err" here we're not going to
		// because if we can't stop the container by this point then
		// it's probably because it's already stopped. Meaning, between
		// the time of the IsRunning() call above and now it stopped.
		// Also, since the err return will be environment specific we can't
		// look for any particular (common) error that would indicate
		// that the process is already dead vs something else going wrong.
		// So, instead we'll give it up to 2 more seconds to complete and if
		// by that time the container is still running, then the error
		// we got is probably valid and so we force kill it.
		ctxT, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		if status := <-container.Wait(ctxT, containerpkg.WaitConditionNotRunning); status.Err() != nil {
			logrus.Infof("Container failed to stop after sending signal %d to the process, force killing", stopSignal)
			if err := daemon.killPossiblyDeadProcess(ctx, container, 9); err != nil {
				return err
			}
		}
	}

	// 2. Wait for the process to exit on its own
	ctxT := context.Background()
	if seconds >= 0 {
		var cancel context.CancelFunc
		ctxT, cancel = context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
		defer cancel()
	}

	if status := <-container.Wait(ctxT, containerpkg.WaitConditionNotRunning); status.Err() != nil {
		logrus.Infof("Container %v failed to exit within %d seconds of signal %d - using the force", container.ID, seconds, stopSignal)
		// 3. If it doesn't, then send SIGKILL
		if err := daemon.Kill(ctx, container); err != nil {
			// Wait without a timeout, ignore result.
			ctxT, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
			<-container.Wait(ctxT, containerpkg.WaitConditionNotRunning)
			cancel()
			logrus.Warn(err) // Don't return error because we only care that container is stopped, not what function stopped it
		}
	}

	daemon.LogContainerEvent(container, "stop")
	return nil
}
