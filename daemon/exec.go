package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/log"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/stream"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/errdefs"
	"github.com/moby/moby/v2/pkg/pools"
	"github.com/moby/sys/signal"
	"github.com/moby/term"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
)

func (daemon *Daemon) registerExecCommand(container *container.Container, config *container.ExecConfig) {
	// Storing execs in container in order to kill them gracefully whenever the container is stopped or removed.
	container.ExecCommands.Add(config.ID, config)
	// Storing execs in daemon for easy access via Engine API.
	daemon.execCommands.Add(config.ID, config)
}

// ExecExists looks up the exec instance and returns a bool if it exists or not.
// It will also return the error produced by `getConfig`
func (daemon *Daemon) ExecExists(name string) (bool, error) {
	if _, err := daemon.getExecConfig(name); err != nil {
		return false, err
	}
	return true, nil
}

// getExecConfig looks up the exec instance by name. If the container associated
// with the exec instance is stopped or paused, it will return an error.
func (daemon *Daemon) getExecConfig(name string) (*container.ExecConfig, error) {
	ec := daemon.execCommands.Get(name)
	if ec == nil {
		return nil, errExecNotFound(name)
	}

	// If the exec is found but its container is not in the daemon's list of
	// containers then it must have been deleted, in which case instead of
	// saying the container isn't running, we should return a 404 so that
	// the user sees the same error now that they will after the
	// 5 minute clean-up loop is run which erases old/dead execs.
	ctr := daemon.containers.Get(ec.Container.ID)
	if ctr == nil {
		return nil, containerNotFound(name)
	}
	if !ctr.State.IsRunning() {
		return nil, errNotRunning(ctr.ID)
	}
	if ctr.State.IsPaused() {
		return nil, errExecPaused(ctr.ID)
	}
	if ctr.State.IsRestarting() {
		return nil, errContainerIsRestarting(ctr.ID)
	}
	return ec, nil
}

func (daemon *Daemon) unregisterExecCommand(container *container.Container, execConfig *container.ExecConfig) {
	container.ExecCommands.Delete(execConfig.ID)
	daemon.execCommands.Delete(execConfig.ID)
}

func (daemon *Daemon) getActiveContainer(name string) (*container.Container, error) {
	ctr, err := daemon.GetContainer(name)
	if err != nil {
		return nil, err
	}

	if !ctr.State.IsRunning() {
		return nil, errNotRunning(ctr.ID)
	}
	if ctr.State.IsPaused() {
		return nil, errExecPaused(name)
	}
	if ctr.State.IsRestarting() {
		return nil, errContainerIsRestarting(ctr.ID)
	}
	return ctr, nil
}

// ContainerExecCreate sets up an exec in a running container.
func (daemon *Daemon) ContainerExecCreate(name string, options *containertypes.ExecCreateRequest) (string, error) {
	cntr, err := daemon.getActiveContainer(name)
	if err != nil {
		return "", err
	}
	if user := options.User; user != "" {
		// Lookup the user inside the container before starting the exec to
		// allow for an early exit.
		//
		// Note that "technically" this check may have some TOCTOU issues,
		// because '/etc/passwd' and '/etc/groups' may be mutated by the
		// container in between creating the exec and starting it.
		//
		// This is very likely a corner-case, but something we can consider
		// changing in future (either allow creating an invalid exec, and
		// checking before starting, or checking both before create and
		// before start).
		if _, err := getUser(cntr, user); err != nil {
			return "", errdefs.InvalidParameter(err)
		}
	}

	keys := []byte{}
	if options.DetachKeys != "" {
		keys, err = term.ToBytes(options.DetachKeys)
		if err != nil {
			err = fmt.Errorf("Invalid escape keys (%s) provided", options.DetachKeys)
			return "", err
		}
	}

	execConfig := container.NewExecConfig(cntr)
	execConfig.OpenStdin = options.AttachStdin
	execConfig.OpenStdout = options.AttachStdout
	execConfig.OpenStderr = options.AttachStderr
	execConfig.DetachKeys = keys
	execConfig.Entrypoint, execConfig.Args = options.Cmd[0], options.Cmd[1:]
	execConfig.Tty = options.Tty
	execConfig.ConsoleSize = options.ConsoleSize
	execConfig.Privileged = options.Privileged
	execConfig.User = options.User
	execConfig.WorkingDir = options.WorkingDir

	linkedEnv, err := daemon.setupLinkedContainers(cntr)
	if err != nil {
		return "", err
	}
	execConfig.Env = container.ReplaceOrAppendEnvValues(cntr.CreateDaemonEnvironment(options.Tty, linkedEnv), options.Env)
	if execConfig.User == "" {
		execConfig.User = cntr.Config.User
	}
	if execConfig.WorkingDir == "" {
		execConfig.WorkingDir = cntr.Config.WorkingDir
	}

	daemon.registerExecCommand(cntr, execConfig)
	attributes := map[string]string{
		"execID": execConfig.ID,
	}
	if execConfig.ExecType != "" {
		attributes["execType"] = execConfig.ExecType
	}
	daemon.LogContainerEventWithAttributes(cntr, events.Action(string(events.ActionExecCreate)+": "+execConfig.Entrypoint+" "+strings.Join(execConfig.Args, " ")), attributes)

	return execConfig.ID, nil
}

