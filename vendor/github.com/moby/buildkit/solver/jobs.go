package solver

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/flightcontrol"
	"github.com/moby/buildkit/util/progress"
	"github.com/moby/buildkit/util/tracing"
	digest "github.com/opencontainers/go-digest"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
)

// ResolveOpFunc finds an Op implementation for a Vertex
type ResolveOpFunc func(Vertex, Builder) (Op, error)

type Builder interface {
	Build(ctx context.Context, e Edge) (CachedResult, error)
	Context(ctx context.Context) context.Context
	EachValue(ctx context.Context, key string, fn func(interface{}) error) error
}

// Solver provides a shared graph of all the vertexes currently being
// processed. Every vertex that is being solved needs to be loaded into job
// first. Vertex operations are invoked and progress tracking happens through
// jobs.
type Solver struct {
	mu      sync.RWMutex
	jobs    map[string]*Job
	actives map[digest.Digest]*state
	opts    SolverOpt

	updateCond *sync.Cond
	s          *scheduler
	index      *edgeIndex
}

type state struct {
	jobs     map[*Job]struct{}
	parents  map[digest.Digest]struct{}
	childVtx map[digest.Digest]struct{}

	mpw     *progress.MultiWriter
	allPw   map[progress.Writer]struct{}
	mspan   *tracing.MultiSpan
	allSpan map[opentracing.Span]struct{}

	vtx          Vertex
	clientVertex client.Vertex

	mu    sync.Mutex
	op    *sharedOp
	edges map[Index]*edge
	opts  SolverOpt
	index *edgeIndex

	cache     map[string]CacheManager
	mainCache CacheManager
	solver    *Solver
}

func (s *state) getSessionID() string {
	// TODO: connect with sessionmanager to avoid getting dropped sessions
	s.mu.Lock()
	for j := range s.jobs {
		if j.SessionID != "" {
			s.mu.Unlock()
			return j.SessionID
		}
	}
	parents := map[digest.Digest]struct{}{}
	for p := range s.parents {
		parents[p] = struct{}{}
	}
	s.mu.Unlock()

	for p := range parents {
		s.solver.mu.Lock()
		pst, ok := s.solver.actives[p]
		s.solver.mu.Unlock()
		if ok {
			if sessionID := pst.getSessionID(); sessionID != "" {
				return sessionID
			}
		}
	}
	return ""
}

func (s *state) builder() *subBuilder {
	return &subBuilder{state: s}
}

func (s *state) getEdge(index Index) *edge {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.edges[index]; ok {
		return e
	}

	if s.op == nil {
		s.op = newSharedOp(s.opts.ResolveOpFunc, s.opts.DefaultCache, s)
	}

	e := newEdge(Edge{Index: index, Vertex: s.vtx}, s.op, s.index)
	s.edges[index] = e
	return e
}

func (s *state) setEdge(index Index, newEdge *edge) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.edges[index]
	if ok {
		if e == newEdge {
			return
		}
		e.release()
	}

	newEdge.incrementReferenceCount()
	s.edges[index] = newEdge
}

func (s *state) combinedCacheManager() CacheManager {
	s.mu.Lock()
	cms := make([]CacheManager, 0, len(s.cache)+1)
	cms = append(cms, s.mainCache)
	for _, cm := range s.cache {
		cms = append(cms, cm)
	}
	s.mu.Unlock()

	if len(cms) == 1 {
		return s.mainCache
	}

	return newCombinedCacheManager(cms, s.mainCache)
}

func (s *state) Release() {
	for _, e := range s.edges {
		e.release()
	}
	if s.op != nil {
		s.op.release()
	}
}

type subBuilder struct {
	*state
	mu        sync.Mutex
	exporters []ExportableCacheKey
}

