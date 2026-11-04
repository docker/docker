package logger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/logger/internal/logdriver"
	"github.com/moby/moby/v2/pkg/plugingetter"
)

// pluginAdapter takes a plugin and implements the Logger interface for logger
// instances
type pluginAdapter struct {
	driverName   string
	id           string
	plugin       logPlugin
	fifoPath     string
	capabilities Capability
	logInfo      Info

	// synchronize access to the log stream and shared buffer
	mu     sync.Mutex
	enc    logdriver.LogEntryEncoder
	stream io.WriteCloser
	// buf is shared for each `Log()` call to reduce allocations.
	// buf must be protected by mutex
	buf logdriver.LogEntry
}

func (a *pluginAdapter) Log(msg *Message) (err error) {
	defer func() {
		if err == nil {
			PutMessage(msg)
		}
	}()
	a.mu.Lock()

	a.buf.Line = msg.Line
	a.buf.TimeNano = msg.Timestamp.UnixNano()
	a.buf.Partial = msg.PLogMetaData != nil
	a.buf.Source = msg.Source
	if msg.PLogMetaData != nil {
		a.buf.PartialLogMetadata = &logdriver.PartialLogEntryMetadata{
			Id:      msg.PLogMetaData.ID,
			Last:    msg.PLogMetaData.Last,
			Ordinal: int32(msg.PLogMetaData.Ordinal),
		}
	}

	err = a.enc.Encode(&a.buf)
	a.buf.Reset()

	a.mu.Unlock()

	return err
}

func (a *pluginAdapter) Name() string {
	return a.driverName
}

func (a *pluginAdapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.plugin.StopLogging(filepath.Join("/", "run", "docker", "logging", a.id)); err != nil {
		return err
	}

	if err := a.stream.Close(); err != nil {
		log.G(context.TODO()).WithError(err).Error("error closing plugin fifo")
	}
	if err := os.Remove(a.fifoPath); err != nil && !os.IsNotExist(err) {
		log.G(context.TODO()).WithError(err).Error("error cleaning up plugin fifo")
	}

	// may be nil, especially for unit tests
	if pluginGetter != nil {
		_, _ = pluginGetter.Get(a.Name(), extName, plugingetter.Release)
	}
	return nil
}

type pluginAdapterWithRead struct {
	*pluginAdapter
}

func (a *pluginAdapterWithRead) ReadLogs(ctx context.Context, config ReadConfig) *LogWatcher {
	watcher := NewLogWatcher()

	go func() {
		defer close(watcher.Msg)
		stream, err := a.plugin.ReadLogs(a.logInfo, config)
		if err != nil {
			watcher.Err <- fmt.Errorf("error getting log reader: %w", err)
			return
		}
		defer func() { _ = stream.Close() }()

		dec := logdriver.NewLogEntryDecoder(stream)
		for {
			if ctx.Err() != nil {
				return
			}

			var buf logdriver.LogEntry
			if err := dec.Decode(&buf); err != nil {
				if errors.Is(err, io.EOF) {
					return
				}
				watcher.Err <- fmt.Errorf("error decoding log message: %w", err)
				return
			}

			msg := &Message{
				Timestamp: time.Unix(0, buf.TimeNano),
				Line:      buf.Line,
				Source:    buf.Source,
			}

			// plugin should handle this, but check just in case
			if !config.Since.IsZero() && msg.Timestamp.Before(config.Since) {
				continue
			}
			if !config.Until.IsZero() && msg.Timestamp.After(config.Until) {
				return
			}

			// send the message unless the consumer is gone
			select {
			case watcher.Msg <- msg:
			case <-ctx.Done():
				return
			case <-watcher.WatchConsumerGone():
				return
			}
		}
	}()

	return watcher
}
