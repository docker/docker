package worker

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	nethttp "net/http"
	"runtime"
	"time"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/rootfs"
	"github.com/docker/docker/distribution"
	distmetadata "github.com/docker/docker/distribution/metadata"
	"github.com/docker/docker/distribution/xfer"
	"github.com/docker/docker/image"
	"github.com/docker/docker/layer"
	pkgprogress "github.com/docker/docker/pkg/progress"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/cache/metadata"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/executor"
	"github.com/moby/buildkit/exporter"
	localexporter "github.com/moby/buildkit/exporter/local"
	tarexporter "github.com/moby/buildkit/exporter/tar"
	"github.com/moby/buildkit/frontend"
	gw "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/llbsolver/ops"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/source"
	"github.com/moby/buildkit/source/git"
	"github.com/moby/buildkit/source/http"
	"github.com/moby/buildkit/source/local"
	"github.com/moby/buildkit/util/contentutil"
	"github.com/moby/buildkit/util/progress"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const labelCreatedAt = "buildkit/createdat"

// LayerAccess provides access to a moby layer from a snapshot
type LayerAccess interface {
	GetDiffIDs(ctx context.Context, key string) ([]layer.DiffID, error)
	EnsureLayer(ctx context.Context, key string) ([]layer.DiffID, error)
}

// Opt defines a structure for creating a worker.
type Opt struct {
	ID                string
	Labels            map[string]string
	GCPolicy          []client.PruneInfo
	MetadataStore     *metadata.Store
	Executor          executor.Executor
	Snapshotter       snapshot.Snapshotter
	ContentStore      content.Store
	CacheManager      cache.Manager
	ImageSource       source.Source
	DownloadManager   distribution.RootFSDownloadManager
	V2MetadataService distmetadata.V2MetadataService
	Transport         nethttp.RoundTripper
	Exporter          exporter.Exporter
	Layers            LayerAccess
	Platforms         []ocispec.Platform
}

// Worker is a local worker instance with dedicated snapshotter, cache, and so on.
// TODO: s/Worker/OpWorker/g ?
type Worker struct {
	Opt
	SourceManager *source.Manager
}

// NewWorker instantiates a local worker
func NewWorker(opt Opt) (*Worker, error) {
	sm, err := source.NewManager()
	if err != nil {
		return nil, err
	}

	cm := opt.CacheManager
	sm.Register(opt.ImageSource)

	gs, err := git.NewSource(git.Opt{
		CacheAccessor: cm,
		MetadataStore: opt.MetadataStore,
	})
	if err == nil {
		sm.Register(gs)
	} else {
		logrus.Warnf("Could not register builder git source: %s", err)
	}

	hs, err := http.NewSource(http.Opt{
		CacheAccessor: cm,
		MetadataStore: opt.MetadataStore,
		Transport:     opt.Transport,
	})
	if err == nil {
		sm.Register(hs)
	} else {
		logrus.Warnf("Could not register builder http source: %s", err)
	}

	ss, err := local.NewSource(local.Opt{
		CacheAccessor: cm,
		MetadataStore: opt.MetadataStore,
	})
	if err == nil {
		sm.Register(ss)
	} else {
		logrus.Warnf("Could not register builder local source: %s", err)
	}

	return &Worker{
		Opt:           opt,
		SourceManager: sm,
	}, nil
}

// ID returns worker ID
func (w *Worker) ID() string {
	return w.Opt.ID
}

// Labels returns map of all worker labels
func (w *Worker) Labels() map[string]string {
	return w.Opt.Labels
}

// Platforms returns one or more platforms supported by the image.
func (w *Worker) Platforms() []ocispec.Platform {
	if len(w.Opt.Platforms) == 0 {
		return []ocispec.Platform{platforms.DefaultSpec()}
	}
	return w.Opt.Platforms
}

// GCPolicy returns automatic GC Policy
func (w *Worker) GCPolicy() []client.PruneInfo {
	return w.Opt.GCPolicy
}

// LoadRef loads a reference by ID
func (w *Worker) LoadRef(id string, hidden bool) (cache.ImmutableRef, error) {
	var opts []cache.RefOption
	if hidden {
		opts = append(opts, cache.NoUpdateLastUsed)
	}
	return w.CacheManager.Get(context.TODO(), id, opts...)
}

