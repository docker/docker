package jsonfilelog

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/moby/moby/v2/daemon/logger"
	"github.com/moby/moby/v2/daemon/logger/loggertest"
	"gotest.tools/v3/assert"
)

func BenchmarkJSONFileLoggerReadLogs(b *testing.B) {
	tmp := b.TempDir()

	jsonlogger, err := New(logger.Info{
		ContainerID: "a7317399f3f857173c6179d44823594f8294678dea9999662e5c625b5a1c7657",
		LogPath:     filepath.Join(tmp, "container.log"),
		Config: map[string]string{
			logger.AttrLabels: "first,second",
		},
		ContainerLabels: map[string]string{
			"first":  "label_value",
			"second": "label_foo",
		},
	})
	assert.NilError(b, err)
	defer jsonlogger.Close()

	const line = "Line that thinks that it is log line from docker\n"
	ts := time.Date(2007, 1, 2, 3, 4, 5, 6, time.UTC)
	msg := func() *logger.Message {
		m := logger.NewMessage()
		m.Line = append(m.Line, line...)
		m.Source = "stderr"
		m.Timestamp = ts
		return m
	}

	var buf bytes.Buffer
	assert.NilError(b, marshalMessage(msg(), nil, &buf))
	b.SetBytes(int64(buf.Len()))

	b.ResetTimer()

	chError := make(chan error)
	go func() {
		for i := 0; i < b.N; i++ {
			if err := jsonlogger.Log(msg()); err != nil {
				chError <- err
			}
		}
		if err := jsonlogger.Close(); err != nil {
			chError <- err
		}
	}()

	lw := jsonlogger.(*JSONFileLogger).ReadLogs(b.Context(), logger.ReadConfig{Follow: true})
	for {
		select {
		case _, ok := <-lw.Msg:
			if !ok {
				return
			}
		case err := <-chError:
			b.Fatal(err)
		}
	}
}

func TestEncodeDecode(t *testing.T) {
	t.Parallel()

	m1 := &logger.Message{Line: []byte("hello 1"), Timestamp: time.Now(), Source: "stdout"}
	m2 := &logger.Message{Line: []byte("hello 2"), Timestamp: time.Now(), Source: "stdout"}
	m3 := &logger.Message{Line: []byte("hello 3"), Timestamp: time.Now(), Source: "stdout"}

	buf := bytes.NewBuffer(nil)
	assert.Assert(t, marshalMessage(m1, nil, buf))
	assert.Assert(t, marshalMessage(m2, nil, buf))
	assert.Assert(t, marshalMessage(m3, nil, buf))

	dec := decodeFunc(buf)
	defer dec.Close()

	msg, err := dec.Decode()
	assert.NilError(t, err)
	assert.Assert(t, string(msg.Line) == "hello 1\n", string(msg.Line))

	msg, err = dec.Decode()
	assert.NilError(t, err)
	assert.Assert(t, string(msg.Line) == "hello 2\n")

	msg, err = dec.Decode()
	assert.NilError(t, err)
	assert.Assert(t, string(msg.Line) == "hello 3\n")

	_, err = dec.Decode()
	assert.Assert(t, errors.Is(err, io.EOF))
}

