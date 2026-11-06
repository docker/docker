package jsonfilelog

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/moby/moby/v2/daemon/logger"
	"github.com/moby/moby/v2/daemon/logger/jsonfilelog/jsonlog"
	"github.com/moby/moby/v2/daemon/server/backend"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestJSONFileLogger(t *testing.T) {
	const cid = "a7317399f3f857173c6179d44823594f8294678dea9999662e5c625b5a1c7657"
	tmpDir := t.TempDir()
	filename := filepath.Join(tmpDir, "container.log")
	l, err := New(logger.Info{
		ContainerID: cid,
		LogPath:     filename,
	})
	assert.NilError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	if err := l.Log(&logger.Message{Line: []byte("line1"), Source: "src1"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Log(&logger.Message{Line: []byte("line2"), Source: "src2"}); err != nil {
		t.Fatal(err)
	}
	if err := l.Log(&logger.Message{Line: []byte("line3"), Source: "src3"}); err != nil {
		t.Fatal(err)
	}
	res, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"log":"line1\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line2\n","stream":"src2","time":"0001-01-01T00:00:00Z"}
{"log":"line3\n","stream":"src3","time":"0001-01-01T00:00:00Z"}
`

	if string(res) != expected {
		t.Fatalf("Wrong log content: %q, expected %q", res, expected)
	}
}

func TestJSONFileLoggerWithTags(t *testing.T) {
	const cid = "a7317399f3f857173c6179d44823594f8294678dea9999662e5c625b5a1c7657"
	const cname = "test-container"
	tmpDir := t.TempDir()
	filename := filepath.Join(tmpDir, "container.log")
	l, err := New(logger.Info{
		Config: map[string]string{
			logger.AttrLogTag: "{{.ID}}/{{.Name}}", // first 12 characters of ContainerID and full ContainerName
		},
		ContainerID:   cid,
		ContainerName: cname,
		LogPath:       filename,
	})

	assert.NilError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	err = l.Log(&logger.Message{Line: []byte("line1"), Source: "src1"})
	assert.NilError(t, err)

	err = l.Log(&logger.Message{Line: []byte("line2"), Source: "src2"})
	assert.NilError(t, err)

	err = l.Log(&logger.Message{Line: []byte("line3"), Source: "src3"})
	assert.NilError(t, err)

	res, err := os.ReadFile(filename)
	assert.NilError(t, err)

	expected := `{"log":"line1\n","stream":"src1","attrs":{"tag":"a7317399f3f8/test-container"},"time":"0001-01-01T00:00:00Z"}
{"log":"line2\n","stream":"src2","attrs":{"tag":"a7317399f3f8/test-container"},"time":"0001-01-01T00:00:00Z"}
{"log":"line3\n","stream":"src3","attrs":{"tag":"a7317399f3f8/test-container"},"time":"0001-01-01T00:00:00Z"}
`
	assert.Check(t, is.Equal(expected, string(res)))
}

func BenchmarkJSONFileLoggerLog(b *testing.B) {
	tmpDir := b.TempDir()

	jsonlogger, err := New(logger.Info{
		ContainerID: "a7317399f3f857173c6179d44823594f8294678dea9999662e5c625b5a1c7657",
		LogPath:     filepath.Join(tmpDir, "container.log"),
		Config: map[string]string{
			"max-file": "10",
			"compress": "true",
			"max-size": "20m",

			logger.AttrLabels: "first,second",
		},
		ContainerLabels: map[string]string{
			"first":  "label_value",
			"second": "label_foo",
		},
	})
	assert.NilError(b, err)
	b.Cleanup(func() { _ = jsonlogger.Close() })

	t := time.Now().UTC()
	for _, data := range [][]byte{
		[]byte(""),
		[]byte("a short string"),
		bytes.Repeat([]byte("a long string"), 100),
		bytes.Repeat([]byte("a really long string"), 10000),
	} {
		b.Run(strconv.Itoa(len(data)), func(b *testing.B) {
			testMsg := &logger.Message{
				Line:      data,
				Source:    "stderr",
				Timestamp: t,
			}

			buf := bytes.NewBuffer(nil)
			assert.NilError(b, marshalMessage(testMsg, nil, buf))
			b.SetBytes(int64(buf.Len()))
			b.ResetTimer()
			for b.Loop() {
				msg := logger.NewMessage()
				msg.Line = testMsg.Line
				msg.Timestamp = testMsg.Timestamp
				msg.Source = testMsg.Source
				if err := jsonlogger.Log(msg); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestJSONFileLoggerWithOpts(t *testing.T) {
	const cid = "a7317399f3f857173c6179d44823594f8294678dea9999662e5c625b5a1c7657"

	tmpDir := t.TempDir()
	filename := filepath.Join(tmpDir, "container.log")
	config := map[string]string{"max-file": "3", "max-size": "1k", "compress": "true"}
	l, err := New(logger.Info{
		ContainerID: cid,
		LogPath:     filename,
		Config:      config,
	})
	assert.NilError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	for i := range 36 {
		if err := l.Log(&logger.Message{Line: []byte("line" + strconv.Itoa(i)), Source: "src1"}); err != nil {
			t.Fatal(err)
		}
	}

	res, err := os.ReadFile(filename)
	assert.NilError(t, err)

	penUlt, err := os.ReadFile(filename + ".1")
	if err != nil {
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}

		penUlt = readGzipFile(t, filename+".1.gz")
	}

	antepenult := readGzipFile(t, filename+".2.gz")

	expectedAntepenultimate := `{"log":"line0\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line1\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line2\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line3\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line4\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line5\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line6\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line7\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line8\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line9\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line10\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line11\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line12\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line13\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line14\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line15\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
`
	expectedPenultimate := `{"log":"line16\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line17\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line18\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line19\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line20\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line21\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line22\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line23\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line24\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line25\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line26\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line27\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line28\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line29\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line30\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line31\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
`
	expected := `{"log":"line32\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line33\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line34\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
{"log":"line35\n","stream":"src1","time":"0001-01-01T00:00:00Z"}
`

	assert.Check(t, is.Equal(string(res), expected), "Wrong log content")
	assert.Check(t, is.Equal(string(penUlt), expectedPenultimate), "Wrong log content")
	assert.Check(t, is.Equal(string(antepenult), expectedAntepenultimate), "Wrong log content")
}

func readGzipFile(t *testing.T, name string) []byte {
	t.Helper()

	file, err := os.Open(name)
	assert.NilError(t, err)

	gz, err := gzip.NewReader(file)
	assert.NilError(t, err)

	b, err := io.ReadAll(gz)
	assert.NilError(t, err)

	assert.NilError(t, gz.Close())
	assert.NilError(t, file.Close())

	return b
}

func TestJSONFileLoggerWithLabelsEnv(t *testing.T) {
	cid := "a7317399f3f857173c6179d44823594f8294678dea9999662e5c625b5a1c7657"
	tmpDir := t.TempDir()
	filename := filepath.Join(tmpDir, "container.log")
	l, err := New(logger.Info{
		ContainerID: cid,
		LogPath:     filename,
		Config: map[string]string{
			logger.AttrLabels:      "rack,dc",
			logger.AttrLabelsRegex: "^loc",
			logger.AttrEnv:         "environ,debug,ssl",
			logger.AttrEnvRegex:    "^dc",
		},
		ContainerLabels: map[string]string{"rack": "101", "dc": "lhr", "location": "here"},
		ContainerEnv:    []string{"environ=production", "debug=false", "port=10001", "ssl=true", "dc_region=west"},
	})
	assert.NilError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	err = l.Log(&logger.Message{Line: []byte("line"), Source: "src1"})
	assert.NilError(t, err)

	res, err := os.ReadFile(filename)
	assert.NilError(t, err)

	var jsonLog jsonlog.JSONLogs
	err = json.Unmarshal(res, &jsonLog)
	assert.NilError(t, err)

	extra := make(map[string]string)
	err = json.Unmarshal(jsonLog.RawAttrs, &extra)
	assert.NilError(t, err)

	expected := map[string]string{
		"rack":      "101",
		"dc":        "lhr",
		"location":  "here",
		"environ":   "production",
		"debug":     "false",
		"ssl":       "true",
		"dc_region": "west",
	}
	assert.Check(t, is.DeepEqual(extra, expected))
}

// Per-message attributes must be merged over the logger-level static ones,
// and messages without attributes must keep the zero-cost static path.
func TestMarshalMessageMergesPerMessageAttrs(t *testing.T) {
	extra := json.RawMessage(`{"static":"yes"}`)

	var buf bytes.Buffer
	msg := &logger.Message{
		Line:      []byte("captured"),
		Source:    "stdout",
		Timestamp: time.Unix(0, 0).UTC(),
		Attrs: []backend.LogAttr{
			{Key: "exec_id", Value: "abc123"},
			{Key: "com.example.hook", Value: "post_start"},
		},
	}
	assert.NilError(t, marshalMessage(msg, extra, &buf))

	var entry jsonlog.JSONLog
	assert.NilError(t, json.Unmarshal(buf.Bytes(), &entry))
	assert.DeepEqual(t, entry.Attrs, map[string]string{
		"static":           "yes",
		"exec_id":          "abc123",
		"com.example.hook": "post_start",
	})

	// without per-message attrs, the static raw attributes are passed through
	buf.Reset()
	assert.NilError(t, marshalMessage(&logger.Message{Line: []byte("plain"), Source: "stdout"}, extra, &buf))
	entry = jsonlog.JSONLog{}
	assert.NilError(t, json.Unmarshal(buf.Bytes(), &entry))
	assert.DeepEqual(t, entry.Attrs, map[string]string{"static": "yes"})
}