// ResolveOp converts a LLB vertex into a LLB operation
func (w *Worker) ResolveOp(v solver.Vertex, s frontend.FrontendLLBBridge, sm *session.Manager) (solver.Op, error) {
	if baseOp, ok := v.Sys().(*pb.Op); ok {
		switch op := baseOp.Op.(type) {
		case *pb.Op_Source:
			return ops.NewSourceOp(v, op, baseOp.Platform, w.SourceManager, sm, w)
		case *pb.Op_Exec:
			return ops.NewExecOp(v, op, baseOp.Platform, w.CacheManager, sm, w.MetadataStore, w.Executor, w)
		case *pb.Op_File:
			return ops.NewFileOp(v, op, w.CacheManager, w.MetadataStore, w)
		case *pb.Op_Build:
			return ops.NewBuildOp(v, op, s, w)
		}
	}
	return nil, errors.Errorf("could not resolve %v", v)
}

// ResolveImageConfig returns image config for an image
func (w *Worker) ResolveImageConfig(ctx context.Context, ref string, opt gw.ResolveImageConfigOpt, sm *session.Manager) (digest.Digest, []byte, error) {
	// ImageSource is typically source/containerimage
	resolveImageConfig, ok := w.ImageSource.(resolveImageConfig)
	if !ok {
		return "", nil, errors.Errorf("worker %q does not implement ResolveImageConfig", w.ID())
	}
	return resolveImageConfig.ResolveImageConfig(ctx, ref, opt, sm)
}

// Exec executes a process directly on a worker
func (w *Worker) Exec(ctx context.Context, meta executor.Meta, rootFS cache.ImmutableRef, stdin io.ReadCloser, stdout, stderr io.WriteCloser) error {
	active, err := w.CacheManager.New(ctx, rootFS)
	if err != nil {
		return err
	}
	defer active.Release(context.TODO())
	return w.Executor.Exec(ctx, meta, active, nil, stdin, stdout, stderr)
}

// DiskUsage returns disk usage report
func (w *Worker) DiskUsage(ctx context.Context, opt client.DiskUsageInfo) ([]*client.UsageInfo, error) {
	return w.CacheManager.DiskUsage(ctx, opt)
}

// Prune deletes reclaimable build cache
func (w *Worker) Prune(ctx context.Context, ch chan client.UsageInfo, info ...client.PruneInfo) error {
	return w.CacheManager.Prune(ctx, ch, info...)
}

// Exporter returns exporter by name
func (w *Worker) Exporter(name string, sm *session.Manager) (exporter.Exporter, error) {
	switch name {
	case "moby":
		return w.Opt.Exporter, nil
	case client.ExporterLocal:
		return localexporter.New(localexporter.Opt{
			SessionManager: sm,
		})
	case client.ExporterTar:
		return tarexporter.New(tarexporter.Opt{
			SessionManager: sm,
		})
	default:
		return nil, errors.Errorf("exporter %q could not be found", name)
	}
}

// GetRemote returns a remote snapshot reference for a local one
func (w *Worker) GetRemote(ctx context.Context, ref cache.ImmutableRef, createIfNeeded bool) (*solver.Remote, error) {
	var diffIDs []layer.DiffID
	var err error
	if !createIfNeeded {
		diffIDs, err = w.Layers.GetDiffIDs(ctx, ref.ID())
		if err != nil {
			return nil, err
		}
	} else {
		if err := ref.Finalize(ctx, true); err != nil {
			return nil, err
		}
		diffIDs, err = w.Layers.EnsureLayer(ctx, ref.ID())
		if err != nil {
			return nil, err
		}
	}

	descriptors := make([]ocispec.Descriptor, len(diffIDs))
	for i, dgst := range diffIDs {
		descriptors[i] = ocispec.Descriptor{
			MediaType: images.MediaTypeDockerSchema2Layer,
			Digest:    digest.Digest(dgst),
			Size:      -1,
		}
	}

	return &solver.Remote{
		Descriptors: descriptors,
		Provider:    &emptyProvider{},
	}, nil
}

