package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/distribution/reference"
	apitypes "github.com/moby/buildkit/api/types"
	"github.com/moby/buildkit/cache"
	cacheutil "github.com/moby/buildkit/cache/util"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/executor"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/frontend"
	gw "github.com/moby/buildkit/frontend/gateway/client"
	pb "github.com/moby/buildkit/frontend/gateway/pb"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/solver"
	opspb "github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/apicaps"
	"github.com/moby/buildkit/util/tracing"
	"github.com/moby/buildkit/worker"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

const (
	keySource = "source"
	keyDevel  = "gateway-devel"
)

func NewGatewayFrontend(w frontend.WorkerInfos) frontend.Frontend {
	return &gatewayFrontend{
		workers: w,
	}
}

type gatewayFrontend struct {
	workers frontend.WorkerInfos
}

func filterPrefix(opts map[string]string, pfx string) map[string]string {
	m := map[string]string{}
	for k, v := range opts {
		if strings.HasPrefix(k, pfx) {
			m[strings.TrimPrefix(k, pfx)] = v
		}
	}
	return m
}

func (gf *gatewayFrontend) Solve(ctx context.Context, llbBridge frontend.FrontendLLBBridge, opts map[string]string) (*frontend.Result, error) {
	source, ok := opts[keySource]
	if !ok {
		return nil, errors.Errorf("no source specified for gateway")
	}

	sid := session.FromContext(ctx)

	_, isDevel := opts[keyDevel]
	var img specs.Image
	var rootFS cache.ImmutableRef
	var readonly bool // TODO: try to switch to read-only by default.

	if isDevel {
		devRes, err := llbBridge.Solve(session.NewContext(ctx, "gateway:"+sid),
			frontend.SolveRequest{
				Frontend:    source,
				FrontendOpt: filterPrefix(opts, "gateway-"),
			})
		if err != nil {
			return nil, err
		}
		defer func() {
			devRes.EachRef(func(ref solver.CachedResult) error {
				return ref.Release(context.TODO())
			})
		}()
		if devRes.Ref == nil {
			return nil, errors.Errorf("development gateway didn't return default result")
		}
		workerRef, ok := devRes.Ref.Sys().(*worker.WorkerRef)
		if !ok {
			return nil, errors.Errorf("invalid ref: %T", devRes.Ref.Sys())
		}
		rootFS = workerRef.ImmutableRef
		config, ok := devRes.Metadata[exptypes.ExporterImageConfigKey]
		if ok {
			if err := json.Unmarshal(config, &img); err != nil {
				return nil, err
			}
		}
	} else {
		sourceRef, err := reference.ParseNormalizedNamed(source)
		if err != nil {
			return nil, err
		}

		dgst, config, err := llbBridge.ResolveImageConfig(ctx, reference.TagNameOnly(sourceRef).String(), gw.ResolveImageConfigOpt{})
		if err != nil {
			return nil, err
		}

		if err := json.Unmarshal(config, &img); err != nil {
			return nil, err
		}

		if dgst != "" {
			sourceRef, err = reference.WithDigest(sourceRef, dgst)
			if err != nil {
				return nil, err
			}
		}

		src := llb.Image(sourceRef.String(), &markTypeFrontend{})

		def, err := src.Marshal()
		if err != nil {
			return nil, err
		}

		res, err := llbBridge.Solve(ctx, frontend.SolveRequest{
			Definition: def.ToPB(),
		})
		if err != nil {
			return nil, err
		}
		defer func() {
			res.EachRef(func(ref solver.CachedResult) error {
				return ref.Release(context.TODO())
			})
		}()
		if res.Ref == nil {
			return nil, errors.Errorf("gateway source didn't return default result")

		}
		workerRef, ok := res.Ref.Sys().(*worker.WorkerRef)
		if !ok {
			return nil, errors.Errorf("invalid ref: %T", res.Ref.Sys())
		}
		rootFS = workerRef.ImmutableRef
	}

	lbf, ctx, err := newLLBBridgeForwarder(ctx, llbBridge, gf.workers)
	defer lbf.conn.Close()
	if err != nil {
		return nil, err
	}

	args := []string{"/run"}
	env := []string{}
	cwd := "/"
	if img.Config.Env != nil {
		env = img.Config.Env
	}
	if img.Config.Entrypoint != nil {
		args = img.Config.Entrypoint
	}
	if img.Config.WorkingDir != "" {
		cwd = img.Config.WorkingDir
	}
	i := 0
	for k, v := range opts {
		env = append(env, fmt.Sprintf("BUILDKIT_FRONTEND_OPT_%d", i)+"="+k+"="+v)
		i++
	}

	env = append(env, "BUILDKIT_SESSION_ID="+sid)

	dt, err := json.Marshal(gf.workers.WorkerInfos())
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal workers array")
	}
	env = append(env, "BUILDKIT_WORKERS="+string(dt))

	defer lbf.Discard()

	env = append(env, "BUILDKIT_EXPORTEDPRODUCT="+apicaps.ExportedProduct)

	meta := executor.Meta{
		Env:            env,
		Args:           args,
		Cwd:            cwd,
		ReadonlyRootFS: readonly,
	}

	if v, ok := img.Config.Labels["moby.buildkit.frontend.network.none"]; ok {
		if ok, _ := strconv.ParseBool(v); ok {
			meta.NetMode = opspb.NetMode_NONE
		}
	}

	err = llbBridge.Exec(ctx, meta, rootFS, lbf.Stdin, lbf.Stdout, os.Stderr)

	if err != nil {
		if errors.Cause(err) == context.Canceled && lbf.isErrServerClosed {
			err = errors.Errorf("frontend grpc server closed unexpectedly")
		}
		// An existing error (set via Return rpc) takes
		// precedence over this error, which in turn takes
		// precedence over a success reported via Return.
		lbf.mu.Lock()
		if lbf.err == nil {
			lbf.result = nil
			lbf.err = err
		}
		lbf.mu.Unlock()
	}

	return lbf.Result()
}