// ContainerExecStart starts a previously set up exec instance. The
// std streams are set up.
// If ctx is cancelled, the process is terminated.
func (daemon *Daemon) ContainerExecStart(ctx context.Context, name string, options backend.ExecStartConfig) (retErr error) {
	var (
		cStdin           io.ReadCloser
		cStdout, cStderr io.Writer
	)

	ec, err := daemon.getExecConfig(name)
	if err != nil {
		return err
	}

	ec.Lock()
	if ec.ExitCode != nil {
		ec.Unlock()
		return errdefs.Conflict(fmt.Errorf("exec command %s has already run", ec.ID))
	}

	if ec.Running {
		ec.Unlock()
		return errdefs.Conflict(fmt.Errorf("exec command %s is already running", ec.ID))
	}
	ec.Running = true
	ec.Unlock()

	log.G(ctx).Debugf("starting exec command %s in container %s", ec.ID, ec.Container.ID)
	attributes := map[string]string{
		"execID": ec.ID,
	}
	if ec.ExecType != "" {
		attributes["execType"] = ec.ExecType
	}
	daemon.LogContainerEventWithAttributes(ec.Container, events.Action(string(events.ActionExecStart)+": "+ec.Entrypoint+" "+strings.Join(ec.Args, " ")), attributes)

	defer func() {
		if retErr != nil {
			ec.Lock()
			ec.Container.ExecCommands.Delete(ec.ID)
			ec.Running = false
			if ec.ExitCode == nil {
				// default to `126` (`EACCES`) if we fail to start
				// the exec without setting an exit code.
				exitCode := exitEaccess
				ec.ExitCode = &exitCode
			}
			if err := ec.CloseStreams(); err != nil {
				log.G(ctx).Errorf("failed to cleanup exec %s streams: %s", ec.Container.ID, err)
			}
			ec.Unlock()
		}
	}()

	if ec.OpenStdin && options.Stdin != nil {
		r, w := io.Pipe()
		go func() {
			defer func() {
				log.G(ctx).Debug("Closing buffered stdin pipe")
				_ = w.Close()
			}()
			_, _ = pools.Copy(w, options.Stdin)
		}()
		cStdin = r
	}
	if ec.OpenStdout {
		cStdout = options.Stdout
	}
	if ec.OpenStderr {
		cStderr = options.Stderr
	}

	if ec.OpenStdin {
		ec.StreamConfig.NewInputPipes()
	} else {
		ec.StreamConfig.NewNopInputPipe()
	}

	p := &specs.Process{}
	if runtime.GOOS != "windows" {
		// TODO(thaJeztah): also enable on Windows;
		//  This was added in https://github.com/moby/moby/commit/7603c22c7365d7d7150597fe396e0707d6e561da,
		//  which mentions that it failed on Windows "Probably needs to wait for container to be in running state"
		ctr, err := daemon.containerdClient.LoadContainer(ctx, ec.Container.ID)
		if err != nil {
			return err
		}
		md, err := ctr.Info(ctx, containerd.WithoutRefreshedMetadata)
		if err != nil {
			return err
		}

		// Technically, this should be a [specs.Spec], but we're only
		// interested in the Process field.
		spec := struct{ Process *specs.Process }{p}
		if err := json.Unmarshal(md.Spec.GetValue(), &spec); err != nil {
			return err
		}
	}

	// merge/override properties of the container's process with the exec-config.
	p.Args = append([]string{ec.Entrypoint}, ec.Args...)
	p.Env = ec.Env
	p.Cwd = ec.WorkingDir
	p.Terminal = ec.Tty

	consoleSize := options.ConsoleSize
	// If size isn't specified for start, use the one provided for create
	if consoleSize == nil {
		consoleSize = ec.ConsoleSize
	}
	if p.Terminal && consoleSize != nil {
		p.ConsoleSize = &specs.Box{
			Height: consoleSize[0],
			Width:  consoleSize[1],
		}
	}

	if p.Cwd == "" {
		p.Cwd = "/"
	}

	daemonCfg := &daemon.config().Config
	if err := daemon.execSetPlatformOpt(ctx, daemonCfg, ec, p); err != nil {
		return err
	}

	attachConfig := stream.AttachConfig{
		TTY:        ec.Tty,
		UseStdin:   cStdin != nil,
		UseStdout:  cStdout != nil,
		UseStderr:  cStderr != nil,
		Stdin:      cStdin,
		Stdout:     cStdout,
		Stderr:     cStderr,
		DetachKeys: ec.DetachKeys,
		CloseStdin: true,
	}
	ec.StreamConfig.AttachStreams(&attachConfig)
	// using context.Background() so that attachErr does not race ctx.Done().
	copyCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attachErr := ec.StreamConfig.CopyStreams(copyCtx, &attachConfig)

	ec.Container.Lock()
	tsk, err := ec.Container.GetRunningTask()
	ec.Container.Unlock()
	if err != nil {
		return err
	}

	// Synchronize with libcontainerd event loop
	ec.Lock()
	ec.Process, err = tsk.Exec(ctx, ec.ID, p, cStdin != nil, ec.InitializeStdio)
	// the exec context should be ready, or error happened.
	// close the chan to notify readiness
	close(ec.Started)
	if err != nil {
		defer ec.Unlock()
		return setExitCodeFromError(ec.SetExitCode, err)
	}
	ec.Unlock()

	select {
	case <-ctx.Done():
		logger := log.G(ctx).WithFields(log.Fields{
			"container": ec.Container.ID,
			"execID":    ec.ID,
		})
		logger.Debug("Sending KILL signal to container process")
		sigCtx, cancelFunc := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelFunc()
		if err := ec.Process.Kill(sigCtx, signal.SignalMap["KILL"]); err != nil {
			logger.WithError(err).Error("Could not send KILL signal to container process")
		}
		return ctx.Err()
	case err := <-attachErr:
		if err != nil {
			if _, ok := err.(term.EscapeError); !ok {
				return errdefs.System(errors.Wrap(err, "exec attach failed"))
			}
			detachAttributes := map[string]string{
				"execID": ec.ID,
			}
			if ec.ExecType != "" {
				detachAttributes["execType"] = ec.ExecType
			}
			daemon.LogContainerEventWithAttributes(ec.Container, events.ActionExecDetach, detachAttributes)
		}
	}
	return nil
}

// execCommandGC periodically cleans up daemon references to exec configs that
// are no longer associated with a container.
func (daemon *Daemon) execCommandGC(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var (
				cleaned          int
				liveExecCommands = daemon.containerExecIds()
			)
			for id, config := range daemon.execCommands.Commands() {
				if config.CanRemove {
					cleaned++
					daemon.execCommands.Delete(id)
					continue
				}
				if _, exists := liveExecCommands[id]; !exists {
					config.CanRemove = true
				}
			}
			if cleaned > 0 {
				log.G(ctx).Debugf("clean %d unused exec commands", cleaned)
			}
		}
	}
}

// containerExecIds returns a list of all the current exec ids that are in use
// and running inside a container.
func (daemon *Daemon) containerExecIds() map[string]struct{} {
	ids := map[string]struct{}{}
	for _, c := range daemon.containers.List() {
		for _, id := range c.ExecCommands.List() {
			ids[id] = struct{}{}
		}
	}
	return ids
}
