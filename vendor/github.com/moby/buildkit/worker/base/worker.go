package base

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/pkg/gc"
	"github.com/containerd/platforms"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/cache/metadata"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/executor"
	"github.com/moby/buildkit/executor/resources"
	resourcestypes "github.com/moby/buildkit/executor/resources/types"
	"github.com/moby/buildkit/exporter"
	imageexporter "github.com/moby/buildkit/exporter/containerimage"
	localexporter "github.com/moby/buildkit/exporter/local"
	ociexporter "github.com/moby/buildkit/exporter/oci"
	tarexporter "github.com/moby/buildkit/exporter/tar"
	"github.com/moby/buildkit/frontend"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/snapshot"
	containerdsnapshot "github.com/moby/buildkit/snapshot/containerd"
	"github.com/moby/buildkit/snapshot/imagerefchecker"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/llbsolver/cdidevices"
	"github.com/moby/buildkit/solver/llbsolver/linuxresources"
	"github.com/moby/buildkit/solver/llbsolver/mounts"
	"github.com/moby/buildkit/solver/llbsolver/ops"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/source"
	"github.com/moby/buildkit/source/containerblob"
	"github.com/moby/buildkit/source/containerimage"
	"github.com/moby/buildkit/source/git"
	"github.com/moby/buildkit/source/http"
	"github.com/moby/buildkit/source/local"
	"github.com/moby/buildkit/util/archutil"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/contentutil"
	"github.com/moby/buildkit/util/leaseutil"
	"github.com/moby/buildkit/util/network"
	"github.com/moby/buildkit/util/progress"
	"github.com/moby/buildkit/util/progress/controller"
	"github.com/moby/buildkit/worker"
	"github.com/moby/sys/user"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
)

const labelCreatedAt = "buildkit/createdat"

// TODO: this file should be removed. containerd defines ContainerdWorker, oci defines OCIWorker. There is no base worker.

// WorkerOpt is specific to a worker.
// See also CommonOpt.
type WorkerOpt struct {
	ID               string
	Root             string
	Labels           map[string]string
	Platforms        []ocispecs.Platform
	GCPolicy         []client.PruneInfo
	BuildkitVersion  client.BuildkitVersion
	NetworkProviders map[pb.NetMode]network.Provider
	ProxyProvider    network.ProxyProvider
	Executor         executor.Executor
	Snapshotter      snapshot.Snapshotter
	ContentStore     *containerdsnapshot.Store
	Applier          diff.Applier
	Differ           diff.Comparer
	ImageStore       images.Store // optional
	RegistryHosts    docker.RegistryHosts
	IdentityMapping  *user.IdentityMapping
	LeaseManager     *leaseutil.Manager
	GarbageCollect   func(context.Context) (gc.Stats, error)
	ParallelismSem   *semaphore.Weighted
	MetadataStore    *metadata.Store
	ContentMetadata  io.Closer
	MountPoolRoot    string
	ResourceMonitor  *resources.Monitor
	CDIManager       *cdidevices.Manager
}

// Worker is a local worker instance with dedicated snapshotter, cache, and so on.
// TODO: s/Worker/OpWorker/g ?
type Worker struct {
	WorkerOpt
	CacheMgr        cache.Manager
	SourceManager   *source.Manager
	imageWriter     *imageexporter.ImageWriter
	ImageSource     *containerimage.Source
	OCILayoutSource *containerimage.Source
	GitSource       *git.Source
	HTTPSource      *http.Source
	platformsMu     sync.Mutex
}

