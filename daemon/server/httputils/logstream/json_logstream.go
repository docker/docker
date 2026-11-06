package logstream

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/daemon/server/httputils"
	"github.com/moby/moby/v2/pkg/ioutils"
)

// WriteJSON writes a JSON stream of log messages from the messages channel.
func WriteJSON(ctx context.Context, w http.ResponseWriter, msgs <-chan *backend.LogMessage, config *backend.ContainerLogsOptions) {
	// See https://github.com/moby/moby/issues/47448
	// Trigger headers to be written immediately.
	w.WriteHeader(http.StatusOK)

	wf := ioutils.NewWriteFlusher(w)
	defer wf.Close()

	wf.Flush()

	jsonWriter := newJSONLogWriter(wf, w.Header().Get("Content-Type"), config)
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-msgs:
			if !ok {
				return
			}
			if msg.Err != nil {
				// message contains an error; write the error and continue
				jsonWriter.write(msg)
				continue
			}
			switch msg.Source {
			case "stdout":
				if config.ShowStdout {
					jsonWriter.write(msg)
				}
			case "stderr":
				if config.ShowStderr {
					jsonWriter.write(msg)
				}
			case "exec-stdout":
				if config.ShowExecStdout {
					jsonWriter.write(msg)
				}
			case "exec-stderr":
				if config.ShowExecStderr {
					jsonWriter.write(msg)
				}
			default:
				// unknown source
			}
		}
	}
}

type jsonLogWriter struct {
	encode  httputils.EncoderFn
	details bool
}

func newJSONLogWriter(w io.Writer, contentType string, opts *backend.ContainerLogsOptions) *jsonLogWriter {
	encode := httputils.NewJSONStreamEncoder(w, contentType)
	return &jsonLogWriter{
		encode:  encode,
		details: opts != nil && opts.Details,
	}
}

// jsonLogMessage represents a single log entry in JSON log streaming format.
//
// Each message is serialized as a standalone JSON object and emitted as
// part of a stream (one object per line) in container log responses when
// a JSON-formatted output is requested.
//
// TODO(thaJeztah): move to the api module and generate from swagger.
type jsonLogMessage struct {
	// Line contains the log payload as UTF-8 text when text encoding is used.
	// When an alternate encoding is requested, this field is omitted.
	Line string `json:"Line,omitempty"`

	// Source identifies the originating stream ("stdout", "stderr",
	// "exec-stdout" or "exec-stderr").
	Source string `json:"Source,omitempty"`

	// Timestamp is the time at which the log record was produced,
	// encoded in RFC3339Nano format.
	Timestamp time.Time `json:"Timestamp,omitempty"`

	// Attrs contains optional structured attributes when "details" is
	// enabled, and if supported by the logging driver in use.
	Attrs []backend.LogAttr `json:"Attrs,omitempty"`

	// MetaData contains metadata for partial log records that must be
	// reassembled by the client.
	MetaData *backend.PartialLogMetaData `json:"MetaData,omitempty"`

	// Error contains an associated error encountered while processing the
	// log message, if any.
	Error string `json:"Error,omitempty"`
}

func (w *jsonLogWriter) write(msg *backend.LogMessage) {
	var errMsg string
	if msg.Err != nil {
		errMsg = msg.Err.Error()
	}

	var attrs []backend.LogAttr
	if w.details {
		attrs = msg.Attrs
	}

	_ = w.encode(jsonLogMessage{
		Line:      string(msg.Line),
		Source:    msg.Source,
		Timestamp: msg.Timestamp,
		Attrs:     attrs,
		MetaData:  msg.PLogMetaData,
		Error:     errMsg,
	})
}
