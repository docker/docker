//go:build linux

package snapshot

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/moby/buildkit/cache"
	cachemetadata "github.com/moby/buildkit/cache/metadata"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/frontend/dockerfile/dockerfile2llb"
	"github.com/moby/buildkit/session"
	bksnapshot "github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/llbsolver"
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

//go:embed testdata/gc-contract/Dockerfile
var contractDockerfile string

//go:embed testdata/gc-contract/A/file.txt
var contractFileA []byte

//go:embed testdata/gc-contract/B/file.txt
var contractFileB []byte

// Use the same files as the standalone Docker build context. In-memory LLB
// avoids requiring a client session when exercising the Dockerfile frontend.
func contractBuildContext() llb.State {
	return llb.Scratch().File(llb.Mkdir("/A", 0o755).Mkfile("/A/file.txt", 0o644, contractFileA)).
		File(llb.Mkdir("/B", 0o755).Mkfile("/B/file.txt", 0o644, contractFileB))
}

// Stop at a boundary where the caller holds no cache/record/lease mutex.
// Complete real GC before returning to the build. No sleep, background-start
// acknowledgement, or probabilistic scheduler overlap is used here. Windows
// inside the lease mutex are covered by TestLeaseManagerHandoffCannotLoseSource.
type gcCheckpoint struct {
	wanted     string
	armed, hit atomic.Bool
	collect    func(context.Context) error
}

func (p *gcCheckpoint) at(ctx context.Context, name string) error {
	if !p.armed.Load() || name != p.wanted || !p.hit.CompareAndSwap(false, true) {
		return nil
	}
	return p.collect(ctx)
}

// Resolve aliases and ancestors independently of the lease-manager mirror.
// Every physical Remove is checked against the real durable lease database.
type leaseDeletionGuard struct {
	graphdriver.Driver
	base             leases.Manager
	ctx              context.Context
	mu               sync.Mutex
	aliases, parents map[string]string
	violations       []string
	removed          []string
}

func (g *leaseDeletionGuard) prepare(key, parent string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.aliases[key], g.parents[key] = key, parent
}
func (g *leaseDeletionGuard) alias(key, target string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.aliases[key] = target
}
func (g *leaseDeletionGuard) requires(key, physical string, seen map[string]bool) bool {
	if key == "" || seen[key] {
		return false
	}
	seen[key] = true
	target := g.aliases[key]
	if target == "" {
		target = key
	}
	if target == physical {
		return true
	}
	if target != key && g.requires(target, physical, seen) {
		return true
	}
	return g.requires(g.parents[key], physical, seen)
}
func (g *leaseDeletionGuard) Remove(id string) error {
	ll, err := g.base.List(g.ctx)
	if err != nil {
		return err
	}
	for _, l := range ll {
		rr, err := g.base.ListResources(g.ctx, l)
		if err != nil {
			return err
		}
		for _, r := range rr {
			if r.Type != "snapshots/default" {
				continue
			}
			g.mu.Lock()
			if g.requires(r.ID, id, map[string]bool{}) {
				g.violations = append(g.violations, fmt.Sprintf("Remove(%s) while durable lease %s requires snapshot %s", id, l.ID, r.ID))
			}
			g.mu.Unlock()
		}
	}
	g.mu.Lock()
	g.removed = append(g.removed, id)
	g.mu.Unlock()
	return g.Driver.Remove(id)
}

// Record physical snapshot aliases independently of the adapter's ref maps.
type guardedSnapshotter struct {
	bksnapshot.Snapshotter
	guard *leaseDeletionGuard
}

func (s *guardedSnapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) error {
	s.guard.prepare(key, parent)
	return s.Snapshotter.Prepare(ctx, key, parent, opts...)
}
func (s *guardedSnapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	s.guard.alias(name, key)
	return s.Snapshotter.Commit(ctx, name, key, opts...)
}
func (s *guardedSnapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) (bksnapshot.Mountable, error) {
	s.guard.alias(key, parent)
	return s.Snapshotter.View(ctx, key, parent, opts...)
}

type checkpointFileOp struct {
	solver.Op
	p     *gcCheckpoint
	label string
}

func (o *checkpointFileOp) CacheMap(ctx context.Context, job solver.JobContext, index int) (*solver.CacheMap, bool, error) {
	cm, done, err := o.Op.CacheMap(ctx, job, index)
	if err != nil {
		return nil, done, err
	}
	for i := range cm.Deps {
		compute := cm.Deps[i].ComputeDigestFunc
		if compute != nil {
			cm.Deps[i].ComputeDigestFunc = func(ctx context.Context, res solver.Result, group session.Group) (digest.Digest, error) {
				if strings.HasPrefix(o.label, "final/") {
					if err := res.Sys().(*worker.WorkerRef).ImmutableRef.Finalize(ctx); err != nil {
						return "", err
					}
				}
				if err := o.p.at(ctx, o.label+"/checksum-before"); err != nil {
					return "", err
				}
				d, err := compute(ctx, res, group)
				if err == nil {
					err = o.p.at(ctx, o.label+"/checksum-after")
				}
				return d, err
			}
		}
	}
	return cm, done, nil
}
func (o *checkpointFileOp) Exec(ctx context.Context, job solver.JobContext, inputs []solver.Result) ([]solver.Result, error) {
	if err := o.p.at(ctx, o.label+"/copy-before"); err != nil {
		return nil, err
	}
	result, err := o.Op.Exec(ctx, job, inputs)
	if err == nil {
		err = o.p.at(ctx, o.label+"/copy-after")
	}
	return result, err
}

