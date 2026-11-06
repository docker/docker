//go:build linux

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/moby/buildkit/cache"
	cachemetadata "github.com/moby/buildkit/cache/metadata"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/frontend/dockerfile/dockerfile2llb"
	"github.com/moby/buildkit/session"
	bksnapshot "github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/llbsolver"
	"github.com/moby/buildkit/solver/llbsolver/cdidevices"
	"github.com/moby/buildkit/solver/llbsolver/ops"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/leaseutil"
	"github.com/moby/buildkit/worker"
	"github.com/moby/moby/v2/daemon/graphdriver"
	"github.com/moby/moby/v2/daemon/graphdriver/overlay2"
	"github.com/moby/moby/v2/daemon/internal/layer"
	"github.com/moby/sys/user"
	digest "github.com/opencontainers/go-digest"
	bolt "go.etcd.io/bbolt"
	"gotest.tools/v3/assert"
)

// File operations only need the worker's identity on Linux. Filesystem operations,
// cache references, leases, checksums and graph scheduling use production code.
type copyTestWorker struct{ worker.Worker }

func (*copyTestWorker) ID() string                      { return "copy-regression" }
func (*copyTestWorker) CDIManager() *cdidevices.Manager { return nil }

type heldCopyMount struct{ path string }

func (m *heldCopyMount) Mount() ([]mount.Mount, func() error, error) {
	// The retained view is already mounted read-only. Let LocalMounter reuse its
	// path, as it would after mounting but before starting the checksum walk.
	return []mount.Mount{{Type: "bind", Source: m.path, Options: []string{"rbind"}}}, func() error { return nil }, nil
}
func (*heldCopyMount) IdentityMapping() *user.IdentityMapping { return nil }

type heldCopyRef struct {
	cache.ImmutableRef
	path string
}

func (r *heldCopyRef) Mount(context.Context, bool, session.Group) (bksnapshot.Mountable, error) {
	return &heldCopyMount{path: r.path}, nil
}

type beforeCopyChecksumOp struct {
	solver.Op
	before  func(context.Context, solver.Result) error
	prepare func(context.Context, solver.Result) (solver.Result, error)
}

func (o *beforeCopyChecksumOp) CacheMap(ctx context.Context, job solver.JobContext, index int) (*solver.CacheMap, bool, error) {
	cm, done, err := o.Op.CacheMap(ctx, job, index)
	if err != nil {
		return nil, done, err
	}
	for i := range cm.Deps {
		compute := cm.Deps[i].ComputeDigestFunc
		if compute != nil {
			cm.Deps[i].ComputeDigestFunc = func(ctx context.Context, res solver.Result, group session.Group) (digest.Digest, error) {
				if o.prepare != nil {
					var err error
					res, err = o.prepare(ctx, res)
					if err != nil {
						return "", err
					}
				} else if err := o.before(ctx, res); err != nil {
					return "", err
				}
				return compute(ctx, res, group)
			}
		}
	}
	return cm, done, nil
}