func TestDecodeNULBytes(t *testing.T) {
	t.Parallel()

	t.Run("PureNULGap", func(t *testing.T) {
		t.Parallel()
		m1 := &logger.Message{Line: []byte("hello 1"), Timestamp: time.Now(), Source: "stdout"}
		m2 := &logger.Message{Line: []byte("hello 2"), Timestamp: time.Now(), Source: "stdout"}

		buf := bytes.NewBuffer(nil)
		assert.Assert(t, marshalMessage(m1, nil, buf))
		// Corrupt gap of NUL bytes (e.g. from an unclean shutdown / sparse allocation)
		buf.WriteString("\x00\x00\x00\x00\x00\x00\x00\x00\n")
		assert.Assert(t, marshalMessage(m2, nil, buf))

		dec := decodeFunc(buf)
		defer dec.Close()

		msg, err := dec.Decode()
		assert.NilError(t, err)
		assert.Assert(t, string(msg.Line) == "hello 1\n", string(msg.Line))

		msg, err = dec.Decode()
		assert.NilError(t, err)
		assert.Assert(t, string(msg.Line) == "hello 2\n", string(msg.Line))

		_, err = dec.Decode()
		assert.Assert(t, errors.Is(err, io.EOF))
	})

	t.Run("MidRecordNULGapSwallowingNewline", func(t *testing.T) {
		t.Parallel()
		m1 := &logger.Message{Line: []byte("hello 1"), Timestamp: time.Now(), Source: "stdout"}
		m2 := &logger.Message{Line: []byte("hello 2"), Timestamp: time.Now(), Source: "stdout"}
		m3 := &logger.Message{Line: []byte("hello 3"), Timestamp: time.Now(), Source: "stdout"}
		m4 := &logger.Message{Line: []byte("hello 4"), Timestamp: time.Now(), Source: "stdout"}

		var buf2 bytes.Buffer
		assert.Assert(t, marshalMessage(m2, nil, &buf2))
		// Simulate power loss cutting m2 mid-write, with NULs replacing the rest
		// of m2 and its newline, followed immediately by intact m3.
		mid := buf2.Len() / 2
		partialM2 := buf2.Bytes()[:mid]

		buf := bytes.NewBuffer(nil)
		assert.Assert(t, marshalMessage(m1, nil, buf))
		buf.Write(partialM2)
		buf.WriteString("\x00\x00\x00\x00\x00\x00\x00\x00")
		assert.Assert(t, marshalMessage(m3, nil, buf))
		assert.Assert(t, marshalMessage(m4, nil, buf))

		dec := decodeFunc(buf)
		defer dec.Close()

		msg, err := dec.Decode()
		assert.NilError(t, err)
		assert.Assert(t, string(msg.Line) == "hello 1\n", string(msg.Line))

		// m3 should be recovered following the NUL gap despite sharing the line with partial m2
		msg, err = dec.Decode()
		assert.NilError(t, err)
		assert.Assert(t, string(msg.Line) == "hello 3\n", string(msg.Line))

		msg, err = dec.Decode()
		assert.NilError(t, err)
		assert.Assert(t, string(msg.Line) == "hello 4\n", string(msg.Line))

		_, err = dec.Decode()
		assert.Assert(t, errors.Is(err, io.EOF))
	})
}

func TestDecodeEdgeCases(t *testing.T) {
	t.Parallel()

	t.Run("AllNULBytes", func(t *testing.T) {
		t.Parallel()
		buf := bytes.NewBufferString("\x00\x00\x00\x00\x00\x00\x00\x00\n\x00\x00\x00\x00")
		dec := decodeFunc(buf)
		defer dec.Close()

		_, err := dec.Decode()
		assert.Assert(t, errors.Is(err, io.EOF))
	})

	t.Run("LeadingTrailingNULAndCorruptedLine", func(t *testing.T) {
		t.Parallel()
		m1 := &logger.Message{Line: []byte("hello 1"), Timestamp: time.Now(), Source: "stdout"}
		m2 := &logger.Message{Line: []byte("hello 2"), Timestamp: time.Now(), Source: "stdout"}
		m3 := &logger.Message{Line: []byte("hello 3"), Timestamp: time.Now(), Source: "stdout"}

		var buf1 bytes.Buffer
		assert.Assert(t, marshalMessage(m1, nil, &buf1))
		var buf2 bytes.Buffer
		assert.Assert(t, marshalMessage(m2, nil, &buf2))
		var buf3 bytes.Buffer
		assert.Assert(t, marshalMessage(m3, nil, &buf3))

		combined := bytes.NewBuffer(nil)
		// valid m1
		combined.Write(buf1.Bytes())
		// multiple consecutive NUL lines
		combined.WriteString("\x00\x00\x00\x00\n\x00\x00\x00\x00\x00\n")
		// corrupted line with non-JSON text
		combined.WriteString("this is corrupted garbage\n")
		// valid m2 surrounded by NULs
		combined.WriteString("\x00\x00")
		combined.Write(bytes.TrimRight(buf2.Bytes(), "\n"))
		combined.WriteString("\x00\n")
		// valid m3 without trailing newline at EOF
		combined.Write(bytes.TrimRight(buf3.Bytes(), "\n"))

		dec := decodeFunc(combined)
		defer dec.Close()

		msg, err := dec.Decode()
		assert.NilError(t, err)
		assert.Assert(t, string(msg.Line) == "hello 1\n")

		msg, err = dec.Decode()
		assert.NilError(t, err)
		assert.Assert(t, string(msg.Line) == "hello 2\n")

		msg, err = dec.Decode()
		assert.NilError(t, err)
		assert.Assert(t, string(msg.Line) == "hello 3\n")

		_, err = dec.Decode()
		assert.Assert(t, errors.Is(err, io.EOF))
	})
}