func (lbf *llbBridgeForwarder) Discard() {
	lbf.mu.Lock()
	defer lbf.mu.Unlock()
	for id, r := range lbf.refs {
		if lbf.err == nil && lbf.result != nil {
			keep := false
			lbf.result.EachRef(func(r2 solver.CachedResult) error {
				if r == r2 {
					keep = true
				}
				return nil
			})
			if keep {
				continue
			}
		}
		r.Release(context.TODO())
		delete(lbf.refs, id)
	}
}

func (lbf *llbBridgeForwarder) Done() <-chan struct{} {
	return lbf.doneCh
}

func (lbf *llbBridgeForwarder) setResult(r *frontend.Result, err error) (*pb.ReturnResponse, error) {
	lbf.mu.Lock()
	defer lbf.mu.Unlock()

	if (r == nil) == (err == nil) {
		return nil, errors.New("gateway return must be either result or err")
	}

	if lbf.result != nil || lbf.err != nil {
		return nil, errors.New("gateway result is already set")
	}

	lbf.result = r
	lbf.err = err
	close(lbf.doneCh)
	return &pb.ReturnResponse{}, nil
}

func (lbf *llbBridgeForwarder) Result() (*frontend.Result, error) {
	lbf.mu.Lock()
	defer lbf.mu.Unlock()

	if lbf.result == nil && lbf.err == nil {
		return nil, errors.New("no result for incomplete build")
	}

	if lbf.err != nil {
		return nil, lbf.err
	}

	return lbf.result, nil
}

func NewBridgeForwarder(ctx context.Context, llbBridge frontend.FrontendLLBBridge, workers frontend.WorkerInfos) *llbBridgeForwarder {
	lbf := &llbBridgeForwarder{
		callCtx:   ctx,
		llbBridge: llbBridge,
		refs:      map[string]solver.CachedResult{},
		doneCh:    make(chan struct{}),
		pipe:      newPipe(),
		workers:   workers,
	}
	return lbf
}

