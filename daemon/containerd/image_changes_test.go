package containerd

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"
)

func TestAcquireChangesCanCancelWaiter(t *testing.T) {
	service := &ImageService{changesLimiter: semaphore.NewWeighted(1)}
	release, err := service.acquireChanges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		release, err := service.acquireChanges(ctx)
		if release != nil {
			release()
		}
		result <- err
	}()
	<-started
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting container diff did not stop after context cancellation")
	}
}

func TestAcquireChangesBoundsConcurrentDiffs(t *testing.T) {
	service := &ImageService{changesLimiter: semaphore.NewWeighted(maxConcurrentContainerDiffs)}
	releases := make([]func(), 0, maxConcurrentContainerDiffs)
	for range maxConcurrentContainerDiffs {
		release, err := service.acquireChanges(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if release, err := service.acquireChanges(ctx); err == nil {
		release()
		t.Fatal("acquired a slot beyond the configured concurrent diff limit")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}
