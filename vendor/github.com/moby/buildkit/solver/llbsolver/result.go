package llbsolver

import (
	"context"
	"slices"
	"sync"

	cacheconfig "github.com/moby/buildkit/cache/config"
	"github.com/moby/buildkit/frontend"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/errdefs"
	llberrdefs "github.com/moby/buildkit/solver/llbsolver/errdefs"
	"github.com/moby/buildkit/solver/llbsolver/provenance"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/flightcontrol"
	"github.com/moby/buildkit/worker"
	"github.com/pkg/errors"
)

type Result struct {
	*frontend.Result
	Provenance *provenance.Result
}

type Attestation = frontend.Attestation

func workerRefResolver(refCfg cacheconfig.RefConfig, all bool, g session.Group) func(ctx context.Context, res solver.Result) ([]*solver.Remote, error) {
	return func(ctx context.Context, res solver.Result) ([]*solver.Remote, error) {
		ref, ok := res.Sys().(*worker.WorkerRef)
		if !ok {
			return nil, errors.Errorf("invalid result: %T", res.Sys())
		}

		return ref.GetRemotes(ctx, true, refCfg, all, g)
	}
}

type resultProxy struct {
	id         string
	b          *provenanceBridge
	req        frontend.SolveRequest
	g          flightcontrol.Group[solver.CachedResult]
	mu         sync.Mutex
	released   bool
	v          solver.CachedResult
	err        error
	execErrors []*llberrdefs.ExecError
	provenance *provenance.Capture
}

func newResultProxy(b *provenanceBridge, req frontend.SolveRequest) *resultProxy {
	return &resultProxy{req: req, b: b, id: identity.NewID()}
}

func (rp *resultProxy) ID() string {
	return rp.id
}

func (rp *resultProxy) Definition() *pb.Definition {
	return rp.req.Definition
}

func (rp *resultProxy) Provenance() any {
	if rp.provenance == nil {
		return nil
	}
	return rp.provenance
}

func (rp *resultProxy) Release(ctx context.Context) (err error) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	for _, ee := range rp.execErrors {
		rerr := ee.Release()
		if rerr != nil {
			err = rerr
		}
	}
	if rp.v != nil {
		if rp.released {
			bklog.G(ctx).Warnf("release of already released result")
		}
		if rerr := rp.v.Release(ctx); rerr != nil {
			err = rerr
		}
	}
	rp.released = true
	return
}

func (rp *resultProxy) wrapError(err error) error {
	if err == nil {
		return nil
	}
	var ve *errdefs.VertexError
	if errors.As(err, &ve) {
		if rp.req.Definition.Source != nil {
			if locs := rp.req.Definition.Source.Locations[ve.Digest]; locs != nil {
				for _, loc := range locs.Locations {
					if loc == nil || loc.SourceIndex < 0 || int(loc.SourceIndex) >= len(rp.req.Definition.Source.Infos) {
						continue
					}
					err = errdefs.WithSource(err, &errdefs.Source{
						Info:   rp.req.Definition.Source.Infos[loc.SourceIndex],
						Ranges: loc.Ranges,
					})
				}
			}
		}
	}
	return err
}

func (rp *resultProxy) loadResult(ctx context.Context) (solver.CachedResultWithProvenance, error) {
	res, err := rp.b.loadResult(ctx, rp.req.Definition, rp.req.CacheImports, rp.req.SourcePolicies)
	var ee *llberrdefs.ExecError
	if errors.As(err, &ee) {
		rp.mu.Lock()
		if rp.released {
			ee.Release()
		} else if !slices.Contains(rp.execErrors, ee) {
			rp.execErrors = append(rp.execErrors, ee)
		}
		rp.mu.Unlock()
	}
	return res, err
}

func (rp *resultProxy) Result(ctx context.Context) (res solver.CachedResult, err error) {
	defer func() {
		err = rp.wrapError(err)
	}()
	return rp.g.Do(ctx, "result", func(ctx context.Context) (solver.CachedResult, error) {
		rp.mu.Lock()
		if rp.released {
			rp.mu.Unlock()
			return nil, errors.New("accessing released result")
		}
		if rp.v != nil || rp.err != nil {
			rp.mu.Unlock()
			return rp.v, rp.err
		}
		rp.mu.Unlock()
		v, err := rp.loadResult(ctx)
		if err != nil {
			select {
			case <-ctx.Done():
				if errdefs.IsCanceled(ctx, err) {
					return v, err
				}
			default:
			}
		}
		rp.mu.Lock()
		if rp.released {
			if v != nil {
				v.Release(context.TODO())
			}
			rp.mu.Unlock()
			return nil, errors.New("evaluating released result")
		}
		if err == nil {
			var capture *provenance.Capture
			capture, err = captureProvenance(ctx, v)
			if err != nil {
				err = errors.Errorf("failed to capture provenance: %v", err)
				v.Release(context.TODO())
				v = nil
			}
			rp.provenance = capture
		}
		rp.v = v
		rp.err = err
		rp.mu.Unlock()
		return v, err
	})
}