func newLLBBridgeForwarder(ctx context.Context, llbBridge frontend.FrontendLLBBridge, workers frontend.WorkerInfos) (*llbBridgeForwarder, context.Context, error) {
	ctx, cancel := context.WithCancel(ctx)
	lbf := NewBridgeForwarder(ctx, llbBridge, workers)
	server := grpc.NewServer()
	grpc_health_v1.RegisterHealthServer(server, health.NewServer())
	pb.RegisterLLBBridgeServer(server, lbf)

	go func() {
		serve(ctx, server, lbf.conn)
		select {
		case <-ctx.Done():
		default:
			lbf.isErrServerClosed = true
		}
		cancel()
	}()

	return lbf, ctx, nil
}

type pipe struct {
	Stdin  io.ReadCloser
	Stdout io.WriteCloser
	conn   net.Conn
}

func newPipe() *pipe {
	pr1, pw1, _ := os.Pipe()
	pr2, pw2, _ := os.Pipe()
	return &pipe{
		Stdin:  pr1,
		Stdout: pw2,
		conn: &conn{
			Reader: pr2,
			Writer: pw1,
			Closer: pw2,
		},
	}
}

type conn struct {
	io.Reader
	io.Writer
	io.Closer
}

func (s *conn) LocalAddr() net.Addr {
	return dummyAddr{}
}
func (s *conn) RemoteAddr() net.Addr {
	return dummyAddr{}
}
func (s *conn) SetDeadline(t time.Time) error {
	return nil
}
func (s *conn) SetReadDeadline(t time.Time) error {
	return nil
}
func (s *conn) SetWriteDeadline(t time.Time) error {
	return nil
}

type dummyAddr struct {
}

func (d dummyAddr) Network() string {
	return "pipe"
}

func (d dummyAddr) String() string {
	return "localhost"
}

type LLBBridgeForwarder interface {
	pb.LLBBridgeServer
	Done() <-chan struct{}
	Result() (*frontend.Result, error)
}

type llbBridgeForwarder struct {
	mu        sync.Mutex
	callCtx   context.Context
	llbBridge frontend.FrontendLLBBridge
	refs      map[string]solver.CachedResult
	// lastRef      solver.CachedResult
	// lastRefs     map[string]solver.CachedResult
	// err          error
	doneCh            chan struct{} // closed when result or err become valid through a call to a Return
	result            *frontend.Result
	err               error
	exporterAttr      map[string][]byte
	workers           frontend.WorkerInfos
	isErrServerClosed bool
	*pipe
}

func (lbf *llbBridgeForwarder) ResolveImageConfig(ctx context.Context, req *pb.ResolveImageConfigRequest) (*pb.ResolveImageConfigResponse, error) {
	ctx = tracing.ContextWithSpanFromContext(ctx, lbf.callCtx)
	var platform *specs.Platform
	if p := req.Platform; p != nil {
		platform = &specs.Platform{
			OS:           p.OS,
			Architecture: p.Architecture,
			Variant:      p.Variant,
			OSVersion:    p.OSVersion,
			OSFeatures:   p.OSFeatures,
		}
	}
	dgst, dt, err := lbf.llbBridge.ResolveImageConfig(ctx, req.Ref, gw.ResolveImageConfigOpt{
		Platform:    platform,
		ResolveMode: req.ResolveMode,
		LogName:     req.LogName,
	})
	if err != nil {
		return nil, err
	}
	return &pb.ResolveImageConfigResponse{
		Digest: dgst,
		Config: dt,
	}, nil
}

func translateLegacySolveRequest(req *pb.SolveRequest) error {
	// translates ImportCacheRefs to new CacheImports (v0.4.0)
	for _, legacyImportRef := range req.ImportCacheRefsDeprecated {
		im := &pb.CacheOptionsEntry{
			Type:  "registry",
			Attrs: map[string]string{"ref": legacyImportRef},
		}
		// FIXME(AkihiroSuda): skip append if already exists
		req.CacheImports = append(req.CacheImports, im)
	}
	req.ImportCacheRefsDeprecated = nil
	return nil
}

