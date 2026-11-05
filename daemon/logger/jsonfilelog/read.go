package jsonfilelog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/logger"
	"github.com/moby/moby/v2/daemon/logger/jsonfilelog/jsonlog"
	"github.com/moby/moby/v2/daemon/logger/loggerutils"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/pkg/tailfile"
)

var _ logger.LogReader = (*JSONFileLogger)(nil)

// ReadLogs implements the logger's LogReader interface for the logs
// created by this driver.
func (l *JSONFileLogger) ReadLogs(ctx context.Context, config logger.ReadConfig) *logger.LogWatcher {
	return l.writer.ReadLogs(ctx, config)
}

func decodeLogBytes(data []byte, l *jsonlog.JSONLog) (*logger.Message, error) {
	l.Reset()
	if err := json.Unmarshal(data, l); err != nil {
		return nil, err
	}

	var attrs []backend.LogAttr
	if len(l.Attrs) != 0 {
		attrs = make([]backend.LogAttr, 0, len(l.Attrs))
		for k, v := range l.Attrs {
			attrs = append(attrs, backend.LogAttr{Key: k, Value: v})
		}
	}
	msg := &logger.Message{
		Source:    l.Stream,
		Timestamp: l.Created,
		Line:      []byte(l.Log),
		Attrs:     attrs,
	}
	return msg, nil
}

type decoder struct {
	rdr io.Reader
	buf *bufio.Reader
	jl  *jsonlog.JSONLog
}

func (d *decoder) Reset(rdr io.Reader) {
	d.rdr = rdr
	if d.buf == nil {
		d.buf = bufio.NewReader(rdr)
	} else {
		d.buf.Reset(rdr)
	}
	if d.jl != nil {
		d.jl.Reset()
	}
}

func (d *decoder) Close() {
	d.buf = nil
	d.rdr = nil
	d.jl = nil
}

func (d *decoder) Decode() (*logger.Message, error) {
	if d.buf == nil {
		d.buf = bufio.NewReader(d.rdr)
	}
	if d.jl == nil {
		d.jl = &jsonlog.JSONLog{}
	}

	for {
		line, err := d.buf.ReadBytes('\n')
		if err != nil && len(line) == 0 {
			return nil, err
		}

		trimmed := bytes.Trim(line, "\x00\r\n \t")
		if len(trimmed) == 0 {
			if err != nil {
				return nil, err
			}
			continue
		}

		msg, jsonErr := decodeLogBytes(trimmed, d.jl)
		if jsonErr != nil {
			// A record cut mid-way by a NUL gap can share its line with the next,
			// intact record (the gap swallowed the newline). Try what follows the
			// last NUL before giving up on the line.
			if i := bytes.LastIndexByte(trimmed, 0); i >= 0 {
				if m, err2 := decodeLogBytes(trimmed[i+1:], d.jl); err2 == nil {
					log.G(context.TODO()).WithError(jsonErr).Warn("Error decoding log line, recovered the record after the NUL gap")
					return m, nil
				}
			}
			log.G(context.TODO()).WithError(jsonErr).Warn("Error decoding log line, skipping corrupted line")
			if err != nil {
				return nil, err
			}
			continue
		}

		return msg, nil
	}
}

// decodeFunc is used to create a decoder for the log file reader
func decodeFunc(rdr io.Reader) loggerutils.Decoder {
	return &decoder{
		rdr: rdr,
	}
}

func getTailReader(ctx context.Context, r loggerutils.SizeReaderAt, req int) (loggerutils.SizeReaderAt, int, error) {
	return tailfile.NewTailReader(ctx, r, req)
}
