package container

import (
	"testing"

	"github.com/moby/moby/v2/daemon/logger"
	"github.com/moby/moby/v2/daemon/server/backend"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
)

// recordingLogger captures every message handed to Log.
type recordingLogger struct {
	msgs chan *logger.Message
}

func (l *recordingLogger) Log(msg *logger.Message) error {
	// Copy: the copier recycles messages through a pool after Log returns.
	m := logger.NewMessage()
	m.Line = append(m.Line, msg.Line...)
	m.Source = msg.Source
	m.Attrs = append(m.Attrs, msg.Attrs...)
	l.msgs <- m
	return nil
}

func (l *recordingLogger) Name() string { return "recording" }
func (l *recordingLogger) Close() error { return nil }

func TestExecLoggerStampsAttrs(t *testing.T) {
	rec := &recordingLogger{msgs: make(chan *logger.Message, 1)}
	l := &execLogger{Logger: rec, attrs: []backend.LogAttr{{Key: "exec_id", Value: "abc"}, {Key: "com.example.hook", Value: "post_start"}}}

	err := l.Log(&logger.Message{Line: []byte("hello"), Source: "stdout"})
	assert.NilError(t, err)
	msg := <-rec.msgs
	assert.Check(t, is.DeepEqual(msg.Attrs, []backend.LogAttr{
		{Key: "exec_id", Value: "abc"},
		{Key: "com.example.hook", Value: "post_start"},
	}))
}

// TestStartLogCapture exercises the full capture path: bytes written to the
// exec's stream reach the container's logging driver, stamped with the
// exec's identity, and CloseStreams waits for the copy to drain.
func TestStartLogCapture(t *testing.T) {
	rec := &recordingLogger{msgs: make(chan *logger.Message, 16)}
	ctr := &Container{State: &State{}}
	ctr.LogDriver = rec

	ec := NewExecConfig(ctr)
	ec.CaptureStdout = true
	ec.CaptureStderr = true
	ec.Labels = map[string]string{"com.example.hook": "post_start"}
	ec.startLogCapture()

	_, err := ec.StreamConfig.Stdout().Write([]byte("captured line\n"))
	assert.NilError(t, err)

	var msg *logger.Message
	poll.WaitOn(t, func(poll.LogT) poll.Result {
		select {
		case msg = <-rec.msgs:
			return poll.Success()
		default:
			return poll.Continue("no message captured yet")
		}
	})
	assert.Check(t, is.Equal(string(msg.Line), "captured line"))
	assert.Check(t, is.Equal(msg.Source, "exec-stdout"))
	assert.Check(t, is.DeepEqual(msg.Attrs, []backend.LogAttr{
		{Key: "exec_id", Value: ec.ID},
		{Key: "com.example.hook", Value: "post_start"},
	}))

	assert.NilError(t, ec.CloseStreams())
}

// TestStartLogCaptureStderrOnly verifies that capture is per stream: with
// only CaptureStderr set, stderr output is captured and stdout output is not.
func TestStartLogCaptureStderrOnly(t *testing.T) {
	rec := &recordingLogger{msgs: make(chan *logger.Message, 16)}
	ctr := &Container{State: &State{}}
	ctr.LogDriver = rec

	ec := NewExecConfig(ctr)
	ec.CaptureStderr = true
	ec.startLogCapture()

	_, err := ec.StreamConfig.Stdout().Write([]byte("ignored line\n"))
	assert.NilError(t, err)
	_, err = ec.StreamConfig.Stderr().Write([]byte("captured line\n"))
	assert.NilError(t, err)

	var msg *logger.Message
	poll.WaitOn(t, func(poll.LogT) poll.Result {
		select {
		case msg = <-rec.msgs:
			return poll.Success()
		default:
			return poll.Continue("no message captured yet")
		}
	})
	assert.Check(t, is.Equal(string(msg.Line), "captured line"))
	assert.Check(t, is.Equal(msg.Source, "exec-stderr"))

	assert.NilError(t, ec.CloseStreams())
	// the uncaptured stdout line never reached the driver
	select {
	case extra := <-rec.msgs:
		t.Fatalf("unexpected captured message: %q from %s", extra.Line, extra.Source)
	default:
	}
}

func TestStartLogCaptureWithoutDriver(t *testing.T) {
	ctr := &Container{State: &State{}}
	ec := NewExecConfig(ctr)
	ec.CaptureStdout = true
	ec.CaptureStderr = true
	// no logging driver on the container: capture is skipped, not fatal
	ec.startLogCapture()
	assert.Check(t, ec.LogCopier == nil)
	assert.NilError(t, ec.CloseStreams())
}
