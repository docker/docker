package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/distribution/reference"
	apitypes "github.com/moby/buildkit/api/types"
	"github.com/moby/buildkit/cache"
	cacheutil "github.com/moby/buildkit/cache/util"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/executor"
	"github.com/moby/buildkit/frontend"
	"github.com/moby/buildkit/frontend/dockerui"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/frontend/gateway/container"
	"github.com/moby/buildkit/frontend/gateway/forwarder"
	pb "github.com/moby/buildkit/frontend/gateway/pb"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/errdefs"
	llberrdefs "github.com/moby/buildkit/solver/llbsolver/errdefs"
	opspb "github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/apicaps"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/grpcerrors"
	"github.com/moby/buildkit/util/stack"
	"github.com/moby/buildkit/util/tracing"
	"github.com/moby/buildkit/worker"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/sys/signal"
	"github.com/moby/sys/user"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/net/http2"
	"golang.org/x/sync/errgroup"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	timestamp "google.golang.org/protobuf/types/known/timestamppb"
)

func NewGatewayFrontend(workers worker.Infos, allowedRepositories []string) (frontend.Frontend, error) {
	var parsedAllowedRepositories []string

	for _, allowedRepository := range allowedRepositories {
		sourceRef, err := reference.ParseNormalizedNamed(allowedRepository)
		if err != nil {
			return nil, err
		}
		parsedAllowedRepositories = append(parsedAllowedRepositories, reference.TrimNamed(sourceRef).Name())
	}

	return &gatewayFrontend{
		workers:             workers,
		allowedRepositories: parsedAllowedRepositories,
	}, nil
}

type gatewayFrontend struct {
	workers             worker.Infos
	allowedRepositories []string
}

func (gf *gatewayFrontend) checkSourceIsAllowed(source string) error {
	// Returns nil if the source is allowed.
	// Returns an error if the source is not allowed.
	if len(gf.allowedRepositories) == 0 {
		// No source restrictions in place
		return nil
	}

	sourceRef, err := reference.ParseNormalizedNamed(source)
	if err != nil {
		return err
	}

	taglessSource := reference.TrimNamed(sourceRef).Name()

	if slices.Contains(gf.allowedRepositories, taglessSource) {
		// Allowed
		return nil
	}
	return errors.Errorf("'%s' is not an allowed gateway source", source)
}

