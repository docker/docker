package llbsolver

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/platforms"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/cache/remotecache"
	"github.com/moby/buildkit/executor"
	"github.com/moby/buildkit/frontend"
	gw "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/util/tracing"
	"github.com/moby/buildkit/worker"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type llbBridge struct {
	builder                   solver.Builder
	frontends                 map[string]frontend.Frontend
	resolveWorker             func() (worker.Worker, error)
	resolveCacheImporterFuncs map[string]remotecache.ResolveCacheImporterFunc
	cms                       map[string]solver.CacheManager
	cmsMu                     sync.Mutex
	platforms                 []specs.Platform
	sm                        *session.Manager
}

func (b *llbBridge) Solve(ctx context.Context, req frontend.SolveRequest) (res *frontend.Result, err error) {
	w, err := b.resolveWorker()
	if err != nil {
		return nil, err
	}
	var cms []solver.CacheManager
	for _, im := range req.CacheImports {
		b.cmsMu.Lock()
		var cm solver.CacheManager
		cmId := identity.NewID()
		if im.Type == "registry" {
			// For compatibility with < v0.4.0
			if ref := im.Attrs["ref"]; ref != "" {
				cmId = ref
			}
		}
		if prevCm, ok := b.cms[cmId]; !ok {
			func(cmId string) {
				cm = newLazyCacheManager(cmId, func() (solver.CacheManager, error) {
					var cmNew solver.CacheManager
					if err := inVertexContext(b.builder.Context(ctx), "importing cache manifest from "+cmId, "", func(ctx context.Context) error {
						resolveCI, ok := b.resolveCacheImporterFuncs[im.Type]
						if !ok {
							return errors.Errorf("unknown cache importer: %s", im.Type)
						}
						ci, desc, err := resolveCI(ctx, im.Attrs)
						if err != nil {
							return err
						}
						cmNew, err = ci.Resolve(ctx, desc, cmId, w)
						return err
					}); err != nil {
						logrus.Debugf("error while importing cache manifest from cmId=%s: %v", cmId, err)
						return nil, err
					}
					return cmNew, nil
				})
			}(cmId)
			b.cms[cmId] = cm
		} else {
			cm = prevCm
		}
		cms = append(cms, cm)
		b.cmsMu.Unlock()
	}

	if req.Definition != nil && req.Definition.Def != nil && req.Frontend != "" {
		return nil, errors.New("cannot solve with both Definition and Frontend specified")
	}

	if req.Definition != nil && req.Definition.Def != nil {
		ent, err := loadEntitlements(b.builder)
		if err != nil {
			return nil, err
		}

		edge, err := Load(req.Definition, ValidateEntitlements(ent), WithCacheSources(cms), RuntimePlatforms(b.platforms), WithValidateCaps())
		if err != nil {
			return nil, err
		}
		ref, err := b.builder.Build(ctx, edge)
		if err != nil {
			return nil, err
		}

		res = &frontend.Result{Ref: ref}
	} else if req.Frontend != "" {
		f, ok := b.frontends[req.Frontend]
		if !ok {
			return nil, errors.Errorf("invalid frontend: %s", req.Frontend)
		}
		res, err = f.Solve(ctx, b, req.FrontendOpt)
		if err != nil {
			return nil, err
		}
	} else {
		return &frontend.Result{}, nil
	}

	if err := res.EachRef(func(r solver.CachedResult) error {
		wr, ok := r.Sys().(*worker.WorkerRef)
		if !ok {
			return errors.Errorf("invalid reference for exporting: %T", r.Sys())
		}
		if wr.ImmutableRef != nil {
			if err := wr.ImmutableRef.Finalize(ctx, false); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return
}

func (s *llbBridge) Exec(ctx context.Context, meta executor.Meta, root cache.ImmutableRef, stdin io.ReadCloser, stdout, stderr io.WriteCloser) (err error) {
	w, err := s.resolveWorker()
	if err != nil {
		return err
	}
	span, ctx := tracing.StartSpan(ctx, strings.Join(meta.Args, " "))
	err = w.Exec(ctx, meta, root, stdin, stdout, stderr)
	tracing.FinishWithError(span, err)
	return err
}

func (s *llbBridge) ResolveImageConfig(ctx context.Context, ref string, opt gw.ResolveImageConfigOpt) (dgst digest.Digest, config []byte, err error) {
	w, err := s.resolveWorker()
	if err != nil {
		return "", nil, err
	}
	if opt.LogName == "" {
		opt.LogName = fmt.Sprintf("resolve image config for %s", ref)
	}
	id := ref // make a deterministic ID for avoiding duplicates
	if platform := opt.Platform; platform == nil {
		id += platforms.Format(platforms.DefaultSpec())
	} else {
		id += platforms.Format(*platform)
	}
	err = inVertexContext(s.builder.Context(ctx), opt.LogName, id, func(ctx context.Context) error {
		dgst, config, err = w.ResolveImageConfig(ctx, ref, opt, s.sm)
		return err
	})
	return dgst, config, err
}

type lazyCacheManager struct {
	id   string
	main solver.CacheManager

	waitCh chan struct{}
	err    error
}

func (lcm *lazyCacheManager) ID() string {
	return lcm.id
}
func (lcm *lazyCacheManager) Query(inp []solver.CacheKeyWithSelector, inputIndex solver.Index, dgst digest.Digest, outputIndex solver.Index) ([]*solver.CacheKey, error) {
	lcm.wait()
	if lcm.main == nil {
		return nil, nil
	}
	return lcm.main.Query(inp, inputIndex, dgst, outputIndex)
}
func (lcm *lazyCacheManager) Records(ck *solver.CacheKey) ([]*solver.CacheRecord, error) {
	lcm.wait()
	if lcm.main == nil {
		return nil, nil
	}
	return lcm.main.Records(ck)
}
func (lcm *lazyCacheManager) Load(ctx context.Context, rec *solver.CacheRecord) (solver.Result, error) {
	if err := lcm.wait(); err != nil {
		return nil, err
	}
	return lcm.main.Load(ctx, rec)
}
func (lcm *lazyCacheManager) Save(key *solver.CacheKey, s solver.Result, createdAt time.Time) (*solver.ExportableCacheKey, error) {
	if err := lcm.wait(); err != nil {
		return nil, err
	}
	return lcm.main.Save(key, s, createdAt)
}

func (lcm *lazyCacheManager) wait() error {
	<-lcm.waitCh
	return lcm.err
}

func newLazyCacheManager(id string, fn func() (solver.CacheManager, error)) solver.CacheManager {
	lcm := &lazyCacheManager{id: id, waitCh: make(chan struct{})}
	go func() {
		defer close(lcm.waitCh)
		cm, err := fn()
		if err != nil {
			lcm.err = err
			return
		}
		lcm.main = cm
	}()
	return lcm
}
