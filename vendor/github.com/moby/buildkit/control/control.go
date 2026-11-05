package control

import (
	"context"
	stderrors "errors"
	"fmt"
	"runtime/trace"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	contentapi "github.com/containerd/containerd/api/services/content/v1"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/plugins/services/content/contentserver"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/gohugoio/hashstructure"
	controlapi "github.com/moby/buildkit/api/services/control"
	apitypes "github.com/moby/buildkit/api/types"
	"github.com/moby/buildkit/cache/remotecache"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/cmd/buildkitd/config"
	controlgateway "github.com/moby/buildkit/control/gateway"
	"github.com/moby/buildkit/exporter"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/exporter/util/epoch"
	"github.com/moby/buildkit/frontend"
	"github.com/moby/buildkit/frontend/attestations"
	dockerfileversion "github.com/moby/buildkit/frontend/dockerfile/version"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/grpchijack"
	containerdsnapshot "github.com/moby/buildkit/snapshot/containerd"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/bboltcachestorage"
	"github.com/moby/buildkit/solver/llbsolver"
	"github.com/moby/buildkit/solver/llbsolver/cdidevices"
	"github.com/moby/buildkit/solver/llbsolver/compat"
	"github.com/moby/buildkit/solver/llbsolver/history"
	"github.com/moby/buildkit/solver/llbsolver/proc"
	provenancetypes "github.com/moby/buildkit/solver/llbsolver/provenance/types"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/db"
	"github.com/moby/buildkit/util/entitlements"
	"github.com/moby/buildkit/util/imageutil"
	"github.com/moby/buildkit/util/leaseutil"
	"github.com/moby/buildkit/util/throttle"
	"github.com/moby/buildkit/util/tracing/forwarder"
	"github.com/moby/buildkit/util/tracing/transform"
	"github.com/moby/buildkit/version"
	"github.com/moby/buildkit/worker"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracev1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const traceShutdownTimeout = 5 * time.Second

type Opt struct {
	SessionManager            *session.Manager
	WorkerController          *worker.Controller
	Frontends                 map[string]frontend.Frontend
	CacheManager              solver.CacheManager
	ResolveCacheExporterFuncs map[string]remotecache.ResolveCacheExporterFunc
	ResolveCacheImporterFuncs map[string]remotecache.ResolveCacheImporterFunc
	Entitlements              []string
	TraceCollector            sdktrace.SpanExporter
	MeterProvider             metric.MeterProvider
	HistoryDB                 db.DB
	CacheStore                *bboltcachestorage.Store
	LeaseManager              *leaseutil.Manager
	ContentStore              *containerdsnapshot.Store
	HistoryConfig             *config.HistoryConfig
	ProxyNetwork              bool
	GarbageCollect            func(context.Context) error
	GracefulStop              <-chan struct{}
	ProvenanceEnv             map[string]any
}

type Controller struct { // TODO: ControlService
	buildCount                   atomic.Int64
	opt                          Opt
	solver                       *llbsolver.Solver
	history                      *history.Queue
	cache                        solver.CacheManager
	gatewayForwarder             *controlgateway.GatewayForwarder
	traceForwarder               *forwarder.Exporter
	throttledGC                  func()
	throttledReleaseUnreferenced func()
	gcmu                         sync.Mutex
	tracev1.UnimplementedTraceServiceServer
}

func NewController(opt Opt) (*Controller, error) {
	gatewayForwarder := controlgateway.NewGatewayForwarder()

	hq, err := history.NewQueue(history.QueueOpt{
		DB:             opt.HistoryDB,
		LeaseManager:   opt.LeaseManager,
		ContentStore:   opt.ContentStore,
		CleanConfig:    opt.HistoryConfig,
		GarbageCollect: opt.GarbageCollect,
		GracefulStop:   opt.GracefulStop,
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to create history queue")
	}

	s, err := llbsolver.New(llbsolver.Opt{
		WorkerController: opt.WorkerController,
		Frontends:        opt.Frontends,
		CacheManager:     opt.CacheManager,
		CacheResolvers:   opt.ResolveCacheImporterFuncs,
		GatewayForwarder: gatewayForwarder,
		SessionManager:   opt.SessionManager,
		Entitlements:     opt.Entitlements,
		HistoryQueue:     hq,
		ProxyNetwork:     opt.ProxyNetwork,
		ProvenanceEnv:    opt.ProvenanceEnv,
		MeterProvider:    opt.MeterProvider,
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to create solver")
	}

	c := &Controller{
		opt:              opt,
		solver:           s,
		history:          hq,
		cache:            opt.CacheManager,
		gatewayForwarder: gatewayForwarder,
	}
	c.throttledGC = throttle.After(time.Minute, c.gc)
	// use longer interval for releaseUnreferencedCache deleting links quickly is less important
	c.throttledReleaseUnreferenced = throttle.After(5*time.Minute, func() { c.releaseUnreferencedCache(context.TODO()) })

	if opt.TraceCollector != nil {
		fwd, err := forwarder.New(context.Background(), opt.TraceCollector)
		if err != nil {
			return nil, errors.Wrap(err, "failed to start trace forwarder")
		}
		c.traceForwarder = fwd
	}

	defer func() {
		time.AfterFunc(time.Second, c.throttledGC)
	}()

	return c, nil
}

func (c *Controller) Close() error {
	var errs []error
	if err := c.opt.HistoryDB.Close(); err != nil {
		errs = append(errs, err)
	}
	if c.traceForwarder != nil {
		func() {
			ctx, cancel := context.WithTimeoutCause(context.Background(), traceShutdownTimeout, errors.WithStack(context.DeadlineExceeded))
			defer cancel()
			if err := c.traceForwarder.Shutdown(ctx); err != nil {
				errs = append(errs, err)
			}
		}()
	}
	if err := c.opt.WorkerController.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := c.opt.CacheStore.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := c.solver.Close(); err != nil {
		errs = append(errs, err)
	}
	return stderrors.Join(errs...)
}

func (c *Controller) Register(server *grpc.Server) {
	controlapi.RegisterControlServer(server, c)
	c.gatewayForwarder.Register(server)
	tracev1.RegisterTraceServiceServer(server, c)

	store := &roContentStore{c.opt.ContentStore.WithFallbackNS(c.opt.ContentStore.Namespace() + "_history")}
	contentapi.RegisterContentServer(server, contentserver.New(store))
}

func (c *Controller) DiskUsage(ctx context.Context, r *controlapi.DiskUsageRequest) (*controlapi.DiskUsageResponse, error) {
	resp := &controlapi.DiskUsageResponse{}
	workers, err := c.opt.WorkerController.List()
	if err != nil {
		return nil, err
	}
	for _, w := range workers {
		du, err := w.DiskUsage(ctx, client.DiskUsageInfo{
			Filter:   r.Filter,
			AgeLimit: time.Duration(r.AgeLimit),
		})
		if err != nil {
			return nil, err
		}

		for _, r := range du {
			resp.Record = append(resp.Record, &controlapi.UsageRecord{
				// TODO: add worker info
				ID:          r.ID,
				Mutable:     r.Mutable,
				InUse:       r.InUse,
				Size:        r.Size,
				Parents:     r.Parents,
				UsageCount:  int64(r.UsageCount),
				Description: r.Description,
				CreatedAt:   timestamppb.New(r.CreatedAt),
				LastUsedAt: func() *timestamppb.Timestamp {
					if r.LastUsedAt != nil {
						return timestamppb.New(*r.LastUsedAt)
					}
					return nil
				}(),
				RecordType: string(r.RecordType),
				Shared:     r.Shared,
			})
		}
	}
	return resp, nil
}

func (c *Controller) releaseUnreferencedCache(ctx context.Context) error {
	return c.cache.ReleaseUnreferenced(ctx)
}

func (c *Controller) Prune(req *controlapi.PruneRequest, stream controlapi.Control_PruneServer) error {
	if c.buildCount.Load() == 0 {
		imageutil.CancelCacheLeases()
	}

	ch := make(chan client.UsageInfo, 32)

	ctx := stream.Context()
	eg, ectx := errgroup.WithContext(ctx)
	workers, err := c.opt.WorkerController.List()
	if err != nil {
		return errors.Wrap(err, "failed to list workers for prune")
	}

	didPrune := false
	defer func() {
		if didPrune {
			if c, ok := c.cache.(interface {
				ReleaseUnreferenced(context.Context) error
			}); ok {
				if err := c.ReleaseUnreferenced(ctx); err != nil {
					bklog.G(ctx).Errorf("failed to release cache metadata: %+v", err)
				}
			}
		}
	}()

	for _, w := range workers {
		func(w worker.Worker) {
			eg.Go(func() error {
				return w.Prune(ectx, ch, client.PruneInfo{
					Filter:        req.Filter,
					All:           req.All,
					KeepDuration:  time.Duration(req.KeepDuration),
					ReservedSpace: req.ReservedSpace,
					MaxUsedSpace:  req.MaxUsedSpace,
					MinFreeSpace:  req.MinFreeSpace,
				})
			})
		}(w)
	}

	eg2, _ := errgroup.WithContext(stream.Context())

	eg2.Go(func() error {
		defer close(ch)
		return eg.Wait()
	})

	eg2.Go(func() error {
		defer func() {
			// drain channel on error
			for range ch {
			}
		}()
		for r := range ch {
			didPrune = true
			if err := stream.Send(&controlapi.UsageRecord{
				// TODO: add worker info
				ID:          r.ID,
				Mutable:     r.Mutable,
				InUse:       r.InUse,
				Size:        r.Size,
				Parents:     r.Parents,
				UsageCount:  int64(r.UsageCount),
				Description: r.Description,
				CreatedAt:   timestamppb.New(r.CreatedAt),
				LastUsedAt: func() *timestamppb.Timestamp {
					if r.LastUsedAt != nil {
						return timestamppb.New(*r.LastUsedAt)
					}
					return nil
				}(),
				RecordType: string(r.RecordType),
				Shared:     r.Shared,
			}); err != nil {
				return err
			}
		}
		return nil
	})

	return eg2.Wait()
}

func (c *Controller) Export(ctx context.Context, req *tracev1.ExportTraceServiceRequest) (*tracev1.ExportTraceServiceResponse, error) {
	if c.traceForwarder == nil {
		return nil, status.Errorf(codes.Unavailable, "trace collector not configured")
	}

	if err := c.traceForwarder.ExportSpans(ctx, transform.Spans(req.GetResourceSpans())); err != nil {
		return nil, err
	}
	return &tracev1.ExportTraceServiceResponse{}, nil
}

func (c *Controller) ListenBuildHistory(req *controlapi.BuildHistoryRequest, srv controlapi.Control_ListenBuildHistoryServer) error {
	if err := sendTimestampHeader(srv); err != nil {
		return err
	}
	return c.history.Listen(srv.Context(), req, func(h *controlapi.BuildHistoryEvent) error {
		if err := srv.Send(h); err != nil {
			return err
		}
		return nil
	})
}

func (c *Controller) UpdateBuildHistory(ctx context.Context, req *controlapi.UpdateBuildHistoryRequest) (*controlapi.UpdateBuildHistoryResponse, error) {
	if req.Delete {
		c.history.Finalize(ctx, req.Ref) // ignore error
		err := c.history.Delete(ctx, req.Ref)
		return &controlapi.UpdateBuildHistoryResponse{}, err
	}

	if req.Finalize {
		err := c.history.Finalize(ctx, req.Ref)
		return &controlapi.UpdateBuildHistoryResponse{}, err
	}

	err := c.history.UpdateRef(ctx, req.Ref, func(r *controlapi.BuildHistoryRecord) error {
		if req.Pinned == r.Pinned {
			return nil
		}
		r.Pinned = req.Pinned
		return nil
	})
	return &controlapi.UpdateBuildHistoryResponse{}, err
}

func translateLegacySolveRequest(req *controlapi.SolveRequest) {
	// translates ExportRef and ExportAttrs to new Exports (v0.4.0)
	if legacyExportRef := req.Cache.ExportRefDeprecated; legacyExportRef != "" {
		ex := &controlapi.CacheOptionsEntry{
			Type:  "registry",
			Attrs: req.Cache.ExportAttrsDeprecated,
		}
		if ex.Attrs == nil {
			ex.Attrs = make(map[string]string)
		}
		ex.Attrs["ref"] = legacyExportRef
		// FIXME(AkihiroSuda): skip append if already exists
		req.Cache.Exports = append(req.Cache.Exports, ex)
		req.Cache.ExportRefDeprecated = ""
		req.Cache.ExportAttrsDeprecated = nil
	}

	// translates ImportRefs to new Imports (v0.4.0)
	for _, legacyImportRef := range req.Cache.ImportRefsDeprecated {
		im := &controlapi.CacheOptionsEntry{
			Type:  "registry",
			Attrs: map[string]string{"ref": legacyImportRef},
		}
		// FIXME(AkihiroSuda): skip append if already exists
		req.Cache.Imports = append(req.Cache.Imports, im)
	}
	req.Cache.ImportRefsDeprecated = nil

	// translate single exporter to a slice (v0.13.0)
	if len(req.Exporters) == 0 && req.ExporterDeprecated != "" {
		req.Exporters = append(req.Exporters, &controlapi.Exporter{
			Type:  req.ExporterDeprecated,
			Attrs: req.ExporterAttrsDeprecated,
		})
		req.ExporterDeprecated = ""
		req.ExporterAttrsDeprecated = nil
	}
}

func (c *Controller) Solve(ctx context.Context, req *controlapi.SolveRequest) (*controlapi.SolveResponse, error) {
	defer trace.StartRegion(ctx, "Solve").End()
	trace.Logf(ctx, "Request", "solve request: %v", req.Ref)
	c.buildCount.Add(1)
	defer c.buildCount.Add(-1)

	if req.Cache == nil {
		req.Cache = &controlapi.CacheOptions{} // make sure cache options are initialized
	}
	translateLegacySolveRequest(req)

	compatibilityVersion := int(req.CompatibilityVersion)
	if compatibilityVersion == 0 {
		compatibilityVersion = compat.CompatibilityVersionCurrent
	}
	if err := compat.ValidateCompatibilityVersion(compatibilityVersion); err != nil {
		return nil, err
	}

	defer func() {
		time.AfterFunc(time.Second, c.throttledGC)
	}()

	// TODO: multiworker
	// This is actually tricky, as the exporter should come from the worker that has the returned reference. We may need to delay this so that the solver loads this.
	w, err := c.opt.WorkerController.GetDefault()
	if err != nil {
		return nil, err
	}

	// if SOURCE_DATE_EPOCH is set, enable it for the exporters
	if v, ok := epoch.ParseBuildArgs(req.FrontendAttrs); ok {
		for _, ex := range req.Exporters {
			if _, ok := ex.Attrs[string(exptypes.OptKeySourceDateEpoch)]; !ok {
				if ex.Attrs == nil {
					ex.Attrs = make(map[string]string)
				}
				ex.Attrs[string(exptypes.OptKeySourceDateEpoch)] = v
			}
		}
	}

	var expis []exporter.ExporterInstance
	for i, ex := range req.Exporters {
		exp, err := w.Exporter(ex.Type, c.opt.SessionManager)
		if err != nil {
			return nil, err
		}
		bklog.G(ctx).Debugf("resolve exporter %s with %v", ex.Type, ex.Attrs)
		expi, err := exp.Resolve(ctx, i, ex.Attrs)
		if err != nil {
			return nil, err
		}
		expis = append(expis, expi)
	}

	rest, dupes, err := findDuplicateCacheOptions(req.Cache.Exports)
	if err != nil {
		return nil, err
	} else if len(dupes) > 0 {
		types := []string{}
		for _, c := range dupes {
			types = append(types, c.Type)
		}
		return nil, errors.Errorf("duplicate cache exports %s", types)
	}
	req.Cache.Exports = rest
	var cacheExporters []llbsolver.RemoteCacheExporter
	for _, e := range req.Cache.Exports {
		cacheExporterFunc, ok := c.opt.ResolveCacheExporterFuncs[e.Type]
		if !ok {
			return nil, errors.Errorf("unknown cache exporter: %q", e.Type)
		}
		var exp llbsolver.RemoteCacheExporter
		exp.Exporter, err = cacheExporterFunc(ctx, session.NewGroup(req.Session), e.Attrs)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to configure %v cache exporter", e.Type)
		}
		if exp.Exporter == nil {
			bklog.G(ctx).Debugf("cache exporter resolver for %v returned nil, skipping exporter", e.Type)
			continue
		}
		if exportMode, supported := parseCacheExportMode(e.Attrs["mode"]); !supported {
			bklog.G(ctx).Debugf("skipping invalid cache export mode: %s", e.Attrs["mode"])
		} else {
			exp.CacheExportMode = exportMode
		}
		if ignoreErrorStr, ok := e.Attrs["ignore-error"]; ok {
			if ignoreError, supported := parseCacheExportIgnoreError(ignoreErrorStr); !supported {
				bklog.G(ctx).Debugf("skipping invalid cache export ignore-error: %s", e.Attrs["ignore-error"])
			} else {
				exp.IgnoreError = ignoreError
			}
		}
		cacheExporters = append(cacheExporters, exp)
	}

	var cacheImports []frontend.CacheOptionsEntry
	for _, im := range req.Cache.Imports {
		if im == nil {
			continue
		}
		cacheImports = append(cacheImports, frontend.CacheOptionsEntry{
			Type:  im.Type,
			Attrs: im.Attrs,
		})
	}

	attests, err := attestations.Parse(req.FrontendAttrs)
	if err != nil {
		return nil, err
	}

	var procs []llbsolver.Processor

	if attrs, ok := attests["sbom"]; ok {
		var ref reference.Named
		params := make(map[string]string)
		for k, v := range attrs {
			if k == "generator" {
				if v == "" {
					return nil, errors.Errorf("sbom generator cannot be empty")
				}
				ref, err = reference.ParseNormalizedNamed(v)
				if err != nil {
					return nil, errors.Wrapf(err, "failed to parse sbom generator %s", v)
				}
				ref = reference.TagNameOnly(ref)
			} else {
				params[k] = v
			}
		}

		useCache := true
		if v, ok := req.FrontendAttrs["no-cache"]; ok && v == "" {
			// disable cache if cache is disabled for all stages
			useCache = false
		}
		resolveMode := llb.ResolveModeDefault.String()
		if v, ok := req.FrontendAttrs["image-resolve-mode"]; ok {
			resolveMode = v
		}

		scannerPlatform := platforms.Normalize(platforms.DefaultSpec())
		if ps := w.Platforms(false); len(ps) > 0 {
			scannerPlatform = ps[0]
		}
		procs = append(procs, proc.SBOMProcessor(ref.String(), scannerPlatform, useCache, resolveMode, params))
	}

	if attrs, ok := attests["provenance"]; ok {
		var slsaVersion provenancetypes.ProvenanceSLSA
		params := make(map[string]string)
		for k, v := range attrs {
			if k == "version" {
				slsaVersion = provenancetypes.ProvenanceSLSA(v)
				if err := slsaVersion.Validate(); err != nil {
					return nil, err
				}
			} else {
				params[k] = v
			}
		}
		procs = append(procs, proc.ProvenanceProcessor(slsaVersion, params, c.opt.ProvenanceEnv))
	}

	resp, err := c.solver.Solve(ctx, req.Ref, req.Session, frontend.SolveRequest{
		Frontend:       req.Frontend,
		Definition:     req.Definition,
		FrontendOpt:    req.FrontendAttrs,
		FrontendInputs: req.FrontendInputs,
		CacheImports:   cacheImports,
	}, compatibilityVersion, llbsolver.ExporterRequest{
		Exporters:             expis,
		CacheExporters:        cacheExporters,
		EnableSessionExporter: req.EnableSessionExporter,
	}, entitlementsFromPB(req.Entitlements), procs, req.Internal, req.SourcePolicy, req.SourcePolicySession, req.ProxyNetwork)
	if err != nil {
		return nil, err
	}
	return &controlapi.SolveResponse{
		ExporterResponse: resp.ExporterResponse,
	}, nil
}

func (c *Controller) Status(req *controlapi.StatusRequest, stream controlapi.Control_StatusServer) error {
	if err := sendTimestampHeader(stream); err != nil {
		return err
	}
	ch := make(chan *client.SolveStatus, 8)

	eg, ctx := errgroup.WithContext(stream.Context())
	eg.Go(func() error {
		return c.solver.Status(ctx, req.Ref, ch)
	})

	eg.Go(func() error {
		defer func() {
			// drain channel on error
			for range ch {
			}
		}()
		for {
			ss, ok := <-ch
			if !ok {
				return nil
			}
			for _, sr := range ss.Marshal() {
				if err := stream.SendMsg(sr); err != nil {
					return err
				}
			}
		}
	})

	return eg.Wait()
}

func (c *Controller) Session(stream controlapi.Control_SessionServer) error {
	bklog.G(stream.Context()).Debugf("session started")

	conn, closeCh, opts := grpchijack.Hijack(stream)
	defer conn.Close()

	ctx, cancel := context.WithCancelCause(stream.Context())
	go func() {
		<-closeCh
		cancel(errors.WithStack(context.Canceled))
	}()

	err := c.opt.SessionManager.HandleConn(ctx, conn, opts)
	bklog.G(ctx).Debugf("session finished: %v", err)
	return err
}

func (c *Controller) ListWorkers(ctx context.Context, r *controlapi.ListWorkersRequest) (*controlapi.ListWorkersResponse, error) {
	resp := &controlapi.ListWorkersResponse{}
	workers, err := c.opt.WorkerController.List(r.Filter...)
	if err != nil {
		return nil, err
	}
	for _, w := range workers {
		resp.Record = append(resp.Record, &apitypes.WorkerRecord{
			ID:              w.ID(),
			Labels:          w.Labels(),
			Platforms:       pb.PlatformsFromSpec(w.Platforms(true)),
			GCPolicy:        toPBGCPolicy(w.GCPolicy()),
			BuildkitVersion: toPBBuildkitVersion(w.BuildkitVersion()),
			CDIDevices:      toPBCDIDevices(w.CDIManager()),
		})
	}
	return resp, nil
}

func (c *Controller) Info(ctx context.Context, r *controlapi.InfoRequest) (*controlapi.InfoResponse, error) {
	buildkitVersion := toPBBuildkitVersion(client.BuildkitVersion{
		Package:  version.Package,
		Version:  version.Version,
		Revision: version.Revision,
	})
	if dockerfileVersion := dockerfileversion.Version(); dockerfileVersion != "" {
		buildkitVersion.DockerfileVersion = dockerfileVersion
	}
	return &controlapi.InfoResponse{
		BuildkitVersion: buildkitVersion,
	}, nil
}

func (c *Controller) gc() {
	c.gcmu.Lock()
	defer c.gcmu.Unlock()

	workers, err := c.opt.WorkerController.List()
	if err != nil {
		return
	}

	eg, ctx := errgroup.WithContext(context.TODO())

	var size int64
	ch := make(chan client.UsageInfo)
	done := make(chan struct{})
	go func() {
		for ui := range ch {
			size += ui.Size
		}
		close(done)
	}()

	for _, w := range workers {
		eg.Go(func() error {
			if policy := w.GCPolicy(); len(policy) > 0 {
				return w.Prune(ctx, ch, policy...)
			}
			return nil
		})
	}

	err = eg.Wait()
	close(ch)
	if err != nil {
		bklog.G(ctx).Errorf("gc error: %+v", err)
	}
	<-done
	if size > 0 {
		bklog.G(ctx).Debugf("gc cleaned up %d bytes", size)
		go c.throttledReleaseUnreferenced()
	}
}

func parseCacheExportMode(mode string) (solver.CacheExportMode, bool) {
	switch mode {
	case "min":
		return solver.CacheExportModeMin, true
	case "max":
		return solver.CacheExportModeMax, true
	}
	return solver.CacheExportModeMin, false
}

func parseCacheExportIgnoreError(ignoreErrorStr string) (bool, bool) {
	ignoreError, err := strconv.ParseBool(ignoreErrorStr)
	if err != nil {
		return false, false
	}
	return ignoreError, true
}

func toPBGCPolicy(in []client.PruneInfo) []*apitypes.GCPolicy {
	policy := make([]*apitypes.GCPolicy, 0, len(in))
	for _, p := range in {
		policy = append(policy, &apitypes.GCPolicy{
			All:           p.All,
			Filters:       p.Filter,
			KeepDuration:  int64(p.KeepDuration),
			ReservedSpace: p.ReservedSpace,
			MaxUsedSpace:  p.MaxUsedSpace,
			MinFreeSpace:  p.MinFreeSpace,
		})
	}
	return policy
}

func toPBBuildkitVersion(in client.BuildkitVersion) *apitypes.BuildkitVersion {
	return &apitypes.BuildkitVersion{
		Package:           in.Package,
		Version:           in.Version,
		Revision:          in.Revision,
		DockerfileVersion: in.DockerfileVersion,
	}
}

func toPBCDIDevices(manager *cdidevices.Manager) []*apitypes.CDIDevice {
	if manager == nil {
		return nil
	}
	devs := manager.ListDevices()
	out := make([]*apitypes.CDIDevice, 0, len(devs))
	for _, dev := range devs {
		out = append(out, &apitypes.CDIDevice{
			Name:        dev.Name,
			AutoAllow:   dev.AutoAllow,
			Annotations: dev.Annotations,
			OnDemand:    dev.OnDemand,
		})
	}
	return out
}

func findDuplicateCacheOptions(cacheOpts []*controlapi.CacheOptionsEntry) ([]*controlapi.CacheOptionsEntry, []*controlapi.CacheOptionsEntry, error) {
	seen := map[string]*controlapi.CacheOptionsEntry{}
	duplicate := map[string]struct{}{}
	hasInline := false
	rest := make([]*controlapi.CacheOptionsEntry, 0, len(cacheOpts))
	for _, opt := range cacheOpts {
		if opt.Type == "inline" && hasInline {
			continue
		}
		k, err := cacheOptKey(opt)
		if err != nil {
			return nil, nil, err
		}
		if _, ok := seen[k]; ok {
			duplicate[k] = struct{}{}
		}
		seen[k] = opt
		if opt.Type == "inline" {
			hasInline = true
		}
		rest = append(rest, opt)
	}

	var duplicates []*controlapi.CacheOptionsEntry
	for k := range duplicate {
		duplicates = append(duplicates, seen[k])
	}
	return rest, duplicates, nil
}

func cacheOptKey(opt *controlapi.CacheOptionsEntry) (string, error) {
	if opt.Type == "registry" && opt.Attrs["ref"] != "" {
		return opt.Attrs["ref"], nil
	}
	rawOpt := struct {
		Type  string
		Attrs map[string]string
	}{
		Type:  opt.Type,
		Attrs: opt.Attrs,
	}
	hash, err := hashstructure.Hash(rawOpt, nil)
	if err != nil {
		return "", err
	}
	return fmt.Sprint(opt.Type, ":", hash), nil
}

type roContentStore struct {
	content.Store
}

func (cs *roContentStore) Writer(ctx context.Context, opts ...content.WriterOpt) (content.Writer, error) {
	return nil, errors.Errorf("read-only content store")
}

func (cs *roContentStore) Delete(ctx context.Context, dgst digest.Digest) error {
	return errors.Errorf("read-only content store")
}

func (cs *roContentStore) Update(ctx context.Context, info content.Info, fieldpaths ...string) (content.Info, error) {
	return content.Info{}, errors.Errorf("read-only content store")
}

func (cs *roContentStore) Abort(ctx context.Context, ref string) error {
	return errors.Errorf("read-only content store")
}

const timestampKey = "buildkit-current-timestamp"

func sendTimestampHeader(srv grpc.ServerStream) error {
	return srv.SendHeader(metadata.Pairs(timestampKey, time.Now().Format(time.RFC3339Nano)))
}

func entitlementsFromPB(elems []string) []entitlements.Entitlement {
	clone := make([]entitlements.Entitlement, len(elems))
	for i, e := range elems {
		clone[i] = entitlements.Entitlement(e)
	}
	return clone
}