func TestReadLogsNULBytesResynchronization(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "container.log")

	info := logger.Info{
		ContainerID: "testcontainerid123456",
		LogPath:     logPath,
	}

	l, err := New(info)
	assert.NilError(t, err)

	now := time.Now().UTC().Truncate(time.Millisecond)
	var messages []*logger.Message
	for i := 0; i < 5; i++ {
		msg := &logger.Message{
			Source:    "stdout",
			Timestamp: now.Add(time.Duration(i) * time.Second),
			Line:      []byte(fmt.Sprintf("message %d", i+1)),
		}
		messages = append(messages, msg)
		assert.NilError(t, l.Log(msg))
	}
	assert.NilError(t, l.Close())

	// Read file content and corrupt message 3 with NUL bytes
	content, err := os.ReadFile(logPath)
	assert.NilError(t, err)

	lines := bytes.Split(content, []byte("\n"))
	assert.Assert(t, len(lines) >= 5)
	for i := range lines[2] {
		lines[2][i] = 0
	}
	corruptedContent := bytes.Join(lines, []byte("\n"))
	assert.NilError(t, os.WriteFile(logPath, corruptedContent, 0o640))

	readerLogger, err := New(info)
	assert.NilError(t, err)
	defer readerLogger.Close()
	lr := readerLogger.(logger.LogReader)

	t.Run("FullRead", func(t *testing.T) {
		lw := lr.ReadLogs(t.Context(), logger.ReadConfig{Tail: -1})
		defer lw.ConsumerGone()

		var readLines []string
		for msg := range lw.Msg {
			readLines = append(readLines, string(msg.Line))
		}
		expected := []string{"message 1\n", "message 2\n", "message 4\n", "message 5\n"}
		assert.DeepEqual(t, readLines, expected)
	})

	t.Run("SinceAcrossNULGap", func(t *testing.T) {
		lw := lr.ReadLogs(t.Context(), logger.ReadConfig{
			Tail:  -1,
			Since: now.Add(3 * time.Second), // timestamp of message 4
		})
		defer lw.ConsumerGone()

		var readLines []string
		for msg := range lw.Msg {
			readLines = append(readLines, string(msg.Line))
		}
		expected := []string{"message 4\n", "message 5\n"}
		assert.DeepEqual(t, readLines, expected)
	})

	t.Run("TailAcrossNULGap", func(t *testing.T) {
		lw := lr.ReadLogs(t.Context(), logger.ReadConfig{Tail: 4})
		defer lw.ConsumerGone()

		var readLines []string
		for msg := range lw.Msg {
			readLines = append(readLines, string(msg.Line))
		}
		expected := []string{"message 2\n", "message 4\n", "message 5\n"}
		assert.DeepEqual(t, readLines, expected)
	})
}

func TestReadLogsMidRecordNULBytesResynchronization(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "container.log")

	info := logger.Info{
		ContainerID: "testcontainerid123456",
		LogPath:     logPath,
	}

	l, err := New(info)
	assert.NilError(t, err)

	now := time.Now().UTC().Truncate(time.Millisecond)
	for i := 0; i < 5; i++ {
		msg := &logger.Message{
			Source:    "stdout",
			Timestamp: now.Add(time.Duration(i) * time.Second),
			Line:      []byte(fmt.Sprintf("message %d", i+1)),
		}
		assert.NilError(t, l.Log(msg))
	}
	assert.NilError(t, l.Close())

	// Read file content and corrupt message 3 mid-record through its newline
	content, err := os.ReadFile(logPath)
	assert.NilError(t, err)

	lines := bytes.Split(content, []byte("\n"))
	assert.Assert(t, len(lines) >= 5)

	// Keep message 3 up to its midpoint, then replace the rest of message 3
	// and its newline with NUL bytes up to message 4.
	mid := len(lines[2]) / 2
	nulCount := len(lines[2]) - mid + 1 // +1 for the newline between lines[2] and lines[3]

	var corrupted bytes.Buffer
	corrupted.Write(lines[0])
	corrupted.WriteByte('\n')
	corrupted.Write(lines[1])
	corrupted.WriteByte('\n')
	corrupted.Write(lines[2][:mid])
	corrupted.Write(bytes.Repeat([]byte{0}, nulCount))
	corrupted.Write(lines[3])
	corrupted.WriteByte('\n')
	corrupted.Write(lines[4])
	corrupted.WriteByte('\n')

	assert.Equal(t, corrupted.Len(), len(content))
	assert.NilError(t, os.WriteFile(logPath, corrupted.Bytes(), 0o640))

	readerLogger, err := New(info)
	assert.NilError(t, err)
	defer readerLogger.Close()
	lr := readerLogger.(logger.LogReader)

	t.Run("FullRead", func(t *testing.T) {
		lw := lr.ReadLogs(t.Context(), logger.ReadConfig{Tail: -1})
		defer lw.ConsumerGone()

		var readLines []string
		for msg := range lw.Msg {
			readLines = append(readLines, string(msg.Line))
		}
		expected := []string{"message 1\n", "message 2\n", "message 4\n", "message 5\n"}
		assert.DeepEqual(t, readLines, expected)
	})

	t.Run("SinceAcrossNULGap", func(t *testing.T) {
		lw := lr.ReadLogs(t.Context(), logger.ReadConfig{
			Tail:  -1,
			Since: now.Add(3 * time.Second), // timestamp of message 4
		})
		defer lw.ConsumerGone()

		var readLines []string
		for msg := range lw.Msg {
			readLines = append(readLines, string(msg.Line))
		}
		expected := []string{"message 4\n", "message 5\n"}
		assert.DeepEqual(t, readLines, expected)
	})

	t.Run("TailAcrossNULGap", func(t *testing.T) {
		// The corrupted file has 4 physical lines because the gap swallowed
		// the newline between message 3 and 4. Tail: 3 reads the last 3
		// physical lines, which recovers message 4 following the NUL gap.
		lw := lr.ReadLogs(t.Context(), logger.ReadConfig{Tail: 3})
		defer lw.ConsumerGone()

		var readLines []string
		for msg := range lw.Msg {
			readLines = append(readLines, string(msg.Line))
		}
		expected := []string{"message 2\n", "message 4\n", "message 5\n"}
		assert.DeepEqual(t, readLines, expected)
	})
}

