package logger

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/log"
)

var ringLoggerTimeout = 15 * time.Second

const (
	defaultRingMaxSize = 1e6 // 1MB
)

// ringLogger is a ring buffer that implements the Logger interface.
// This is used when lossy logging is OK.
type ringLogger struct {
	buffer    *messageRing
	l         Logger
	logInfo   Info
	closeFlag atomic.Bool
	wg        sync.WaitGroup
	closeOnce sync.Once
	closedErr error
}

var (
	_ SizedLogger = (*ringLogger)(nil)
	_ LogReader   = (*ringWithReader)(nil)
)

type ringWithReader struct {
	*ringLogger
}

func (r *ringWithReader) ReadLogs(ctx context.Context, cfg ReadConfig) *LogWatcher {
	reader, ok := r.l.(LogReader)
	if !ok {
		// something is wrong if we get here
		panic("expected log reader")
	}
	return reader.ReadLogs(ctx, cfg)
}

func newRingLogger(driver Logger, logInfo Info, maxSize int64) *ringLogger {
	l := &ringLogger{
		buffer:  newRing(maxSize),
		l:       driver,
		logInfo: logInfo,
	}
	l.wg.Go(l.run)
	return l
}

// NewRingLogger creates a new Logger that is implemented as a RingBuffer wrapping
// the passed in logger.
func NewRingLogger(driver Logger, logInfo Info, maxSize int64) Logger {
	if maxSize < 0 {
		maxSize = defaultRingMaxSize
	}
	l := newRingLogger(driver, logInfo, maxSize)
	if _, ok := driver.(LogReader); ok {
		return &ringWithReader{l}
	}
	return l
}

// BufSize returns the buffer size of the underlying logger.
// Returns -1 if the logger doesn't match SizedLogger interface.
func (r *ringLogger) BufSize() int {
	if sl, ok := r.l.(SizedLogger); ok {
		return sl.BufSize()
	}
	return -1
}

// Log queues messages into the ring buffer
func (r *ringLogger) Log(msg *Message) error {
	if r.closed() {
		return errClosed
	}
	return r.buffer.Enqueue(msg)
}

// Name returns the name of the underlying logger
func (r *ringLogger) Name() string {
	return r.l.Name()
}

func (r *ringLogger) closed() bool {
	return r.closeFlag.Load()
}

func (r *ringLogger) setClosed() {
	r.closeFlag.Store(true)
}

// closeUnderlying closes the underlying logger exactly once and caches the error.
func (r *ringLogger) closeUnderlying() error {
	r.closeOnce.Do(func() {
		r.closedErr = r.l.Close()
	})
	return r.closedErr
}

// Close closes the logger
func (r *ringLogger) Close() error {
	r.setClosed()
	r.buffer.Close()

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()

	// Wait for run() to exit so that we fulfill the io.Closer contract.
	// We use a 15-second timeout (ample time for normal flush operations to complete)
	// to forcefully close the underlying driver to interrupt blocked Log() calls.
	//
	// Note: We MUST wait for run() to exit to prevent use-after-return bugs
	// where run() continues accessing ringLogger state. Therefore, if the
	// underlying driver's Close() method does NOT unblock its Log() method
	// (e.g., uninterruptible kernel I/O, or lock inversion), ringLogger.Close()
	// will still hang indefinitely. This timeout is a targeted mitigation for
	// drivers that support interruptible Close() semantics (such as awslogs).
	timer := time.NewTimer(ringLoggerTimeout)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
		log.G(context.TODO()).WithField("logger", r.l.Name()).Warn("ringlogger: timed out waiting for logger flush; forcing underlying logger close (works only for drivers whose Close interrupts Log)")
		go r.closeUnderlying()
		<-done
	}

	// empty out the queue
	var logErr bool
	for _, msg := range r.buffer.Drain() {
		if logErr {
			// some error logging a previous message, so re-insert to message pool
			// and assume log driver is hosed
			PutMessage(msg)
			continue
		}

		if err := r.l.Log(msg); err != nil {
			logDriverError(r.l.Name(), string(msg.Line), err)
			PutMessage(msg)
			logErr = true
		}
	}
	return r.closeUnderlying()
}

// run consumes messages from the ring buffer and forwards them to the underling
// logger.
// This is run in a goroutine when the ringLogger is created
func (r *ringLogger) run() {
	for {
		if r.closed() {
			return
		}
		msg, err := r.buffer.Dequeue()
		if err != nil {
			// buffer is closed
			return
		}
		if err := r.l.Log(msg); err != nil {
			logDriverError(r.l.Name(), string(msg.Line), err)
			PutMessage(msg)
		}
	}
}

type messageRing struct {
	mu sync.Mutex
	// signals callers of `Dequeue` to wake up either on `Close` or when a new `Message` is added
	wait *sync.Cond

	sizeBytes int64 // current buffer size
	maxBytes  int64 // max buffer size
	queue     []*Message
	closed    bool
}

func newRing(maxBytes int64) *messageRing {
	queueSize := 1000
	if maxBytes == 0 || maxBytes == 1 {
		// With 0 or 1 max byte size, the maximum size of the queue would only ever be 1
		// message long.
		queueSize = 1
	}

	r := &messageRing{queue: make([]*Message, 0, queueSize), maxBytes: maxBytes}
	r.wait = sync.NewCond(&r.mu)
	return r
}

// Enqueue adds a message to the buffer queue
// If the message is too big for the buffer it drops the new message.
// If there are no messages in the queue and the message is still too big, it adds the message anyway.
func (r *messageRing) Enqueue(m *Message) error {
	mSize := int64(len(m.Line))

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return errClosed
	}
	if mSize+r.sizeBytes > r.maxBytes && len(r.queue) > 0 {
		r.wait.Signal()
		return nil
	}

	r.queue = append(r.queue, m)
	r.sizeBytes += mSize
	r.wait.Signal()
	return nil
}

// Dequeue pulls a message off the queue
// If there are no messages, it waits for one.
// If the buffer is closed, it will return immediately.
func (r *messageRing) Dequeue() (*Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.queue) == 0 && !r.closed {
		r.wait.Wait()
	}

	if r.closed {
		return nil, errClosed
	}

	msg := r.queue[0]
	r.queue = r.queue[1:]
	r.sizeBytes -= int64(len(msg.Line))
	return msg, nil
}

var errClosed = errors.New("closed")

// Close closes the buffer ensuring no new messages can be added.
// Any callers waiting to dequeue a message will be woken up.
func (r *messageRing) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}

	r.closed = true
	r.wait.Broadcast()
}

// Drain drains all messages from the queue.
// This can be used after `Close()` to get any remaining messages that were in queue.
func (r *messageRing) Drain() []*Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	ls := make([]*Message, 0, len(r.queue))
	ls = append(ls, r.queue...)
	r.sizeBytes = 0
	r.queue = r.queue[:0]
	return ls
}