func (gf *gatewayFrontend) Solve(ctx context.Context, llbBridge frontend.FrontendLLBBridge, exec executor.Executor, opts map[string]string, inputs map[string]*opspb.Definition, sid string, sm *session.Manager) (*frontend.Result, error) {
	if _, isDevel := opts[frontend.KeyDevelDeprecated]; isDevel {
		return nil, errors.New("development gateway is no longer supported")
	}

	source, ok := opts[frontend.KeySource]
	if !ok {
		return nil, errors.Errorf("no source specified for gateway")
	}

	var img dockerspec.DockerOCIImage
	var mfstDigest digest.Digest
	var rootFS cache.MutableRef
	var readonly bool // TODO: try to switch to read-only by default.

	var frontendDef *opspb.Definition

	err := gf.checkSourceIsAllowed(source)
	if err != nil {
		return nil, err
	}

	c, err := forwarder.LLBBridgeToGatewayClient(ctx, llbBridge, exec, opts, inputs, gf.workers, sid, sm)
	if err != nil {
		return nil, err
	}
	dc, err := dockerui.NewClient(c)
	if err != nil {
		return nil, err
	}
	nc, err := dc.NamedContext(source, dockerui.ContextOpt{
		CaptureDigest: &mfstDigest,
	})
	if err != nil {
		return nil, err
	}
	var st *llb.State
	if nc != nil {
		var dockerImage *dockerspec.DockerOCIImage
		st, dockerImage, err = nc.Load(ctx)
		if err != nil {
			return nil, err
		}
		if dockerImage != nil {
			img = *dockerImage
		}
	}
	if st == nil {
		sourceRef, err := reference.ParseNormalizedNamed(source)
		if err != nil {
			return nil, err
		}

		imr := sourceresolver.NewImageMetaResolver(llbBridge)
		ref, dgst, config, err := imr.ResolveImageConfig(ctx, reference.TagNameOnly(sourceRef).String(), sourceresolver.Opt{})
		if err != nil {
			return nil, err
		}

		sourceRef, err = reference.ParseNormalizedNamed(ref)
		if err != nil {
			return nil, err
		}

		mfstDigest = dgst

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
		st = &src
	}

	def, err := st.Marshal(ctx)
	if err != nil {
		return nil, err
	}

	res, err := llbBridge.Solve(ctx, frontend.SolveRequest{
		Definition: def.ToPB(),
	}, sid)
	if err != nil {
		return nil, err
	}
	defer func() {
		ctx := context.WithoutCancel(ctx)
		res.EachRef(func(ref solver.ResultProxy) error {
			return ref.Release(ctx)
		})
	}()
	if res.Ref == nil {
		return nil, errors.Errorf("gateway source didn't return default result")
	}
	frontendDef = res.Ref.Definition()
	r, err := res.Ref.Result(ctx)
	if err != nil {
		return nil, err
	}
	workerRef, ok := r.Sys().(*worker.WorkerRef)
	if !ok {
		return nil, errors.Errorf("invalid ref: %T", r.Sys())
	}
	rootFS, err = workerRef.Worker.CacheManager().New(ctx, workerRef.ImmutableRef, session.NewGroup(sid))
	if err != nil {
		return nil, err
	}
	defer rootFS.Release(context.TODO())

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

	env = append(env, "BUILDKIT_EXPORTEDPRODUCT="+apicaps.ExportedProduct)

	meta := executor.Meta{
		Env:                       env,
		Args:                      args,
		Cwd:                       cwd,
		ReadonlyRootFS:            readonly,
		RemoveMountStubsRecursive: true,
	}

	if v, ok := img.Config.Labels["moby.buildkit.frontend.network.none"]; ok {
		if ok, _ := strconv.ParseBool(v); ok {
			meta.NetMode = opspb.NetMode_NONE
		}
	}

	curCaps := getCaps(img.Config.Labels["moby.buildkit.frontend.caps"])
	addCapsForKnownFrontends(curCaps, mfstDigest)
	reqCaps := getCaps(opts["frontend.caps"])
	if len(inputs) > 0 {
		reqCaps["moby.buildkit.frontend.inputs"] = struct{}{}
	}

	for c := range reqCaps {
		if _, ok := curCaps[c]; !ok {
			return nil, stack.Enable(grpcerrors.WrapCode(errdefs.NewUnsupportedFrontendCapError(c), codes.Unimplemented))
		}
	}

	lbf, ctx := serveLLBBridgeForwarder(ctx, llbBridge, exec, gf.workers, inputs, sid, sm)
	defer lbf.Discard()
	defer lbf.conn.Close()
	// The executor may pass these fds directly to the frontend process, so
	// close them only after Run has returned and no fork can still use them.
	defer lbf.Stdin.Close()
	defer lbf.Stdout.Close()

	mdmnt, release, err := metadataMount(frontendDef)
	if err != nil {
		return nil, err
	}
	if release != nil {
		defer release()
	}
	var mnts []executor.Mount
	if mdmnt != nil {
		mnts = append(mnts, *mdmnt)
	}

	_, err = exec.Run(ctx, "", container.MountWithSession(rootFS, session.NewGroup(sid)), mnts, executor.ProcessInfo{Meta: meta, Stdin: lbf.Stdin, Stdout: lbf.Stdout, Stderr: os.Stderr}, nil)
	if err != nil {
		if errdefs.IsCanceled(ctx, err) && lbf.isErrServerClosed {
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

func metadataMount(def *opspb.Definition) (*executor.Mount, func(), error) {
	dt, err := def.MarshalVT()
	if err != nil {
		return nil, nil, err
	}
	dir, err := os.MkdirTemp("", "buildkit-metadata")
	if err != nil {
		return nil, nil, err
	}

	if err := os.WriteFile(filepath.Join(dir, "frontend.bin"), dt, 0o400); err != nil {
		return nil, nil, err
	}

	return &executor.Mount{
		Src:      &bind{dir},
		Dest:     "/run/config/buildkit/metadata",
		Readonly: true,
	}, func() {
		os.RemoveAll(dir)
	}, nil
}

type bind struct {
	dir string
}

func (b *bind) Mount(ctx context.Context, readonly bool) (snapshot.Mountable, error) {
	return &bindMount{b.dir, readonly}, nil
}

type bindMount struct {
	dir      string
	readonly bool
}

func (b *bindMount) Mount() ([]mount.Mount, func() error, error) {
	return []mount.Mount{{
		Type:    "bind",
		Source:  b.dir,
		Options: []string{"bind", "ro", "nosuid", "nodev", "noexec"},
	}}, func() error { return nil }, nil
}

func (b *bindMount) IdentityMapping() *user.IdentityMapping {
	return nil
}

func (lbf *llbBridgeForwarder) Discard() {
	lbf.discardOnce.Do(lbf.discard)
}

func (lbf *llbBridgeForwarder) discard() {
	lbf.mu.Lock()
	lbf.closing = true
	cancelContainerCtx := lbf.cancelContainerCtx
	lbf.mu.Unlock()
	if cancelContainerCtx != nil {
		cancelContainerCtx(errors.WithStack(context.Canceled))
	}

	// Release existing containers before waiting so their resources cannot
	// block an admitted container creation. A second pass below collects any
	// container registered while this pass is running.
	lbf.releaseContainers()

	// New admissions are disabled before Wait, so no Add can race this Wait.
	lbf.containerCreation.Wait()
	lbf.releaseContainers()

	lbf.mu.Lock()
	defer lbf.mu.Unlock()

	lbf.mountsMu.Lock()
	lbf.mountsClosed = true
	for id, mount := range lbf.mounts {
		mount.Unmount()
		delete(lbf.mounts, id)
	}
	lbf.mountsMu.Unlock()

	lbf.discarded = true
	for _, ee := range lbf.execErrors {
		ee.Release()
	}
	lbf.execErrors = nil
	for id, res := range lbf.resultByID {
		res.Release(context.TODO())
		delete(lbf.resultByID, id)
	}
	if lbf.err != nil && lbf.result != nil {
		lbf.result.EachRef(func(r solver.ResultProxy) error {
			r.Release(context.TODO())
			return nil
		})
	}
	for _, r := range lbf.refs {
		r.Release(context.TODO())
	}
	lbf.refs = map[string]solver.ResultProxy{}
}

func (lbf *llbBridgeForwarder) releaseContainers() {
	lbf.ctrsMu.Lock()
	ctrs := slices.Collect(maps.Keys(lbf.ctrs))
	lbf.ctrsMu.Unlock()
	for _, ctr := range ctrs {
		lbf.ReleaseContainer(context.TODO(), &pb.ReleaseContainerRequest{
			ContainerID: ctr,
		})
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

func NewBridgeForwarder(ctx context.Context, llbBridge frontend.FrontendLLBBridge, exec executor.Executor, workers worker.Infos, inputs map[string]*opspb.Definition, sid string, sm *session.Manager) LLBBridgeForwarder {
	return newBridgeForwarder(ctx, llbBridge, exec, workers, inputs, sid, sm)
}

func newBridgeForwarder(ctx context.Context, llbBridge frontend.FrontendLLBBridge, exec executor.Executor, workers worker.Infos, inputs map[string]*opspb.Definition, sid string, sm *session.Manager) *llbBridgeForwarder {
	// Container lifetime is independent of an individual gateway RPC and ends
	// when the forwarder is discarded.
	containerCtx, cancelContainerCtx := context.WithCancelCause(context.WithoutCancel(ctx))
	lbf := &llbBridgeForwarder{
		callCtx:            ctx,
		containerCtx:       containerCtx,
		cancelContainerCtx: cancelContainerCtx,
		llbBridge:          llbBridge,
		refs:               map[string]solver.ResultProxy{},
		resultByID:         map[string]solver.Result{},
		doneCh:             make(chan struct{}),
		workers:            workers,
		inputs:             inputs,
		sid:                sid,
		sm:                 sm,
		ctrs:               map[string]gwclient.Container{},
		mounts:             map[string]snapshot.Mounter{},
		executor:           exec,
	}
	return lbf
}

func serveLLBBridgeForwarder(ctx context.Context, llbBridge frontend.FrontendLLBBridge, exec executor.Executor, workers worker.Infos, inputs map[string]*opspb.Definition, sid string, sm *session.Manager) (*llbBridgeForwarder, context.Context) {
	ctx, cancel := context.WithCancelCause(ctx)
	lbf := newBridgeForwarder(ctx, llbBridge, exec, workers, inputs, sid, sm)
	lbf.pipe = newPipe()
	serverOpt := []grpc.ServerOption{
		grpc.UnaryInterceptor(grpcerrors.UnaryServerInterceptor),
		grpc.StreamInterceptor(grpcerrors.StreamServerInterceptor),
		grpc.MaxRecvMsgSize(defaults.DefaultMaxRecvMsgSize),
		grpc.MaxSendMsgSize(defaults.DefaultMaxSendMsgSize),
	}
	server := grpc.NewServer(serverOpt...)
	grpc_health_v1.RegisterHealthServer(server, health.NewServer())
	pb.RegisterLLBBridgeServer(server, lbf)

	go func() {
		serve(ctx, server, lbf.conn)
		select {
		case <-ctx.Done():
		default:
			lbf.isErrServerClosed = true
		}
		cancel(errors.WithStack(context.Canceled))
	}()

	return lbf, ctx
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
			Reader:  pr2,
			Writer:  pw1,
			closers: []io.Closer{pr2, pw1},
		},
	}
}

type conn struct {
	io.Reader
	io.Writer
	closers []io.Closer
}

func (s *conn) Close() error {
	var err error
	for _, closer := range s.closers {
		if closeErr := closer.Close(); err == nil {
			err = closeErr
		}
	}
	return errors.WithStack(err)
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

type dummyAddr struct{}

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
	Discard()
}

type llbBridgeForwarder struct {
	mu                 sync.Mutex
	discardOnce        sync.Once
	closing            bool
	containerCreation  sync.WaitGroup
	containerCtx       context.Context
	cancelContainerCtx context.CancelCauseFunc
	// newContainer is overridden by tests.
	newContainer func(context.Context, container.NewContainerRequest) (gwclient.Container, error)
	callCtx      context.Context
	llbBridge    frontend.FrontendLLBBridge
	refs         map[string]solver.ResultProxy
	resultByID   map[string]solver.Result
	discarded    bool
	execErrors   []*llberrdefs.ExecError
	// lastRef      solver.CachedResult
	// lastRefs     map[string]solver.CachedResult
	// err          error
	doneCh            chan struct{} // closed when result or err become valid through a call to a Return
	result            *frontend.Result
	err               error
	workers           worker.Infos
	inputs            map[string]*opspb.Definition
	isErrServerClosed bool
	sid               string
	sm                *session.Manager
	executor          executor.Executor
	*pipe
	ctrs         map[string]gwclient.Container
	ctrsMu       sync.Mutex
	mounts       map[string]snapshot.Mounter
	mountsMu     sync.Mutex
	mountsClosed bool
}

func (lbf *llbBridgeForwarder) ResolveSourceMeta(ctx context.Context, req *pb.ResolveSourceMetaRequest) (*pb.ResolveSourceMetaResponse, error) {
	if req.Source == nil {
		return nil, status.Error(codes.InvalidArgument, "source is required")
	}

	ctx = tracing.ContextWithSpanFromContext(ctx, lbf.callCtx)
	var platform *ocispecs.Platform
	if p := req.Platform; p != nil {
		platform = &ocispecs.Platform{
			OS:           p.OS,
			Architecture: p.Architecture,
			Variant:      p.Variant,
			OSVersion:    p.OSVersion,
			OSFeatures:   p.OSFeatures,
		}
	}
	resolveopt := sourceresolver.Opt{
		LogName:        req.LogName,
		SourcePolicies: req.SourcePolicies,
	}
	resolveopt.ImageOpt = &sourceresolver.ResolveImageOpt{
		ResolveMode: req.ResolveMode,
		Platform:    platform,
	}
	if req.Image != nil {
		resolveopt.ImageOpt.NoConfig = req.Image.NoConfig
		resolveopt.ImageOpt.AttestationChain = req.Image.AttestationChain
		resolveopt.ImageOpt.ResolveAttestations = slices.Clone(req.Image.ResolveAttestations)
	}
	resolveopt.OCILayoutOpt = &sourceresolver.ResolveOCILayoutOpt{
		Platform: platform,
	}
	if req.Git != nil {
		resolveopt.GitOpt = &sourceresolver.ResolveGitOpt{
			ReturnObject: req.Git.ReturnObject,
		}
	}
	if req.HTTP != nil && req.HTTP.ChecksumRequest != nil {
		resolveopt.HTTPOpt = &sourceresolver.ResolveHTTPOpt{
			ChecksumReq: &sourceresolver.ResolveHTTPChecksumRequest{
				Algo:   fromPBHTTPChecksumAlgo(req.HTTP.ChecksumRequest.Algo),
				Suffix: slices.Clone(req.HTTP.ChecksumRequest.Suffix),
			},
		}
	}

	resp, err := lbf.llbBridge.ResolveSourceMetadata(ctx, req.Source, resolveopt)
	if err != nil {
		return nil, err
	}
	return ToPBResolveSourceMetaResponse(resp), nil
}

func (lbf *llbBridgeForwarder) ResolveImageConfig(ctx context.Context, req *pb.ResolveImageConfigRequest) (*pb.ResolveImageConfigResponse, error) {
	ctx = tracing.ContextWithSpanFromContext(ctx, lbf.callCtx)
	var platform *ocispecs.Platform
	if p := req.Platform; p != nil {
		platform = &ocispecs.Platform{
			OS:           p.OS,
			Architecture: p.Architecture,
			Variant:      p.Variant,
			OSVersion:    p.OSVersion,
			OSFeatures:   p.OSFeatures,
		}
	}
	imr := sourceresolver.NewImageMetaResolver(lbf.llbBridge)
	resolveopt := sourceresolver.Opt{
		LogName:        req.LogName,
		SourcePolicies: req.SourcePolicies,
	}
	if sourceresolver.ResolverType(req.ResolverType) == sourceresolver.ResolverTypeRegistry {
		resolveopt.ImageOpt = &sourceresolver.ResolveImageOpt{
			ResolveMode: req.ResolveMode,
			Platform:    platform,
		}
	} else if sourceresolver.ResolverType(req.ResolverType) == sourceresolver.ResolverTypeOCILayout {
		resolveopt.OCILayoutOpt = &sourceresolver.ResolveOCILayoutOpt{
			Store: sourceresolver.ResolveImageConfigOptStore{
				SessionID: req.SessionID,
				StoreID:   req.StoreID,
			},
			Platform: platform,
		}
	}

	ref, dgst, dt, err := imr.ResolveImageConfig(ctx, req.Ref, resolveopt)
	if err != nil {
		return nil, err
	}
	return &pb.ResolveImageConfigResponse{
		Ref:    ref,
		Digest: string(dgst),
		Config: dt,
	}, nil
}

func (lbf *llbBridgeForwarder) wrapSolveError(solveErr error) error {
	var (
		ee       *llberrdefs.ExecError
		fae      *llberrdefs.FileActionError
		sce      *solver.SlowCacheError
		inputIDs []string
		mountIDs []string
		subject  errdefs.IsSolve_Subject
	)
	if errors.As(solveErr, &ee) {
		lbf.mu.Lock()
		if lbf.discarded {
			ee.Release()
		} else if !slices.Contains(lbf.execErrors, ee) {
			lbf.execErrors = append(lbf.execErrors, ee)
		}
		lbf.mu.Unlock()
		var err error
		inputIDs, err = lbf.registerResultIDs(ee.Inputs...)
		if err != nil {
			return err
		}
		mountIDs, err = lbf.registerResultIDs(ee.Mounts...)
		if err != nil {
			return err
		}
	}
	if errors.As(solveErr, &fae) {
		subject = fae.ToSubject()
	}
	if errors.As(solveErr, &sce) {
		var err error
		inputIDs, err = lbf.registerResultIDs(sce.Result)
		if err != nil {
			return err
		}
		subject = sce.ToSubject()
	}
	return errdefs.WithSolveError(solveErr, subject, inputIDs, mountIDs)
}

// registerResultIDs retains gateway-owned clones of error refs until discard.
func (lbf *llbBridgeForwarder) registerResultIDs(results ...solver.Result) (ids []string, err error) {
	lbf.mu.Lock()
	defer lbf.mu.Unlock()

	if lbf.discarded {
		return nil, nil
	}
	ids = make([]string, len(results))
	for i, res := range results {
		if res == nil {
			continue
		}
		workerRef, ok := res.Sys().(*worker.WorkerRef)
		if !ok {
			return ids, errors.Errorf("unexpected type for result, got %T", res.Sys())
		}
		id := workerRef.ID()
		ids[i] = id
		if _, ok := lbf.resultByID[id]; !ok {
			// Cached sharedOp.Exec errors have snapshots pinned by the active
			// solver state, even after the original error refs are released.
			lbf.resultByID[id] = res.Clone()
		}
	}
	return ids, nil
}

func (lbf *llbBridgeForwarder) Solve(ctx context.Context, req *pb.SolveRequest) (*pb.SolveResponse, error) {
	var cacheImports []frontend.CacheOptionsEntry
	for _, e := range req.CacheImports {
		if e == nil {
			return nil, errors.Errorf("invalid nil cache import")
		}
		cacheImports = append(cacheImports, frontend.CacheOptionsEntry{
			Type:  e.Type,
			Attrs: e.Attrs,
		})
	}

	for _, p := range req.SourcePolicies {
		if p == nil {
			return nil, errors.Errorf("invalid nil source policy")
		}
	}

	ctx = tracing.ContextWithSpanFromContext(ctx, lbf.callCtx)
	res, err := lbf.llbBridge.Solve(ctx, frontend.SolveRequest{
		Evaluate:       req.Evaluate,
		Definition:     req.Definition,
		Frontend:       req.Frontend,
		FrontendOpt:    req.FrontendOpt,
		FrontendInputs: req.FrontendInputs,
		CacheImports:   cacheImports,
		SourcePolicies: req.SourcePolicies,
	}, lbf.sid)
	if err != nil {
		return nil, lbf.wrapSolveError(err)
	}

	if len(res.Refs) > 0 && !req.AllowResultReturn {
		// this should never happen because old client shouldn't make a map request
		return nil, errors.Errorf("solve did not return default result")
	}

	pbRes := &pb.Result{
		Metadata: res.Metadata,
	}
	var defaultID string

	lbf.mu.Lock()

	if lbf.discarded {
		// A solve that finished after Discard has no one to release its refs.
		lbf.mu.Unlock()
		res.EachRef(func(r solver.ResultProxy) error {
			r.Release(context.TODO())
			return nil
		})
		return nil, stack.Enable(status.Error(codes.Unavailable, "gateway forwarder is closing"))
	}

	if res.Refs != nil {
		ids := make(map[string]string, len(res.Refs))
		defs := make(map[string]*opspb.Definition, len(res.Refs))
		for k, ref := range res.Refs {
			var id string
			var def *opspb.Definition
			if ref != nil {
				id = identity.NewID()
				def = ref.Definition()
				lbf.refs[id] = ref
			}
			ids[k] = id
			defs[k] = def
		}

		if req.AllowResultArrayRef {
			refMap := make(map[string]*pb.Ref, len(res.Refs))
			for k, id := range ids {
				refMap[k] = &pb.Ref{Id: id, Def: defs[k]}
			}
			pbRes.Result = &pb.Result_Refs{Refs: &pb.RefMap{Refs: refMap}}
		} else {
			pbRes.Result = &pb.Result_RefsDeprecated{RefsDeprecated: &pb.RefMapDeprecated{Refs: ids}}
		}
	} else {
		ref := res.Ref
		var id string
		var def *opspb.Definition
		if ref != nil {
			id = identity.NewID()
			def = ref.Definition()
			lbf.refs[id] = ref
		}
		defaultID = id

		if req.AllowResultArrayRef {
			pbRes.Result = &pb.Result_Ref{Ref: &pb.Ref{Id: id, Def: def}}
		} else {
			pbRes.Result = &pb.Result_RefDeprecated{RefDeprecated: id}
		}
	}

	if res.Attestations != nil {
		pbRes.Attestations = map[string]*pb.Attestations{}
		for k, atts := range res.Attestations {
			for _, att := range atts {
				pbAtt, err := gwclient.AttestationToPB(&att)
				if err != nil {
					lbf.mu.Unlock()
					return nil, err
				}

				if att.Ref != nil {
					id := identity.NewID()
					def := att.Ref.Definition()
					lbf.refs[id] = att.Ref
					pbAtt.Ref = &pb.Ref{Id: id, Def: def}
				}

				if pbRes.Attestations[k] == nil {
					pbRes.Attestations[k] = &pb.Attestations{}
				}
				pbRes.Attestations[k].Attestation = append(pbRes.Attestations[k].Attestation, pbAtt)
			}
		}
	}

	lbf.mu.Unlock()

	// compatibility mode for older clients
	if req.Final {
		exp := map[string][]byte{}
		if err := json.Unmarshal(req.ExporterAttr, &exp); err != nil {
			return nil, err
		}
		maps.Copy(exp, res.Metadata)

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

func (lbf *llbBridgeForwarder) getImmutableRef(ctx context.Context, id string) (cache.ImmutableRef, error) {
	lbf.mu.Lock()
	ref, ok := lbf.refs[id]
	if !ok {
		if lbf.result != nil {
			if r, ok := lbf.result.FindRef(id); ok {
				ref = r
			}
		}
		if ref == nil {
			lbf.mu.Unlock()
			return nil, errors.Errorf("no such ref: %s, all %+v", id, slices.Collect(maps.Keys(lbf.refs)))
		}
	}
	lbf.mu.Unlock()
	if ref == nil {
		return nil, errors.Errorf("empty ref: %s", id)
	}

	r, err := ref.Result(ctx)
	if err != nil {
		return nil, lbf.wrapSolveError(err)
	}

	workerRef, ok := r.Sys().(*worker.WorkerRef)
	if !ok {
		return nil, errors.Errorf("invalid ref: %T", r.Sys())
	}

	return workerRef.ImmutableRef, nil
}

func (lbf *llbBridgeForwarder) getMounter(ctx context.Context, id string, ref cache.ImmutableRef) (snapshot.Mounter, error) {
	lbf.mountsMu.Lock()
	defer lbf.mountsMu.Unlock()
	if lbf.mountsClosed {
		return nil, stack.Enable(status.Error(codes.Unavailable, "gateway forwarder is closing"))
	}

	mounter, ok := lbf.mounts[id]
	if ok {
		return mounter, nil
	}
	var mountable snapshot.Mountable
	if ref != nil {
		var err error
		mountable, err = ref.Mount(ctx, true, session.NewGroup(lbf.sid))
		if err != nil {
			return nil, err
		}
	}

	mounter = snapshot.LocalMounter(mountable)
	lbf.mounts[id] = mounter
	return mounter, nil
}

func (lbf *llbBridgeForwarder) getMount(ctx context.Context, id string, ref cache.ImmutableRef) (string, error) {
	mounter, err := lbf.getMounter(ctx, id, ref)
	if err != nil {
		return "", err
	}
	// corresponding Unmount call is made in Discard()
	return mounter.Mount()
}

func (lbf *llbBridgeForwarder) ReadFile(ctx context.Context, req *pb.ReadFileRequest) (*pb.ReadFileResponse, error) {
	ctx = tracing.ContextWithSpanFromContext(ctx, lbf.callCtx)

	ref, err := lbf.getImmutableRef(ctx, req.Ref)
	if err != nil {
		return nil, err
	}
	root, err := lbf.getMount(ctx, req.Ref, ref)
	if err != nil {
		return nil, err
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

	dt, err := cacheutil.ReadFile(ctx, root, newReq, defaults.DefaultMaxSendMsgSize)
	if err != nil {
		return nil, lbf.wrapSolveError(err)
	}

	return &pb.ReadFileResponse{Data: dt}, nil
}

func (lbf *llbBridgeForwarder) ReadDir(ctx context.Context, req *pb.ReadDirRequest) (*pb.ReadDirResponse, error) {
	ctx = tracing.ContextWithSpanFromContext(ctx, lbf.callCtx)

	ref, err := lbf.getImmutableRef(ctx, req.Ref)
	if err != nil {
		return nil, err
	}
	root, err := lbf.getMount(ctx, req.Ref, ref)
	if err != nil {
		return nil, err
	}

	newReq := cacheutil.ReadDirRequest{
		Path:           req.DirPath,
		IncludePattern: req.IncludePattern,
	}

	entries, err := cacheutil.ReadDir(ctx, root, newReq)
	if err != nil {
		return nil, lbf.wrapSolveError(err)
	}

	return &pb.ReadDirResponse{Entries: entries}, nil
}

func (lbf *llbBridgeForwarder) StatFile(ctx context.Context, req *pb.StatFileRequest) (*pb.StatFileResponse, error) {
	ctx = tracing.ContextWithSpanFromContext(ctx, lbf.callCtx)

	ref, err := lbf.getImmutableRef(ctx, req.Ref)
	if err != nil {
		return nil, err
	}
	root, err := lbf.getMount(ctx, req.Ref, ref)
	if err != nil {
		return nil, err
	}

	st, err := cacheutil.StatFile(ctx, root, req.Path)
	if err != nil {
		return nil, err
	}

	return &pb.StatFileResponse{Stat: st}, nil
}

func (lbf *llbBridgeForwarder) Evaluate(ctx context.Context, req *pb.EvaluateRequest) (*pb.EvaluateResponse, error) {
	ctx = tracing.ContextWithSpanFromContext(ctx, lbf.callCtx)

	_, err := lbf.getImmutableRef(ctx, req.Ref)
	if err != nil {
		return nil, err
	}
	return &pb.EvaluateResponse{}, nil
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
		return lbf.setResult(nil, grpcerrors.FromGRPC(status.ErrorProto(&spb.Status{
			Code:    in.Error.Code,
			Message: in.Error.Message,
			Details: in.Error.Details,
		})))
	}
	if in.Result == nil {
		return nil, stack.Enable(status.Error(codes.InvalidArgument, "result is required"))
	}
	r := &frontend.Result{
		Metadata: in.Result.Metadata,
	}

	switch res := in.Result.Result.(type) {
	case *pb.Result_RefDeprecated:
		ref, err := lbf.cloneRef(res.RefDeprecated)
		if err != nil {
			return nil, err
		}
		r.SetRef(ref)
	case *pb.Result_RefsDeprecated:
		for k, id := range res.RefsDeprecated.Refs {
			ref, err := lbf.cloneRef(id)
			if err != nil {
				return nil, err
			}
			r.AddRef(k, ref)
		}
	case *pb.Result_Ref:
		ref, err := lbf.cloneRef(res.Ref.Id)
		if err != nil {
			return nil, err
		}
		r.SetRef(ref)
	case *pb.Result_Refs:
		for k, ref := range res.Refs.Refs {
			ref, err := lbf.cloneRef(ref.Id)
			if err != nil {
				return nil, err
			}
			r.AddRef(k, ref)
		}
	}

	if in.Result.Attestations != nil {
		for k, pbAtts := range in.Result.Attestations {
			for _, pbAtt := range pbAtts.Attestation {
				att, err := gwclient.AttestationFromPB[solver.ResultProxy](pbAtt)
				if err != nil {
					return nil, err
				}
				if pbAtt.Ref != nil {
					ref, err := lbf.cloneRef(pbAtt.Ref.Id)
					if err != nil {
						return nil, err
					}
					att.Ref = ref
				}
				r.AddAttestation(k, *att)
			}
		}
	}

	return lbf.setResult(r, nil)
}

func (lbf *llbBridgeForwarder) Inputs(ctx context.Context, in *pb.InputsRequest) (*pb.InputsResponse, error) {
	return &pb.InputsResponse{
		Definitions: lbf.inputs,
	}, nil
}

func (lbf *llbBridgeForwarder) NewContainer(ctx context.Context, in *pb.NewContainerRequest) (_ *pb.NewContainerResponse, err error) {
	bklog.G(ctx).Debugf("|<--- NewContainer %s", in.ContainerID)
	if err := validateNewContainerRequest(in); err != nil {
		return nil, err
	}
	if err := lbf.beginContainerCreation(); err != nil {
		return nil, err
	}
	// Registered first so Done runs after container cleanup and ctrsMu unlock.
	defer lbf.containerCreation.Done()
	ctx, cancelCtx := lbf.containerRequestContext(ctx)
	defer cancelCtx()

	ctrReq := container.NewContainerRequest{
		ContainerID: in.ContainerID,
		NetMode:     in.Network,
		Hostname:    in.Hostname,
		Platform:    in.Platform,
		Constraints: in.Constraints,
	}

	for _, m := range in.Mounts {
		var workerRef *worker.WorkerRef
		if m.ResultID != "" {
			lbf.mu.Lock()
			res, found := lbf.resultByID[m.ResultID]
			lbf.mu.Unlock()
			var ok bool
			if found {
				workerRef, ok = res.Sys().(*worker.WorkerRef)
			}
			if !ok {
				refProxy, err := lbf.convertRef(m.ResultID)
				if err != nil {
					return nil, errors.Wrapf(err, "failed to find ref %s for %q mount", m.ResultID, m.Dest)
				}

				res, err := refProxy.Result(ctx)
				if err != nil {
					return nil, stack.Enable(err)
				}

				workerRef, ok = res.Sys().(*worker.WorkerRef)
				if !ok {
					return nil, errors.Errorf("invalid reference %T", res.Sys())
				}
			}
		}
		ctrReq.Mounts = append(ctrReq.Mounts, container.Mount{
			WorkerRef: workerRef,
			Mount: &opspb.Mount{
				Dest:      m.Dest,
				Selector:  m.Selector,
				Readonly:  m.Readonly,
				MountType: m.MountType,
				CacheOpt:  m.CacheOpt,
				SecretOpt: m.SecretOpt,
				SSHOpt:    m.SSHOpt,
			},
		})
	}

	group := session.NewGroup(lbf.sid)

	ctrReq.ExtraHosts, err = container.ParseExtraHosts(in.ExtraHosts)
	if err != nil {
		return nil, stack.Enable(err)
	}

	ctr, err := lbf.createContainer(group, ctrReq)
	if err != nil {
		return nil, stack.Enable(err)
	}
	defer func() {
		if err != nil {
			ctr.Release(context.WithoutCancel(ctx)) // ensure release on error
		}
	}()

	lbf.ctrsMu.Lock()
	defer lbf.ctrsMu.Unlock()
	// ensure we are not clobbering a dup container id request
	if _, ok := lbf.ctrs[in.ContainerID]; ok {
		return nil, stack.Enable(status.Errorf(codes.AlreadyExists, "Container %s already exists", in.ContainerID))
	}
	lbf.ctrs[in.ContainerID] = ctr
	return &pb.NewContainerResponse{}, nil
}

func (lbf *llbBridgeForwarder) beginContainerCreation() error {
	lbf.mu.Lock()
	defer lbf.mu.Unlock()
	if lbf.closing {
		return stack.Enable(status.Error(codes.Unavailable, "gateway forwarder is closing"))
	}
	// Add is serialized with the closing transition in Discard.
	lbf.containerCreation.Add(1)
	return nil
}

func (lbf *llbBridgeForwarder) createContainer(group session.Group, req container.NewContainerRequest) (gwclient.Container, error) {
	// The RPC context is cancelled when NewContainer returns, while this context
	// remains valid for the container lifetime and is cancelled by Discard.
	if lbf.newContainer != nil {
		return lbf.newContainer(lbf.containerCtx, req)
	}
	cm, err := lbf.workers.DefaultCacheManager()
	if err != nil {
		return nil, stack.Enable(err)
	}
	return container.NewContainer(lbf.containerCtx, cm, lbf.executor, lbf.sm, group, req)
}

func (lbf *llbBridgeForwarder) containerRequestContext(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(lbf.containerCtx, func() {
		cancel(context.Cause(lbf.containerCtx))
	})
	return ctx, func() {
		stop()
		cancel(errors.WithStack(context.Canceled))
	}
}

func validateNewContainerRequest(in *pb.NewContainerRequest) error {
	hasRoot := false
	for i, m := range in.Mounts {
		if m == nil {
			return stack.Enable(status.Errorf(codes.InvalidArgument, "mount %d is nil", i))
		}
		if m.Dest == opspb.RootMount {
			hasRoot = true
		}
		if m.MountType == opspb.MountType_SSH && m.SSHOpt == nil {
			return stack.Enable(status.Errorf(codes.InvalidArgument, "SSH mount %q requires options", m.Dest))
		}
	}
	if !hasRoot {
		return stack.Enable(status.Error(codes.InvalidArgument, "root mount is required"))
	}
	return nil
}

func (lbf *llbBridgeForwarder) ReadFileContainer(ctx context.Context, in *pb.ReadFileRequest) (*pb.ReadFileResponse, error) {
	bklog.G(ctx).Debugf("|<--- ReadFileContainer %s@%d", in.Ref, in.MountIndex)
	lbf.ctrsMu.Lock()
	ctr, ok := lbf.ctrs[in.Ref]
	lbf.ctrsMu.Unlock()
	if !ok {
		return nil, errors.Errorf("container details for %s@%d not found", in.Ref, in.MountIndex)
	}

	var fileRange *gwclient.FileRange
	if in.Range != nil {
		fileRange = &gwclient.FileRange{
			Length: int(in.Range.Length),
			Offset: int(in.Range.Offset),
		}
	}
	req := gwclient.ReadContainerRequest{
		ReadRequest: gwclient.ReadRequest{
			Filename: in.FilePath,
			Range:    fileRange,
		},
		MountIndex: int(in.MountIndex),
	}

	data, err := ctr.ReadFile(ctx, req)
	if err != nil {
		return nil, err
	}

	return &pb.ReadFileResponse{
		Data: data,
	}, nil
}

func (lbf *llbBridgeForwarder) ReadDirContainer(ctx context.Context, in *pb.ReadDirRequest) (*pb.ReadDirResponse, error) {
	bklog.G(ctx).Debugf("|<--- ReadDirContainer %s@%d", in.Ref, in.MountIndex)
	lbf.ctrsMu.Lock()
	ctr, ok := lbf.ctrs[in.Ref]
	lbf.ctrsMu.Unlock()
	if !ok {
		return nil, errors.Errorf("container details for %s@%d not found", in.Ref, in.MountIndex)
	}

	req := gwclient.ReadDirContainerRequest{
		ReadDirRequest: gwclient.ReadDirRequest{
			Path:           in.DirPath,
			IncludePattern: in.IncludePattern,
		},
		MountIndex: int(in.MountIndex),
	}

	files, err := ctr.ReadDir(ctx, req)
	if err != nil {
		return nil, err
	}

	return &pb.ReadDirResponse{
		Entries: files,
	}, nil
}

func (lbf *llbBridgeForwarder) StatFileContainer(ctx context.Context, in *pb.StatFileRequest) (*pb.StatFileResponse, error) {
	bklog.G(ctx).Debugf("|<--- StatFileContainer %s@%d", in.Ref, in.MountIndex)
	lbf.ctrsMu.Lock()
	ctr, ok := lbf.ctrs[in.Ref]
	lbf.ctrsMu.Unlock()
	if !ok {
		return nil, errors.Errorf("container details for %s@%d not found", in.Ref, in.MountIndex)
	}

	req := gwclient.StatContainerRequest{
		StatRequest: gwclient.StatRequest{
			Path: in.Path,
		},
		MountIndex: int(in.MountIndex),
	}

	stat, err := ctr.StatFile(ctx, req)
	if err != nil {
		return nil, err
	}

	return &pb.StatFileResponse{
		Stat: stat,
	}, nil
}

func (lbf *llbBridgeForwarder) ReleaseContainer(ctx context.Context, in *pb.ReleaseContainerRequest) (*pb.ReleaseContainerResponse, error) {
	bklog.G(ctx).Debugf("|<--- ReleaseContainer %s", in.ContainerID)
	lbf.ctrsMu.Lock()
	ctr, ok := lbf.ctrs[in.ContainerID]
	delete(lbf.ctrs, in.ContainerID)
	lbf.ctrsMu.Unlock()
	if !ok {
		return nil, errors.Errorf("container details for %s not found", in.ContainerID)
	}
	err := ctr.Release(ctx)
	return &pb.ReleaseContainerResponse{}, stack.Enable(err)
}

func (lbf *llbBridgeForwarder) Warn(ctx context.Context, in *pb.WarnRequest) (*pb.WarnResponse, error) {
	// validate ranges are valid
	for _, r := range in.Ranges {
		if r == nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid source range")
		}
	}
	err := lbf.llbBridge.Warn(ctx, digest.Digest(in.Digest), string(in.Short), frontend.WarnOpts{
		Level:      int(in.Level),
		SourceInfo: in.Info,
		Range:      in.Ranges,
		Detail:     in.Detail,
		URL:        in.Url,
	})
	if err != nil {
		return nil, err
	}
	return &pb.WarnResponse{}, nil
}

type processIO struct {
	id       string
	mu       sync.Mutex
	resize   func(context.Context, gwclient.WinSize) error
	signal   func(context.Context, syscall.Signal) error
	done     chan struct{}
	doneOnce sync.Once
	// these track the process side of the io pipe for
	// read (fd=0) and write (fd=1, fd=2)
	processReaders map[uint32]io.ReadCloser
	processWriters map[uint32]io.WriteCloser
	// these track the server side of the io pipe, so
	// when we receive an EOF over grpc, we will close
	// this end
	serverWriters map[uint32]io.WriteCloser
	serverReaders map[uint32]io.ReadCloser
}

func newProcessIO(id string, openFds []uint32) *processIO {
	pio := &processIO{
		id:             id,
		processReaders: map[uint32]io.ReadCloser{},
		processWriters: map[uint32]io.WriteCloser{},
		serverReaders:  map[uint32]io.ReadCloser{},
		serverWriters:  map[uint32]io.WriteCloser{},
		done:           make(chan struct{}),
	}

	for _, fd := range openFds {
		// TODO do we know which way to pipe each fd?  For now assume fd0 is for
		// reading, and the rest are for writing
		r, w := io.Pipe()
		if fd == 0 {
			pio.processReaders[fd] = r
			pio.serverWriters[fd] = w
		} else {
			pio.processWriters[fd] = w
			pio.serverReaders[fd] = r
		}
	}

	return pio
}

func (pio *processIO) Close() (err error) {
	pio.mu.Lock()
	defer pio.mu.Unlock()
	for fd, r := range pio.processReaders {
		delete(pio.processReaders, fd)
		err1 := r.Close()
		if err1 != nil && err == nil {
			err = stack.Enable(err1)
		}
	}
	for fd, w := range pio.serverReaders {
		delete(pio.serverReaders, fd)
		err1 := w.Close()
		if err1 != nil && err == nil {
			err = stack.Enable(err1)
		}
	}
	pio.Done()
	return err
}

func (pio *processIO) Discard() {
	pio.mu.Lock()
	defer pio.mu.Unlock()
	pio.processReaders = nil
	pio.processWriters = nil
	pio.serverReaders = nil
	pio.serverWriters = nil
	pio.Done()
}

// hold lock before calling
func (pio *processIO) Done() {
	stillOpen := len(pio.processReaders) + len(pio.processWriters) + len(pio.serverReaders) + len(pio.serverWriters)
	if stillOpen == 0 {
		pio.doneOnce.Do(func() {
			close(pio.done)
		})
	}
}

func (pio *processIO) Write(f *pb.FdMessage) (err error) {
	pio.mu.Lock()
	writer := pio.serverWriters[f.Fd]
	pio.mu.Unlock()
	if writer == nil {
		return status.Errorf(codes.OutOfRange, "fd %d unavailable to write", f.Fd)
	}
	defer func() {
		if err != nil || f.EOF {
			writer.Close()
			pio.mu.Lock()
			defer pio.mu.Unlock()
			delete(pio.serverWriters, f.Fd)
			pio.Done()
		}
	}()
	if len(f.Data) > 0 {
		_, err = writer.Write(f.Data)
		return stack.Enable(err)
	}
	return nil
}

type outputWriter struct {
	stream    pb.LLBBridge_ExecProcessServer
	fd        uint32
	processID string
}

func (w *outputWriter) Write(msg []byte) (int, error) {
	bklog.G(w.stream.Context()).Debugf("|---> File Message %s, fd=%d, %d bytes", w.processID, w.fd, len(msg))
	err := w.stream.Send(&pb.ExecMessage{
		ProcessID: w.processID,
		Input: &pb.ExecMessage_File{
			File: &pb.FdMessage{
				Fd:   w.fd,
				Data: msg,
			},
		},
	})
	return len(msg), stack.Enable(err)
}

type execProcessServerThreadSafe struct {
	pb.LLBBridge_ExecProcessServer
	sendMu sync.Mutex
}

func (w *execProcessServerThreadSafe) Send(m *pb.ExecMessage) error {
	w.sendMu.Lock()
	defer w.sendMu.Unlock()
	return w.LLBBridge_ExecProcessServer.Send(m)
}

func (lbf *llbBridgeForwarder) ExecProcess(srv pb.LLBBridge_ExecProcessServer) error {
	srv = &execProcessServerThreadSafe{
		LLBBridge_ExecProcessServer: srv,
	}
	eg, ctx := errgroup.WithContext(srv.Context())

	msgs := make(chan *pb.ExecMessage)

	eg.Go(func() error {
		defer close(msgs)
		for {
			execMsg, err := srv.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return stack.Enable(err)
			}
			switch m := execMsg.GetInput().(type) {
			case *pb.ExecMessage_Init:
				bklog.G(ctx).Debugf("|<--- Init Message %s", execMsg.ProcessID)
			case *pb.ExecMessage_File:
				if m.File.EOF {
					bklog.G(ctx).Debugf("|<--- File Message %s, fd=%d, EOF", execMsg.ProcessID, m.File.Fd)
				} else {
					bklog.G(ctx).Debugf("|<--- File Message %s, fd=%d, %d bytes", execMsg.ProcessID, m.File.Fd, len(m.File.Data))
				}
			case *pb.ExecMessage_Resize:
				bklog.G(ctx).Debugf("|<--- Resize Message %s", execMsg.ProcessID)
			case *pb.ExecMessage_Signal:
				bklog.G(ctx).Debugf("|<--- Signal Message %s: %s", execMsg.ProcessID, m.Signal.Name)
			}
			select {
			case <-ctx.Done():
			case msgs <- execMsg:
			}
		}
	})

	eg.Go(func() error {
		pios := make(map[string]*processIO)
		// close any stray pios on exit to make sure
		// all the associated resources get cleaned up
		defer func() {
			for _, pio := range pios {
				pio.Close()
				pio.Discard()
			}
		}()

		for {
			var execMsg *pb.ExecMessage
			select {
			case <-ctx.Done():
				return nil
			case execMsg = <-msgs:
			}
			if execMsg == nil {
				return nil
			}

			pid := execMsg.ProcessID
			if pid == "" {
				return stack.Enable(status.Errorf(codes.InvalidArgument, "ProcessID required"))
			}

			pio, pioFound := pios[pid]

			if data := execMsg.GetFile(); data != nil {
				if !pioFound {
					return stack.Enable(status.Errorf(codes.NotFound, "IO for process %q not found", pid))
				}
				err := pio.Write(data)
				if err != nil {
					return stack.Enable(err)
				}
			} else if resize := execMsg.GetResize(); resize != nil {
				if !pioFound {
					return stack.Enable(status.Errorf(codes.NotFound, "IO for process %q not found", pid))
				}
				pio.resize(ctx, gwclient.WinSize{
					Cols: resize.Cols,
					Rows: resize.Rows,
				})
			} else if sig := execMsg.GetSignal(); sig != nil {
				if !pioFound {
					return stack.Enable(status.Errorf(codes.NotFound, "IO for process %q not found", pid))
				}
				syscallSignal, ok := signal.SignalMap[sig.Name]
				if !ok {
					return stack.Enable(status.Errorf(codes.InvalidArgument, "Unknown signal %s", sig.Name))
				}
				pio.signal(ctx, syscallSignal)
			} else if init := execMsg.GetInit(); init != nil {
				if init.Meta == nil {
					return stack.Enable(status.Error(codes.InvalidArgument, "process meta is required"))
				}
				if pioFound {
					return stack.Enable(status.Errorf(codes.AlreadyExists, "Process %s already exists", pid))
				}
				id := init.ContainerID
				lbf.ctrsMu.Lock()
				ctr, ok := lbf.ctrs[id]
				lbf.ctrsMu.Unlock()
				if !ok {
					return stack.Enable(status.Errorf(codes.NotFound, "container %q previously released or not created", id))
				}

				initCtx, initCancel := context.WithCancelCause(context.Background())
				defer initCancel(errors.WithStack(context.Canceled))

				pio := newProcessIO(pid, init.Fds)
				pios[pid] = pio

				proc, err := ctr.Start(initCtx, gwclient.StartRequest{
					Args:                      init.Meta.Args,
					Env:                       init.Meta.Env,
					SecretEnv:                 init.Secretenv,
					User:                      init.Meta.User,
					Cwd:                       init.Meta.Cwd,
					Tty:                       init.Tty,
					Stdin:                     pio.processReaders[0],
					Stdout:                    pio.processWriters[1],
					Stderr:                    pio.processWriters[2],
					SecurityMode:              init.Security,
					RemoveMountStubsRecursive: init.Meta.RemoveMountStubsRecursive,
				})
				if err != nil {
					return stack.Enable(err)
				}
				pio.resize = proc.Resize
				pio.signal = proc.Signal

				eg.Go(func() error {
					<-pio.done
					bklog.G(ctx).Debugf("|---> Done Message %s", pid)
					err := srv.Send(&pb.ExecMessage{
						ProcessID: pid,
						Input: &pb.ExecMessage_Done{
							Done: &pb.DoneMessage{},
						},
					})
					return stack.Enable(err)
				})

				// startedSent gates the proc.Wait goroutine until
				// Started is sent and output readers are spawned,
				// preventing Exit-before-Started and a deadlock
				// where pio.Close() races with reader setup.
				startedSent := make(chan struct{})

				eg.Go(func() error {
					defer func() {
						pio.Close()
					}()
					err := proc.Wait()

					<-startedSent

					var statusCode uint32
					var exitError *pb.ExitError
					var statusError *spb.Status
					if err != nil {
						statusCode = pb.UnknownExitStatus
						st, _ := status.FromError(grpcerrors.ToGRPC(ctx, err))
						stp := st.Proto()
						statusError = &spb.Status{
							Code:    stp.Code,
							Message: stp.Message,
							Details: stp.Details,
						}
					}
					if errors.As(err, &exitError) {
						statusCode = exitError.ExitCode
					}
					bklog.G(ctx).Debugf("|---> Exit Message %s, code=%d, error=%s", pid, statusCode, err)
					sendErr := srv.Send(&pb.ExecMessage{
						ProcessID: pid,
						Input: &pb.ExecMessage_Exit{
							Exit: &pb.ExitMessage{
								Code:  statusCode,
								Error: statusError,
							},
						},
					})

					if sendErr != nil && err != nil {
						return errors.Wrap(sendErr, err.Error())
					} else if sendErr != nil {
						return stack.Enable(sendErr)
					}

					if err != nil && statusCode != 0 {
						// this was a container exit error which is "normal" so
						// don't return this error from the errgroup
						return nil
					}
					return stack.Enable(err)
				})

				bklog.G(ctx).Debugf("|---> Started Message %s", pid)
				err = srv.Send(&pb.ExecMessage{
					ProcessID: pid,
					Input: &pb.ExecMessage_Started{
						Started: &pb.StartedMessage{},
					},
				})
				if err != nil {
					close(startedSent)
					return stack.Enable(err)
				}

				// start sending Fd output back to client, this is done after
				// StartedMessage so that Fd output will not potentially arrive
				// to the client before "Started" as the container starts up.
				for fd, file := range pio.serverReaders {
					eg.Go(func() error {
						defer func() {
							file.Close()
							pio.mu.Lock()
							defer pio.mu.Unlock()
							w := pio.processWriters[fd]
							if w != nil {
								w.Close()
							}
							delete(pio.processWriters, fd)
							pio.Done()
						}()
						dest := &outputWriter{
							stream:    srv,
							fd:        fd,
							processID: pid,
						}
						_, err := io.Copy(dest, file)
						// ignore ErrClosedPipe, it is EOF for our usage.
						if err != nil && !errors.Is(err, io.ErrClosedPipe) {
							return stack.Enable(err)
						}
						// no error so must be EOF
						bklog.G(ctx).Debugf("|---> File Message %s, fd=%d, EOF", pid, fd)
						err = srv.Send(&pb.ExecMessage{
							ProcessID: pid,
							Input: &pb.ExecMessage_File{
								File: &pb.FdMessage{
									Fd:  fd,
									EOF: true,
								},
							},
						})
						return stack.Enable(err)
					})
				}
				close(startedSent)
			}
		}
	})

	err := eg.Wait()
	return stack.Enable(err)
}

func (lbf *llbBridgeForwarder) convertRef(id string) (solver.ResultProxy, error) {
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

func (lbf *llbBridgeForwarder) cloneRef(id string) (solver.ResultProxy, error) {
	if id == "" {
		return nil, nil
	}

	lbf.mu.Lock()
	defer lbf.mu.Unlock()

	r, ok := lbf.refs[id]
	if !ok {
		return nil, errors.Errorf("return reference %s not found", id)
	}

	s1, s2 := solver.SplitResultProxy(r)
	lbf.refs[id] = s1
	return s2, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func serve(ctx context.Context, grpcServer *grpc.Server, conn net.Conn) {
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	defer conn.Close()

	// The frontend container may take a while to start. Start HTTP/2's
	// client preface timeout only once the frontend begins communicating.
	reader := bufio.NewReader(conn)
	if _, err := reader.Peek(1); err != nil {
		return
	}
	bklog.G(ctx).Debugf("serving grpc connection")
	(&http2.Server{}).ServeConn(&bufferedConn{Conn: conn, reader: reader}, &http2.ServeConnOpts{Handler: grpcServer}) //nolint:staticcheck // This existing HTTP/2 connection is already established.
}

type markTypeFrontend struct{}

func (*markTypeFrontend) SetImageOption(ii *llb.ImageInfo) {
	ii.RecordType = string(client.UsageRecordTypeFrontend)
}

func getCaps(label string) map[string]struct{} {
	if label == "" {
		return make(map[string]struct{})
	}
	caps := strings.Split(label, ",")
	out := make(map[string]struct{}, len(caps))
	for _, c := range caps {
		name := strings.SplitN(c, "+", 2)
		if name[0] != "" {
			out[name[0]] = struct{}{}
		}
	}
	return out
}

func ToPBResolveSourceMetaResponse(in *sourceresolver.MetaResponse) *pb.ResolveSourceMetaResponse {
	r := &pb.ResolveSourceMetaResponse{
		Source: in.Op,
	}

	if in.Image != nil {
		r.Image = &pb.ResolveSourceImageResponse{
			Digest: string(in.Image.Digest),
			Config: in.Image.Config,
		}
		if in.Image.AttestationChain != nil {
			r.Image.AttestationChain = toPBAttestationChain(in.Image.AttestationChain)
		}
	}
	if in.Git != nil {
		r.Git = &pb.ResolveSourceGitResponse{
			Checksum:       in.Git.Checksum,
			Ref:            in.Git.Ref,
			CommitChecksum: in.Git.CommitChecksum,
			CommitObject:   in.Git.CommitObject,
			TagObject:      in.Git.TagObject,
		}
	}
	if in.HTTP != nil {
		var lastModified *timestamp.Timestamp
		if in.HTTP.LastModified != nil {
			lastModified = &timestamp.Timestamp{
				Seconds: in.HTTP.LastModified.Unix(),
			}
		}
		r.HTTP = &pb.ResolveSourceHTTPResponse{
			Checksum:     in.HTTP.Digest.String(),
			Filename:     in.HTTP.Filename,
			LastModified: lastModified,
		}
		if in.HTTP.ChecksumResponse != nil {
			r.HTTP.ChecksumResponse = &pb.ChecksumResponse{
				Digest: in.HTTP.ChecksumResponse.Digest,
				Suffix: slices.Clone(in.HTTP.ChecksumResponse.Suffix),
			}
		}
	}
	return r
}

func fromPBHTTPChecksumAlgo(in pb.ChecksumRequest_ChecksumAlgo) sourceresolver.ResolveHTTPChecksumAlgo {
	switch in {
	case pb.ChecksumRequest_CHECKSUM_ALGO_SHA256:
		return sourceresolver.ResolveHTTPChecksumAlgoSHA256
	case pb.ChecksumRequest_CHECKSUM_ALGO_SHA384:
		return sourceresolver.ResolveHTTPChecksumAlgoSHA384
	case pb.ChecksumRequest_CHECKSUM_ALGO_SHA512:
		return sourceresolver.ResolveHTTPChecksumAlgoSHA512
	default:
		return sourceresolver.ResolveHTTPChecksumAlgo(in)
	}
}

func toPBAttestationChain(ac *sourceresolver.AttestationChain) *pb.AttestationChain {
	if ac == nil {
		return nil
	}
	out := &pb.AttestationChain{
		Root:                string(ac.Root),
		ImageManifest:       string(ac.ImageManifest),
		AttestationManifest: string(ac.AttestationManifest),
		Blobs:               make(map[string]*pb.Blob),
	}
	for _, s := range ac.SignatureManifests {
		out.SignatureManifests = append(out.SignatureManifests, string(s))
	}
	for k, v := range ac.Blobs {
		out.Blobs[k.String()] = &pb.Blob{
			Descriptor_: &pb.Descriptor{
				MediaType:   v.Descriptor.MediaType,
				Size:        v.Descriptor.Size,
				Digest:      string(v.Descriptor.Digest),
				Annotations: maps.Clone(v.Descriptor.Annotations),
			},
			Data: v.Data,
		}
	}
	return out
}

func addCapsForKnownFrontends(caps map[string]struct{}, dgst digest.Digest) {
	// these frontends were built without caps detection but do support inputs
	defaults := map[digest.Digest]struct{}{
		"sha256:9ac1c43a60e31dca741a6fe8314130a9cd4c4db0311fbbc636ff992ef60ae76d": {}, // docker/dockerfile:1.1.6
		"sha256:080bd74d8778f83e7b670de193362d8c593c8b14f5c8fb919d28ee8feda0d069": {}, // docker/dockerfile:1.1.7
		"sha256:60543a9d92b92af5088fb2938fb09b2072684af8384399e153e137fe081f8ab4": {}, // docker/dockerfile:1.1.6-experimental
		"sha256:de85b2f3a3e8a2f7fe48e8e84a65f6fdd5cd5183afa6412fff9caa6871649c44": {}, // docker/dockerfile:1.1.7-experimental
	}
	if _, ok := defaults[dgst]; ok {
		caps["moby.buildkit.frontend.inputs"] = struct{}{}
	}
}