func TestReadLogs(t *testing.T) {
	t.Parallel()
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

func TestTailLogsWithRotation(t *testing.T) {
	t.Parallel()
	compress := func(cmprs bool) {
		t.Run(fmt.Sprintf("compress=%v", cmprs), func(t *testing.T) {
			t.Parallel()
			(&loggertest.Reader{
				Factory: func(t *testing.T, info logger.Info) func(*testing.T) logger.Logger {
					info.Config = map[string]string{
						"compress": strconv.FormatBool(cmprs),
						"max-size": "1b",
						"max-file": "10",
					}
					dir := t.TempDir()
					t.Cleanup(func() {
						t.Logf("%s:\n%s", t.Name(), dirStringer{dir})
					})
					info.LogPath = filepath.Join(dir, info.ContainerID+".log")
					return func(t *testing.T) logger.Logger {
						l, err := New(info)
						assert.NilError(t, err)
						return l
					}
				},
			}).TestTail(t)
		})
	}
	compress(true)
	compress(false)
}

func TestFollowLogsWithRotation(t *testing.T) {
	t.Parallel()
	compress := func(cmprs bool) {
		t.Run(fmt.Sprintf("compress=%v", cmprs), func(t *testing.T) {
			t.Parallel()
			(&loggertest.Reader{
				Factory: func(t *testing.T, info logger.Info) func(*testing.T) logger.Logger {
					// The log follower can fall behind and drop logs if there are too many
					// rotations in a short time. If that was to happen, loggertest would fail the
					// test. Configure the logger so that there will be only one rotation with the
					// set of logs that loggertest writes.
					info.Config = map[string]string{
						"compress": strconv.FormatBool(cmprs),
						"max-size": "4096b",
						"max-file": "3",
					}
					dir := t.TempDir()
					t.Cleanup(func() {
						t.Logf("%s:\n%s", t.Name(), dirStringer{dir})
					})
					info.LogPath = filepath.Join(dir, info.ContainerID+".log")
					return func(t *testing.T) logger.Logger {
						l, err := New(info)
						assert.NilError(t, err)
						return l
					}
				},
			}).TestFollow(t)
		})
	}
	compress(true)
	compress(false)
}

type dirStringer struct {
	d string
}

func (d dirStringer) String() string {
	ls, err := os.ReadDir(d.d)
	if err != nil {
		return ""
	}
	buf := bytes.NewBuffer(nil)
	tw := tabwriter.NewWriter(buf, 1, 8, 1, '\t', 0)
	buf.WriteString("\n")

	btw := bufio.NewWriter(tw)

	for _, entry := range ls {
		fi, err := entry.Info()
		if err != nil {
			return ""
		}

		fmt.Fprintf(btw, "%s\t%s\t%dB\t%s\n", fi.Name(), fi.Mode(), fi.Size(), fi.ModTime())
	}
	btw.Flush()
	tw.Flush()
	return buf.String()
}
