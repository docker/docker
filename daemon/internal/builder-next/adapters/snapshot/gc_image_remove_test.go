//go:build linux

package snapshot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/plugins/content/local"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/buildkit/cache"
	cachemetadata "github.com/moby/buildkit/cache/metadata"
	"github.com/moby/buildkit/exporter"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/frontend/dockerfile/dockerfile2llb"
	bksnapshot "github.com/moby/buildkit/snapshot"
	containerdsnapshot "github.com/moby/buildkit/snapshot/containerd"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/llbsolver"
	"github.com/moby/buildkit/solver/llbsolver/ops"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/worker"
	"github.com/moby/moby/client"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/events"
	"github.com/moby/moby/v2/daemon/graphdriver"
	"github.com/moby/moby/v2/daemon/images"
	"github.com/moby/moby/v2/daemon/internal/builder-next/exporter/mobyexporter"
	"github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/internal/layer"
	refstore "github.com/moby/moby/v2/daemon/internal/refstore"
	"github.com/moby/moby/v2/daemon/server"
	imagerouter "github.com/moby/moby/v2/daemon/server/router/image"
	bolt "go.etcd.io/bbolt"
	"gotest.tools/v3/assert"
)

// gcImageFixture uses the production image API, stores and snapshotter.
type gcImageFixture struct {
	root     string
	images   image.Store
	cache    cache.Manager
	leases   leases.Manager
	client   *client.Client
	snapshot *snapshotter
	exporter exporter.ExporterInstance
}