func (sb *subBuilder) Build(ctx context.Context, e Edge) (CachedResult, error) {
	res, err := sb.solver.subBuild(ctx, e, sb.vtx)
	if err != nil {
		return nil, err
	}
	sb.mu.Lock()
	sb.exporters = append(sb.exporters, res.CacheKeys()[0]) // all keys already have full export chain
	sb.mu.Unlock()
	return res, nil
}

func (sb *subBuilder) Context(ctx context.Context) context.Context {
	ctx = session.NewContext(ctx, sb.state.getSessionID())
	return opentracing.ContextWithSpan(progress.WithProgress(ctx, sb.mpw), sb.mspan)
}

func (sb *subBuilder) EachValue(ctx context.Context, key string, fn func(interface{}) error) error {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	for j := range sb.jobs {
		if err := j.EachValue(ctx, key, fn); err != nil {
			return err
		}
	}
	return nil
}

type Job struct {
	list   *Solver
	pr     *progress.MultiReader
	pw     progress.Writer
	span   opentracing.Span
	values sync.Map

	progressCloser func()
	SessionID      string
}

type SolverOpt struct {
	ResolveOpFunc ResolveOpFunc
	DefaultCache  CacheManager
}

func NewSolver(opts SolverOpt) *Solver {
	if opts.DefaultCache == nil {
		opts.DefaultCache = NewInMemoryCacheManager()
	}
	jl := &Solver{
		jobs:    make(map[string]*Job),
		actives: make(map[digest.Digest]*state),
		opts:    opts,
		index:   newEdgeIndex(),
	}
	jl.s = newScheduler(jl)
	jl.updateCond = sync.NewCond(jl.mu.RLocker())
	return jl
}

func (jl *Solver) setEdge(e Edge, newEdge *edge) {
	jl.mu.RLock()
	defer jl.mu.RUnlock()

	st, ok := jl.actives[e.Vertex.Digest()]
	if !ok {
		return
	}

	st.setEdge(e.Index, newEdge)
}

func (jl *Solver) getEdge(e Edge) *edge {
	jl.mu.RLock()
	defer jl.mu.RUnlock()

	st, ok := jl.actives[e.Vertex.Digest()]
	if !ok {
		return nil
	}
	return st.getEdge(e.Index)
}

func (jl *Solver) subBuild(ctx context.Context, e Edge, parent Vertex) (CachedResult, error) {
	v, err := jl.load(e.Vertex, parent, nil)
	if err != nil {
		return nil, err
	}
	e.Vertex = v
	return jl.s.build(ctx, e)
}

func (jl *Solver) Close() {
	jl.s.Stop()
}

func (jl *Solver) load(v, parent Vertex, j *Job) (Vertex, error) {
	jl.mu.Lock()
	defer jl.mu.Unlock()

	cache := map[Vertex]Vertex{}

	return jl.loadUnlocked(v, parent, j, cache)
}