// NewWorker instantiates a local worker
func NewWorker(ctx context.Context, opt WorkerOpt) (*Worker, error) {
	imageRefChecker := imagerefchecker.New(imagerefchecker.Opt{
		ImageStore:   opt.ImageStore,
		ContentStore: opt.ContentStore,
	})

	cm, err := cache.NewManager(cache.ManagerOpt{
		Snapshotter:     opt.Snapshotter,
		PruneRefChecker: imageRefChecker,
		Applier:         opt.Applier,
		GarbageCollect:  opt.GarbageCollect,
		LeaseManager:    opt.LeaseManager,
		ContentStore:    opt.ContentStore,
		Differ:          opt.Differ,
		MetadataStore:   opt.MetadataStore,
		Root:            opt.Root,
		MountPoolRoot:   opt.MountPoolRoot,
	})
	if err != nil {
		return nil, err
	}

	sm, err := source.NewManager()
	if err != nil {
		return nil, err
	}

	is, err := containerimage.NewSource(containerimage.SourceOpt{
		Snapshotter:   opt.Snapshotter,
		ContentStore:  opt.ContentStore,
		Applier:       opt.Applier,
		ImageStore:    opt.ImageStore,
		CacheAccessor: cm,
		RegistryHosts: opt.RegistryHosts,
		ResolverType:  containerimage.ResolverTypeRegistry,
		LeaseManager:  opt.LeaseManager,
	})
	if err != nil {
		return nil, err
	}

	sm.Register(is)

	var gitSource *git.Source
	ibs, err := containerblob.NewSource(containerblob.SourceOpt{
		ContentStore:  opt.ContentStore,
		CacheAccessor: cm,
		RegistryHosts: opt.RegistryHosts,
	})
	if err != nil {
		return nil, err
	}

	sm.Register(ibs)
	if err := git.Supported(); err == nil {
		gs, err := git.NewSource(git.Opt{
			CacheAccessor: cm,
			RegistryHosts: opt.RegistryHosts,
		})
		if err != nil {
			return nil, err
		}
		sm.Register(gs)
		gitSource = gs
	} else {
		bklog.G(ctx).Warnf("git source cannot be enabled: %v", err)
	}

	hs, err := http.NewSource(http.Opt{
		CacheAccessor: cm,
	})
	if err != nil {
		return nil, err
	}

	sm.Register(hs)

	ss, err := local.NewSource(local.Opt{
		CacheAccessor: cm,
	})
	if err != nil {
		return nil, err
	}
	sm.Register(ss)

	os, err := containerimage.NewSource(containerimage.SourceOpt{
		Snapshotter:   opt.Snapshotter,
		ContentStore:  opt.ContentStore,
		Applier:       opt.Applier,
		ImageStore:    opt.ImageStore,
		CacheAccessor: cm,
		ResolverType:  containerimage.ResolverTypeOCILayout,
		LeaseManager:  opt.LeaseManager,
	})
	if err != nil {
		return nil, err
	}

	sm.Register(os)

	iw, err := imageexporter.NewImageWriter(imageexporter.WriterOpt{
		Snapshotter:  opt.Snapshotter,
		ContentStore: opt.ContentStore,
		Applier:      opt.Applier,
		Differ:       opt.Differ,
	})
	if err != nil {
		return nil, err
	}

	leases, err := opt.LeaseManager.List(ctx, "labels.\"buildkit/lease.temporary\"")
	if err != nil {
		return nil, err
	}
	for _, l := range leases {
		opt.LeaseManager.Delete(ctx, l)
	}

	return &Worker{
		WorkerOpt:       opt,
		CacheMgr:        cm,
		SourceManager:   sm,
		imageWriter:     iw,
		ImageSource:     is,
		OCILayoutSource: os,
		GitSource:       gitSource,
		HTTPSource:      hs,
	}, nil
}

func (w *Worker) GarbageCollect(ctx context.Context) error {
	if w.WorkerOpt.GarbageCollect == nil {
		return nil
	}
	_, err := w.WorkerOpt.GarbageCollect(ctx)
	return err
}