func (lbf *llbBridgeForwarder) Solve(ctx context.Context, req *pb.SolveRequest) (*pb.SolveResponse, error) {
	if err := translateLegacySolveRequest(req); err != nil {
		return nil, err
	}
	var cacheImports []frontend.CacheOptionsEntry
	for _, e := range req.CacheImports {
		cacheImports = append(cacheImports, frontend.CacheOptionsEntry{
			Type:  e.Type,
			Attrs: e.Attrs,
		})
	}
	ctx = tracing.ContextWithSpanFromContext(ctx, lbf.callCtx)
	res, err := lbf.llbBridge.Solve(ctx, frontend.SolveRequest{
		Definition:   req.Definition,
		Frontend:     req.Frontend,
		FrontendOpt:  req.FrontendOpt,
		CacheImports: cacheImports,
	})
	if err != nil {
		return nil, err
	}

	if len(res.Refs) > 0 && !req.AllowResultReturn {
		// this should never happen because old client shouldn't make a map request
		return nil, errors.Errorf("solve did not return default result")
	}

	pbRes := &pb.Result{}
	var defaultID string

	lbf.mu.Lock()
	if res.Refs != nil {
		ids := make(map[string]string, len(res.Refs))
		for k, ref := range res.Refs {
			id := identity.NewID()
			if ref == nil {
				id = ""
			} else {
				lbf.refs[id] = ref
			}
			ids[k] = id
		}
		pbRes.Result = &pb.Result_Refs{Refs: &pb.RefMap{Refs: ids}}
	} else {
		id := identity.NewID()
		if res.Ref == nil {
			id = ""
		} else {
			lbf.refs[id] = res.Ref
		}
		defaultID = id
		pbRes.Result = &pb.Result_Ref{Ref: id}
	}
	lbf.mu.Unlock()

	// compatibility mode for older clients
	if req.Final {
		exp := map[string][]byte{}
		if err := json.Unmarshal(req.ExporterAttr, &exp); err != nil {
			return nil, err
		}

		for k, v := range res.Metadata {
			exp[k] = v
		}

		lbf.mu.Lock()
		lbf.result = &frontend.Result{
			Ref:      lbf.refs[defaultID],
			Metadata: exp,
		}
		lbf.mu.Unlock()
	}

	resp := &pb.SolveResponse{
		Result: pbRes,
	}

	if !req.AllowResultReturn {
		resp.Ref = defaultID
	}

	return resp, nil
}
func (lbf *llbBridgeForwarder) ReadFile(ctx context.Context, req *pb.ReadFileRequest) (*pb.ReadFileResponse, error) {
	ctx = tracing.ContextWithSpanFromContext(ctx, lbf.callCtx)
	lbf.mu.Lock()
	ref, ok := lbf.refs[req.Ref]
	lbf.mu.Unlock()
	if !ok {
		return nil, errors.Errorf("no such ref: %v", req.Ref)
	}
	if ref == nil {
		return nil, errors.Wrapf(os.ErrNotExist, "%s not found", req.FilePath)
	}
	workerRef, ok := ref.Sys().(*worker.WorkerRef)
	if !ok {
		return nil, errors.Errorf("invalid ref: %T", ref.Sys())
	}

	newReq := cacheutil.ReadRequest{
		Filename: req.FilePath,
	}
	if r := req.Range; r != nil {
		newReq.Range = &cacheutil.FileRange{
			Offset: int(r.Offset),
			Length: int(r.Length),
		}
	}

	dt, err := cacheutil.ReadFile(ctx, workerRef.ImmutableRef, newReq)
	if err != nil {
		return nil, err
	}

	return &pb.ReadFileResponse{Data: dt}, nil
}