func (jl *Solver) loadUnlocked(v, parent Vertex, j *Job, cache map[Vertex]Vertex) (Vertex, error) {
	if v, ok := cache[v]; ok {
		return v, nil
	}
	origVtx := v

	inputs := make([]Edge, len(v.Inputs()))
	for i, e := range v.Inputs() {
		v, err := jl.loadUnlocked(e.Vertex, parent, j, cache)
		if err != nil {
			return nil, err
		}
		inputs[i] = Edge{Index: e.Index, Vertex: v}
	}

	dgst := v.Digest()

	dgstWithoutCache := digest.FromBytes([]byte(fmt.Sprintf("%s-ignorecache", dgst)))

	// if same vertex is already loaded without cache just use that
	st, ok := jl.actives[dgstWithoutCache]

	if !ok {
		st, ok = jl.actives[dgst]

		// !ignorecache merges with ignorecache but ignorecache doesn't merge with !ignorecache
		if ok && !st.vtx.Options().IgnoreCache && v.Options().IgnoreCache {
			dgst = dgstWithoutCache
		}

		v = &vertexWithCacheOptions{
			Vertex: v,
			dgst:   dgst,
			inputs: inputs,
		}

		st, ok = jl.actives[dgst]
	}

	if !ok {
		st = &state{
			opts:         jl.opts,
			jobs:         map[*Job]struct{}{},
			parents:      map[digest.Digest]struct{}{},
			childVtx:     map[digest.Digest]struct{}{},
			allPw:        map[progress.Writer]struct{}{},
			allSpan:      map[opentracing.Span]struct{}{},
			mpw:          progress.NewMultiWriter(progress.WithMetadata("vertex", dgst)),
			mspan:        tracing.NewMultiSpan(),
			vtx:          v,
			clientVertex: initClientVertex(v),
			edges:        map[Index]*edge{},
			index:        jl.index,
			mainCache:    jl.opts.DefaultCache,
			cache:        map[string]CacheManager{},
			solver:       jl,
		}
		jl.actives[dgst] = st
	}

	st.mu.Lock()
	for _, cache := range v.Options().CacheSources {
		if cache.ID() != st.mainCache.ID() {
			if _, ok := st.cache[cache.ID()]; !ok {
				st.cache[cache.ID()] = cache
			}
		}
	}

	if j != nil {
		if _, ok := st.jobs[j]; !ok {
			st.jobs[j] = struct{}{}
		}
	}
	st.mu.Unlock()

	if parent != nil {
		if _, ok := st.parents[parent.Digest()]; !ok {
			st.parents[parent.Digest()] = struct{}{}
			parentState, ok := jl.actives[parent.Digest()]
			if !ok {
				return nil, errors.Errorf("inactive parent %s", parent.Digest())
			}
			parentState.childVtx[dgst] = struct{}{}

			for id, c := range parentState.cache {
				st.cache[id] = c
			}
		}
	}

	jl.connectProgressFromState(st, st)
	cache[origVtx] = v
	return v, nil
}

func (jl *Solver) connectProgressFromState(target, src *state) {
	for j := range src.jobs {
		if _, ok := target.allPw[j.pw]; !ok {
			target.mpw.Add(j.pw)
			target.allPw[j.pw] = struct{}{}
			j.pw.Write(target.clientVertex.Digest.String(), target.clientVertex)
			target.mspan.Add(j.span)
			target.allSpan[j.span] = struct{}{}
		}
	}
	for p := range src.parents {
		jl.connectProgressFromState(target, jl.actives[p])
	}
}

func (jl *Solver) NewJob(id string) (*Job, error) {
	jl.mu.Lock()
	defer jl.mu.Unlock()

	if _, ok := jl.jobs[id]; ok {
		return nil, errors.Errorf("job ID %s exists", id)
	}

	pr, ctx, progressCloser := progress.NewContext(context.Background())
	pw, _, _ := progress.FromContext(ctx) // TODO: expose progress.Pipe()

	j := &Job{
		list:           jl,
		pr:             progress.NewMultiReader(pr),
		pw:             pw,
		progressCloser: progressCloser,
		span:           (&opentracing.NoopTracer{}).StartSpan(""),
	}
	jl.jobs[id] = j

	jl.updateCond.Broadcast()

	return j, nil
}

func (jl *Solver) Get(id string) (*Job, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() {
		<-ctx.Done()
		jl.mu.Lock()
		jl.updateCond.Broadcast()
		jl.mu.Unlock()
	}()

	jl.mu.RLock()
	defer jl.mu.RUnlock()
	for {
		select {
		case <-ctx.Done():
			return nil, errors.Errorf("no such job %s", id)
		default:
		}
		j, ok := jl.jobs[id]
		if !ok {
			jl.updateCond.Wait()
			continue
		}
		return j, nil
	}
}