func TestGCNeverDeletesLeasedMultiStageLayer(t *testing.T) {
	points := []string{"build-start", "build-result"}
	for _, path := range []string{"A", "B"} {
		for _, boundary := range []string{"copy-before", "copy-after"} {
			points = append(points, "source/"+path+"/"+boundary)
		}
		for _, boundary := range []string{"checksum-before", "checksum-after", "copy-before", "copy-after"} {
			points = append(points, "final/"+path+"/"+boundary)
		}
	}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			ctx := namespaces.WithNamespace(t.Context(), "gc-contract")
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
			content, err := local.NewStore(filepath.Join(root, "content"))
			assert.NilError(t, err)
			mdb := metadata.NewDB(leaseDB, content, nil)
			base := metadata.NewLeaseManager(mdb)
			p := &gcCheckpoint{wanted: point}
			guard := &leaseDeletionGuard{Driver: driver, base: base, ctx: ctx, aliases: map[string]string{}, parents: map[string]string{}}
			s := &snapshotter{opt: Opt{GraphDriver: guard}, db: db, refs: map[string]layer.Layer{}}
			lm := leaseutil.WithNamespace(newLeaseManager(s, base), "gc-contract")
			sn := &guardedSnapshotter{Snapshotter: s, guard: guard}
			md, err := cachemetadata.NewStore(filepath.Join(root, "metadata_v2.db"))
			assert.NilError(t, err)
			cm, err := cache.NewManager(cache.ManagerOpt{Snapshotter: sn, LeaseManager: lm, MetadataStore: md, GarbageCollect: mdb.GarbageCollect, Root: root, MountPoolRoot: filepath.Join(root, "mounts")})
			assert.NilError(t, err)
			t.Cleanup(func() { cm.Close() })
			// Real retained cache from a finished build ensures every GC attempt
			// actually evicts something, rather than vacuously passing on no work.
			garbage, err := cm.New(ctx, nil, nil, cache.CachePolicyRetain)
			assert.NilError(t, err)
			garbageID := garbage.ID()
			mnt, err := garbage.Mount(ctx, false, nil)
			assert.NilError(t, err)
			localMount := bksnapshot.LocalMounter(mnt)
			path, err := localMount.Mount()
			assert.NilError(t, err)
			if err := os.WriteFile(filepath.Join(path, "old-cache"), make([]byte, 4096), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := localMount.Unmount(); err != nil {
				t.Fatal(err)
			}
			if err := garbage.Release(ctx); err != nil {
				t.Fatal(err)
			}
			p.collect = func(ctx context.Context) error {
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
					return errors.New("GC did not evict the eligible finished-build cache")
				}
				return nil
			}
			buildContext := contractBuildContext()
			converted, err := dockerfile2llb.Dockerfile2LLB(ctx, []byte(contractDockerfile), dockerfile2llb.ConvertOpt{MainContext: &buildContext})
			assert.NilError(t, err)
			def, err := converted.State.Marshal(ctx)
			assert.NilError(t, err)
			edge, err := llbsolver.Load(ctx, def.ToPB(), nil)
			assert.NilError(t, err)
			w := &copyTestWorker{}
			graph := solver.NewSolver(solver.SolverOpt{ResolveOpFunc: func(v solver.Vertex, _ solver.Builder) (solver.Op, error) {
				op := v.Sys().(*pb.Op)
				if op.GetFile() != nil {
					file, err := ops.NewFileOp(v, &pb.Op_File{File: op.GetFile()}, cm, nil, w)
					if err != nil {
						return nil, err
					}
					label := "context"
					for _, action := range op.GetFile().Actions {
						if copyAction := action.GetCopy(); copyAction != nil {
							label = "source/" + strings.Trim(copyAction.Src, "/")
							if strings.HasPrefix(copyAction.Dest, "/app/") {
								label = "final/" + strings.Trim(copyAction.Src, "/")
							}
						}
					}
					return &checkpointFileOp{Op: file, p: p, label: label}, nil
				}
				return nil, fmt.Errorf("unexpected fixture op: %T", op.Op)
			}})
			t.Cleanup(graph.Close)
			job, err := graph.NewJob("active-multi-stage-build")
			assert.NilError(t, err)
			t.Cleanup(func() { job.Discard() })
			p.armed.Store(true)
			if err := p.at(ctx, "build-start"); err != nil {
				t.Fatal(err)
			}
			result, err := job.Build(ctx, edge)
			if err != nil {
				t.Fatalf("multi-stage build failed with GC at %s: %v", point, err)
			}
			t.Cleanup(func() { result.Release(context.WithoutCancel(ctx)) })
			if err := p.at(ctx, "build-result"); err != nil {
				t.Fatal(err)
			}
			if !p.hit.Load() {
				t.Fatalf("checkpoint %s was not reached", point)
			}
			guard.mu.Lock()
			violations := append([]string(nil), guard.violations...)
			removals := len(guard.removed)
			guard.mu.Unlock()
			if len(violations) > 0 {
				t.Fatalf("active-layer deletion invariant violated: %v", violations)
			}
			if removals == 0 {
				t.Fatal("the physical deletion guard was never exercised")
			}
			ref := result.Sys().(*worker.WorkerRef).ImmutableRef
			mnt, err = ref.Mount(ctx, true, nil)
			assert.NilError(t, err)
			output := bksnapshot.LocalMounter(mnt)
			path, err = output.Mount()
			assert.NilError(t, err)
			defer output.Unmount()
			for file, want := range map[string]string{"app/A/file.txt": "A\n", "app/B/file.txt": "B\n"} {
				got, err := os.ReadFile(filepath.Join(path, file))
				if err != nil || string(got) != want {
					t.Fatalf("invalid final artifact %s: %q %v", file, got, err)
				}
			}
			t.Logf("GC at %s: active leases and ancestors intact, %d physical removals checked, A and B verified", point, removals)
		})
	}
}
