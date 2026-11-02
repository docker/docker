package daemon

import (
	"context"
	"runtime"
	"strconv"
	"strings"
	"time"

	containerdcli "github.com/containerd/containerd/v2/client"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/container"
	libcontainerdtypes "github.com/moby/moby/v2/daemon/internal/libcontainerd/types"
	"github.com/moby/moby/v2/daemon/internal/metrics"
	"github.com/moby/moby/v2/daemon/internal/restartmanager"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/pkg/errors"
)

func (daemon *Daemon) setStateCounter(c *container.Container) {
	switch c.State.State() {
	case containertypes.StatePaused:
		metrics.StateCtr.Set(c.ID, "paused")
	case containertypes.StateRunning:
		metrics.StateCtr.Set(c.ID, "running")
	default:
		metrics.StateCtr.Set(c.ID, "stopped")
	}
}

func (daemon *Daemon) handleContainerExit(c *container.Container, e *libcontainerdtypes.EventInfo) error {
	var ctrExitStatus container.ExitStatus
	c.Lock()

	// If the latest container error is related to networking setup, don't try
	// to restart the container, and don't change the container state to
	// 'exited'. This happens when, for example, [daemon.allocateNetwork] fails
	// due to published ports being already in use. In that case, we want to
	// keep the container in the 'created' state.
	//
	// c.ErrorMsg is set by [daemon.containerStart], and doesn't preserve the
	// error type (because this field is persisted on disk). So, use string
	// matching instead of usual error comparison methods.
	if strings.Contains(c.State.ErrorMsg, errSetupNetworking) {
		c.Unlock()
		return nil
	}

	// Ignore duplicate exit event that may arrive after the first one.
	// See moby/moby#46212.
	if daemon.shouldIgnoreExitEventWithLock(c, e) {
		c.Unlock()
		return nil
	}

	cfg := daemon.config()

	// Health checks will be automatically restarted if/when the
	// container is started again.
	daemon.stopHealthchecks(c)

	tsk, ok := c.State.Task()
	if ok {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		es, err := tsk.Delete(ctx)
		cancel()
		if err != nil {
			log.G(ctx).WithFields(log.Fields{
				"error":     err,
				"container": c.ID,
			}).Warn("failed to delete container from containerd")
		} else {
			ctrExitStatus = container.ExitStatus{
				ExitCode: int(es.ExitCode()),
				ExitedAt: es.ExitTime(),
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	c.StreamConfig.Wait(ctx)
	cancel()

	c.Reset()

	if e != nil {
		ctrExitStatus.ExitCode = int(e.ExitCode)
		ctrExitStatus.ExitedAt = e.ExitedAt
		ctrExitStatus.Error = e.Error
	}

	daemonShutdown := daemon.IsShuttingDown()
	execDuration := time.Since(c.State.StartedAt)
	restart, wait, err := c.RestartManager().ShouldRestart(uint32(ctrExitStatus.ExitCode), daemonShutdown || c.HasBeenManuallyStopped, execDuration)
	if err != nil {
		// Ignore ErrRestartCanceled errors, which mean the restart-manager
		// was stopped (e.g., during daemon shutdown).
		if !errors.Is(err, restartmanager.ErrRestartCanceled) {
			log.G(ctx).WithFields(log.Fields{
				"error":                  err,
				"container":              c.ID,
				"restartCount":           c.RestartCount,
				"exitCode":               ctrExitStatus.ExitCode,
				"exitedAt":               ctrExitStatus.ExitedAt,
				"daemonShuttingDown":     daemonShutdown,
				"hasBeenManuallyStopped": c.HasBeenManuallyStopped,
				"execDuration":           execDuration,
			}).Warn("ShouldRestart failed: container will not be restarted")
		}
		restart = false
	}

	attributes := map[string]string{
		"exitCode":     strconv.Itoa(ctrExitStatus.ExitCode),
		"execDuration": strconv.Itoa(int(execDuration.Seconds())),
	}
	if restart || (c.HasBeenManuallyRestarted && !daemonShutdown) {
		// The container will be restarted, either by the restart policy or
		// by a manual "container restart" in progress (which doesn't
		// complete if the daemon is shutting down).
		attributes["restarting"] = "true"
	}

	daemon.Cleanup(context.TODO(), c)

	if restart {
		c.RestartCount++
		log.G(ctx).WithFields(log.Fields{
			"container":     c.ID,
			"restartPolicy": c.HostConfig.RestartPolicy,
			"restartCount":  c.RestartCount,
			"exitCode":      ctrExitStatus.ExitCode,
			"exitedAt":      ctrExitStatus.ExitedAt,
			"manualRestart": c.HasBeenManuallyRestarted,
		}).Info("restarting container")
		c.State.SetRestarting(&ctrExitStatus)
	} else {
		c.State.SetStopped(&ctrExitStatus)
		if !c.HasBeenManuallyRestarted {
			defer daemon.autoRemove(&cfg.Config, c)
		}
	}
	defer c.Unlock() // needs to be called before autoRemove

	daemon.setStateCounter(c)
	checkpointErr := c.CheckpointTo(context.TODO(), daemon.containersReplica)

	daemon.LogContainerEventWithAttributes(c, events.ActionDie, attributes)

	if restart {
		go func() {
			waitErr := <-wait
			if waitErr == nil {
				// daemon.netController is initialized when daemon is restoring containers.
				// But containerStart will use daemon.netController segment.
				// So to avoid panic at startup process, here must wait util daemon restore done.
				daemon.waitForStartupDone()

				// Apply the most up-to-date daemon config to the restarted container.
				if err := daemon.containerStart(context.Background(), daemon.config(), c, "", "", false); err != nil {
					// update the error if we fail to start the container, so that the cleanup code
					// below can handle updating the container's status, and auto-remove (if set).
					waitErr = err
					log.G(ctx).Debugf("failed to restart container: %+v", waitErr)
				}
			}
			if waitErr != nil {
				c.Lock()
				// Keep the recorded error, which a failed restart may have replaced.
				c.State.SetStopped(&container.ExitStatus{
					ExitCode: ctrExitStatus.ExitCode,
					ExitedAt: ctrExitStatus.ExitedAt,
				})
				daemon.setStateCounter(c)
				c.CheckpointTo(context.TODO(), daemon.containersReplica)
				c.Unlock()
				defer daemon.autoRemove(&cfg.Config, c)
				if !errors.Is(waitErr, restartmanager.ErrRestartCanceled) {
					log.G(ctx).Errorf("restartmanger wait error: %+v", waitErr)
				}
			}
		}()
	}

	return checkpointErr
}

// ProcessEvent is called by libcontainerd whenever an event occurs
func (daemon *Daemon) ProcessEvent(id string, e libcontainerdtypes.EventType, ei libcontainerdtypes.EventInfo) error {
	c, err := daemon.GetContainer(id)
	if err != nil {
		return errors.Wrapf(err, "could not find container %s", id)
	}

	switch e {
	case libcontainerdtypes.EventOOM:
		// StateOOM is Linux specific and should never be hit on Windows
		if runtime.GOOS == "windows" {
			return errors.New("received StateOOM from libcontainerd on Windows. This should never happen")
		}

		c.Lock()
		defer c.Unlock()
		c.State.OOMKilled = true
		daemon.updateHealthMonitor(c)
		if err := c.CheckpointTo(context.TODO(), daemon.containersReplica); err != nil {
			return err
		}

		daemon.LogContainerEvent(c, events.ActionOOM)
		return nil
	case libcontainerdtypes.EventExit:
		if ei.ProcessID == ei.ContainerID {
			return daemon.handleContainerExit(c, &ei)
		}

		exitCode := 127
		if execConfig := c.ExecCommands.Get(ei.ProcessID); execConfig != nil {
			ec := int(ei.ExitCode)
			execConfig.Lock()
			defer execConfig.Unlock()

			// Remove the exec command from the container's store only and not the
			// daemon's store so that the exec command can be inspected. Remove it
			// before mutating execConfig to maintain the invariant that
			// c.ExecCommands only contains execs that have not exited.
			c.ExecCommands.Delete(execConfig.ID)

			execConfig.ExitCode = &ec
			execConfig.Running = false

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			execConfig.StreamConfig.Wait(ctx)
			cancel()

			if err := execConfig.CloseStreams(); err != nil {
				log.G(ctx).Errorf("failed to cleanup exec %s streams: %s", c.ID, err)
			}

			exitCode = ec

			// If the exec failed at start in such a way that containerd
			// publishes an exit event for it, we will race processing the event
			// with daemon.ContainerExecStart() removing the exec from
			// c.ExecCommands. If we win the race, we will find that there is no
			// process to clean up. (And ContainerExecStart will clobber the
			// exit code we set.) Prevent a nil-dereference panic in that
			// situation to restore the status quo where this is merely a
			// logical race condition.
			if execConfig.Process != nil {
				go func() {
					if _, err := execConfig.Process.Delete(context.Background()); err != nil {
						log.G(ctx).WithFields(log.Fields{
							"error":     err,
							"container": ei.ContainerID,
							"process":   ei.ProcessID,
						}).Warn("failed to delete process")
					}
				}()
			}
		}
		daemon.LogContainerEventWithAttributes(c, events.ActionExecDie, map[string]string{
			"execID":   ei.ProcessID,
			"exitCode": strconv.Itoa(exitCode),
		})
		return nil
	case libcontainerdtypes.EventStart:
		c.Lock()
		defer c.Unlock()

		// This is here to handle start not generated by docker
		if !c.State.Running {
			ctr, err := daemon.containerd.LoadContainer(context.Background(), c.ID)
			if err != nil {
				if cerrdefs.IsNotFound(err) {
					// The container was started by not-docker and so could have been deleted by
					// not-docker before we got around to loading it from containerd.
					log.G(context.TODO()).WithFields(log.Fields{
						"error":     err,
						"container": c.ID,
					}).Debug("could not load containerd container for start event")
					return nil
				}
				return err
			}
			tsk, err := ctr.Task(context.Background())
			if err != nil {
				if cerrdefs.IsNotFound(err) {
					log.G(context.TODO()).WithFields(log.Fields{
						"error":     err,
						"container": c.ID,
					}).Debug("failed to load task for externally-started container")
					return nil
				}
				return err
			}
			c.State.SetRunningExternal(ctr, tsk)
			c.HasBeenManuallyStopped = false
			c.HasBeenStartedBefore = true
			daemon.setStateCounter(c)

			daemon.initHealthMonitor(c)

			if err := c.CheckpointTo(context.TODO(), daemon.containersReplica); err != nil {
				return err
			}
			daemon.LogContainerEvent(c, events.ActionStart)
		}

		return nil
	case libcontainerdtypes.EventPaused:
		c.Lock()
		defer c.Unlock()

		if !c.State.Paused {
			c.State.SetPaused(true)
			daemon.setStateCounter(c)
			daemon.updateHealthMonitor(c)
			if err := c.CheckpointTo(context.TODO(), daemon.containersReplica); err != nil {
				return err
			}
			daemon.LogContainerEvent(c, events.ActionPause)
		}
		return nil
	case libcontainerdtypes.EventResumed:
		c.Lock()
		defer c.Unlock()

		if c.State.Paused {
			c.State.SetPaused(false)
			daemon.setStateCounter(c)
			daemon.updateHealthMonitor(c)

			if err := c.CheckpointTo(context.TODO(), daemon.containersReplica); err != nil {
				return err
			}
			daemon.LogContainerEvent(c, events.ActionUnPause)
		}
		return nil
	default:
		// TODO(thaJeztah): make switch exhaustive; add types.EventUnknown, types.EventCreate, types.EventExecAdded, types.EventExecStarted
		return nil
	}
}

func (daemon *Daemon) autoRemove(cfg *config.Config, c *container.Container) {
	c.Lock()
	ar := c.HostConfig.AutoRemove
	c.Unlock()
	if !ar {
		return
	}

	err := daemon.containerRm(cfg, c.ID, &backend.ContainerRmConfig{ForceRemove: true, RemoveVolume: true})
	if err != nil {
		if daemon.containers.Get(c.ID) == nil {
			// container no longer found, so remove worked after all.
			return
		}
		log.G(context.TODO()).WithFields(log.Fields{"error": err, "container": c.ID}).Error("error removing container")
	}
}

func (daemon *Daemon) shouldIgnoreExitEventWithLock(c *container.Container, e *libcontainerdtypes.EventInfo) (ret bool) {
	if e == nil {
		return false
	}

	defer func() {
		if ret {
			log.G(context.TODO()).WithFields(log.Fields{
				"container": c.ID,
				"state":     c.State.State(),
				"exitCode":  e.ExitCode,
				"exitedAt":  e.ExitedAt,
			}).Info("ignoring duplicate container exit event")
		}
	}()

	switch c.State.State() {
	case containertypes.StateRemoving,
		containertypes.StateExited,
		containertypes.StateDead:

		return true

	case containertypes.StateRunning:
		task, ok := c.State.Task()
		if !ok {
			log.G(context.TODO()).WithFields(log.Fields{
				"container": c.ID,
			}).Warn("container in running state but no task found while checking for duplicate exit event")
			return false
		}

		ctx, cancel := context.WithTimeout(context.TODO(), 30*time.Second)
		status, err := task.Status(ctx)
		cancel()
		if err != nil {
			// If containerd-shim crashed, the task will be deleted
			// automatically by containerd, so treat a NotFound
			// error as meaning the task is not running.
			log.G(ctx).WithFields(log.Fields{
				"error":     err,
				"container": c.ID,
			}).Warn("failed to get task status while checking for duplicate exit event")
			return false
		}

		// If the container is still running, then this exit event must
		// be a duplicate from a previous run, so ignore it. If the
		// container is not running, then we should process the exit
		// event to transition the container to exited.
		//
		// Timestamp is not reliable for determining whether an exit
		// event is a duplicate, because CLOCK_REALTIME can jump backwards
		// (e.g., due to NTP adjustments).
		//
		// See moby/moby#52153 for more details.
		if status.Status == containerdcli.Running {
			return true
		}

		if status.Status == containerdcli.Stopped {
			if status.ExitStatus != e.ExitCode {
				log.G(ctx).WithFields(log.Fields{
					"container": c.ID,
					"exitCode":  status.ExitStatus,
					"eventCode": e.ExitCode,
				}).Warn("container stopped with different exit code than exit event while checking for duplicate exit event")
				return true
			}
		}
		return false

	case containertypes.StateRestarting:
		// The restart path acquires and holds the container lock before
		// processing. So, if we're currently restarting, then we know
		// for certain that we are still processing the previous exit
		// event, and any new exit events must be duplicates.
		return true

	default:
		return false
	}
}