// called with solver lock
func (jl *Solver) deleteIfUnreferenced(k digest.Digest, st *state) {
	if len(st.jobs) == 0 && len(st.parents) == 0 {
		for chKey := range st.childVtx {
			chState := jl.actives[chKey]
			delete(chState.parents, k)
			jl.deleteIfUnreferenced(chKey, chState)
		}
		st.Release()
		delete(jl.actives, k)
	}
}

func (j *Job) Build(ctx context.Context, e Edge) (CachedResult, error) {
	if span := opentracing.SpanFromContext(ctx); span != nil {
		j.span = span
	}

	v, err := j.list.load(e.Vertex, nil, j)
	if err != nil {
		return nil, err
	}
	e.Vertex = v
	return j.list.s.build(ctx, e)
}

func (j *Job) Discard() error {
	defer j.progressCloser()

	j.list.mu.Lock()
	defer j.list.mu.Unlock()

	j.pw.Close()

	for k, st := range j.list.actives {
		st.mu.Lock()
		if _, ok := st.jobs[j]; ok {
			delete(st.jobs, j)
			j.list.deleteIfUnreferenced(k, st)
		}
		if _, ok := st.allPw[j.pw]; ok {
			delete(st.allPw, j.pw)
		}
		if _, ok := st.allSpan[j.span]; ok {
			delete(st.allSpan, j.span)
		}
		st.mu.Unlock()
	}
	return nil
}

func (j *Job) Context(ctx context.Context) context.Context {
	ctx = session.NewContext(ctx, j.SessionID)
	return progress.WithProgress(ctx, j.pw)
}

func (j *Job) SetValue(key string, v interface{}) {
	j.values.Store(key, v)
}

func (j *Job) EachValue(ctx context.Context, key string, fn func(interface{}) error) error {
	v, ok := j.values.Load(key)
	if ok {
		return fn(v)
	}
	return nil
}

type cacheMapResp struct {
	*CacheMap
	complete bool
}

type activeOp interface {
	CacheMap(context.Context, int) (*cacheMapResp, error)
	LoadCache(ctx context.Context, rec *CacheRecord) (Result, error)
	Exec(ctx context.Context, inputs []Result) (outputs []Result, exporters []ExportableCacheKey, err error)
	IgnoreCache() bool
	Cache() CacheManager
	CalcSlowCache(context.Context, Index, ResultBasedCacheFunc, Result) (digest.Digest, error)
}

func newSharedOp(resolver ResolveOpFunc, cacheManager CacheManager, st *state) *sharedOp {
	so := &sharedOp{
		resolver:     resolver,
		st:           st,
		slowCacheRes: map[Index]digest.Digest{},
		slowCacheErr: map[Index]error{},
	}
	return so
}

type execRes struct {
	execRes       []*SharedResult
	execExporters []ExportableCacheKey
}

type sharedOp struct {
	resolver ResolveOpFunc
	st       *state
	g        flightcontrol.Group

	opOnce     sync.Once
	op         Op
	subBuilder *subBuilder
	err        error

	execRes *execRes
	execErr error

	cacheRes  []*CacheMap
	cacheDone bool
	cacheErr  error

	slowMu       sync.Mutex
	slowCacheRes map[Index]digest.Digest
	slowCacheErr map[Index]error
}

func (s *sharedOp) IgnoreCache() bool {
	return s.st.vtx.Options().IgnoreCache
}

func (s *sharedOp) Cache() CacheManager {
	return s.st.combinedCacheManager()
}

func (s *sharedOp) LoadCache(ctx context.Context, rec *CacheRecord) (Result, error) {
	ctx = opentracing.ContextWithSpan(progress.WithProgress(ctx, s.st.mpw), s.st.mspan)
	// no cache hit. start evaluating the node
	span, ctx := tracing.StartSpan(ctx, "load cache: "+s.st.vtx.Name())
	notifyStarted(ctx, &s.st.clientVertex, true)
	res, err := s.Cache().Load(ctx, rec)
	tracing.FinishWithError(span, err)
	notifyCompleted(ctx, &s.st.clientVertex, err, true)
	return res, err
}

