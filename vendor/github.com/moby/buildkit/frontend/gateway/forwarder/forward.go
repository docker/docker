package forwarder

import (
	"context"
	"slices"
	"sync"

	"github.com/containerd/containerd/v2/defaults"
	cacheutil "github.com/moby/buildkit/cache/util"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/executor"
	"github.com/moby/buildkit/frontend"
	"github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/frontend/gateway/container"
	gwpb "github.com/moby/buildkit/frontend/gateway/pb"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/errdefs"
	llberrdefs "github.com/moby/buildkit/solver/llbsolver/errdefs"
	opspb "github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/solver/result"
	"github.com/moby/buildkit/util/apicaps"
	"github.com/moby/buildkit/worker"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	fstypes "github.com/tonistiigi/fsutil/types"
	"golang.org/x/sync/errgroup"
)

func LLBBridgeToGatewayClient(ctx context.Context, llbBridge frontend.FrontendLLBBridge, exec executor.Executor, opts map[string]string, inputs map[string]*opspb.Definition, w worker.Infos, sid string, sm *session.Manager) (*BridgeClient, error) {
	// The enclosing solve bounds every in-process container lifetime; an
	// individual NewContainer caller may narrow it further.
	containerCtx, cancelContainerCtx := context.WithCancelCause(ctx)
	bc := &BridgeClient{
		opts:               opts,
		inputs:             inputs,
		FrontendLLBBridge:  llbBridge,
		sid:                sid,
		sm:                 sm,
		workers:            w,
		resultByID:         make(map[string]solver.Result),
		executor:           exec,
		mounts:             make(map[string]snapshot.Mounter),
		containerCtx:       containerCtx,
		cancelContainerCtx: cancelContainerCtx,
	}
	bc.buildOpts = bc.loadBuildOpts()
	return bc, nil
}

type BridgeClient struct {
	frontend.FrontendLLBBridge
	mu                 sync.Mutex
	closing            bool
	containerCreation  sync.WaitGroup
	containerCtx       context.Context
	cancelContainerCtx context.CancelCauseFunc
	// newContainer is overridden by tests.
	newContainer func(context.Context, container.NewContainerRequest) (client.Container, error)
	opts         map[string]string
	inputs       map[string]*opspb.Definition
	sid          string
	sm           *session.Manager
	refs         []*ref
	workers      worker.Infos
	resultByID   map[string]solver.Result
	discarded    bool
	execErrors   []*llberrdefs.ExecError
	buildOpts    client.BuildOpts
	ctrs         []client.Container
	executor     executor.Executor

	mounts       map[string]snapshot.Mounter
	mountsMu     sync.Mutex
	mountsClosed bool
}

func (c *BridgeClient) Solve(ctx context.Context, req client.SolveRequest) (*client.Result, error) {
	res, err := c.FrontendLLBBridge.Solve(ctx, req, c.sid)
	if err != nil {
		return nil, c.wrapSolveError(err)
	}
	for _, atts := range res.Attestations {
		for _, att := range atts {
			if att.ContentFunc != nil {
				return nil, errors.New("attestation callback cannot be sent through gateway")
			}
		}
	}

	c.mu.Lock()
	cRes, err := result.ConvertResult(res, func(r solver.ResultProxy) (client.Reference, error) {
		rr, err := c.newRef(r, session.NewGroup(c.sid))
		if err != nil {
			return nil, err
		}
		c.refs = append(c.refs, rr)
		return rr, nil
	})
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}

	return cRes, nil
}

func (c *BridgeClient) ResolveImageConfig(ctx context.Context, ref string, opt sourceresolver.Opt) (string, digest.Digest, []byte, error) {
	imr := sourceresolver.NewImageMetaResolver(c)
	return imr.ResolveImageConfig(ctx, ref, opt)
}

