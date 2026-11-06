package daemon

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"syscall"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	containerpkg "github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/errdefs"
	"github.com/moby/sys/signal"
	"github.com/pkg/errors"
)

type errNoSuchProcess struct {
	pid    int
	signal syscall.Signal
}

func (e errNoSuchProcess) Error() string {
	return fmt.Sprintf("cannot kill process (pid=%d) with signal %d: no such process", e.pid, e.signal)
}

func (errNoSuchProcess) NotFound() {}

// ContainerKill sends signal to the container
// If no signal is given, then Kill with SIGKILL and wait
// for the container to exit.
// If a signal is given, then just send it to the container and return.
func (daemon *Daemon) ContainerKill(ctx context.Context, name, stopSignal string) error {
	var (
		err error
		sig = syscall.SIGKILL
	)
	if stopSignal != "" {
		sig, err = signal.ParseSignal(stopSignal)
		if err != nil {
			return errdefs.InvalidParameter(err)
		}
		if !signal.ValidSignalForPlatform(sig) {
			return errdefs.InvalidParameter(errors.Errorf("the %s daemon does not support signal %d", runtime.GOOS, sig))
		}
	}
	container, err := daemon.GetContainer(name)
	if err != nil {
		return err
	}
	if sig == syscall.SIGKILL {
		// perform regular Kill (SIGKILL + wait())
		return daemon.kill(ctx, container)
	}
	return daemon.killWithSignal(ctx, container, sig)
}

// killWithSignal sends the container the given signal. This wrapper for the
// host specific kill command prepares the container before attempting
// to send the signal. An error is returned if the container is paused
// or not running, or if there is a problem returned from the
// underlying kill command.
func (daemon *Daemon) killWithSignal(ctx context.Context, container *containerpkg.Container, stopSignal syscall.Signal) error {
	ctx = context.WithoutCancel(ctx)
	log.G(ctx).WithFields(log.Fields{
		"signal":    int(stopSignal),
		"container": container.ID,
	}).Debugf("sending signal %[1]d (%[1]s) to container", stopSignal)
	container.Lock()
	defer container.Unlock()

	task, err := container.GetRunningTask()
	if err != nil {
		return err
	}

	var isStopSignal bool
	if container.Config.StopSignal != "" && stopSignal != syscall.SIGKILL {
		containerStopSignal, err := signal.ParseSignal(container.Config.StopSignal)
		if err != nil {
			return err
		}
		isStopSignal = containerStopSignal == stopSignal
	} else {
		// Without a configured StopSignal, the container is stopped with the
		// default one, so anything else (e.g. SIGHUP for a config reload) is
		// not a stop either.
		isStopSignal = stopSignal == syscall.SIGKILL || stopSignal == container.StopSignal()
	}

	var unpause bool
	if isStopSignal {
		container.ExitOnNext()
		unpause = container.State.Paused
	}

	if isStopSignal && !daemon.IsShuttingDown() {
		container.HasBeenManuallyStopped = true
		if err := container.CheckpointTo(ctx, daemon.containersReplica); err != nil {
			log.G(ctx).WithFields(log.Fields{
				"error":     err,
				"container": container.ID,
			}).Warn("error checkpointing container state")
		}
	}

	// if the container is currently restarting we do not need to send the signal
	// to the process. Telling the monitor that it should exit on its next event
	// loop is enough
	if container.State.Restarting {
		return nil
	}

	if err := task.Kill(ctx, stopSignal); err != nil {
		if cerrdefs.IsNotFound(err) {
			unpause = false
			log.G(ctx).WithFields(log.Fields{
				"error":     err,
				"container": container.ID,
				"action":    "kill",
			}).Debug("container kill failed because of 'container not found' or 'no such process'")
			go func() {
				// We need to clean up this container but it is possible there is a case where we hit here before the exit event is processed
				// but after it was fired off.
				// So let's wait the container's stop timeout amount of time to see if the event is eventually processed.
				// Doing this has the side effect that if no event was ever going to come we are waiting a longer period of time unnecessarily.
				// But this prevents race conditions in processing the container.
				stopTimeout := daemon.config().DefaultStopTimeout
				if container.Config.StopTimeout != nil {
					stopTimeout = *container.Config.StopTimeout
				}
				var waitCtx context.Context
				var cancel context.CancelFunc
				if stopTimeout >= 0 {
					waitCtx, cancel = context.WithTimeout(ctx, time.Duration(stopTimeout)*time.Second)
				} else {
					waitCtx, cancel = context.WithCancel(ctx)
				}

				defer cancel()
				s := <-container.State.Wait(waitCtx, containertypes.WaitConditionNotRunning)
				if s.Err() != nil {
					if err := daemon.handleContainerExit(container, nil); err != nil {
						log.G(waitCtx).WithFields(log.Fields{
							"error":     err,
							"container": container.ID,
							"action":    "kill",
						}).Warn("error while handling container exit")
					}
				}
			}()
		} else {
			return errors.Wrapf(err, "Cannot kill container %s", container.ID)
		}
	}

	if unpause {
		// above kill signal will be sent once resume is finished
		if err := task.Resume(ctx); err != nil {
			log.G(ctx).WithFields(log.Fields{
				"error":     err,
				"container": container.ID,
				"action":    "kill",
			}).Warn("cannot unpause container")
		}
	}

	daemon.LogContainerEventWithAttributes(container, events.ActionKill, map[string]string{
		"signal": strconv.Itoa(int(stopSignal)),
	})
	return nil
}

func (daemon *Daemon) kill(ctx context.Context, container *containerpkg.Container) error {
	ctx = context.WithoutCancel(ctx)
	if !container.State.IsRunning() {
		return errNotRunning(container.ID)
	}

	// 1. Send SIGKILL
	if err := daemon.killPossiblyDeadProcess(ctx, container, syscall.SIGKILL); err != nil {
		// kill failed, check if process is no longer running.
		if errors.As(err, &errNoSuchProcess{}) {
			return nil
		}
	}

	waitTimeout := 10 * time.Second
	if runtime.GOOS == "windows" {
		waitTimeout = 75 * time.Second // runhcs can be sloooooow.
	}

	waitCtx, cancel := context.WithTimeout(ctx, waitTimeout)
	defer cancel()

	status := <-container.State.Wait(waitCtx, containertypes.WaitConditionNotRunning)
	if status.Err() == nil {
		return nil
	}

	log.G(waitCtx).WithFields(log.Fields{"error": status.Err(), "container": container.ID}).Warnf("Container failed to exit within %v of kill - trying direct SIGKILL", waitTimeout)

	if err := killProcessDirectly(container); err != nil {
		if errors.As(err, &errNoSuchProcess{}) {
			return nil
		}
		return err
	}

	// wait for container to exit one last time, if it doesn't then kill didn't work, so return error
	finalWaitCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	if status := <-container.State.Wait(finalWaitCtx, containertypes.WaitConditionNotRunning); status.Err() != nil {
		return errors.New("tried to kill container, but did not receive an exit event")
	}
	return nil
}

// killPossiblyDeadProcess is a wrapper around killSig() suppressing "no such process" error.
func (daemon *Daemon) killPossiblyDeadProcess(ctx context.Context, container *containerpkg.Container, sig syscall.Signal) error {
	ctx = context.WithoutCancel(ctx)
	err := daemon.killWithSignal(ctx, container, sig)
	if cerrdefs.IsNotFound(err) {
		err = errNoSuchProcess{container.State.GetPID(), sig}
		log.G(ctx).Debug(err)
		return err
	}
	return err
}
