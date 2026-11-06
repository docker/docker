//go:build linux

package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/moby/moby/v2/daemon/graphdriver"
	"github.com/moby/moby/v2/daemon/graphdriver/overlay2"
	"github.com/moby/moby/v2/daemon/internal/layer"
	"github.com/moby/sys/user"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/sys/unix"
	"gotest.tools/v3/assert"
)

// Pause after Prepare has resolved the parent's graphdriver ID, before the real
// overlay2 driver creates the child. No production implementation is mocked.
type pausedGraphDriver struct {
	graphdriver.Driver
	entered chan struct{}
	resume  chan struct{}
}

func (d *pausedGraphDriver) Create(id, parent string, opts *graphdriver.CreateOpts) error {
	if id == "child" {
		close(d.entered)
		<-d.resume
	}
	return d.Driver.Create(id, parent, opts)
}

func TestLeaseManagerSharedParent(t *testing.T) {
	for _, tc := range []struct {
		name           string
		removeResource bool
		leaseID        string
	}{
		{name: "delete-lease", leaseID: "build-a"},
		{name: "delete-resource", removeResource: true, leaseID: "build-a"},
		{name: "delete-lease-same-id", leaseID: "parent-shared"},
		{name: "delete-resource-same-id", removeResource: true, leaseID: "parent-shared"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := namespaces.WithNamespace(t.Context(), "regression")
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
			paused := &pausedGraphDriver{Driver: driver, entered: make(chan struct{}), resume: make(chan struct{})}
			s := &snapshotter{opt: Opt{GraphDriver: paused}, db: db, refs: map[string]layer.Layer{}}
			lm := newLeaseManager(s, metadata.NewLeaseManager(metadata.NewDB(leaseDB, nil, nil)))
			if err := s.Prepare(ctx, "parent-active", ""); err != nil {
				t.Fatal(err)
			}
			if err := s.Commit(ctx, "parent-shared", "parent-active"); err != nil {
				t.Fatal(err)
			}
			resource := leases.Resource{ID: "parent-shared", Type: "snapshots/default"}
			buildA, err := lm.Create(ctx, leases.WithID(tc.leaseID))
			assert.NilError(t, err)
			buildB, err := lm.Create(ctx, leases.WithID("build-b"))
			assert.NilError(t, err)
			for _, build := range []leases.Lease{buildA, buildB} {
				if err := lm.AddResource(ctx, build, resource); err != nil {
					t.Fatal(err)
				}
			}
			result := make(chan error, 1)
			go func() { result <- s.Prepare(ctx, "child", resource.ID) }()
			select {
			case <-paused.entered:
			case err := <-result:
				t.Fatalf("child preparation ended before the checkpoint: %v", err)
			}
			if tc.removeResource {
				err = lm.DeleteResource(ctx, buildA, resource)
			} else {
				err = lm.Delete(ctx, buildA)
			}
			close(paused.resume)
			prepareErr := <-result
			assert.NilError(t, err)
			resources, err := lm.ListResources(ctx, buildB)
			if err != nil || len(resources) != 1 || resources[0] != resource {
				t.Fatalf("build B's durable lease must still protect its parent: %v, %v", resources, err)
			}
			if prepareErr != nil {
				var pathErr *os.PathError
				if !errors.As(prepareErr, &pathErr) || (!strings.Contains(pathErr.Path, ".tmp-committed") && !strings.Contains(prepareErr.Error(), "invalid output path")) || !errors.Is(prepareErr, os.ErrNotExist) {
					t.Fatalf("unexpected failure: %v", prepareErr)
				}
				t.Fatalf("parent removed while build B still holds its lease: %v", prepareErr)
			}
			if err := s.opt.GraphDriver.Remove("child"); err != nil {
				t.Fatal(err)
			}
			if err := lm.Delete(ctx, buildB); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(root, "overlay2", "parent-active")); !os.IsNotExist(err) {
				t.Fatalf("parent must be reclaimed after its last lease: %v", err)
			}
		})
	}
}

// newSnapshotTestRoot avoids overlay-on-overlay when tests run in a container.
// Mount permission and overlayfs support are required for these Linux fixtures.
func newSnapshotTestRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	err := unix.Mount("tmpfs", root, "tmpfs", 0, "")
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENODEV) {
		t.Skipf("tmpfs mount unavailable: %v", err)
	}
	assert.NilError(t, err)
	// BuildKit may release view mounts asynchronously; detach the isolated tree.
	t.Cleanup(func() { assert.NilError(t, unix.Unmount(root, unix.MNT_DETACH)) })
	return root
}
