package container

import (
	"testing"
	"time"

	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/daemon/logger"
)

// blockingCloseLogger is a logger whose Close method blocks on a channel.
// It simulates a log driver with an unresponsive backend.
type blockingCloseLogger struct {
	unblock chan struct{}
	closed  chan struct{}
}

func (l *blockingCloseLogger) Log(*logger.Message) error { return nil }
func (l *blockingCloseLogger) Name() string              { return "blocking" }

func (l *blockingCloseLogger) Close() error {
	<-l.unblock
	close(l.closed)
	return nil
}

// TestResetDoesNotBlockOnStuckLogger verifies that Reset() returns
// promptly even when the log driver's Close method blocks indefinitely.
// Before the fix, Reset() would hold the container lock for the entire
// duration of LogDriver.Close(), blocking unrelated operations.
func TestResetDoesNotBlockOnStuckLogger(t *testing.T) {
	bl := &blockingCloseLogger{
		unblock: make(chan struct{}),
		closed:  make(chan struct{}),
	}
	defer close(bl.unblock)

	c := NewBaseContainer("test", t.TempDir())
	c.Config = &containertypes.Config{}
	c.LogDriver = bl

	done := make(chan struct{})
	go func() {
		c.Reset()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Reset() blocked on stuck LogDriver.Close()")
	}

	if c.LogDriver != nil {
		t.Fatal("LogDriver should be nil after Reset()")
	}
	if c.LogCopier != nil {
		t.Fatal("LogCopier should be nil after Reset()")
	}

	// Verify Close is still running in the background (not finished yet).
	select {
	case <-bl.closed:
		t.Fatal("background Close finished prematurely; unblock was sent")
	default:
	}

	// Reset() must be idempotent — a second call with LogDriver already nil
	// should return quickly without spawning another close goroutine.
	c.Reset()
}
