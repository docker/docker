package stream

import (
	"bytes"
	"errors"
	"strings"
	"sync"
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

// blockingWriter's Write blocks until Close is called, mirroring how
// bytespipe.BytesPipe's Write blocks under backpressure until its Close
// broadcasts the writer's condition variable and unblocks it.
type blockingWriter struct {
	started     chan struct{}
	startedOnce sync.Once
	closed      chan struct{}
	closeOnce   sync.Once
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{
		started: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.startedOnce.Do(func() { close(w.started) })
	<-w.closed
	return 0, errors.New("write to closed writer")
}

func (w *blockingWriter) Close() error {
	w.closeOnce.Do(func() { close(w.closed) })
	return nil
}

// TestUnbufferedCleanUnblocksDuringBlockedWrite guards against Clean deadlocking
// forever while a concurrent Write is blocked inside one of the writers (e.g. a
// bytespipe applying backpressure to a client that stopped reading). Clean is
// what closes that writer and would unblock it, so Clean must be able to
// acquire the broadcaster's lock even while such a Write is in progress.
// See https://github.com/moby/moby/issues/53614.
func TestUnbufferedCleanUnblocksDuringBlockedWrite(t *testing.T) {
	writer := new(unbuffered)
	bw := newBlockingWriter()
	writer.Add(bw)

	writeDone := make(chan struct{})
	go func() {
		_, _ = writer.Write([]byte("x"))
		close(writeDone)
	}()

	select {
	case <-bw.started:
	case <-time.After(5 * time.Second):
		t.Fatal("blockingWriter.Write was never entered")
	}

	cleanDone := make(chan struct{})
	go func() {
		_ = writer.Clean()
		close(cleanDone)
	}()

	select {
	case <-cleanDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Clean() did not return: it deadlocked waiting for the lock held by a blocked Write() call")
	}

	select {
	case <-writeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Write() never returned after Clean() closed its writer")
	}
}