func TestLeaseManagerMultiStageCopyFrom(t *testing.T) {
	for _, tc := range []struct {
		name, source, wantError                         string
		pressureGC, dropOtherLease, mounted, singleCopy bool
	}{
		{name: "two-copies/A", source: "/A", dropOtherLease: true},
		{name: "two-copies/B", source: "/B", dropOtherLease: true},
		{name: "missing-source", source: "/missing", singleCopy: true, wantError: `"/missing": not found`},
		{name: "mount-before-scan/A", source: "/A", dropOtherLease: true, mounted: true, singleCopy: true},
		{name: "mount-before-scan/B", source: "/B", dropOtherLease: true, mounted: true, singleCopy: true},
		{name: "pressure-gc/A", source: "/A", dropOtherLease: true, mounted: true, singleCopy: true, pressureGC: true},
		{name: "pressure-gc/B", source: "/B", dropOtherLease: true, mounted: true, singleCopy: true, pressureGC: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := namespaces.WithNamespace(t.Context(), "copy-regression")
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
			containerdDB := metadata.NewDB(leaseDB, nil, nil)
			if tc.pressureGC {
				contentStore, err := local.NewStore(filepath.Join(root, "content"))
				assert.NilError(t, err)
				containerdDB = metadata.NewDB(leaseDB, contentStore, nil)
			}
			lm := newLeaseManager(s, metadata.NewLeaseManager(containerdDB))
			leaseManager := leaseutil.WithNamespace(lm, "copy-regression")
			md, err := cachemetadata.NewStore(filepath.Join(root, "metadata_v2.db"))
			assert.NilError(t, err)
			cm, err := cache.NewManager(cache.ManagerOpt{Snapshotter: s, LeaseManager: leaseManager, MetadataStore: md, Root: root, MountPoolRoot: filepath.Join(root, "mounts"), GarbageCollect: containerdDB.GarbageCollect})
			if err != nil {
				md.Close()
				t.Fatal(err)
			}
			t.Cleanup(func() { cm.Close() })

			// Use a real Dockerfile frontend and real FileOps. An in-memory LLB build
			// context keeps the test independent of registries and client sessions.
			buildContext := contractBuildContext()
			dockerfile := contractDockerfile
			if tc.singleCopy {
				prefix, _, found := strings.Cut(dockerfile, "COPY --from=source")
				assert.Assert(t, found)
				dockerfile = prefix + fmt.Sprintf("COPY --from=source %s/ %s/\n", tc.source, "/app"+tc.source)
			}
			converted, err := dockerfile2llb.Dockerfile2LLB(ctx, []byte(dockerfile), dockerfile2llb.ConvertOpt{MainContext: &buildContext})
			assert.NilError(t, err)
			definition, err := converted.State.Marshal(ctx)
			assert.NilError(t, err)
			edge, err := llbsolver.Load(ctx, definition.ToPB(), nil)
			assert.NilError(t, err)
			var checksumCalls atomic.Int32
			graph := solver.NewSolver(solver.SolverOpt{ResolveOpFunc: func(v solver.Vertex, _ solver.Builder) (solver.Op, error) {
				op, ok := v.Sys().(*pb.Op)
				if !ok {
					return nil, fmt.Errorf("unexpected fixture vertex: %T", v.Sys())
				}
				if op.GetFile() == nil {
					return nil, fmt.Errorf("unexpected fixture operation: %T", op.Op)
				}
				fileOp, err := ops.NewFileOp(v, &pb.Op_File{File: op.GetFile()}, cm, nil, &copyTestWorker{})
				if err != nil {
					return nil, err
				}
				for _, action := range op.GetFile().Actions {
					if copyAction := action.GetCopy(); copyAction != nil && strings.HasPrefix(copyAction.Dest, "/app/") && strings.TrimSuffix(copyAction.Src, "/") == tc.source {
						dropLease := func(ctx context.Context, result solver.Result) error {
							checksumCalls.Add(1)
							if !tc.dropOtherLease {
								return nil
							}
							ref := result.Sys().(*worker.WorkerRef).ImmutableRef
							leaseID := ref.ID()
							if active, ok := ref.GetEqualMutable(); ok {
								leaseID = active.ID()
							}
							resources, err := leaseManager.ListResources(ctx, leases.Lease{ID: leaseID})
							if err != nil || len(resources) != 1 || resources[0].Type != "snapshots/default" {
								return fmt.Errorf("source must have a durable snapshot lease: %v, %v", resources, err)
							}
							if tc.pressureGC {
								if err := pruneSharedCopySource(ctx, cm, leaseManager, resources[0], ref.ID()); err != nil {
									return err
								}
							} else {
								other, err := leaseManager.Create(ctx, leases.WithID("other-build"))
								if err != nil {
									return err
								}
								if err := leaseManager.AddResource(ctx, other, resources[0]); err != nil {
									return err
								}
								// Another build finishes just before COPY computes the checksum.
								if err := leaseManager.Delete(ctx, other); err != nil {
									return err
								}
							}
							remaining, err := leaseManager.ListResources(ctx, leases.Lease{ID: leaseID})
							if err != nil || len(remaining) != 1 || remaining[0] != resources[0] {
								return fmt.Errorf("COPY source must still have its durable lease: %v, %v", remaining, err)
							}
							return nil
						}
						if !tc.mounted {
							return &beforeCopyChecksumOp{Op: fileOp, before: dropLease}, nil
						}
						return &beforeCopyChecksumOp{Op: fileOp, prepare: func(ctx context.Context, result solver.Result) (solver.Result, error) {
							wr := result.Sys().(*worker.WorkerRef)
							mnt, err := wr.ImmutableRef.Mount(ctx, true, nil)
							if err != nil {
								return nil, err
							}
							local := bksnapshot.LocalMounter(mnt)
							path, err := local.Mount()
							if err != nil {
								return nil, err
							}
							// Retain the real mounted view while the other lease is released.
							// The checksum now reaches the filesystem rather than re-statting
							// the deleted graphdriver directory before mounting it.
							t.Cleanup(func() { local.Unmount() })
							for _, source := range []string{"A/file.txt", "B/file.txt"} {
								if _, err := os.ReadFile(filepath.Join(path, source)); err != nil {
									return nil, fmt.Errorf("source must exist before lease release: %w", err)
								}
							}
							if err := dropLease(ctx, result); err != nil {
								return nil, err
							}
							return worker.NewWorkerRefResult(&heldCopyRef{ImmutableRef: wr.ImmutableRef, path: path}, wr.Worker), nil
						}}, nil
					}
				}
				return fileOp, nil
			}})
			t.Cleanup(graph.Close)
			job, err := graph.NewJob("multi-stage-copy")
			assert.NilError(t, err)
			t.Cleanup(func() { job.Discard() })
			result, err := job.Build(ctx, edge)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), "failed to compute cache key") || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("expected missing COPY source file, got %v", err)
				}
				t.Logf("expected validation error: %v", err)
				return
			}
			if err != nil {
				t.Fatalf("COPY --from lost a leased stage: %v", err)
			}
			t.Cleanup(func() { result.Release(context.WithoutCancel(ctx)) })
			if checksumCalls.Load() == 0 {
				t.Fatal("final COPY must calculate its source checksum")
			}
			ref := result.Sys().(*worker.WorkerRef).ImmutableRef
			mnt, err := ref.Mount(ctx, true, nil)
			assert.NilError(t, err)
			local := bksnapshot.LocalMounter(mnt)
			path, err := local.Mount()
			assert.NilError(t, err)
			defer local.Unmount()
			expected := map[string]string{"app/A/file.txt": "A\n", "app/B/file.txt": "B\n"}
			if tc.singleCopy {
				expected = map[string]string{"app" + tc.source + "/file.txt": strings.TrimPrefix(tc.source, "/") + "\n"}
			}
			for file, want := range expected {
				payload, err := os.ReadFile(filepath.Join(path, file))
				if err != nil || string(payload) != want {
					t.Fatalf("final stage must contain %s: %q, %v", file, payload, err)
				}
			}
		})
	}
}