// newGCImageFixture isolates real overlay2 layers and durable leases per case.
func newGCImageFixture(t *testing.T, ctx context.Context) *gcImageFixture {
	t.Helper()
	root := newSnapshotTestRoot(t)
	ls, err := layer.NewStoreFromOptions(layer.StoreOptions{Root: root, GraphDriver: "overlay2"})
	if graphdriver.IsDriverNotSupported(err) || errors.Is(err, graphdriver.ErrNotSupported) {
		t.Skipf("overlay2 unavailable: %v", err)
	}
	assert.NilError(t, err)
	t.Cleanup(func() { ls.Cleanup() })
	driver := ls.(interface{ Driver() graphdriver.Driver }).Driver()
	backend, err := image.NewFSStoreBackend(filepath.Join(root, "images"))
	assert.NilError(t, err)
	imgStore, err := image.NewImageStore(backend, ls)
	assert.NilError(t, err)
	refs, err := refstore.NewReferenceStore(filepath.Join(root, "repositories.json"))
	assert.NilError(t, err)
	db, err := bolt.Open(filepath.Join(root, "leases.db"), 0o600, nil)
	assert.NilError(t, err)
	t.Cleanup(func() { db.Close() })
	content, err := local.NewStore(filepath.Join(root, "content"))
	assert.NilError(t, err)
	mdb := metadata.NewDB(db, content, nil)
	if err := mdb.Init(ctx); err != nil {
		t.Fatal(err)
	}
	store := containerdsnapshot.NewContentStore(mdb.ContentStore(), "gc-image-copy")
	base := metadata.NewLeaseManager(mdb)
	buildRoot := filepath.Join(root, "buildkit")
	if err := os.MkdirAll(buildRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	sn, lm, err := NewSnapshotter(Opt{GraphDriver: driver, LayerStore: ls, Root: buildRoot}, base, "gc-image-copy")
	assert.NilError(t, err)
	t.Cleanup(func() { sn.Close() })
	md, err := cachemetadata.NewStore(filepath.Join(buildRoot, "cache.db"))
	assert.NilError(t, err)
	cm, err := cache.NewManager(cache.ManagerOpt{Snapshotter: sn, LeaseManager: lm, ContentStore: store, MetadataStore: md, Root: buildRoot, MountPoolRoot: filepath.Join(root, "mounts"), GarbageCollect: mdb.GarbageCollect})
	if err != nil {
		md.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { cm.Close() })
	svc := images.NewImageService(ctx, images.ImageServiceConfig{
		ContainerStore: container.NewMemoryStore(), EventsService: events.New(),
		ImageStore: imgStore, LayerStore: ls, ReferenceStore: refs,
		Leases: base, ContentStore: store, ContentNamespace: "gc-image-copy",
	})
	// Exercise the client's DELETE request, the production HTTP router and the
	// classic image/layer stores used by docker rmi with the overlay2 backend.
	router := (&server.Server{}).CreateMux(ctx, imagerouter.NewRouter(svc, nil))
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1.56/images/gc-copy:source" || r.URL.Query().Get("noprune") != "1" {
			t.Errorf("expected DELETE with noprune=1, got %s", r.URL)
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(api.Close)
	cli, err := client.New(client.WithHost("tcp://"+api.Listener.Addr().String()), client.WithHTTPClient(api.Client()), client.WithAPIVersion("1.56"))
	assert.NilError(t, err)
	t.Cleanup(func() { cli.Close() })
	exp, err := mobyexporter.New(mobyexporter.Opt{ImageStore: imgStore, Differ: sn.(*snapshotter), ImageTagger: svc, ContentStore: store, LeaseManager: lm})
	assert.NilError(t, err)
	instance, err := exp.Resolve(ctx, 0, map[string]string{"name": "gc-copy:source"})
	assert.NilError(t, err)
	return &gcImageFixture{root: root, images: imgStore, cache: cm, leases: lm, client: cli, snapshot: sn.(*snapshotter), exporter: instance}
}

// Publish an already exported source to both COPY consumers. Exporting after
// publishing would introduce a separate Finalize-versus-checksum race unrelated
// to image deletion and would not model removing an existing image.
type exportCopySourceOp struct {
	solver.Op
	fixture *gcImageFixture
	imageID *image.ID
}

func (o *exportCopySourceOp) Exec(ctx context.Context, job solver.JobContext, inputs []solver.Result) ([]solver.Result, error) {
	outputs, err := o.Op.Exec(ctx, job, inputs)
	if err != nil {
		return nil, err
	}
	if len(outputs) != 1 {
		return nil, errors.New("source stage must have one result")
	}
	ref := outputs[0].Sys().(*worker.WorkerRef).ImmutableRef
	resp, finalize, desc, err := o.fixture.exporter.Export(ctx, &exporter.Source{Ref: ref}, exporter.ExportBuildInfo{})
	if err != nil {
		return nil, err
	}
	if desc != nil {
		defer desc.Release()
	}
	if finalize != nil {
		if err := finalize(ctx); err != nil {
			return nil, err
		}
	}
	*o.imageID = image.ID(resp[exptypes.ExporterImageDigestKey])
	return outputs, nil
}

// GC may evict another owner, and RMI may remove the shared local image, but
// neither ordering may reclaim layers still owned by an active COPY source.
// Export the image before publishing its source to concurrent COPY consumers.
func TestGCImageRemovePreservesMultiStageCopy(t *testing.T) {
	for _, tc := range []struct {
		name, selector, cleanup string
	}{
		{name: "rmi-only/A", selector: "/A"},
		{name: "rmi-only/B", selector: "/B"},
		{name: "gc-before-rmi/A", selector: "/A", cleanup: "before"},
		{name: "gc-before-rmi/B", selector: "/B", cleanup: "before"},
		{name: "gc-after-rmi/A", selector: "/A", cleanup: "after"},
		{name: "gc-after-rmi/B", selector: "/B", cleanup: "after"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := namespaces.WithNamespace(t.Context(), "gc-image-copy")
			f := newGCImageFixture(t, ctx)
			buildContext := contractBuildContext()
			converted, err := dockerfile2llb.Dockerfile2LLB(ctx, []byte(contractDockerfile), dockerfile2llb.ConvertOpt{MainContext: &buildContext})
			assert.NilError(t, err)
			def, err := converted.State.Marshal(ctx)
			assert.NilError(t, err)
			edge, err := llbsolver.Load(ctx, def.ToPB(), nil)
			assert.NilError(t, err)
			removed := false
			var imgID image.ID
			graph := solver.NewSolver(solver.SolverOpt{ResolveOpFunc: func(v solver.Vertex, _ solver.Builder) (solver.Op, error) {
				op := v.Sys().(*pb.Op)
				if op.GetFile() == nil {
					return nil, fmt.Errorf("unexpected fixture op: %T", op.Op)
				}
				file, err := ops.NewFileOp(v, &pb.Op_File{File: op.GetFile()}, f.cache, nil, &copyTestWorker{})
				if err != nil {
					return nil, err
				}
				for _, action := range op.GetFile().Actions {
					copyAction := action.GetCopy()
					if copyAction != nil && strings.TrimSuffix(copyAction.Src, "/") == "/B" && strings.TrimSuffix(copyAction.Dest, "/") == "/B" {
						return &exportCopySourceOp{Op: file, fixture: f, imageID: &imgID}, nil
					}
					if copyAction == nil || !strings.HasPrefix(copyAction.Dest, "/app/") || strings.TrimSuffix(copyAction.Src, "/") != tc.selector {
						continue
					}
					return &beforeCopyChecksumOp{Op: file, prepare: func(ctx context.Context, result solver.Result) (solver.Result, error) {
						wr := result.Sys().(*worker.WorkerRef)
						if imgID == "" {
							return nil, errors.New("source image must be exported before COPY starts")
						}

						img, err := f.images.Get(imgID)
						if err != nil {
							return nil, err
						}
						resources, err := f.leases.ListResources(ctx, leases.Lease{ID: wr.ImmutableRef.ID()})
						if err != nil || len(resources) != 1 || resources[0].Type != "snapshots/default" {
							return nil, fmt.Errorf("active COPY must own its durable snapshot: %v %v", resources, err)
						}
						chain := img.RootFS.ChainID()
						// Inspect an existing handle without acquiring another layer
						// reference that could accidentally protect the fixture.
						f.snapshot.mu.Lock()
						shared := f.snapshot.refs[resources[0].ID]
						f.snapshot.mu.Unlock()
						if shared == nil || shared.ChainID() != chain {
							return nil, errors.New("image and active COPY must share the same registered layer chain")
						}
						physical := map[layer.ChainID]string{}
						for l := shared; l != nil; l = l.Parent() {
							id, err := getGraphID(l)
							if err != nil {
								return nil, err
							}
							physical[l.ChainID()] = filepath.Join(f.root, "overlay2", id, "diff")
						}
						if len(physical) < 2 {
							return nil, errors.New("source image must contain a real multilayer chain")
						}
						if tc.cleanup == "before" {
							if err := pruneSharedCopySource(ctx, f.cache, f.leases, resources[0], wr.ImmutableRef.ID()); err != nil {
								return nil, err
							}
						}
						ref := "gc-copy:source"
						// Complete DELETE while COPY is paused, before its checksum walk.
						deletion, err := f.client.ImageRemove(ctx, ref, client.ImageRemoveOptions{PruneChildren: false})
						if err != nil {
							return nil, err
						}
						if tc.cleanup == "after" {
							if err := pruneSharedCopySource(ctx, f.cache, f.leases, resources[0], wr.ImmutableRef.ID()); err != nil {
								return nil, err
							}
						}
						for _, item := range deletion.Items {
							if item.Deleted == imgID.String() {
								removed = true
							}
							if _, required := physical[layer.ChainID(item.Deleted)]; required {
								return nil, fmt.Errorf("rmi physically deleted layer %s still required by active COPY", item.Deleted)
							}
						}
						if _, err := f.images.Get(imgID); !cerrdefs.IsNotFound(err) || !removed {
							return nil, fmt.Errorf("rmi must delete the image, not only its tag: %v", err)
						}
						remaining, err := f.leases.ListResources(ctx, leases.Lease{ID: wr.ImmutableRef.ID()})
						if err != nil || len(remaining) != 1 || remaining[0] != resources[0] {
							return nil, fmt.Errorf("active COPY must retain its durable source lease: %v %v", remaining, err)
						}
						for id, path := range physical {
							if _, err := os.Stat(path); err != nil {
								return nil, fmt.Errorf("rmi removed active layer or ancestor %s: %w", id, err)
							}
						}
						t.Logf("DELETE removed image %s; %d shared layers remain for COPY %s", imgID, len(physical), tc.selector)
						return result, nil
					}}, nil
				}
				return file, nil
			}})
			t.Cleanup(graph.Close)
			job, err := graph.NewJob("rmi-active-copy")
			assert.NilError(t, err)
			t.Cleanup(func() { job.Discard() })
			result, err := job.Build(ctx, edge)
			if err != nil {
				t.Fatalf("rmi interrupted active COPY: %v", err)
			}
			t.Cleanup(func() { result.Release(context.WithoutCancel(ctx)) })
			if !removed {
				t.Fatal("build never reached ImageRemove")
			}
			mnt, err := result.Sys().(*worker.WorkerRef).ImmutableRef.Mount(ctx, true, nil)
			assert.NilError(t, err)
			output := bksnapshot.LocalMounter(mnt)
			path, err := output.Mount()
			assert.NilError(t, err)
			defer output.Unmount()
			for _, name := range []string{"A", "B"} {
				payload, err := os.ReadFile(filepath.Join(path, "app", name, "file.txt"))
				if err != nil || string(payload) != name+"\n" {
					t.Fatalf("invalid COPY artifact %s after rmi: %q %v", name, payload, err)
				}
			}
		})
	}
}