func (c *BridgeClient) loadBuildOpts() client.BuildOpts {
	wis := c.workers.WorkerInfos()
	workers := make([]client.WorkerInfo, len(wis))
	for i, w := range wis {
		workers[i] = client.WorkerInfo{
			ID:        w.ID,
			Labels:    w.Labels,
			Platforms: w.Platforms,
		}
	}

	return client.BuildOpts{
		Opts:      c.opts,
		SessionID: c.sid,
		Workers:   workers,
		Product:   apicaps.ExportedProduct,
		Caps:      gwpb.Caps.CapSet(gwpb.Caps.All()),
		LLBCaps:   opspb.Caps.CapSet(opspb.Caps.All()),
	}
}

func (c *BridgeClient) BuildOpts() client.BuildOpts {
	return c.buildOpts
}

func (c *BridgeClient) Inputs(ctx context.Context) (map[string]llb.State, error) {
	inputs := make(map[string]llb.State)
	for key, def := range c.inputs {
		defop, err := llb.NewDefinitionOp(def)
		if err != nil {
			return nil, err
		}
		inputs[key] = llb.NewState(defop)
	}
	return inputs, nil
}

func (c *BridgeClient) wrapSolveError(solveErr error) error {
	var (
		ee       *llberrdefs.ExecError
		fae      *llberrdefs.FileActionError
		sce      *solver.SlowCacheError
		inputIDs []string
		mountIDs []string
		subject  errdefs.IsSolve_Subject
	)
	if errors.As(solveErr, &ee) {
		c.mu.Lock()
		if c.discarded {
			ee.Release()
		} else if !slices.Contains(c.execErrors, ee) {
			c.execErrors = append(c.execErrors, ee)
		}
		c.mu.Unlock()
		var err error
		inputIDs, err = c.registerResultIDs(ee.Inputs...)
		if err != nil {
			return err
		}
		mountIDs, err = c.registerResultIDs(ee.Mounts...)
		if err != nil {
			return err
		}
	}
	if errors.As(solveErr, &fae) {
		subject = fae.ToSubject()
	}
	if errors.As(solveErr, &sce) {
		var err error
		inputIDs, err = c.registerResultIDs(sce.Result)
		if err != nil {
			return err
		}
		subject = sce.ToSubject()
	}
	return errdefs.WithSolveError(solveErr, subject, inputIDs, mountIDs)
}

// registerResultIDs retains gateway-owned clones of error refs until discard.
func (c *BridgeClient) registerResultIDs(results ...solver.Result) (ids []string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.discarded {
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
		if _, ok := c.resultByID[id]; !ok {
			// Cached sharedOp.Exec errors have snapshots pinned by the active
			// solver state, even after the original error refs are released.
			c.resultByID[id] = res.Clone()
		}
	}
	return ids, nil
}

