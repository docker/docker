package stream

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type dummyWriter struct {
	buffer      bytes.Buffer
	failOnWrite bool
}

func (dw *dummyWriter) Write(p []byte) (int, error) {
	if dw.failOnWrite {
		return 0, errors.New("Fake fail")
	}
	return dw.buffer.Write(p)
}

func (dw *dummyWriter) String() string {
	return dw.buffer.String()
}

func (dw *dummyWriter) Close() error {
	return nil
}

func TestUnbuffered(t *testing.T) {
	writer := new(unbuffered)

	// Test 1: Both bufferA and bufferB should contain "foo"
	bufferA := &dummyWriter{}
	writer.Add(bufferA)
	bufferB := &dummyWriter{}
	writer.Add(bufferB)
	writer.Write([]byte("foo"))

	if bufferA.String() != "foo" {
		t.Errorf("Buffer contains %v", bufferA.String())
	}

	if bufferB.String() != "foo" {
		t.Errorf("Buffer contains %v", bufferB.String())
	}

	// Test2: bufferA and bufferB should contain "foobar",
	// while bufferC should only contain "bar"
	bufferC := &dummyWriter{}
	writer.Add(bufferC)
	writer.Write([]byte("bar"))

	if bufferA.String() != "foobar" {
		t.Errorf("Buffer contains %v", bufferA.String())
	}

	if bufferB.String() != "foobar" {
		t.Errorf("Buffer contains %v", bufferB.String())
	}

	if bufferC.String() != "bar" {
		t.Errorf("Buffer contains %v", bufferC.String())
	}

	// Test3: Test eviction on failure
	bufferA.failOnWrite = true
	writer.Write([]byte("fail"))
	if bufferA.String() != "foobar" {
		t.Errorf("Buffer contains %v", bufferA.String())
	}
	if bufferC.String() != "barfail" {
		t.Errorf("Buffer contains %v", bufferC.String())
	}
	// Even though we reset the flag, no more writes should go in there
	bufferA.failOnWrite = false
	writer.Write([]byte("test"))
	if bufferA.String() != "foobar" {
		t.Errorf("Buffer contains %v", bufferA.String())
	}
	if bufferC.String() != "barfailtest" {
		t.Errorf("Buffer contains %v", bufferC.String())
	}

	// Test4: Test eviction on multiple simultaneous failures
	bufferB.failOnWrite = true
	bufferC.failOnWrite = true
	bufferD := &dummyWriter{}
	writer.Add(bufferD)
	writer.Write([]byte("yo"))
	writer.Write([]byte("ink"))
	if strings.Contains(bufferB.String(), "yoink") {
		t.Errorf("bufferB received write. contents: %q", bufferB)
	}
	if strings.Contains(bufferC.String(), "yoink") {
		t.Errorf("bufferC received write. contents: %q", bufferC)
	}
	if g, w := bufferD.String(), "yoink"; g != w {
		t.Errorf("bufferD = %q, want %q", g, w)
	}

	writer.Clean()
}

type devNullCloser int

func (d devNullCloser) Close() error {
	return nil
}

func (d devNullCloser) Write(buf []byte) (int, error) {
	return len(buf), nil
}

// This test checks for races. It is only useful when run with the race detector.
func TestRaceUnbuffered(t *testing.T) {
	writer := new(unbuffered)
	c := make(chan bool)
	go func() {
		writer.Add(devNullCloser(0))
		c <- true
	}()
	_, err := writer.Write([]byte("hello"))
	if err != nil {
		t.Error(err)
	}
	<-c
}

// blockingWriter models bytespipe's backpressure behaviour: Write parks
// until Close is called, then returns an error, exactly like BytesPipe.Write
// unblocking with ErrClosed once the pipe is closed.
type blockingWriter struct {
	mu      sync.Mutex
	cond    *sync.Cond
	closed  bool
	closes  atomic.Int32
	started chan struct{}
}

func newBlockingWriter() *blockingWriter {
	w := &blockingWriter{started: make(chan struct{}, 1)}
	w.cond = sync.NewCond(&w.mu)
	return w
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.started <- struct{}{}

	w.mu.Lock()
	defer w.mu.Unlock()
	for !w.closed {
		w.cond.Wait()
	}
	return 0, errClosedBlockingWriter
}

func (w *blockingWriter) Close() error {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.cond.Broadcast()
	w.closes.Add(1)
	return nil
}