func (lbf *llbBridgeForwarder) ReadDir(ctx context.Context, req *pb.ReadDirRequest) (*pb.ReadDirResponse, error) {
	ctx = tracing.ContextWithSpanFromContext(ctx, lbf.callCtx)
	lbf.mu.Lock()
	ref, ok := lbf.refs[req.Ref]
	lbf.mu.Unlock()
	if !ok {
		return nil, errors.Errorf("no such ref: %v", req.Ref)
	}
	if ref == nil {
		return nil, errors.Wrapf(os.ErrNotExist, "%s not found", req.DirPath)
	}
	workerRef, ok := ref.Sys().(*worker.WorkerRef)
	if !ok {
		return nil, errors.Errorf("invalid ref: %T", ref.Sys())
	}

	newReq := cacheutil.ReadDirRequest{
		Path:           req.DirPath,
		IncludePattern: req.IncludePattern,
	}
	entries, err := cacheutil.ReadDir(ctx, workerRef.ImmutableRef, newReq)
	if err != nil {
		return nil, err
	}

	return &pb.ReadDirResponse{Entries: entries}, nil
}

func (lbf *llbBridgeForwarder) StatFile(ctx context.Context, req *pb.StatFileRequest) (*pb.StatFileResponse, error) {
	ctx = tracing.ContextWithSpanFromContext(ctx, lbf.callCtx)
	lbf.mu.Lock()
	ref, ok := lbf.refs[req.Ref]
	lbf.mu.Unlock()
	if !ok {
		return nil, errors.Errorf("no such ref: %v", req.Ref)
	}
	if ref == nil {
		return nil, errors.Wrapf(os.ErrNotExist, "%s not found", req.Path)
	}
	workerRef, ok := ref.Sys().(*worker.WorkerRef)
	if !ok {
		return nil, errors.Errorf("invalid ref: %T", ref.Sys())
	}

	st, err := cacheutil.StatFile(ctx, workerRef.ImmutableRef, req.Path)
	if err != nil {
		return nil, err
	}

	return &pb.StatFileResponse{Stat: st}, nil
}

func (lbf *llbBridgeForwarder) Ping(context.Context, *pb.PingRequest) (*pb.PongResponse, error) {

	workers := lbf.workers.WorkerInfos()
	pbWorkers := make([]*apitypes.WorkerRecord, 0, len(workers))
	for _, w := range workers {
		pbWorkers = append(pbWorkers, &apitypes.WorkerRecord{
			ID:        w.ID,
			Labels:    w.Labels,
			Platforms: opspb.PlatformsFromSpec(w.Platforms),
		})
	}

	return &pb.PongResponse{
		FrontendAPICaps: pb.Caps.All(),
		Workers:         pbWorkers,
		LLBCaps:         opspb.Caps.All(),
	}, nil
}

func (lbf *llbBridgeForwarder) Return(ctx context.Context, in *pb.ReturnRequest) (*pb.ReturnResponse, error) {
	if in.Error != nil {
		return lbf.setResult(nil, status.ErrorProto(&spb.Status{
			Code:    in.Error.Code,
			Message: in.Error.Message,
			// Details: in.Error.Details,
		}))
	} else {
		r := &frontend.Result{
			Metadata: in.Result.Metadata,
		}

		switch res := in.Result.Result.(type) {
		case *pb.Result_Ref:
			ref, err := lbf.convertRef(res.Ref)
			if err != nil {
				return nil, err
			}
			r.Ref = ref
		case *pb.Result_Refs:
			m := map[string]solver.CachedResult{}
			for k, v := range res.Refs.Refs {
				ref, err := lbf.convertRef(v)
				if err != nil {
					return nil, err
				}
				m[k] = ref
			}
			r.Refs = m
		}
		return lbf.setResult(r, nil)
	}
}

func (lbf *llbBridgeForwarder) convertRef(id string) (solver.CachedResult, error) {
	if id == "" {
		return nil, nil
	}
	lbf.mu.Lock()
	defer lbf.mu.Unlock()
	r, ok := lbf.refs[id]
	if !ok {
		return nil, errors.Errorf("return reference %s not found", id)
	}
	return r, nil
}

func serve(ctx context.Context, grpcServer *grpc.Server, conn net.Conn) {
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	logrus.Debugf("serving grpc connection")
	(&http2.Server{}).ServeConn(conn, &http2.ServeConnOpts{Handler: grpcServer})
}

type markTypeFrontend struct{}

func (*markTypeFrontend) SetImageOption(ii *llb.ImageInfo) {
	ii.RecordType = string(client.UsageRecordTypeFrontend)
}
