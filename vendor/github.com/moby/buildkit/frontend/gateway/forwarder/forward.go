package forwarder

import (
	"context"
	"sync"

	"github.com/moby/buildkit/cache"
	cacheutil "github.com/moby/buildkit/cache/util"
	clienttypes "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/frontend"
	"github.com/moby/buildkit/frontend/gateway/client"
	gwpb "github.com/moby/buildkit/frontend/gateway/pb"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/solver"
	opspb "github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/apicaps"
	"github.com/moby/buildkit/worker"
	"github.com/pkg/errors"
	fstypes "github.com/tonistiigi/fsutil/types"
)

func llbBridgeToGatewayClient(ctx context.Context, llbBridge frontend.FrontendLLBBridge, opts map[string]string, workerInfos []clienttypes.WorkerInfo) (*bridgeClient, error) {
	return &bridgeClient{
		opts:              opts,
		FrontendLLBBridge: llbBridge,
		sid:               session.FromContext(ctx),
		workerInfos:       workerInfos,
		final:             map[*ref]struct{}{},
	}, nil
}

type bridgeClient struct {
	frontend.FrontendLLBBridge
	mu           sync.Mutex
	opts         map[string]string
	final        map[*ref]struct{}
	sid          string
	exporterAttr map[string][]byte
	refs         []*ref
	workerInfos  []clienttypes.WorkerInfo
}

func (c *bridgeClient) Solve(ctx context.Context, req client.SolveRequest) (*client.Result, error) {
	res, err := c.FrontendLLBBridge.Solve(ctx, frontend.SolveRequest{
		Definition:   req.Definition,
		Frontend:     req.Frontend,
		FrontendOpt:  req.FrontendOpt,
		CacheImports: req.CacheImports,
	})
	if err != nil {
		return nil, err
	}

	cRes := &client.Result{}
	c.mu.Lock()
	for k, r := range res.Refs {
		rr := &ref{r}
		c.refs = append(c.refs, rr)
		cRes.AddRef(k, rr)
	}
	if r := res.Ref; r != nil {
		rr := &ref{r}
		c.refs = append(c.refs, rr)
		cRes.SetRef(rr)
	}
	c.mu.Unlock()
	cRes.Metadata = res.Metadata

	return cRes, nil
}
func (c *bridgeClient) BuildOpts() client.BuildOpts {
	workers := make([]client.WorkerInfo, 0, len(c.workerInfos))
	for _, w := range c.workerInfos {
		workers = append(workers, client.WorkerInfo{
			ID:        w.ID,
			Labels:    w.Labels,
			Platforms: w.Platforms,
		})
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

func (c *bridgeClient) toFrontendResult(r *client.Result) (*frontend.Result, error) {
	if r == nil {
		return nil, nil
	}

	res := &frontend.Result{}

	if r.Refs != nil {
		res.Refs = make(map[string]solver.CachedResult, len(r.Refs))
		for k, r := range r.Refs {
			rr, ok := r.(*ref)
			if !ok {
				return nil, errors.Errorf("invalid reference type for forward %T", r)
			}
			c.final[rr] = struct{}{}
			res.Refs[k] = rr.CachedResult
		}
	}
	if r := r.Ref; r != nil {
		rr, ok := r.(*ref)
		if !ok {
			return nil, errors.Errorf("invalid reference type for forward %T", r)
		}
		c.final[rr] = struct{}{}
		res.Ref = rr.CachedResult
	}
	res.Metadata = r.Metadata

	return res, nil
}

func (c *bridgeClient) discard(err error) {
	for _, r := range c.refs {
		if r != nil {
			if _, ok := c.final[r]; !ok || err != nil {
				r.Release(context.TODO())
			}
		}
	}
}

type ref struct {
	solver.CachedResult
}

func (r *ref) ReadFile(ctx context.Context, req client.ReadRequest) ([]byte, error) {
	ref, err := r.getImmutableRef()
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
	return cacheutil.ReadFile(ctx, ref, newReq)
}

func (r *ref) ReadDir(ctx context.Context, req client.ReadDirRequest) ([]*fstypes.Stat, error) {
	ref, err := r.getImmutableRef()
	if err != nil {
		return nil, err
	}
	newReq := cacheutil.ReadDirRequest{
		Path:           req.Path,
		IncludePattern: req.IncludePattern,
	}
	return cacheutil.ReadDir(ctx, ref, newReq)
}

func (r *ref) StatFile(ctx context.Context, req client.StatRequest) (*fstypes.Stat, error) {
	ref, err := r.getImmutableRef()
	if err != nil {
		return nil, err
	}
	return cacheutil.StatFile(ctx, ref, req.Path)
}

func (r *ref) getImmutableRef() (cache.ImmutableRef, error) {
	ref, ok := r.CachedResult.Sys().(*worker.WorkerRef)
	if !ok {
		return nil, errors.Errorf("invalid ref: %T", r.CachedResult.Sys())
	}
	return ref.ImmutableRef, nil
}
