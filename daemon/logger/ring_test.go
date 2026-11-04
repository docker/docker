package logger

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

type mockLogger struct{ c chan *Message }

func (l *mockLogger) Log(msg *Message) error {
	l.c <- msg
	return nil
}

func (l *mockLogger) Name() string {
	return "mock"
}

func (l *mockLogger) Close() error {
	return nil
}

func TestRingLogger(t *testing.T) {
	mockLog := &mockLogger{make(chan *Message)} // no buffer on this channel
	ring := newRingLogger(mockLog, Info{}, 1)
	defer ring.Close()

	// this should never block
	ring.Log(&Message{Line: []byte("1")})
	ring.Log(&Message{Line: []byte("2")})
	ring.Log(&Message{Line: []byte("3")})

	select {
	case msg := <-mockLog.c:
		if string(msg.Line) != "1" {
			t.Fatalf("got unexpected msg: %q", string(msg.Line))
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout reading log message")
	}

	select {
	case msg := <-mockLog.c:
		t.Fatalf("expected no more messages in the queue, got: %q", string(msg.Line))
	default:
	}
}

type blockingMockLogger struct {
	closeOnce      sync.Once
	closed         chan struct{}
	closeCnt       int
	mu             sync.Mutex
	startedLog     chan struct{}
	startedLogOnce sync.Once
}

func newBlockingMockLogger() *blockingMockLogger {
	return &blockingMockLogger{
		closed:     make(chan struct{}),
		startedLog: make(chan struct{}),
	}
}

func (l *blockingMockLogger) Log(msg *Message) error {
	// Signal that Log has been entered and is blocking
	l.startedLogOnce.Do(func() {
		close(l.startedLog)
	})

	select {
	case <-l.closed: // block until Close is called
		return errors.New("logger closed")
	case <-time.After(5 * time.Second):
		// fallback to avoid infinite hangs in bad tests
		return errors.New("timeout")
	}
}

func (l *blockingMockLogger) Name() string {
	return "blockingMock"
}

func (l *blockingMockLogger) Close() error {
	l.mu.Lock()
	l.closeCnt++
	l.mu.Unlock()
	l.closeOnce.Do(func() {
		close(l.closed)
	})
	return nil
}

func TestRingLoggerCloseTimeout(t *testing.T) {
	origTimeout := ringLoggerTimeout
	ringLoggerTimeout = 50 * time.Millisecond
	defer func() { ringLoggerTimeout = origTimeout }()

	mockLog := newBlockingMockLogger()
	ring := newRingLogger(mockLog, Info{}, 1)

	// Add a message to trigger the block in Log()
	ring.Log(&Message{Line: []byte("1")})

	// Wait for run() to actually enter Log() and block
	<-mockLog.startedLog

	// Now that run() is solidly blocked, trigger Close.
	// This will hit the timeout, forcefully close the underlying driver,
	// which unblocks Log(), allowing run() to exit naturally.
	done := make(chan struct{})
	go func() {
		ring.Close()
		close(done)
	}()

	select {
	case <-done:
		// ring.Close() cannot complete through its normal shutdown path because
		// run() is blocked inside Log(). The only code path that can unblock Log()
		// before run() exits is the timeout path, which force-closes the underlying
		// logger. Therefore, successful completion of this test demonstrates that
		// the timeout path executed.
	case <-time.After(1 * time.Second):
		t.Fatal("timeout waiting for ring.Close()")
	}

	mockLog.mu.Lock()
	defer mockLog.mu.Unlock()
	if mockLog.closeCnt != 1 {
		t.Fatalf("expected Close to be called exactly once, got %d", mockLog.closeCnt)
	}
}

func TestRingCap(t *testing.T) {
	r := newRing(5)
	for i := range 10 {
		// queue messages with "0" to "10"
		// the "5" to "10" messages should be dropped since we only allow 5 bytes in the buffer
		if err := r.Enqueue(&Message{Line: []byte(strconv.Itoa(i))}); err != nil {
			t.Fatal(err)
		}
	}

	// should have messages in the queue for "0" to "4"
	for i := range 5 {
		m, err := r.Dequeue()
		if err != nil {
			t.Fatal(err)
		}
		if string(m.Line) != strconv.Itoa(i) {
			t.Fatalf("got unexpected message for iter %d: %s", i, string(m.Line))
		}
	}

	// queue a message that's bigger than the buffer cap
	if err := r.Enqueue(&Message{Line: []byte("hello world")}); err != nil {
		t.Fatal(err)
	}

	// queue another message that's bigger than the buffer cap
	if err := r.Enqueue(&Message{Line: []byte("eat a banana")}); err != nil {
		t.Fatal(err)
	}

	m, err := r.Dequeue()
	if err != nil {
		t.Fatal(err)
	}
	if string(m.Line) != "hello world" {
		t.Fatalf("got unexpected message: %s", string(m.Line))
	}
	if len(r.queue) != 0 {
		t.Fatalf("expected queue to be empty, got: %d", len(r.queue))
	}
}

func TestRingClose(t *testing.T) {
	r := newRing(1)
	if err := r.Enqueue(&Message{Line: []byte("hello")}); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if err := r.Enqueue(&Message{}); !errors.Is(err, errClosed) {
		t.Fatalf("expected errClosed, got: %v", err)
	}
	if len(r.queue) != 1 {
		t.Fatal("expected empty queue")
	}
	if m, err := r.Dequeue(); err == nil || m != nil {
		t.Fatal("expected err on Dequeue after close")
	}

	ls := r.Drain()
	if len(ls) != 1 {
		t.Fatalf("expected one message: %v", ls)
	}
	if string(ls[0].Line) != "hello" {
		t.Fatalf("got unexpected message: %s", string(ls[0].Line))
	}
}

func TestRingDrain(t *testing.T) {
	r := newRing(5)
	for i := range 5 {
		if err := r.Enqueue(&Message{Line: []byte(strconv.Itoa(i))}); err != nil {
			t.Fatal(err)
		}
	}

	ls := r.Drain()
	if len(ls) != 5 {
		t.Fatal("got unexpected length after drain")
	}

	for i := range 5 {
		if string(ls[i].Line) != strconv.Itoa(i) {
			t.Fatalf("got unexpected message at position %d: %s", i, string(ls[i].Line))
		}
	}
	if r.sizeBytes != 0 {
		t.Fatalf("expected buffer size to be 0 after drain, got: %d", r.sizeBytes)
	}

	ls = r.Drain()
	if len(ls) != 0 {
		t.Fatalf("expected 0 messages on 2nd drain: %v", ls)
	}
}

type nopLogger struct{}

func (nopLogger) Name() string       { return "nopLogger" }
func (nopLogger) Close() error       { return nil }
func (nopLogger) Log(*Message) error { return nil }

func BenchmarkRingLoggerThroughputNoReceiver(b *testing.B) {
	mockLog := &mockLogger{make(chan *Message)}
	defer mockLog.Close()
	l := NewRingLogger(mockLog, Info{}, -1)
	msg := &Message{Line: []byte("hello humans and everyone else!")}
	b.SetBytes(int64(len(msg.Line)))

	for b.Loop() {
		if err := l.Log(msg); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRingLoggerThroughputWithReceiverDelay0(b *testing.B) {
	l := NewRingLogger(nopLogger{}, Info{}, -1)
	msg := &Message{Line: []byte("hello humans and everyone else!")}
	b.SetBytes(int64(len(msg.Line)))

	for b.Loop() {
		if err := l.Log(msg); err != nil {
			b.Fatal(err)
		}
	}
}

func consumeWithDelay(delay time.Duration, c <-chan *Message) (cancel func()) {
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		close(started)
		ticker := time.NewTicker(delay)
		for range ticker.C {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-c:
			}
		}
	}()
	<-started
	return cancel
}

func BenchmarkRingLoggerThroughputConsumeDelay1(b *testing.B) {
	mockLog := &mockLogger{make(chan *Message)}
	defer mockLog.Close()
	l := NewRingLogger(mockLog, Info{}, -1)
	msg := &Message{Line: []byte("hello humans and everyone else!")}
	b.SetBytes(int64(len(msg.Line)))

	cancel := consumeWithDelay(1*time.Millisecond, mockLog.c)
	defer cancel()

	for b.Loop() {
		if err := l.Log(msg); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRingLoggerThroughputConsumeDelay10(b *testing.B) {
	mockLog := &mockLogger{make(chan *Message)}
	defer mockLog.Close()
	l := NewRingLogger(mockLog, Info{}, -1)
	msg := &Message{Line: []byte("hello humans and everyone else!")}
	b.SetBytes(int64(len(msg.Line)))

	cancel := consumeWithDelay(10*time.Millisecond, mockLog.c)
	defer cancel()

	for b.Loop() {
		if err := l.Log(msg); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRingLoggerThroughputConsumeDelay50(b *testing.B) {
	mockLog := &mockLogger{make(chan *Message)}
	defer mockLog.Close()
	l := NewRingLogger(mockLog, Info{}, -1)
	msg := &Message{Line: []byte("hello humans and everyone else!")}
	b.SetBytes(int64(len(msg.Line)))

	cancel := consumeWithDelay(50*time.Millisecond, mockLog.c)
	defer cancel()

	for b.Loop() {
		if err := l.Log(msg); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRingLoggerThroughputConsumeDelay100(b *testing.B) {
	mockLog := &mockLogger{make(chan *Message)}
	defer mockLog.Close()
	l := NewRingLogger(mockLog, Info{}, -1)
	msg := &Message{Line: []byte("hello humans and everyone else!")}
	b.SetBytes(int64(len(msg.Line)))

	cancel := consumeWithDelay(100*time.Millisecond, mockLog.c)
	defer cancel()

	for b.Loop() {
		if err := l.Log(msg); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRingLoggerThroughputConsumeDelay300(b *testing.B) {
	mockLog := &mockLogger{make(chan *Message)}
	defer mockLog.Close()
	l := NewRingLogger(mockLog, Info{}, -1)
	msg := &Message{Line: []byte("hello humans and everyone else!")}
	b.SetBytes(int64(len(msg.Line)))

	cancel := consumeWithDelay(300*time.Millisecond, mockLog.c)
	defer cancel()

	for b.Loop() {
		if err := l.Log(msg); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRingLoggerThroughputConsumeDelay500(b *testing.B) {
	mockLog := &mockLogger{make(chan *Message)}
	defer mockLog.Close()
	l := NewRingLogger(mockLog, Info{}, -1)
	msg := &Message{Line: []byte("hello humans and everyone else!")}
	b.SetBytes(int64(len(msg.Line)))

	cancel := consumeWithDelay(500*time.Millisecond, mockLog.c)
	defer cancel()

	for b.Loop() {
		if err := l.Log(msg); err != nil {
			b.Fatal(err)
		}
	}
}
