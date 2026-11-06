package local

import (
	"cmp"
	"encoding/binary"
	"io"
	"math/bits"
	"slices"
	"sync"
	"time"

	"github.com/moby/moby/v2/daemon/logger"
	"github.com/moby/moby/v2/daemon/logger/internal/logdriver"
	"github.com/moby/moby/v2/daemon/logger/loggerutils"
	"github.com/moby/moby/v2/daemon/server/backend"
	"github.com/moby/moby/v2/errdefs"
	"github.com/pkg/errors"
)

const (
	// Name is the name of the driver
	Name = "local"

	encodeBinaryLen = 4
	initialBufSize  = 2048
	maxDecodeRetry  = 20000

	defaultMaxFileSize  int64 = 20 * 1024 * 1024
	defaultMaxFileCount       = 5
	defaultCompressLogs       = true
)

var buffersPool = sync.Pool{New: func() any {
	b := make([]byte, initialBufSize)
	return &b
}}

// LogOptKeys are the keys names used for log opts passed in to initialize the driver.
var LogOptKeys = map[string]bool{
	"max-file": true,
	"max-size": true,
	"compress": true,

	// Common attributes handled through [logger.Info.ExtraAttributes] and [loggerutils.ParseLogTag].
	logger.AttrLabels:      true,
	logger.AttrLabelsRegex: true,
	logger.AttrEnv:         true,
	logger.AttrEnvRegex:    true,
	logger.AttrLogTag:      true,
}

// ValidateLogOpt looks for log driver specific options.
func ValidateLogOpt(cfg map[string]string) error {
	for key := range cfg {
		if !LogOptKeys[key] {
			return errors.Errorf("unknown log opt '%s' for log driver %s", key, Name)
		}
	}
	return nil
}

type driver struct {
	logfile *loggerutils.LogFile

	// extra contains prebuilt log attributes attached to every log entry.
	// The slice and its elements must be treated as immutable after initialization,
	// as they may be shared by multiple marshaled log entries.
	extra []*logdriver.LogAttr
}

// New creates a new local logger
// You must provide the `LogPath` in the passed-in info argument, this is the file path that logs are written to.
func New(info logger.Info) (logger.Logger, error) {
	if info.LogPath == "" {
		return nil, errdefs.System(errors.New("log path is missing -- this is a bug and should not happen"))
	}

	cfg, err := newConfig(info.Config)
	if err != nil {
		return nil, errdefs.InvalidParameter(err)
	}
	extraAttrs, err := info.ExtraAttributes(nil)
	if err != nil {
		return nil, err
	}

	if v, ok := info.Config[logger.AttrLogTag]; ok && v != "" {
		// no default template. and only use a tag if the user asked for it.
		if tag, err := loggerutils.ParseLogTag(info, ""); err != nil {
			return nil, err
		} else if tag != "" {
			extraAttrs[logger.AttrLogTag] = tag
		}
	}

	lf, err := loggerutils.NewLogFile(info.LogPath, cfg.MaxFileSize, cfg.MaxFileCount, !cfg.DisableCompression, decodeFunc, 0o640, getTailReader)
	if err != nil {
		return nil, err
	}

	attrs := make([]*logdriver.LogAttr, 0, len(extraAttrs))
	for k, v := range extraAttrs {
		attrs = append(attrs, &logdriver.LogAttr{Key: k, Value: v})
	}
	slices.SortFunc(attrs, func(a, b *logdriver.LogAttr) int {
		if c := cmp.Compare(a.Key, b.Key); c != 0 {
			return c
		}
		return cmp.Compare(a.Value, b.Value)
	})

	return &driver{
		logfile: lf,
		extra:   attrs,
	}, nil
}

