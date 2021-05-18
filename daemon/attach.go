package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/backend"
	"github.com/docker/docker/container/stream/streamv2/stdio"
	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/errdefs"
	"github.com/moby/term"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// ContainerAttachRaw attaches the provided streams to the container's stdio
func (daemon *Daemon) ContainerAttachRaw(prefixOrName string, stdin io.ReadCloser, stdout, stderr io.Writer) error {
	ctr, err := daemon.GetContainer(prefixOrName)
	if err != nil {
		return err
	}
	return ctr.StreamConfig.AttachStreams(context.TODO(), stdin, stdout, stderr)
}

// ContainerAttachMultiplexed handles attaching to a container when there is only one output stream
// This is primarily for legacy behavior for how the attach HTTP API endpoint works.
//
// Once this function returns, the streams are attached.
// We also close the file descriptor related to the client stream in this process.
//
// This is prarimarly used for serving the HTTP API.
// Unless you really need to have everything on a single stream, you probably should use the `ContainerAttachRaw` function.
func (daemon *Daemon) ContainerAttachMultiplexed(ctx context.Context, ref string, cfg *backend.ContainerAttachConfig) error {
	var detachKeys []byte
	if cfg.DetachKeys != "" {
		var err error
		detachKeys, err = term.ToBytes(cfg.DetachKeys)
		if err != nil {
			return errdefs.InvalidParameter(errors.Errorf("Invalid detach keys (%s) provided", cfg.DetachKeys))
		}
	}

	ctr, err := daemon.GetContainer(ref)
	if err != nil {
		return err
	}

	if ctr.IsPaused() {
		err := fmt.Errorf("container %s is paused, unpause the container before attach", ref)
		return errdefs.Conflict(err)
	}

	if ctr.IsRestarting() {
		err := fmt.Errorf("container %s is restarting, wait until the container is running", ref)
		return errdefs.Conflict(err)
	}

	var framing stdio.StreamFraming
	switch cfg.Framing {
	case backend.AttachFramingNone:
		framing.Type = stdio.StreamFraming_NONE
	case backend.AttachFramingStdcopy:
		framing.Type = stdio.StreamFraming_STDCOPY
	case backend.AttachFramingWebsocket:
		framing.Type = stdio.StreamFraming_WEBSOCKET_TEXT
	case backend.AttachFramingWebsocketBinary:
		framing.Type = stdio.StreamFraming_WEBSOCKET_BINARY
	default:
		return errdefs.InvalidParameter(errors.Errorf("invalid stream framing: %d", cfg.Framing))
	}

	if ctr.Config.Tty {
		if len(detachKeys) == 0 {
			detachKeys = []byte{16, 17} // ctrl-p,ctrl-q
		}
		if cfg.AllowTTYNoFraming {
			// This is an idiosyncrosy of the API, which itself does not know if the container has a TTY or not.
			//   ... and historically we do not have framing for TTY's.
			// Except of course for websockets which have their own framing protocol unrelated to our stdio streams...
			//
			// Anyway, things are complicated here due to API compat.
			// It's up to the caller to allow the fallback regardless of the passed in framing.
			framing.Type = stdio.StreamFraming_NONE
		}
	}

	stream, err := cfg.GetStream()
	if err != nil {
		return err
	}

	// TODO: logs needs to be framed according to cfg.Framing
	// Maybe hand this off to a custom stdio.Attacher?
	if cfg.Logs {
		_, stdout, stderr := stdio.GetFramedStreams(stream, framing, false, cfg.IncludeStdout, cfg.IncludeStderr)

		logDriver, logCreated, err := daemon.getLogger(ctr)
		if err != nil {
			return err
		}
		if logCreated {
			defer func() {
				if err = logDriver.Close(); err != nil {
					logrus.Errorf("Error closing logger: %v", err)
				}
			}()
		}
		cLog, ok := logDriver.(logger.LogReader)
		if !ok {
			return logger.ErrReadLogsNotSupported{}
		}
		logs := cLog.ReadLogs(logger.ReadConfig{Tail: -1})
		defer logs.ConsumerGone()

	LogLoop:
		for {
			select {
			case msg, ok := <-logs.Msg:
				if !ok {
					break LogLoop
				}
				if msg.Source == "stdout" {
					stdout.Write(msg.Line)
				}
				if msg.Source == "stderr" {
					stderr.Write(msg.Line)
				}
			case err := <-logs.Err:
				logrus.Errorf("Error streaming logs: %v", err)
				break LogLoop
			}
		}
	}

	if !cfg.Stream {
		return nil
	}

	if !ctr.Config.OpenStdin {
		cfg.IncludeStdin = false
	}
	if ctr.Config.Tty {
		cfg.IncludeStderr = false
	}

	logrus.WithField("container", ctr.ID).WithField("AttachFraming", framing.Type.String()).Debug("AttachStreamsMultiplexed")
	if err := ctr.StreamConfig.AttachStreamsMultiplexed(ctx, stream, &framing, detachKeys, cfg.IncludeStdin, cfg.IncludeStdout, cfg.IncludeStderr); err != nil {
		return err
	}

	daemon.LogContainerEvent(ctr, "attach")
	return nil
}
