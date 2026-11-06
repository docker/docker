//go:build linux

package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/buildkit/util/leaseutil"
	"github.com/moby/moby/v2/daemon/graphdriver"
	"github.com/moby/moby/v2/daemon/graphdriver/overlay2"
	"github.com/moby/moby/v2/daemon/internal/layer"
	"github.com/moby/sys/user"
	bolt "go.etcd.io/bbolt"
	"gotest.tools/v3/assert"
)

type pausedLeaseAdd struct {
	leases.Manager
	entered, resume chan struct{}
	deleted         chan struct{}
}

func (m *pausedLeaseAdd) Delete(ctx context.Context, l leases.Lease, opts ...leases.DeleteOpt) error {
	err := m.Manager.Delete(ctx, l, opts...)
	if l.ID == "previous-owner" {
		m.deleted <- struct{}{}
	}
	return err
}

func (m *pausedLeaseAdd) AddResource(ctx context.Context, l leases.Lease, r leases.Resource) error {
	if err := m.Manager.AddResource(ctx, l, r); err != nil {
		return err
	}
	if l.ID == "active-build" {
		close(m.entered)
		<-m.resume
	}
	return nil
}

func TestLeaseManagerHandoffCannotLoseSource(t *testing.T) {
	ctx := namespaces.WithNamespace(t.Context(), "handoff")
	root := newSnapshotTestRoot(t)
	driver, err := overlay2.Init(filepath.Join(root, "overlay2"), nil, user.IdentityMapping{})
	if graphdriver.IsDriverNotSupported(err) {
		t.Skipf("overlay2 unavailable: %v", err)
	}
	assert.NilError(t, err)
	t.Cleanup(func() { driver.Cleanup() })
	db, err := bolt.Open(filepath.Join(root, "snapshots.db"), 0o600, nil)
	assert.NilError(t, err)
	t.Cleanup(func() { db.Close() })
	leaseDB, err := bolt.Open(filepath.Join(root, "leases.db"), 0o600, nil)
	assert.NilError(t, err)
	t.Cleanup(func() { leaseDB.Close() })
	s := &snapshotter{opt: Opt{GraphDriver: driver}, db: db, refs: map[string]layer.Layer{}}
	base := metadata.NewLeaseManager(metadata.NewDB(leaseDB, nil, nil))
	paused := &pausedLeaseAdd{Manager: base, entered: make(chan struct{}), resume: make(chan struct{}), deleted: make(chan struct{}, 2)}
	var resumeOnce sync.Once
	resume := func() { resumeOnce.Do(func() { close(paused.resume) }) }
	t.Cleanup(resume)
	lm := newLeaseManager(s, paused)
	if err := s.Prepare(ctx, "source", ""); err != nil {
		t.Fatal(err)
	}
	resource := leases.Resource{ID: "source", Type: "snapshots/default"}
	oldLease, _, err := leaseutil.NewLease(ctx, lm, leases.WithID("previous-owner"))
	assert.NilError(t, err)
	if err := lm.AddResource(ctx, leases.Lease{ID: "previous-owner"}, resource); err != nil {
		t.Fatal(err)
	}
	active, err := lm.Create(ctx, leases.WithID("active-build"))
	assert.NilError(t, err)
	adopted := make(chan error, 1)
	go func() { adopted <- oldLease.Adopt(leases.WithLease(ctx, active.ID)) }()
	select {
	case <-paused.entered:
	case err := <-adopted:
		t.Fatalf("lease adoption ended before the checkpoint: %v", err)
	}
	// The durable record now belongs to the new build; its original owner may
	// finish concurrently. Exercise the exact gap between durable and mirrored
	// references. If serialized, deletion must wait for the transfer to finish.
	durable, err := base.ListResources(ctx, active)
	if err != nil || len(durable) != 1 {
		t.Fatalf("new build must already own the source: %v %v", durable, err)
	}
	// Probe while AddResource is stopped, before the deleting goroutine can
	// acquire the mutex itself. This decision cannot depend on its scheduling.
	unprotected := lm.mu.TryLock()
	if unprotected {
		lm.mu.Unlock()
	}
	deleted := make(chan error, 1)
	go func() { deleted <- lm.Delete(ctx, leases.Lease{ID: "previous-owner"}) }()
	var deletionErr error
	if unprotected {
		deletionErr = <-deleted
		resume()
	} else {
		resume()
		deletionErr = <-deleted
	}
	if err := <-adopted; err != nil {
		t.Fatal(err)
	}
	if deletionErr != nil && !cerrdefs.IsNotFound(deletionErr) {
		t.Fatal(deletionErr)
	}
	// Join both the explicit cleanup and Adopt's asynchronous Discard.
	<-paused.deleted
	<-paused.deleted
	if _, err := os.Stat(filepath.Join(root, "overlay2", "source", "diff")); err != nil {
		t.Fatalf("source vanished during adoption while the new build owns its durable lease: %v", err)
	}
	if err := lm.Delete(ctx, active); err != nil {
		t.Fatal(err)
	}
}
