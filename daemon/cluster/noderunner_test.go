package cluster

import (
	"runtime"
	"testing"
	"time"

	lncluster "github.com/moby/moby/v2/daemon/libnetwork/cluster"
)

// TestNodeRunnerStopReleasesLockBeforeClusterEvent reproduces the lock order
// between nodeRunner.mu and Cluster.mu. A reader holds Cluster.mu, a writer
// queues behind it so that new readers block, and Stop runs. Stop must not
// hold nodeRunner.mu while it waits for Cluster.mu in SendClusterEvent,
// otherwise the reader's nodeRunner.State call can never proceed and the
// three goroutines deadlock.
func TestNodeRunnerStopReleasesLockBeforeClusterEvent(t *testing.T) {
	c := &Cluster{configEvent: make(chan lncluster.ConfigEventType, 1)}
	nr := &nodeRunner{cluster: c}
	c.nr = nr

	// Reader: holds Cluster.mu for the whole scenario, like a state read does.
	c.mu.RLock()

	// Writer: queues behind the reader so that further RLock calls block.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		c.mu.Lock()
		defer c.mu.Unlock()
	}()
	for c.mu.TryRLock() {
		c.mu.RUnlock()
		runtime.Gosched()
	}

	// Stop: takes nodeRunner.mu and then wants Cluster.mu for the event.
	stopDone := make(chan error, 1)
	go func() { stopDone <- nr.Stop() }()
	held := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(held) {
		if nr.mu.TryRLock() {
			nr.mu.RUnlock()
			runtime.Gosched()
			continue
		}
		break
	}

	// The reader now asks for node state, which takes nodeRunner.mu.
	stateDone := make(chan struct{})
	go func() {
		_ = nr.State()
		close(stateDone)
	}()
	select {
	case <-stateDone:
	case <-time.After(2 * time.Second):
		t.Error("nodeRunner.State blocked: Stop holds nodeRunner.mu while waiting for Cluster.mu")
	}

	c.mu.RUnlock()
	<-writerDone
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return")
	}
	<-stateDone
}
