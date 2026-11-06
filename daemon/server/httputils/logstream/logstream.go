package logstream

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/v2/daemon/internal/stdcopymux"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/pkg/ioutils"
)

// rfc3339NanoFixed is time.RFC3339Nano with nanoseconds padded using zeros to
// ensure the formatted time isalways the same number of characters.
const rfc3339NanoFixed = "2006-01-02T15:04:05.000000000Z07:00"

// Write writes an encoded byte stream of log messages from the
// messages channel, multiplexing them with a stdcopy.Writer if mux is true
func Write(ctx context.Context, w http.ResponseWriter, msgs <-chan *backend.LogMessage, config *backend.ContainerLogsOptions, mux bool) {
	// See https://github.com/moby/moby/issues/47448
	// Trigger headers to be written immediately.
	w.WriteHeader(http.StatusOK)

	wf := ioutils.NewWriteFlusher(w)
	defer wf.Close()

	wf.Flush()

	outStream := io.Writer(wf)
	errStream := outStream
	sysErrStream := errStream
	execOutStream := outStream
	execErrStream := outStream
	if mux {
		sysErrStream = stdcopymux.NewStdWriter(outStream, stdcopy.Systemerr)
		errStream = stdcopymux.NewStdWriter(outStream, stdcopy.Stderr)
		execOutStream = stdcopymux.NewStdWriter(outStream, stdcopy.ExecStdout)
		execErrStream = stdcopymux.NewStdWriter(outStream, stdcopy.ExecStderr)
		outStream = stdcopymux.NewStdWriter(outStream, stdcopy.Stdout)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-msgs:
			if !ok {
				return
			}
			// check if the message contains an error. if so, write that error
			// and exit
			if msg.Err != nil {
				_, _ = fmt.Fprintf(sysErrStream, "Error grabbing logs: %v\n", msg.Err)
				continue
			}
			var stream io.Writer
			switch msg.Source {
			case "stdout":
				if !config.ShowStdout {
					continue
				}
				stream = outStream
			case "stderr":
				if !config.ShowStderr {
					continue
				}
				stream = errStream
			case "exec-stdout":
				if !config.ShowExecStdout {
					continue
				}
				stream = execOutStream
			case "exec-stderr":
				if !config.ShowExecStderr {
					continue
				}
				stream = execErrStream
			default:
				// unknown source
				continue
			}

			if !config.Timestamps && !config.Details {
				// Fast path: avoid allocating and copying the message.
				_, _ = stream.Write(msg.Line)
			} else {
				size := len(msg.Line)
				if config.Timestamps {
					size += len(rfc3339NanoFixed) + 1
				}
				if config.Details {
					// Reserve some space for attributes to reduce reallocations.
					size += len(msg.Attrs) * 16
				}
				logLine := make([]byte, 0, size)

				if config.Timestamps {
					logLine = append(logLine, msg.Timestamp.Format(rfc3339NanoFixed)...)
					logLine = append(logLine, ' ')
				}
				if config.Details {
					logLine = appendAttrs(logLine, msg.Attrs)
					logLine = append(logLine, ' ')
				}
				logLine = append(logLine, msg.Line...)
				_, _ = stream.Write(logLine)
			}
		}
	}
}

func appendAttrs(dst []byte, attrs []backend.LogAttr) []byte {
	// Note this sorts attrs in-place. That is fine here - nothing else is
	// going to use Attrs or care about the order.
	slices.SortFunc(attrs, func(a, b backend.LogAttr) int {
		return cmp.Compare(a.Key, b.Key)
	})

	for i, pair := range attrs {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, url.QueryEscape(pair.Key)...)
		dst = append(dst, '=')
		dst = append(dst, url.QueryEscape(pair.Value)...)
	}
	return dst
}
