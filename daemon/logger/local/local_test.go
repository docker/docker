package local

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	protoio "github.com/gogo/protobuf/io"
	"github.com/moby/moby/v2/daemon/logger"
	"github.com/moby/moby/v2/daemon/logger/internal/logdriver"
	"github.com/moby/moby/v2/daemon/logger/loggertest"
	"github.com/moby/moby/v2/daemon/server/backend"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestWriteLog(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test.log")

	l, err := New(logger.Info{
		LogPath: logPath,
		Config: map[string]string{
			logger.AttrLabels: "label1",
			logger.AttrLogTag: "custom-tag",
		},
		ContainerLabels: map[string]string{
			"label1": "value1",
		},
	})
	assert.NilError(t, err)
	t.Cleanup(func() { assert.NilError(t, l.Close()) })

	now := time.Now()
	messages := []logger.Message{
		{Source: "stdout", Timestamp: now.Add(-30 * time.Minute), Line: []byte("message 1")},
		{Source: "stdout", Timestamp: now.Add(-20 * time.Minute), Line: []byte("message 2"), PLogMetaData: &backend.PartialLogMetaData{Last: true, ID: "0001", Ordinal: 1}},
		{Source: "stderr", Timestamp: now.Add(-10 * time.Minute), Line: []byte("message 3")},
	}

	for i := range messages {
		// copy the log message because the underlying log writer resets the log message and returns it to a buffer pool
		err = l.Log(copyLogMessage(&messages[i]))
		assert.NilError(t, err)
	}

	f, err := os.Open(logPath)
	assert.NilError(t, err)
	t.Cleanup(func() { assert.NilError(t, f.Close()) })
	dec := protoio.NewUint32DelimitedReader(f, binary.BigEndian, 1e6)

	lenBuf := make([]byte, encodeBinaryLen)
	seekMsgLen := func() {
		_, err := io.ReadFull(f, lenBuf)
		assert.NilError(t, err)
	}

	for i := range messages {
		var got logdriver.LogEntry
		err = dec.ReadMsg(&got)
		assert.NilError(t, err)

		var want logdriver.LogEntry
		var partial logdriver.PartialLogEntryMetadata
		extraAttrs := []*logdriver.LogAttr{
			{Key: "label1", Value: "value1"},
			{Key: "tag", Value: "custom-tag"},
		}
		messageToProto(&messages[i], extraAttrs, &want, &partial)
		assert.Check(t, is.DeepEqual(want, got), "msg %d: expected:\n%+v\ngot:\n%+v", i, want, got)

		if i < len(messages)-1 {
			seekMsgLen()
		}
	}
}

func TestReadLog(t *testing.T) {
	r := loggertest.Reader{
		Factory: func(t *testing.T, info logger.Info) func(*testing.T) logger.Logger {
			dir := t.TempDir()
			info.LogPath = filepath.Join(dir, info.ContainerID+".log")
			return func(t *testing.T) logger.Logger {
				l, err := New(info)
				assert.NilError(t, err)
				return l
			}
		},
	}
	t.Run("Tail", r.TestTail)
	t.Run("Follow", r.TestFollow)
}

func BenchmarkLogWrite(b *testing.B) {
	tmpDir := b.TempDir()
	l, err := New(logger.Info{LogPath: filepath.Join(tmpDir, "test.log")})
	assert.NilError(b, err)
	b.Cleanup(func() { assert.NilError(b, l.Close()) })

	t := time.Now().UTC()
	for _, data := range [][]byte{
		[]byte(""),
		[]byte("a short string"),
		bytes.Repeat([]byte("a long string"), 100),
		bytes.Repeat([]byte("a really long string"), 10000),
	} {
		b.Run(strconv.Itoa(len(data)), func(b *testing.B) {
			entry := &logdriver.LogEntry{Line: data, Source: "stdout", TimeNano: t.UnixNano()}
			b.SetBytes(int64(entry.Size() + encodeBinaryLen + encodeBinaryLen))
			for b.Loop() {
				msg := logger.NewMessage()
				msg.Line = data
				msg.Timestamp = t
				msg.Source = "stdout"
				if err := l.Log(msg); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func copyLogMessage(src *logger.Message) *logger.Message {
	dst := logger.NewMessage()
	dst.Source = src.Source
	dst.Timestamp = src.Timestamp
	dst.Attrs = src.Attrs
	dst.Err = src.Err
	dst.Line = append(dst.Line, src.Line...)
	if src.PLogMetaData != nil {
		lmd := *src.PLogMetaData
		dst.PLogMetaData = &lmd
	}
	return dst
}

// Per-message attributes must be merged after the logger-level static ones
// without mutating the shared static slice.
func TestMessageToProtoMergesPerMessageAttrs(t *testing.T) {
	static := []*logdriver.LogAttr{{Key: "static", Value: "yes"}}

	var proto logdriver.LogEntry
	var md logdriver.PartialLogEntryMetadata
	msg := logger.Message{
		Line:   []byte("captured"),
		Source: "stdout",
		Attrs:  []backend.LogAttr{{Key: "exec_id", Value: "abc123"}},
	}
	messageToProto(&msg, static, &proto, &md)

	assert.Assert(t, is.Len(proto.Attrs, 2))
	assert.Check(t, is.Equal(proto.Attrs[0].Key, "static"))
	assert.Check(t, is.Equal(proto.Attrs[1].Key, "exec_id"))
	assert.Check(t, is.Equal(proto.Attrs[1].Value, "abc123"))
	// the shared static slice must be left untouched
	assert.Assert(t, is.Len(static, 1))

	// without per-message attrs, the static slice is used as-is
	var proto2 logdriver.LogEntry
	messageToProto(&logger.Message{Line: []byte("plain"), Source: "stdout"}, static, &proto2, &md)
	assert.Assert(t, is.Len(proto2.Attrs, 1))
}