func marshal(m *logger.Message, attrs []*logdriver.LogAttr, buffer *[]byte) error {
	var proto logdriver.LogEntry
	var md logdriver.PartialLogEntryMetadata
	messageToProto(m, attrs, &proto, &md)
	protoSize := proto.Size()
	writeLen := protoSize + (2 * encodeBinaryLen) // + len(messageDelimiter)

	buf := *buffer
	if writeLen > cap(buf) {
		// If we already need to reallocate the buffer, make it larger to be more reusable.
		// Round to the next power of two.
		capacity := 1 << (bits.Len(uint(writeLen)) + 1)

		buf = make([]byte, writeLen, capacity)
	} else {
		buf = buf[:writeLen]
	}
	*buffer = buf

	binary.BigEndian.PutUint32(buf[:encodeBinaryLen], uint32(protoSize))
	n, err := proto.MarshalTo(buf[encodeBinaryLen:writeLen])
	if err != nil {
		return errors.Wrap(err, "error marshaling log entry")
	}
	if n+(encodeBinaryLen*2) != writeLen {
		return io.ErrShortWrite
	}
	binary.BigEndian.PutUint32(buf[writeLen-encodeBinaryLen:writeLen], uint32(protoSize))
	return nil
}

func (d *driver) Name() string {
	return Name
}

func (d *driver) Log(msg *logger.Message) (err error) {
	defer func() {
		if err == nil {
			logger.PutMessage(msg)
		}
	}()

	buf := buffersPool.Get().(*[]byte)
	defer buffersPool.Put(buf)

	if err := marshal(msg, d.extra, buf); err != nil {
		return errors.Wrap(err, "error marshalling logger.Message")
	}
	return d.logfile.WriteLogEntry(msg.Timestamp, *buf)
}

func (d *driver) Close() error {
	return d.logfile.Close()
}

func messageToProto(msg *logger.Message, extra []*logdriver.LogAttr, proto *logdriver.LogEntry, partial *logdriver.PartialLogEntryMetadata) {
	proto.Source = msg.Source
	proto.TimeNano = msg.Timestamp.UnixNano()
	proto.Line = append(proto.Line[:0], msg.Line...)
	proto.Partial = msg.PLogMetaData != nil
	if proto.Partial {
		partial.Ordinal = int32(msg.PLogMetaData.Ordinal)
		partial.Last = msg.PLogMetaData.Last
		partial.Id = msg.PLogMetaData.ID
		proto.PartialLogMetadata = partial
	} else {
		proto.PartialLogMetadata = nil
	}
	if len(msg.Attrs) > 0 {
		// Per-message attributes (e.g. an exec's identity) are merged after
		// the static logger-level ones. A fresh slice is required: appending
		// to extra would mutate the shared backing array across messages.
		attrs := make([]*logdriver.LogAttr, 0, len(extra)+len(msg.Attrs))
		attrs = append(attrs, extra...)
		for _, a := range msg.Attrs {
			attrs = append(attrs, &logdriver.LogAttr{Key: a.Key, Value: a.Value})
		}
		proto.Attrs = attrs
	} else {
		proto.Attrs = extra
	}
}

func protoToMessage(proto *logdriver.LogEntry) *logger.Message {
	msg := &logger.Message{
		Source:    proto.Source,
		Timestamp: time.Unix(0, proto.TimeNano).UTC(),
	}
	if len(proto.Attrs) > 0 {
		msg.Attrs = make([]backend.LogAttr, len(proto.Attrs))
		for i, a := range proto.Attrs {
			msg.Attrs[i] = backend.LogAttr{
				Key:   a.Key,
				Value: a.Value,
			}
		}
	}
	if proto.Partial {
		msg.PLogMetaData = &backend.PartialLogMetaData{
			Last:    proto.GetPartialLogMetadata().GetLast(),
			ID:      proto.GetPartialLogMetadata().GetId(),
			Ordinal: int(proto.GetPartialLogMetadata().GetOrdinal()),
		}
	}
	msg.Line = append(msg.Line[:0], proto.Line...)
	return msg
}

func resetProto(proto *logdriver.LogEntry) {
	proto.Source = ""
	proto.Line = proto.Line[:0]
	proto.TimeNano = 0
	proto.Partial = false
	if proto.PartialLogMetadata != nil {
		proto.PartialLogMetadata.Id = ""
		proto.PartialLogMetadata.Last = false
		proto.PartialLogMetadata.Ordinal = 0
	}
	proto.PartialLogMetadata = nil
	proto.Attrs = proto.Attrs[:0]
}
