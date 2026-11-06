package daemon

import (
	"context"
	"strconv"

	"github.com/containerd/containerd/v2/pkg/tracing"
	"github.com/containerd/log"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/logger"
	logcache "github.com/moby/moby/v2/daemon/logger/loggerutils/cache"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/errdefs"
	"github.com/pkg/errors"
)

// ContainerLogs copies the container's log channel to the channel provided in
// the config. If ContainerLogs returns an error, no messages have been copied.
// and the channel will be closed without data.
//
// if it returns nil, the config channel will be active and return log
// messages until it runs out or the context is canceled.
func (daemon *Daemon) ContainerLogs(ctx context.Context, containerName string, config *backend.ContainerLogsOptions) (messages <-chan *backend.LogMessage, isTTY bool, retErr error) {
	ctx, span := tracing.StartSpan(ctx, "daemon.ContainerLogs")
	defer func() {
		span.SetStatus(retErr)
		span.End()
	}()

	lg := log.G(ctx).WithFields(log.Fields{
		"module":    "daemon",
		"method":    "(*Daemon).ContainerLogs",
		"container": containerName,
	})

	if !config.ShowStdout && !config.ShowStderr && !config.ShowExecStdout && !config.ShowExecStderr {
		return nil, false, errdefs.InvalidParameter(errors.New("You must choose at least one stream"))
	}
	ctr, err := daemon.GetContainer(containerName)
	if err != nil {
		return nil, false, err
	}

	if ctr.State.RemovalInProgress || ctr.State.Dead {
		return nil, false, errdefs.Conflict(errors.New("can not get logs from container which is dead or marked for removal"))
	}

	if ctr.HostConfig.LogConfig.Type == "none" {
		return nil, false, logger.ErrReadLogsNotSupported{}
	}

	cLog, cLogCreated, err := daemon.getLogger(ctr)
	if err != nil {
		return nil, false, err
	}
	if cLogCreated {
		defer func() {
			if retErr != nil {
				if err = cLog.Close(); err != nil {
					log.G(ctx).Errorf("Error closing logger: %v", err)
				}
			}
		}()
	}

	logReader, ok := cLog.(logger.LogReader)
	if !ok {
		return nil, false, logger.ErrReadLogsNotSupported{}
	}

	tailLines, err := strconv.Atoi(config.Tail)
	if err != nil {
		tailLines = -1
	}

	follow := config.Follow && !cLogCreated
	logs := logReader.ReadLogs(ctx, logger.ReadConfig{
		Since:  config.Since,
		Until:  config.Until,
		Tail:   tailLines,
		Follow: follow,
	})

	// past this point, we can't possibly return any errors, so we can just
	// start a goroutine and return to tell the caller not to expect errors
	// (if the caller wants to give up on logs, they have to cancel the context)
	// this goroutine functions as a shim between the logger and the caller.
	messageChan := make(chan *backend.LogMessage, 1)
	go func() {
		if cLogCreated {
			defer func() {
				if err = cLog.Close(); err != nil {
					log.G(ctx).Errorf("Error closing logger: %v", err)
				}
			}()
		}
		// signal that the log reader is gone
		defer logs.ConsumerGone()

		// close the messages channel. closing is the only way to signal above
		// that we're doing with logs (other than context cancel i guess).
		defer close(messageChan)

		lg.Debug("begin logs")
		defer lg.Debugf("end logs (%v)", ctx.Err())

		for {
			select {
			// i do not believe as the system is currently designed any error
			// is possible, but we should be prepared to handle it anyway. if
			// we do get an error, copy only the error field to a new object so
			// we don't end up with partial data in the other fields
			case err := <-logs.Err:
				lg.Errorf("Error streaming logs: %v", err)
				select {
				case <-ctx.Done():
				case messageChan <- &backend.LogMessage{Err: err}:
				}
				return
			case <-ctx.Done():
				return
			case msg, ok := <-logs.Msg:
				// there is some kind of pool or ring buffer in the logger that
				// produces these messages, and a possible future optimization
				// might be to use that pool and reuse message objects
				if !ok {
					return
				}
				m := msg.AsLogMessage() // just a pointer conversion, does not copy data

				// there could be a case where the reader stops accepting
				// messages and the context is canceled. we need to check that
				// here, or otherwise we risk blocking forever on the message
				// send.
				select {
				case <-ctx.Done():
					return
				case messageChan <- m:
				}
			}
		}
	}()
	return messageChan, ctr.Config.Tty, nil
}

func (daemon *Daemon) getLogger(container *container.Container) (_ logger.Logger, created bool, _ error) {
	var logDriver logger.Logger
	container.Lock()
	if container.State.Running {
		logDriver = container.LogDriver
	}
	container.Unlock()
	if logDriver != nil {
		return logDriver, false, nil
	}
	logDriver, err := container.StartLogger()
	if err != nil {
		// Let's assume a driver was created, but failed to start;
		// see https://github.com/moby/moby/pull/49493#discussion_r1979120968
		//
		// TODO(thaJeztah): check if we're not leaking resources if a logger was created, but failed to start.
		return nil, true, err
	}
	return logDriver, true, nil
}

// mergeAndVerifyLogConfig merges the daemon log config to the container's log config if the container's log driver is not specified.
func (daemon *Daemon) mergeAndVerifyLogConfig(cfg *containertypes.LogConfig) error {
	if cfg.Type == "" {
		cfg.Type = daemon.defaultLogConfig.Type
	}

	if cfg.Config == nil {
		cfg.Config = make(map[string]string)
	}

	if cfg.Type == daemon.defaultLogConfig.Type {
		for k, v := range daemon.defaultLogConfig.Config {
			if _, ok := cfg.Config[k]; !ok {
				cfg.Config[k] = v
			}
		}
	}

	logcache.MergeDefaultLogConfig(cfg.Config, daemon.defaultLogConfig.Config)

	return logger.ValidateLogOpts(cfg.Type, cfg.Config)
}

func defaultLogConfig(cfg *config.Config) (containertypes.LogConfig, error) {
	if len(cfg.LogConfig.Config) > 0 {
		if err := logger.ValidateLogOpts(cfg.LogConfig.Type, cfg.LogConfig.Config); err != nil {
			return containertypes.LogConfig{}, errors.Wrap(err, "failed to set log opts")
		}
	}
	return containertypes.LogConfig{
		Type:   cfg.LogConfig.Type,
		Config: cfg.LogConfig.Config,
	}, nil
}