func (w *Worker) Close() error {
	var errs []error
	if err := w.MetadataStore.Close(); err != nil {
		errs = append(errs, err)
	}
	if w.ContentMetadata != nil {
		if err := w.ContentMetadata.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if w.ProxyProvider != nil {
		if err := w.ProxyProvider.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	for _, provider := range w.NetworkProviders {
		if err := provider.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if w.ResourceMonitor != nil {
		if err := w.ResourceMonitor.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return stderrors.Join(errs...)
}

func (w *Worker) ContentStore() *containerdsnapshot.Store {
	return w.WorkerOpt.ContentStore
}

func (w *Worker) LeaseManager() *leaseutil.Manager {
	return w.WorkerOpt.LeaseManager
}

func (w *Worker) CDIManager() *cdidevices.Manager {
	return w.WorkerOpt.CDIManager
}

func (w *Worker) ID() string {
	return w.WorkerOpt.ID
}

func (w *Worker) Labels() map[string]string {
	return w.WorkerOpt.Labels
}

func (w *Worker) Platforms(noCache bool) []ocispecs.Platform {
	w.platformsMu.Lock()
	defer w.platformsMu.Unlock()
	if noCache {
		matchers := make([]platforms.MatchComparer, len(w.WorkerOpt.Platforms))
		for i, p := range w.WorkerOpt.Platforms {
			matchers[i] = platforms.Only(p)
		}
		for _, p := range archutil.SupportedPlatforms(noCache) {
			exists := false
			for _, m := range matchers {
				if m.Match(p) {
					exists = true
					break
				}
			}
			if !exists {
				w.WorkerOpt.Platforms = append(w.WorkerOpt.Platforms, p)
			}
		}
	}
	return w.WorkerOpt.Platforms
}

func (w *Worker) GCPolicy() []client.PruneInfo {
	return w.WorkerOpt.GCPolicy
}

func (w *Worker) BuildkitVersion() client.BuildkitVersion {
	return w.WorkerOpt.BuildkitVersion
}

func (w *Worker) LoadRef(ctx context.Context, id string, hidden bool) (cache.ImmutableRef, error) {
	var opts []cache.RefOption
	if hidden {
		opts = append(opts, cache.NoUpdateLastUsed)
	}
	if id == "" {
		// results can have nil refs if they are optimized out to be equal to scratch,
		// i.e. Diff(A,A) == scratch
		return nil, nil
	}

	pg := solver.ProgressControllerFromContext(ctx)
	ref, err := w.CacheMgr.Get(ctx, id, pg, opts...)
	var needsRemoteProviders cache.NeedsRemoteProviderError
	if errors.As(err, &needsRemoteProviders) {
		if optGetter := solver.CacheOptGetterOf(ctx); optGetter != nil {
			var keys []any
			for _, dgst := range needsRemoteProviders {
				keys = append(keys, cache.DescHandlerKey(dgst))
			}
			descHandlers := cache.DescHandlers(make(map[digest.Digest]*cache.DescHandler))
			for k, v := range optGetter(true, keys...) {
				if key, ok := k.(cache.DescHandlerKey); ok {
					if handler, ok := v.(*cache.DescHandler); ok {
						descHandlers[digest.Digest(key)] = handler
					}
				}
			}
			opts = append(opts, descHandlers)
			ref, err = w.CacheMgr.Get(ctx, id, pg, opts...)
		}
	}
	if err != nil {
		return nil, errors.Wrap(err, "failed to load ref")
	}
	return ref, nil
}

func (w *Worker) Executor() executor.Executor {
	return w.WorkerOpt.Executor
}

func (w *Worker) CacheManager() cache.Manager {
	return w.CacheMgr
}

type proxyPolicyExecutor struct {
	executor.Executor
	getProxyPolicy func() (network.ProxyPolicy, error)
}

func (e *proxyPolicyExecutor) Run(ctx context.Context, id string, rootfs executor.Mount, mounts []executor.Mount, process executor.ProcessInfo, started chan<- struct{}) (resourcestypes.Recorder, error) {
	if process.Meta.Proxy != nil {
		policy, err := e.proxyPolicy()
		if err != nil {
			return nil, err
		}
		process.Meta.Proxy.Policy = policy
	}
	return e.Executor.Run(ctx, id, rootfs, mounts, process, started)
}

func (e *proxyPolicyExecutor) Exec(ctx context.Context, id string, process executor.ProcessInfo) error {
	if process.Meta.Proxy != nil {
		policy, err := e.proxyPolicy()
		if err != nil {
			return err
		}
		process.Meta.Proxy.Policy = policy
	}
	return e.Executor.Exec(ctx, id, process)
}

func (e *proxyPolicyExecutor) proxyPolicy() (network.ProxyPolicy, error) {
	policy, err := e.getProxyPolicy()
	if err != nil {
		return nil, err
	}
	return policy, nil
}

func (w *Worker) ResolveOp(v solver.Vertex, s frontend.FrontendLLBBridge, sm *session.Manager, proxyOpt worker.ProxyOpt) (solver.Op, error) {
	if baseOp, ok := v.Sys().(*pb.Op); ok {
		switch op := baseOp.Op.(type) {
		case *pb.Op_Source:
			return ops.NewSourceOp(v, op, baseOp.Platform, w.SourceManager, w.ParallelismSem, sm, w)
		case *pb.Op_Exec:
			var linuxResources *pb.LinuxResources
			if m, ok := v.Options().Metadata.(*linuxresources.Metadata); ok && m != nil {
				linuxResources = m.LinuxResources
			}
			exec := w.WorkerOpt.Executor
			proxyNetwork := proxyOpt.Network && op.Exec.Network != pb.NetMode_NONE
			if proxyNetwork {
				if proxyOpt.Policy != nil {
					exec = &proxyPolicyExecutor{Executor: exec, getProxyPolicy: proxyOpt.Policy}
				}
			}
			return ops.NewExecOp(v, op, baseOp.Platform, w.CacheMgr, w.ParallelismSem, sm, exec, w, linuxResources, proxyNetwork)
		case *pb.Op_File:
			return ops.NewFileOp(v, op, w.CacheMgr, w.ParallelismSem, w)
		case *pb.Op_Build:
			return ops.NewBuildOp(v, op, s, w)
		case *pb.Op_Merge:
			return ops.NewMergeOp(v, op, w)
		case *pb.Op_Diff:
			return ops.NewDiffOp(v, op, w)
		case *pb.Op_Passthrough:
			return ops.NewPassthroughOp(v, op)
		default:
			return nil, errors.Errorf("no support for %T", op)
		}
	}
	return nil, errors.Errorf("could not resolve %v", v)
}

func (w *Worker) PruneCacheMounts(ctx context.Context, ids map[string]bool) error {
	mu := mounts.CacheMountsLocker()
	mu.Lock()
	defer mu.Unlock()

	for id, nested := range ids {
		mds, err := mounts.SearchCacheDir(ctx, w.CacheMgr, id, nested)
		if err != nil {
			return err
		}
		for _, md := range mds {
			if err := md.SetCachePolicyDefault(); err != nil {
				return err
			}
			if err := md.ClearCacheDirIndex(); err != nil {
				return err
			}
			// if ref is unused try to clean it up right away by releasing it
			if mref, err := w.CacheMgr.GetMutable(ctx, md.ID()); err == nil {
				go mref.Release(context.WithoutCancel(ctx))
			}
		}
	}

	mounts.ClearActiveCacheMounts()
	return nil
}

func (w *Worker) ParseSource(op *pb.SourceOp, platform *pb.Platform) (source.Identifier, error) {
	return w.SourceManager.Identifier(&pb.Op_Source{Source: op}, platform)
}

func (w *Worker) ResolveSourceMetadata(ctx context.Context, op *pb.SourceOp, opt sourceresolver.Opt, sm *session.Manager, jobCtx solver.JobContext) (*sourceresolver.MetaResponse, error) {
	if opt.SourcePolicies != nil {
		return nil, errors.New("source policies can not be set for worker")
	}

	var p *ocispecs.Platform
	if imgOpt := opt.ImageOpt; imgOpt != nil && imgOpt.Platform != nil {
		p = imgOpt.Platform
	} else if ociOpt := opt.OCILayoutOpt; ociOpt != nil && ociOpt.Platform != nil {
		p = ociOpt.Platform
	}

	var platform *pb.Platform
	if p != nil {
		platform = &pb.Platform{
			Architecture: p.Architecture,
			OS:           p.OS,
			Variant:      p.Variant,
			OSVersion:    p.OSVersion,
		}
	}

	id, err := w.ParseSource(op, platform)
	if err != nil {
		return nil, err
	}

	var g session.Group
	if jobCtx != nil {
		g = jobCtx.Session()
	}

	switch idt := id.(type) {
	case *containerimage.ImageIdentifier:
		if opt.ImageOpt == nil {
			opt.ImageOpt = &sourceresolver.ResolveImageOpt{}
		}
		if p != nil {
			opt.ImageOpt.Platform = p
		}
		resp, err := w.ImageSource.ResolveImageMetadata(ctx, idt, opt.ImageOpt, sm, g)
		if err != nil {
			return nil, err
		}
		return &sourceresolver.MetaResponse{
			Op:    op,
			Image: resp,
		}, nil
	case *containerimage.OCIIdentifier:
		if opt.OCILayoutOpt == nil {
			opt.OCILayoutOpt = &sourceresolver.ResolveOCILayoutOpt{}
		}
		if p != nil {
			opt.OCILayoutOpt.Platform = p
		}
		resp, err := w.OCILayoutSource.ResolveOCILayoutMetadata(ctx, idt, opt.OCILayoutOpt, sm, g)
		if err != nil {
			return nil, err
		}
		return &sourceresolver.MetaResponse{
			Op:    op,
			Image: resp,
		}, nil
	case *git.GitIdentifier:
		if w.GitSource == nil {
			return nil, errors.New("git source is not supported")
		}
		mdOpt := git.MetadataOpts{}
		if opt.GitOpt != nil {
			mdOpt.ReturnObject = opt.GitOpt.ReturnObject
		}
		md, err := w.GitSource.ResolveMetadata(ctx, idt, sm, jobCtx, mdOpt)
		if err != nil {
			return nil, err
		}
		return &sourceresolver.MetaResponse{
			Op: op,
			Git: &sourceresolver.ResolveGitResponse{
				Checksum:       md.Checksum,
				Ref:            md.Ref,
				CommitChecksum: md.CommitChecksum,
				CommitObject:   md.CommitObject,
				TagObject:      md.TagObject,
			},
		}, nil
	case *http.HTTPIdentifier:
		if w.HTTPSource == nil {
			return nil, errors.New("http source is not supported")
		}
		mdOpt := http.MetadataOpts{}
		if opt.HTTPOpt != nil && opt.HTTPOpt.ChecksumReq != nil {
			mdOpt.ChecksumReq = &http.MetadataChecksumRequest{
				Algo:   http.MetadataChecksumAlgo(opt.HTTPOpt.ChecksumReq.Algo),
				Suffix: slices.Clone(opt.HTTPOpt.ChecksumReq.Suffix),
			}
		}
		md, err := w.HTTPSource.ResolveMetadata(ctx, idt, sm, jobCtx, mdOpt)
		if err != nil {
			return nil, err
		}
		var checksumResponse *sourceresolver.ResolveHTTPChecksumResponse
		if md.ChecksumResponse != nil {
			checksumResponse = &sourceresolver.ResolveHTTPChecksumResponse{
				Digest: md.ChecksumResponse.Digest,
				Suffix: slices.Clone(md.ChecksumResponse.Suffix),
			}
		}
		return &sourceresolver.MetaResponse{
			Op: op,
			HTTP: &sourceresolver.ResolveHTTPResponse{
				Digest:           md.Digest,
				Filename:         md.Filename,
				LastModified:     md.LastModified,
				ChecksumResponse: checksumResponse,
			},
		}, nil
	}

	return &sourceresolver.MetaResponse{
		Op: op,
	}, nil
}

func (w *Worker) DiskUsage(ctx context.Context, opt client.DiskUsageInfo) ([]*client.UsageInfo, error) {
	return w.CacheMgr.DiskUsage(ctx, opt)
}

func (w *Worker) Prune(ctx context.Context, ch chan client.UsageInfo, opt ...client.PruneInfo) error {
	return w.CacheMgr.Prune(ctx, ch, opt...)
}

func (w *Worker) Exporter(name string, sm *session.Manager) (exporter.Exporter, error) {
	switch name {
	case client.ExporterImage:
		return imageexporter.New(imageexporter.Opt{
			Images:         w.ImageStore,
			SessionManager: sm,
			ImageWriter:    w.imageWriter,
			RegistryHosts:  w.RegistryHosts,
			LeaseManager:   w.LeaseManager(),
		})
	case client.ExporterLocal:
		return localexporter.New(localexporter.Opt{
			SessionManager: sm,
		})
	case client.ExporterTar:
		return tarexporter.New(tarexporter.Opt{
			SessionManager: sm,
		})
	case client.ExporterOCI:
		return ociexporter.New(ociexporter.Opt{
			SessionManager: sm,
			ImageWriter:    w.imageWriter,
			Variant:        ociexporter.VariantOCI,
			LeaseManager:   w.LeaseManager(),
		})
	case client.ExporterDocker:
		return ociexporter.New(ociexporter.Opt{
			SessionManager: sm,
			ImageWriter:    w.imageWriter,
			Variant:        ociexporter.VariantDocker,
			LeaseManager:   w.LeaseManager(),
		})
	default:
		return nil, errors.Errorf("exporter %q could not be found", name)
	}
}

func (w *Worker) FromRemote(ctx context.Context, remote *solver.Remote) (ref cache.ImmutableRef, err error) {
	if len(remote.Descriptors) > 0 {
		var eg errgroup.Group
		for _, desc := range remote.Descriptors {
			eg.Go(func() error {
				if _, err := remote.Provider.Info(ctx, desc.Digest); err != nil {
					return err
				}
				return nil
			})
		}
		if err := eg.Wait(); err != nil {
			return nil, err
		}
	}

	pg := solver.ProgressControllerFromContext(ctx)
	if pg == nil {
		pg = &controller.Controller{
			WriterFactory: progress.FromContext(ctx),
		}
	}

	descHandler := &cache.DescHandler{
		Provider: func(g session.Group) content.Provider {
			return contentutil.ProviderForSession(remote.Provider, g)
		},
		Progress: pg,
	}
	snapshotLabels := func([]ocispecs.Descriptor, int) map[string]string { return nil }
	if cd, ok := remote.Provider.(interface {
		SnapshotLabels([]ocispecs.Descriptor, int) map[string]string
	}); ok {
		snapshotLabels = cd.SnapshotLabels
	}
	descHandlers := cache.DescHandlers(make(map[digest.Digest]*cache.DescHandler))
	for i, desc := range remote.Descriptors {
		descHandlers[desc.Digest] = &cache.DescHandler{
			Provider:       descHandler.Provider,
			Progress:       descHandler.Progress,
			Annotations:    desc.Annotations,
			SnapshotLabels: snapshotLabels(remote.Descriptors, i),
		}
	}

	var current cache.ImmutableRef
	for i, desc := range remote.Descriptors {
		tm := time.Now()
		if tmstr, ok := desc.Annotations[labelCreatedAt]; ok {
			if err := (&tm).UnmarshalText([]byte(tmstr)); err != nil {
				if current != nil {
					current.Release(context.TODO())
				}
				return nil, err
			}
		}
		descr := fmt.Sprintf("imported %s", remote.Descriptors[i].Digest)
		if v, ok := desc.Annotations["buildkit/description"]; ok {
			descr = v
		}
		opts := []cache.RefOption{
			cache.WithDescription(descr),
			cache.WithCreationTime(tm),
			descHandlers,
		}
		if ul, ok := remote.Provider.(interface {
			UnlazySession(ocispecs.Descriptor) session.Group
		}); ok {
			s := ul.UnlazySession(desc)
			if s != nil {
				opts = append(opts, cache.Unlazy(s))
			}
		}
		if dh, ok := descHandlers[desc.Digest]; ok {
			if ref, ok := dh.Annotations["containerd.io/distribution.source.ref"]; ok {
				opts = append(opts, cache.WithImageRef(ref)) // can set by registry cache importer
			}
		}
		ref, err := w.CacheMgr.GetByBlob(ctx, desc, current, opts...)
		if current != nil {
			current.Release(context.TODO())
		}
		if err != nil {
			return nil, err
		}
		current = ref
	}
	return current, nil
}

// ID reads the worker id from the `workerid` file.
// If not exist, it creates a random one,
func ID(root string) (string, error) {
	f := filepath.Join(root, "workerid")
	b, err := os.ReadFile(f)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			id := identity.NewID()
			err := os.WriteFile(f, []byte(id), 0o600)
			return id, err
		}
		return "", err
	}
	return string(b), nil
}