func (s *sharedOp) CalcSlowCache(ctx context.Context, index Index, f ResultBasedCacheFunc, res Result) (digest.Digest, error) {
	key, err := s.g.Do(ctx, fmt.Sprintf("slow-compute-%d", index), func(ctx context.Context) (interface{}, error) {
		s.slowMu.Lock()
		// TODO: add helpers for these stored values
		if res := s.slowCacheRes[index]; res != "" {
			s.slowMu.Unlock()
			return res, nil
		}
		if err := s.slowCacheErr[index]; err != nil {
			s.slowMu.Unlock()
			return err, nil
		}
		s.slowMu.Unlock()
		ctx = opentracing.ContextWithSpan(progress.WithProgress(ctx, s.st.mpw), s.st.mspan)
		key, err := f(ctx, res)
		complete := true
		if err != nil {
			select {
			case <-ctx.Done():
				if strings.Contains(err.Error(), context.Canceled.Error()) {
					complete = false
					err = errors.Wrap(ctx.Err(), err.Error())
				}
			default:
			}
		}
		s.slowMu.Lock()
		defer s.slowMu.Unlock()
		if complete {
			if err == nil {
				s.slowCacheRes[index] = key
			}
			s.slowCacheErr[index] = err
		}
		return key, err
	})
	if err != nil {
		ctx = opentracing.ContextWithSpan(progress.WithProgress(ctx, s.st.mpw), s.st.mspan)
		notifyStarted(ctx, &s.st.clientVertex, false)
		notifyCompleted(ctx, &s.st.clientVertex, err, false)
		return "", err
	}
	return key.(digest.Digest), nil
}

func (s *sharedOp) CacheMap(ctx context.Context, index int) (*cacheMapResp, error) {
	op, err := s.getOp()
	if err != nil {
		return nil, err
	}
	res, err := s.g.Do(ctx, "cachemap", func(ctx context.Context) (ret interface{}, retErr error) {
		if s.cacheRes != nil && s.cacheDone || index < len(s.cacheRes) {
			return s.cacheRes, nil
		}
		if s.cacheErr != nil {
			return nil, s.cacheErr
		}
		ctx = opentracing.ContextWithSpan(progress.WithProgress(ctx, s.st.mpw), s.st.mspan)
		ctx = session.NewContext(ctx, s.st.getSessionID())
		if len(s.st.vtx.Inputs()) == 0 {
			// no cache hit. start evaluating the node
			span, ctx := tracing.StartSpan(ctx, "cache request: "+s.st.vtx.Name())
			notifyStarted(ctx, &s.st.clientVertex, false)
			defer func() {
				tracing.FinishWithError(span, retErr)
				notifyCompleted(ctx, &s.st.clientVertex, retErr, false)
			}()
		}
		res, done, err := op.CacheMap(ctx, len(s.cacheRes))
		complete := true
		if err != nil {
			select {
			case <-ctx.Done():
				if strings.Contains(err.Error(), context.Canceled.Error()) {
					complete = false
					err = errors.Wrap(ctx.Err(), err.Error())
				}
			default:
			}
		}
		if complete {
			if err == nil {
				s.cacheRes = append(s.cacheRes, res)
				s.cacheDone = done
			}
			s.cacheErr = err
		}
		return s.cacheRes, err
	})
	if err != nil {
		return nil, err
	}

	if len(res.([]*CacheMap)) <= index {
		return s.CacheMap(ctx, index)
	}

	return &cacheMapResp{CacheMap: res.([]*CacheMap)[index], complete: s.cacheDone}, nil
}