// FromRemote converts a remote snapshot reference to a local one
func (w *Worker) FromRemote(ctx context.Context, remote *solver.Remote) (cache.ImmutableRef, error) {
	rootfs, err := getLayers(ctx, remote.Descriptors)
	if err != nil {
		return nil, err
	}

	layers := make([]xfer.DownloadDescriptor, 0, len(rootfs))

	for _, l := range rootfs {
		// ongoing.add(desc)
		layers = append(layers, &layerDescriptor{
			desc:     l.Blob,
			diffID:   layer.DiffID(l.Diff.Digest),
			provider: remote.Provider,
			w:        w,
			pctx:     ctx,
		})
	}

	defer func() {
		for _, l := range rootfs {
			w.ContentStore.Delete(context.TODO(), l.Blob.Digest)
		}
	}()

	r := image.NewRootFS()
	rootFS, release, err := w.DownloadManager.Download(ctx, *r, runtime.GOOS, layers, &discardProgress{})
	if err != nil {
		return nil, err
	}
	defer release()

	if len(rootFS.DiffIDs) != len(layers) {
		return nil, errors.Errorf("invalid layer count mismatch %d vs %d", len(rootFS.DiffIDs), len(layers))
	}

	for i := range rootFS.DiffIDs {
		tm := time.Now()
		if tmstr, ok := remote.Descriptors[i].Annotations[labelCreatedAt]; ok {
			if err := (&tm).UnmarshalText([]byte(tmstr)); err != nil {
				return nil, err
			}
		}
		descr := fmt.Sprintf("imported %s", remote.Descriptors[i].Digest)
		if v, ok := remote.Descriptors[i].Annotations["buildkit/description"]; ok {
			descr = v
		}
		ref, err := w.CacheManager.GetFromSnapshotter(ctx, string(layer.CreateChainID(rootFS.DiffIDs[:i+1])), cache.WithDescription(descr), cache.WithCreationTime(tm))
		if err != nil {
			return nil, err
		}
		if i == len(remote.Descriptors)-1 {
			return ref, nil
		}
		defer ref.Release(context.TODO())
	}

	return nil, errors.Errorf("unreachable")
}

type discardProgress struct{}

func (*discardProgress) WriteProgress(_ pkgprogress.Progress) error {
	return nil
}

// Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error)
type layerDescriptor struct {
	provider content.Provider
	desc     ocispec.Descriptor
	diffID   layer.DiffID
	// ref      ctdreference.Spec
	w    *Worker
	pctx context.Context
}

func (ld *layerDescriptor) Key() string {
	return "v2:" + ld.desc.Digest.String()
}

func (ld *layerDescriptor) ID() string {
	return ld.desc.Digest.String()
}

func (ld *layerDescriptor) DiffID() (layer.DiffID, error) {
	return ld.diffID, nil
}

func (ld *layerDescriptor) Download(ctx context.Context, progressOutput pkgprogress.Output) (io.ReadCloser, int64, error) {
	done := oneOffProgress(ld.pctx, fmt.Sprintf("pulling %s", ld.desc.Digest))
	if err := contentutil.Copy(ctx, ld.w.ContentStore, ld.provider, ld.desc); err != nil {
		return nil, 0, done(err)
	}
	done(nil)

	ra, err := ld.w.ContentStore.ReaderAt(ctx, ld.desc)
	if err != nil {
		return nil, 0, err
	}

	return ioutil.NopCloser(content.NewReader(ra)), ld.desc.Size, nil
}

func (ld *layerDescriptor) Close() {
	// ld.is.ContentStore.Delete(context.TODO(), ld.desc.Digest)
}

func (ld *layerDescriptor) Registered(diffID layer.DiffID) {
	// Cache mapping from this layer's DiffID to the blobsum
	ld.w.V2MetadataService.Add(diffID, distmetadata.V2Metadata{Digest: ld.desc.Digest})
}

func getLayers(ctx context.Context, descs []ocispec.Descriptor) ([]rootfs.Layer, error) {
	layers := make([]rootfs.Layer, len(descs))
	for i, desc := range descs {
		diffIDStr := desc.Annotations["containerd.io/uncompressed"]
		if diffIDStr == "" {
			return nil, errors.Errorf("%s missing uncompressed digest", desc.Digest)
		}
		diffID, err := digest.Parse(diffIDStr)
		if err != nil {
			return nil, err
		}
		layers[i].Diff = ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageLayer,
			Digest:    diffID,
		}
		layers[i].Blob = ocispec.Descriptor{
			MediaType: desc.MediaType,
			Digest:    desc.Digest,
			Size:      desc.Size,
		}
	}
	return layers, nil
}

func oneOffProgress(ctx context.Context, id string) func(err error) error {
	pw, _, _ := progress.FromContext(ctx)
	now := time.Now()
	st := progress.Status{
		Started: &now,
	}
	pw.Write(id, st)
	return func(err error) error {
		// TODO: set error on status
		now := time.Now()
		st.Completed = &now
		pw.Write(id, st)
		pw.Close()
		return err
	}
}

type resolveImageConfig interface {
	ResolveImageConfig(ctx context.Context, ref string, opt gw.ResolveImageConfigOpt, sm *session.Manager) (digest.Digest, []byte, error)
}

type emptyProvider struct {
}

func (p *emptyProvider) ReaderAt(ctx context.Context, dec ocispec.Descriptor) (content.ReaderAt, error) {
	return nil, errors.Errorf("ReaderAt not implemented for empty provider")
}
