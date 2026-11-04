package loggerutils

import (
	"context"
	"sync"

	"github.com/moby/moby/v2/daemon/logger"
	"github.com/pkg/errors"
)

// MessageQueue is a queue for log messages.
//
// [MessageQueue.Enqueue] will block when the queue is full.
// To dequeue messages call [MessageQueue.Receiver] and pull messages off the
// returned channel.
//
// Closing only prevents new messages from being added to the queue.
// The queue can still be drained after close.
//
// The zero value of MessageQueue is safe to use, but does not do any internal
// buffering (queue size is 0).
type MessageQueue struct {
	maxSize int

	mu      sync.Mutex
	closing bool
	closed  chan struct{}

	// Blocks multiple calls to [MessageQueue.Close] until the queue is actually closed
	closeWait chan struct{}

	// We need to be able to safely close the send channel so that [MessageQueue.Dequeue]
	// can drain the queue without blocking.
	// This cond var helps deal with that.
	cond        *sync.Cond
	sendWaiters int

	ch chan *logger.Message
}

// NewMessageQueue creates a new queue with the specified size.
func NewMessageQueue(maxSize int) *MessageQueue {
	var q MessageQueue
	q.maxSize = maxSize
	q.init()
	return &q
}

func (q *MessageQueue) init() {
	if q.cond == nil {
		q.cond = sync.NewCond(&q.mu)
	}

	if q.ch == nil {
		q.ch = make(chan *logger.Message, q.maxSize)
	}

	if q.closed == nil {
		q.closed = make(chan struct{})
	}

	if q.closeWait == nil {
		q.closeWait = make(chan struct{})
	}
}

var ErrQueueClosed = errors.New("queue is closed")

// Enqueue adds the provided message to the queue.
// Enqueue blocks if the queue is full.
//
// The two possible error cases are:
// 1. The provided context is cancelled
// 2. [ErrQueueClosed] when the queue has been closed.
func (q *MessageQueue) Enqueue(ctx context.Context, m *logger.Message) error {
	q.mu.Lock()
	q.init()

	// Increment the waiter count
	// This prevents the send channel from being closed while we are trying to send.
	q.sendWaiters++
	q.mu.Unlock()

	defer func() {
		q.mu.Lock()
		// Decrement the waiter count and signal to any potential closer to check
		// the wait count again.
		// Only bother signaling if this is the last waiter.
		q.sendWaiters--
		if q.sendWaiters == 0 {
			q.cond.Signal()
		}
		q.mu.Unlock()
	}()

	// Before trying to send on the channel, check if we care closed.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-q.closed:
		return ErrQueueClosed
	default:
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-q.closed:
		return ErrQueueClosed
	case q.ch <- m:
		return nil
	}
}

// Close prevents any new messages from being added to the queue.
func (q *MessageQueue) Close() {
	q.mu.Lock()

	q.init()

	if q.closing {
		// unlock the mutex here so that the goroutine waiting on the cond var can
		// take the lock when signaled.
		q.mu.Unlock()
		<-q.closeWait
		return
	}

	defer q.mu.Unlock()

	// Prevent multiple Close calls from trying to close things.
	q.closing = true

	close(q.closed)

	// Wait for any senders to finish
	// Because we closed the channel above, this shouldn't block for a long period.
	for q.sendWaiters > 0 {
		q.cond.Wait()
	}

	close(q.ch)
	close(q.closeWait)
}

// Done returns a channel that is closed when [MessageQueue.Close] is called.
//
// Unlike the channel returned by [MessageQueue.Receiver], which stays open
// until the queue has been drained, this channel is closed as soon as the queue
// starts closing. It can be used to interrupt work that should not outlive the
// queue.
func (q *MessageQueue) Done() <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.init()

	return q.closed
}

// Receiver returns a channel that can be used to dequeue messages
// The channel will be closed when the message queue is closed but may have
// messages buffered.
func (q *MessageQueue) Receiver() <-chan *logger.Message {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.init()

	return q.ch
}