func (s *sharedOp) Exec(ctx context.Context, inputs []Result) (outputs []Result, exporters []ExportableCacheKey, err error) {
	op, err := s.getOp()
	if err != nil {
		return nil, nil, err
	}
	res, err := s.g.Do(ctx, "exec", func(ctx context.Context) (ret interface{}, retErr error) {
		if s.execRes != nil || s.execErr != nil {
			return s.execRes, s.execErr
		}

		ctx = opentracing.ContextWithSpan(progress.WithProgress(ctx, s.st.mpw), s.st.mspan)
		ctx = session.NewContext(ctx, s.st.getSessionID())

		// no cache hit. start evaluating the node
		span, ctx := tracing.StartSpan(ctx, s.st.vtx.Name())
		notifyStarted(ctx, &s.st.clientVertex, false)
		defer func() {
			tracing.FinishWithError(span, retErr)
			notifyCompleted(ctx, &s.st.clientVertex, retErr, false)
		}()

		res, err := op.Exec(ctx, inputs)
		complete := true
		if err != nil {
			select {
			case <-ctx.Done():
				if strings.Contains(err.Error(), context.Canceled.Error()) {
					complete = false
					err = errors.Wrap(ctx.Err(), err.Error())
				}
			default:
			}
		}
		if complete {
			if res != nil {
				var subExporters []ExportableCacheKey
				s.subBuilder.mu.Lock()
				if len(s.subBuilder.exporters) > 0 {
					subExporters = append(subExporters, s.subBuilder.exporters...)
				}
				s.subBuilder.mu.Unlock()

				s.execRes = &execRes{execRes: wrapShared(res), execExporters: subExporters}
			}
			s.execErr = err
		}
		return s.execRes, err
	})
	if err != nil {
		return nil, nil, err
	}
	r := res.(*execRes)
	return unwrapShared(r.execRes), r.execExporters, nil
}

func (s *sharedOp) getOp() (Op, error) {
	s.opOnce.Do(func() {
		s.subBuilder = s.st.builder()
		s.op, s.err = s.resolver(s.st.vtx, s.subBuilder)
	})
	if s.err != nil {
		return nil, s.err
	}
	return s.op, nil
}

func (s *sharedOp) release() {
	if s.execRes != nil {
		for _, r := range s.execRes.execRes {
			go r.Release(context.TODO())
		}
	}
}

func initClientVertex(v Vertex) client.Vertex {
	inputDigests := make([]digest.Digest, 0, len(v.Inputs()))
	for _, inp := range v.Inputs() {
		inputDigests = append(inputDigests, inp.Vertex.Digest())
	}
	return client.Vertex{
		Inputs: inputDigests,
		Name:   v.Name(),
		Digest: v.Digest(),
	}
}

func wrapShared(inp []Result) []*SharedResult {
	out := make([]*SharedResult, len(inp))
	for i, r := range inp {
		out[i] = NewSharedResult(r)
	}
	return out
}

func unwrapShared(inp []*SharedResult) []Result {
	out := make([]Result, len(inp))
	for i, r := range inp {
		out[i] = r.Clone()
	}
	return out
}

type vertexWithCacheOptions struct {
	Vertex
	inputs []Edge
	dgst   digest.Digest
}

func (v *vertexWithCacheOptions) Digest() digest.Digest {
	return v.dgst
}

func (v *vertexWithCacheOptions) Inputs() []Edge {
	return v.inputs
}

func notifyStarted(ctx context.Context, v *client.Vertex, cached bool) {
	pw, _, _ := progress.FromContext(ctx)
	defer pw.Close()
	now := time.Now()
	v.Started = &now
	v.Completed = nil
	v.Cached = cached
	pw.Write(v.Digest.String(), *v)
}

func notifyCompleted(ctx context.Context, v *client.Vertex, err error, cached bool) {
	pw, _, _ := progress.FromContext(ctx)
	defer pw.Close()
	now := time.Now()
	if v.Started == nil {
		v.Started = &now
	}
	v.Completed = &now
	v.Cached = cached
	if err != nil {
		v.Error = err.Error()
	}
	pw.Write(v.Digest.String(), *v)
}