// Evict a real unused cache record that also owns the shared COPY snapshot.
// Cache pressure and GC complete synchronously before the build resumes.
func pruneSharedCopySource(ctx context.Context, cm cache.Manager, lm leases.Manager, source leases.Resource, activeID string) error {
	garbage, err := cm.New(ctx, nil, nil, cache.CachePolicyRetain)
	if err != nil {
		return err
	}
	garbageReleased := false
	defer func() {
		if !garbageReleased {
			_ = garbage.Release(context.WithoutCancel(ctx))
		}
	}()
	garbageID := garbage.ID()
	mnt, err := garbage.Mount(ctx, false, nil)
	if err != nil {
		return err
	}
	local := bksnapshot.LocalMounter(mnt)
	path, err := local.Mount()
	if err != nil {
		return err
	}
	writeErr := os.WriteFile(filepath.Join(path, "gc-pad"), make([]byte, 4096), 0o644)
	unmountErr := local.Unmount()
	if writeErr != nil {
		return writeErr
	}
	if unmountErr != nil {
		return unmountErr
	}
	if err := lm.AddResource(ctx, leases.Lease{ID: garbageID}, source); err != nil {
		return err
	}
	if err := garbage.Release(ctx); err != nil {
		return err
	}
	garbageReleased = true
	usage, err := cm.DiskUsage(ctx, client.DiskUsageInfo{})
	if err != nil {
		return err
	}
	var eligible, active bool
	for _, item := range usage {
		if item.ID == garbageID {
			eligible = !item.InUse && item.Size > 1
		}
		if item.ID == activeID {
			active = item.InUse
		}
	}
	if !eligible || !active {
		return fmt.Errorf("invalid GC fixture: unused=%v active=%v", eligible, active)
	}
	deleted := make(chan client.UsageInfo, 64)
	if err := cm.Prune(ctx, deleted, client.PruneInfo{All: true, MaxUsedSpace: 1, ReservedSpace: 1}); err != nil {
		return err
	}
	found := false
	for len(deleted) > 0 {
		if (<-deleted).ID == garbageID {
			found = true
		}
	}
	if !found {
		return errors.New("GC must actually reclaim the unused record")
	}
	return nil
}