var errClosedBlockingWriter = errors.New("blockingWriter: closed")

// waitOrFatal waits for done to close, or fails the test if it does not
// happen within the timeout.
func waitOrFatal(t *testing.T, done <-chan struct{}, timeout time.Duration, msg string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal(msg)
	}
}

// TestUnbufferedCleanWithBlockedWriter reproduces moby/moby#53614: an exec's
// exit handler calls Config.CloseStreams -> unbuffered.Clean while a copier
// goroutine is blocked inside Write on an attached client's bytespipe that
// has hit backpressure (the client stopped reading). Clean must not hold
// the same lock that the blocked Write holds, otherwise it never returns,
// and because libcontainerd serialises events per container ID, every
// subsequent event for that container wedges behind it.
func TestUnbufferedCleanWithBlockedWriter(t *testing.T) {
	w := new(unbuffered)
	normal := &dummyWriter{}
	blocked := newBlockingWriter()
	w.Add(normal)
	w.Add(blocked)

	writeDone := make(chan struct{})
	go func() {
		w.Write([]byte("data"))
		close(writeDone)
	}()

	waitOrFatal(t, blocked.started, 10*time.Second, "blockingWriter.Write never started")

	cleanDone := make(chan struct{})
	go func() {
		w.Clean()
		close(cleanDone)
	}()
	waitOrFatal(t, cleanDone, 10*time.Second, "Clean() blocked; see moby/moby#53614")

	waitOrFatal(t, writeDone, 10*time.Second, "blocked Write did not unwind after Clean")

	if got := blocked.closes.Load(); got != 1 {
		t.Errorf("blockingWriter closed %d times, want 1", got)
	}

	if normal.String() != "data" {
		t.Errorf("normal writer contains %q, want %q", normal.String(), "data")
	}

	if _, err := w.Write([]byte("x")); err != nil {
		t.Errorf("Write after Clean returned error: %v", err)
	}
}

// overlapWriter detects whether two Write calls ever ran concurrently
// against it. inProgress is flipped on entry and cleared on exit; a short
// sleep in between widens the window so a missing writeMu shows up as
// observed overlap, not just as a race-detector-only issue.
type overlapWriter struct {
	inProgress atomic.Bool
	overlap    atomic.Bool
	count      atomic.Int64
}

func (w *overlapWriter) Write(p []byte) (int, error) {
	if w.inProgress.Swap(true) {
		w.overlap.Store(true)
	}
	time.Sleep(time.Millisecond)
	w.inProgress.Store(false)
	w.count.Add(1)
	return len(p), nil
}

func (w *overlapWriter) Close() error { return nil }

// TestUnbufferedConcurrentWrites exercises the writeMu ordering guarantee:
// concurrent Write calls on unbuffered must serialize before reaching the
// underlying writer, so the writer never observes two Write calls in
// flight at once.
func TestUnbufferedConcurrentWrites(t *testing.T) {
	const goroutines = 8
	const iterations = 20

	rec := &overlapWriter{}
	w := new(unbuffered)
	w.Add(rec)

	payload := []byte("payload")
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				if _, err := w.Write(payload); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()

	if got, want := rec.count.Load(), int64(goroutines*iterations); got != want {
		t.Fatalf("got %d writes delivered, want %d", got, want)
	}
	if rec.overlap.Load() {
		t.Error("concurrent Write calls overlapped in the underlying writer; writeMu did not serialize them")
	}
}

func BenchmarkUnbuffered(b *testing.B) {
	writer := new(unbuffered)
	setUpWriter := func() {
		for range 100 {
			writer.Add(devNullCloser(0))
			writer.Add(devNullCloser(0))
			writer.Add(devNullCloser(0))
		}
	}
	testLine := "Line that thinks that it is log line from docker"
	var buf bytes.Buffer
	for range 100 {
		buf.WriteString(testLine + "\n")
	}
	// line without eol
	buf.WriteString(testLine)
	testText := buf.Bytes()
	b.SetBytes(int64(5 * len(testText)))

	for b.Loop() {
		b.StopTimer()
		setUpWriter()
		b.StartTimer()

		for range 5 {
			if _, err := writer.Write(testText); err != nil {
				b.Fatal(err)
			}
		}

		b.StopTimer()
		writer.Clean()
		b.StartTimer()
	}
}