func (c *BridgeClient) toFrontendResult(r *client.Result) (*frontend.Result, error) {
	if r == nil {
		return nil, nil
	}
	for _, atts := range r.Attestations {
		for _, att := range atts {
			if att.ContentFunc != nil {
				return nil, errors.New("attestation callback cannot be sent through gateway")
			}
		}
	}

	res, err := result.ConvertResult(r, func(r client.Reference) (solver.ResultProxy, error) {
		rr, ok := r.(*ref)
		if !ok {
			return nil, errors.Errorf("invalid reference type for forward %T", r)
		}
		return rr.acquireResultProxy(), nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (c *BridgeClient) discard(err error) {
	c.mu.Lock()
	c.closing = true
	cancelContainerCtx := c.cancelContainerCtx
	c.mu.Unlock()
	if cancelContainerCtx != nil {
		cancelContainerCtx(errors.WithStack(context.Canceled))
	}

	// Release existing containers before waiting so their resources cannot
	// block an admitted container creation. A second pass below collects any
	// container registered while this pass is running.
	c.releaseContainers()

	// New admissions are disabled before Wait, so no Add can race this Wait.
	c.containerCreation.Wait()
	c.releaseContainers()

	c.discardMounts()

	c.mu.Lock()
	c.discarded = true
	for _, ee := range c.execErrors {
		ee.Release()
	}
	c.execErrors = nil
	for id, res := range c.resultByID {
		res.Release(context.TODO())
		delete(c.resultByID, id)
	}
	c.mu.Unlock()
	for _, r := range c.refs {
		if r != nil {
			r.resultProxy.Release(context.TODO())
			if err != nil {
				for _, clone := range r.resultProxyClones {
					clone.Release(context.TODO())
				}
			}
		}
	}
}

func (c *BridgeClient) releaseContainers() {
	c.mu.Lock()
	ctrs := slices.Clone(c.ctrs)
	c.ctrs = nil
	c.mu.Unlock()
	for _, ctr := range ctrs {
		ctr.Release(context.TODO())
	}
}

func (c *BridgeClient) discardMounts() {
	c.mountsMu.Lock()
	defer c.mountsMu.Unlock()

	c.mountsClosed = true
	for id, mount := range c.mounts {
		mount.Unmount()
		delete(c.mounts, id)
	}
}

func (c *BridgeClient) Warn(ctx context.Context, dgst digest.Digest, msg string, opts client.WarnOpts) error {
	return c.FrontendLLBBridge.Warn(ctx, dgst, msg, opts)
}

func (c *BridgeClient) NewContainer(ctx context.Context, req client.NewContainerRequest) (client.Container, error) {
	if err := c.beginContainerCreation(); err != nil {
		return nil, err
	}
	// Registered first so Done runs after every cleanup defer added below.
	defer c.containerCreation.Done()
	ctx, cancelCtx := c.containerContext(ctx)
	created := false
	defer func() {
		if !created {
			cancelCtx()
		}
	}()

	ctrReq := container.NewContainerRequest{
		ContainerID: identity.NewID(),
		NetMode:     req.NetMode,
		Hostname:    req.Hostname,
		Mounts:      make([]container.Mount, len(req.Mounts)),
		Platform:    req.Platform,
	}

	eg, egCtx := errgroup.WithContext(ctx)

	for i, m := range req.Mounts {
		eg.Go(func() error {
			var workerRef *worker.WorkerRef
			if m.Ref != nil {
				refProxy, ok := m.Ref.(*ref)
				if !ok {
					return errors.Errorf("unexpected Ref type: %T", m.Ref)
				}

				res, err := refProxy.resultProxy.Result(egCtx)
				if err != nil {
					return err
				}

				workerRef, ok = res.Sys().(*worker.WorkerRef)
				if !ok {
					return errors.Errorf("invalid ref: %T", res.Sys())
				}
			} else if m.ResultID != "" {
				c.mu.Lock()
				res, found := c.resultByID[m.ResultID]
				c.mu.Unlock()
				var ok bool
				if found {
					workerRef, ok = res.Sys().(*worker.WorkerRef)
				}
				if !ok {
					return errors.Errorf("failed to find ref %s for %q mount", m.ResultID, m.Dest)
				}
			}
			ctrReq.Mounts[i] = container.Mount{
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
			}
			return nil
		})
	}

	err := eg.Wait()
	if err != nil {
		return nil, err
	}

	ctrReq.ExtraHosts, err = container.ParseExtraHosts(req.ExtraHosts)
	if err != nil {
		return nil, err
	}

	ctr, err := c.createContainer(ctx, ctrReq)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.ctrs = append(c.ctrs, ctr)
	c.mu.Unlock()
	created = true
	return ctr, nil
}

func (c *BridgeClient) containerContext(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(c.containerCtx, func() {
		cancel(context.Cause(c.containerCtx))
	})
	return ctx, func() {
		stop()
		cancel(errors.WithStack(context.Canceled))
	}
}

func (c *BridgeClient) createContainer(ctx context.Context, req container.NewContainerRequest) (client.Container, error) {
	if c.newContainer != nil {
		return c.newContainer(ctx, req)
	}
	cm, err := c.workers.DefaultCacheManager()
	if err != nil {
		return nil, err
	}
	return container.NewContainer(ctx, cm, c.executor, c.sm, session.NewGroup(c.sid), req)
}

func (c *BridgeClient) beginContainerCreation() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return errors.New("gateway client is closing")
	}
	// Add is serialized with the closing transition in discard.
	c.containerCreation.Add(1)
	return nil
}

func (c *BridgeClient) newRef(r solver.ResultProxy, s session.Group) (*ref, error) {
	return &ref{resultProxy: r, session: s, c: c}, nil
}

type ref struct {
	resultProxy       solver.ResultProxy
	resultProxyClones []solver.ResultProxy

	session session.Group
	c       *BridgeClient
}

func (r *ref) acquireResultProxy() solver.ResultProxy {
	s1, s2 := solver.SplitResultProxy(r.resultProxy)
	r.resultProxy = s1
	r.resultProxyClones = append(r.resultProxyClones, s2)
	return s2
}

func (r *ref) ToState() (st llb.State, err error) {
	defop, err := llb.NewDefinitionOp(r.resultProxy.Definition())
	if err != nil {
		return st, err
	}
	return llb.NewState(defop), nil
}

func (r *ref) Evaluate(ctx context.Context) error {
	_, err := r.resultProxy.Result(ctx)
	if err != nil {
		return r.c.wrapSolveError(err)
	}
	return nil
}

func (r *ref) ReadFile(ctx context.Context, req client.ReadRequest) ([]byte, error) {
	root, err := r.getMount(ctx)
	if err != nil {
		return nil, err
	}
	newReq := cacheutil.ReadRequest{
		Filename: req.Filename,
	}
	if r := req.Range; r != nil {
		newReq.Range = &cacheutil.FileRange{
			Offset: r.Offset,
			Length: r.Length,
		}
	}
	return cacheutil.ReadFile(ctx, root, newReq, defaults.DefaultMaxSendMsgSize)
}

func (r *ref) ReadDir(ctx context.Context, req client.ReadDirRequest) ([]*fstypes.Stat, error) {
	root, err := r.getMount(ctx)
	if err != nil {
		return nil, err
	}
	newReq := cacheutil.ReadDirRequest{
		Path:           req.Path,
		IncludePattern: req.IncludePattern,
	}
	return cacheutil.ReadDir(ctx, root, newReq)
}

func (r *ref) StatFile(ctx context.Context, req client.StatRequest) (*fstypes.Stat, error) {
	root, err := r.getMount(ctx)
	if err != nil {
		return nil, err
	}
	return cacheutil.StatFile(ctx, root, req.Path)
}

func (r *ref) getMounter(ctx context.Context) (snapshot.Mounter, error) {
	id := r.resultProxy.ID()

	r.c.mountsMu.Lock()
	defer r.c.mountsMu.Unlock()
	if r.c.mountsClosed {
		return nil, errors.New("gateway client is closing")
	}

	mounter, ok := r.c.mounts[id]
	if !ok {
		rr, err := r.resultProxy.Result(ctx)
		if err != nil {
			return nil, r.c.wrapSolveError(err)
		}
		ref, ok := rr.Sys().(*worker.WorkerRef)
		if !ok {
			return nil, errors.Errorf("invalid ref: %T", rr.Sys())
		}

		mountable, err := ref.ImmutableRef.Mount(ctx, true, r.session)
		if err != nil {
			return nil, err
		}
		mounter = snapshot.LocalMounter(mountable)
		r.c.mounts[id] = mounter
	}
	return mounter, nil
}

func (r *ref) getMount(ctx context.Context) (string, error) {
	mounter, err := r.getMounter(ctx)
	if err != nil {
		return "", err
	}
	// corresponding Unmount call is made in discard()
	return mounter.Mount()
}
