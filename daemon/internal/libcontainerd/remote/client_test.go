package remote

import (
	"context"
	"errors"
	"testing"
	"time"

	c8devents "github.com/containerd/containerd/v2/core/events"
	"github.com/containerd/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestProcessEventStreamRestartBacksOffBetweenSubscriptions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const delay = 30 * time.Millisecond
	var starts []time.Time

	subscribe := func(context.Context, string) (<-chan *c8devents.Envelope, <-chan error) {
		starts = append(starts, time.Now())
		events := make(chan *c8devents.Envelope)
		errs := make(chan error, 1)
		errs <- errors.New("subscription lost")
		return events, errs
	}

	waitReady := func(ctx context.Context) bool {
		if len(starts) >= 3 {
			cancel()
			return false
		}
		return ctx.Err() == nil
	}

	c := &client{logger: log.G(ctx)}
	c.processEventStreamWithRestart(ctx, "moby", delay, subscribe, waitReady)

	if len(starts) != 3 {
		t.Fatalf("expected 3 subscription attempts, got %d", len(starts))
	}
	for i := 1; i < len(starts); i++ {
		if elapsed := starts[i].Sub(starts[i-1]); elapsed < delay {
			t.Fatalf("subscription attempt %d restarted after %s, expected at least %s", i+1, elapsed, delay)
		}
	}
}

// A remote stream reset surfaces as a gRPC Canceled status while the daemon
// context is still active; event processing must reconnect rather than stop.
func TestProcessEventStreamRetriesRemoteCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	subscriptions := 0
	subscribe := func(context.Context, string) (<-chan *c8devents.Envelope, <-chan error) {
		subscriptions++
		events := make(chan *c8devents.Envelope)
		errs := make(chan error, 1)
		errs <- status.Error(codes.Canceled, "stream reset by peer")
		return events, errs
	}

	waitReady := func(ctx context.Context) bool {
		if subscriptions >= 2 {
			cancel()
			return false
		}
		return ctx.Err() == nil
	}

	c := &client{logger: log.G(ctx)}
	c.processEventStreamWithRestart(ctx, "moby", time.Millisecond, subscribe, waitReady)

	if subscriptions != 2 {
		t.Fatalf("expected remote cancellation to be retried, got %d subscription attempts", subscriptions)
	}
}

func TestWaitEventStreamRestartStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if waitEventStreamRestart(ctx, time.Hour) {
		t.Fatal("expected canceled context to stop restart wait")
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("canceled restart wait took %s", elapsed)
	}
}
