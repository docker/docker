package solver

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/solver/errdefs"
	"github.com/moby/buildkit/solver/llbsolver/compat"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/bkmaps"
	"github.com/moby/buildkit/util/flightcontrol"
	"github.com/moby/buildkit/util/progress"
	"github.com/moby/buildkit/util/progress/controller"
	"github.com/moby/buildkit/util/tracing"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// ResolveOpFunc finds an Op implementation for a Vertex
type ResolveOpFunc func(Vertex, Builder) (Op, error)

type Builder interface {
	Build(ctx context.Context, e Edge) (CachedResultWithProvenance, error)
	InContext(ctx context.Context, f func(ctx context.Context, jobCtx JobContext) error) error
	EachValue(ctx context.Context, key string, fn func(any) error) error
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
	jobs      map[*Job]struct{}
	parents   map[digest.Digest]struct{}
	childVtx  map[digest.Digest]struct{}
	releasers []func() error

	mpw      *progress.MultiWriter
	allPw    map[progress.Writer]struct{}
	allPwMu  sync.Mutex // protects allPw
	mspan    *tracing.MultiSpan
	execSpan trace.Span

	vtx            Vertex
	clientVertex   client.Vertex
	clientVertexMu sync.Mutex    // protects mutable clientVertex fields
	origDigest     digest.Digest // original LLB digest. TODO: probably better to use string ID so this isn't needed

	mu    sync.RWMutex
	op    *sharedOp
	edges map[Index]*edge
	opts  SolverOpt
	index *edgeIndex

	cache     map[string]CacheManager
	mainCache CacheManager
	metadata  VertexMetadata
	solver    *Solver
}

func (s *state) Session() session.Group {
	return s
}

func (s *state) Cleanup(fn func() error) error {
	s.mu.Lock()
	s.releasers = append(s.releasers, fn)
	s.mu.Unlock()
	return nil
}

// cloneUntilRelease borrows the solver's lifetime lock so the state and its
// dependencies stay alive until the clone has been registered for cleanup.
func (s *state) cloneUntilRelease(res Result) Result {
	s.solver.mu.RLock()
	defer s.solver.mu.RUnlock()
	if s.solver.actives[s.vtx.Digest()] != s {
		return nil
	}
	clone := res.Clone()
	s.Cleanup(func() error {
		return clone.Release(context.TODO())
	})
	return clone
}

func (s *state) ResolverCache() ResolverCache {
	return s
}

func (s *state) CompatibilityVersion() (int, error) {
	s.mu.RLock()
	jobs := make([]*Job, 0, len(s.jobs))
	for j := range s.jobs {
		jobs = append(jobs, j)
	}
	s.mu.RUnlock()

	version := 0
	for _, j := range jobs {
		v, err := j.CompatibilityVersion()
		if err != nil {
			return 0, err
		}
		if version == 0 {
			version = v
			continue
		}
		if version != v {
			return 0, errors.Errorf("conflicting compatibility versions in shared solve state: %d != %d", version, v)
		}
	}
	if version == 0 {
		return compat.CompatibilityVersionCurrent, nil
	}
	return version, nil
}

func (s *state) Lock(key any) (values []any, release func(any) error, err error) {
	var rcs []ResolverCache
	s.mu.RLock()
	for j := range s.jobs {
		rcs = append(rcs, j.resolverCache)
	}
	s.mu.RUnlock()

	return combinedResolverCache(rcs).Lock(key)
}

func (s *state) SessionIterator() session.Iterator {
	return s.sessionIterator()
}

func (s *state) sessionIterator() *sessionGroup {
	return &sessionGroup{state: s, visited: map[string]struct{}{}}
}

type sessionGroup struct {
	*state
	visited map[string]struct{}
	parents []session.Iterator
	mode    int
}

func (g *sessionGroup) NextSession() string {
	if g.mode == 0 {
		g.mu.Lock()
		for j := range g.jobs {
			if j.SessionID != "" {
				if _, ok := g.visited[j.SessionID]; ok {
					continue
				}
				g.visited[j.SessionID] = struct{}{}
				g.mu.Unlock()
				return j.SessionID
			}
		}
		g.mu.Unlock()
		g.mode = 1
	}
	if g.mode == 1 {
		parents := map[digest.Digest]struct{}{}
		g.mu.Lock()
		for p := range g.state.parents {
			parents[p] = struct{}{}
		}
		g.mu.Unlock()

		for p := range parents {
			g.solver.mu.Lock()
			pst, ok := g.solver.actives[p]
			g.solver.mu.Unlock()
			if ok {
				gg := pst.sessionIterator()
				gg.visited = g.visited
				g.parents = append(g.parents, gg)
			}
		}
		g.mode = 2
	}

	for {
		if len(g.parents) == 0 {
			return ""
		}
		p := g.parents[0]
		id := p.NextSession()
		if id != "" {
			return id
		}
		g.parents = g.parents[1:]
	}
}

func (s *state) builder() *subBuilder {
	return &subBuilder{state: s}
}

func (s *state) getEdge(index Index) *edge {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.edges[index]; ok {
		for e.owner != nil {
			e = e.owner
		}
		return e
	}

	if s.op == nil {
		s.op = newSharedOp(s.opts.ResolveOpFunc, s)
	}

	e := newEdge(Edge{Index: index, Vertex: s.vtx}, s.op, s.index)
	s.edges[index] = e
	return e
}

func (s *state) setEdge(index Index, targetEdge *edge, targetState *state) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.edges[index]
	if ok {
		for e.owner != nil {
			e = e.owner
		}
		if e == targetEdge {
			return
		}
	} else {
		e = newEdge(Edge{Index: index, Vertex: s.vtx}, s.op, s.index)
		s.edges[index] = e
	}
	targetEdge.takeOwnership(e)

	if targetState != nil {
		targetState.addJobs(s, map[*state]struct{}{})
		targetState.mu.Lock()
		targetState.releasers = append(targetState.releasers, s.releasers...)
		targetState.mu.Unlock()
		s.releasers = nil

		targetState.allPwMu.Lock()
		if _, ok := targetState.allPw[s.mpw]; !ok {
			targetState.mpw.Add(s.mpw)
			targetState.allPw[s.mpw] = struct{}{}
		}
		targetState.allPwMu.Unlock()
	}
}

// addJobs recursively adds jobs to state and all its ancestors. currently
// only used during edge merges to add jobs from the source of the merge to the
// target and its ancestors.
// requires that Solver.mu is read-locked and srcState.mu is locked
func (s *state) addJobs(srcState *state, memo map[*state]struct{}) {
	if _, ok := memo[s]; ok {
		return
	}
	memo[s] = struct{}{}

	s.mu.Lock()
	defer s.mu.Unlock()

	for j := range srcState.jobs {
		s.jobs[j] = struct{}{}
	}

	for _, inputEdge := range s.vtx.Inputs() {
		inputState, ok := s.solver.actives[inputEdge.Vertex.Digest()]
		if !ok {
			bklog.G(context.TODO()).
				WithField("vertex_digest", inputEdge.Vertex.Digest()).
				Error("input vertex not found during addJobs")
			continue
		}
		inputState.addJobs(srcState, memo)

		// tricky case: if the inputState's edge was *already* merged we should
		// also add jobs to the merged edge's state
		mergedInputEdge := inputState.getEdge(inputEdge.Index)
		if mergedInputEdge == nil || mergedInputEdge.edge.Vertex.Digest() == inputEdge.Vertex.Digest() {
			// not merged
			continue
		}
		mergedInputState, ok := s.solver.actives[mergedInputEdge.edge.Vertex.Digest()]
		if !ok {
			bklog.G(context.TODO()).
				WithField("vertex_digest", mergedInputEdge.edge.Vertex.Digest()).
				Error("merged input vertex not found during addJobs")
			continue
		}
		mergedInputState.addJobs(srcState, memo)
	}
}

func (s *state) combinedCacheManager() CacheManager {
	s.mu.RLock()
	cms := make([]CacheManager, 0, len(s.cache)+1)
	mainCache := s.mainCache
	cms = append(cms, mainCache)
	for _, cm := range s.cache {
		cms = append(cms, cm)
	}
	s.mu.RUnlock()

	if len(cms) == 1 {
		return mainCache
	}

	return NewCombinedCacheManager(cms, mainCache)
}

// writeVertexSnapshot can read progress concurrently with
// notifyStarted, which is the only writer of the mutable vertex fields.
func (s *state) writeVertexSnapshot(pw progress.Writer) {
	s.clientVertexMu.Lock()
	defer s.clientVertexMu.Unlock()
	// Keep the snapshot and write together so a newer update cannot be
	// published before this snapshot.
	pw.Write(identity.NewID(), s.clientVertex)
}

// vertexFailed lets the ancestor walk read the error concurrently with
// progress updates from notifyStarted.
func (s *state) vertexFailed() bool {
	s.clientVertexMu.Lock()
	defer s.clientVertexMu.Unlock()
	return s.clientVertex.Error != ""
}

func (s *state) notifyStarted(ctx context.Context, cached bool) func(err error, cached bool) {
	pw, _, _ := progress.NewFromContext(ctx)
	s.clientVertexMu.Lock()
	v := &s.clientVertex
	start := time.Now()
	v.Started = &start
	v.Completed = nil
	v.Cached = cached
	id := identity.NewID()
	pw.Write(id, *v)
	s.clientVertexMu.Unlock()
	return func(err error, cached bool) {
		defer pw.Close()
		s.clientVertexMu.Lock()
		defer s.clientVertexMu.Unlock()
		stop := time.Now()
		v.Completed = &stop
		v.Cached = cached
		if err != nil {
			v.Error = err.Error()
		} else {
			v.Error = ""
		}
		pw.Write(id, *v)
	}
}

func (s *state) Release() {
	for _, e := range s.edges {
		for e.owner != nil {
			e = e.owner
		}
		e.release()
	}
	if s.op != nil {
		s.op.release()
	}
	for _, r := range s.releasers {
		if err := r(); err != nil {
			bklog.G(context.TODO()).WithError(err).Error("failed to cleanup job resources")
		}
	}
}

type subBuilder struct {
	*state
	mu        sync.Mutex
	exporters []ExportableCacheKey
}

func (sb *subBuilder) Build(ctx context.Context, e Edge) (CachedResultWithProvenance, error) {
	res, err := sb.solver.subBuild(ctx, e, sb.vtx)
	if err != nil {
		return nil, err
	}
	sb.mu.Lock()
	sb.exporters = append(sb.exporters, res.CacheKeys()[0]) // all keys already have full export chain
	sb.mu.Unlock()
	return &withProvenance{CachedResult: res}, nil
}

func (sb *subBuilder) InContext(ctx context.Context, f func(context.Context, JobContext) error) error {
	ctx = progress.WithProgress(ctx, sb.mpw)
	if sb.mspan.Span != nil {
		ctx = trace.ContextWithSpan(ctx, sb.mspan)
	}
	return f(ctx, sb.state)
}

func (sb *subBuilder) EachValue(ctx context.Context, key string, fn func(any) error) error {
	sb.state.mu.RLock()
	jobs := make([]*Job, 0, len(sb.jobs))
	for j := range sb.jobs {
		jobs = append(jobs, j)
	}
	sb.state.mu.RUnlock()

	for _, j := range jobs {
		if err := j.EachValue(ctx, key, fn); err != nil {
			return err
		}
	}
	return nil
}

type Job struct {
	mu            sync.Mutex // protects completedTime, pw, span, builds, discarded
	builds        int
	discarded     bool
	list          *Solver
	pr            *progress.MultiReader
	pw            progress.Writer
	span          trace.Span
	values        bkmaps.SyncMap[string, any]
	id            string
	startedTime   time.Time
	completedTime time.Time
	releasers     []func() error
	resolverCache *resolverCache

	progressCloser func(error)
	SessionID      string
	uniqueID       string // unique ID is used for provenance. We use a different field that client can't control
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

// hasOwner returns true if the provided target edge (or any of it's sibling
// edges) has the provided owner.
func (jl *Solver) hasOwner(target Edge, owner Edge) bool {
	jl.mu.RLock()
	defer jl.mu.RUnlock()

	st, ok := jl.actives[target.Vertex.Digest()]
	if !ok {
		return false
	}

	var owners []Edge
	for _, e := range st.edges {
		if e.owner != nil {
			owners = append(owners, e.owner.edge)
		}
	}
	for len(owners) > 0 {
		var owners2 []Edge
		for _, e := range owners {
			st, ok = jl.actives[e.Vertex.Digest()]
			if !ok {
				continue
			}

			if st.vtx.Digest() == owner.Vertex.Digest() {
				return true
			}

			for _, e := range st.edges {
				if e.owner != nil {
					owners2 = append(owners2, e.owner.edge)
				}
			}
		}

		// repeat recursively, this time with the linked owners owners
		owners = owners2
	}

	return false
}

func (jl *Solver) setEdge(e Edge, targetEdge *edge) {
	jl.mu.RLock()
	defer jl.mu.RUnlock()

	st, ok := jl.actives[e.Vertex.Digest()]
	if !ok {
		return
	}

	// potentially passing nil targetSt is intentional and handled in st.setEdge
	targetSt := jl.actives[targetEdge.edge.Vertex.Digest()]

	st.setEdge(e.Index, targetEdge, targetSt)
}

func (jl *Solver) getState(e Edge) *state {
	jl.mu.RLock()
	defer jl.mu.RUnlock()

	st, ok := jl.actives[e.Vertex.Digest()]
	if !ok {
		return nil
	}
	return st
}

func (jl *Solver) getEdge(e Edge) (redge *edge) {
	if debugScheduler {
		defer func() {
			lg := bklog.G(context.TODO()).
				WithField("edge_vertex_name", e.Vertex.Name()).
				WithField("edge_vertex_digest", e.Vertex.Digest()).
				WithField("edge_index", e.Index)
			if redge != nil {
				lg = lg.
					WithField("return_edge_vertex_name", redge.edge.Vertex.Name()).
					WithField("return_edge_vertex_digest", redge.edge.Vertex.Digest()).
					WithField("return_edge_index", redge.edge.Index)
			}
			lg.Debug("getEdge return")
		}()
	}

	jl.mu.RLock()
	defer jl.mu.RUnlock()

	st, ok := jl.actives[e.Vertex.Digest()]
	if !ok {
		return nil
	}
	return st.getEdge(e.Index)
}

func (jl *Solver) subBuild(ctx context.Context, e Edge, parent Vertex) (CachedResult, error) {
	v, err := jl.load(ctx, e.Vertex, parent, nil)
	if err != nil {
		return nil, err
	}
	e.Vertex = v
	return jl.s.build(ctx, e)
}

func (jl *Solver) Close() {
	jl.s.Stop()
}

func (jl *Solver) load(ctx context.Context, v, parent Vertex, j *Job) (Vertex, error) {
	jl.mu.Lock()
	defer jl.mu.Unlock()

	cache := map[Vertex]Vertex{}

	return jl.loadUnlocked(ctx, v, parent, j, cache)
}

// called with solver lock
func (jl *Solver) loadUnlocked(ctx context.Context, v, parent Vertex, j *Job, cache map[Vertex]Vertex) (Vertex, error) {
	if v, ok := cache[v]; ok {
		return v, nil
	}
	origVtx := v

	inputs := make([]Edge, len(v.Inputs()))
	for i, e := range v.Inputs() {
		v, err := jl.loadUnlocked(ctx, e.Vertex, parent, j, cache)
		if err != nil {
			return nil, err
		}
		inputs[i] = Edge{Index: e.Index, Vertex: v}
	}

	dgst := v.Digest()

	dgstWithoutCache := digest.FromBytes(fmt.Appendf(nil, "%s-ignorecache", dgst))

	// if same vertex is already loaded without cache just use that
	st, ok := jl.actives[dgstWithoutCache]

	if ok {
		// When matching an existing active vertext by dgstWithoutCache, set v to the
		// existing active vertex, as otherwise the original vertex will use an
		// incorrect digest and can incorrectly delete it while it is still in use.
		v = st.vtx
	}

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
			mpw:          progress.NewMultiWriter(progress.WithMetadata("vertex", dgst)),
			mspan:        tracing.NewMultiSpan(),
			vtx:          v,
			clientVertex: initClientVertex(v),
			edges:        map[Index]*edge{},
			index:        jl.index,
			mainCache:    jl.opts.DefaultCache,
			cache:        map[string]CacheManager{},
			solver:       jl,
			origDigest:   origVtx.Digest(),
		}
		jl.actives[dgst] = st

		if debugScheduler {
			lg := bklog.G(ctx).
				WithField("vertex_name", v.Name()).
				WithField("vertex_digest", v.Digest()).
				WithField("actives_digest_key", dgst)
			if j != nil {
				lg = lg.WithField("job", j.id)
			}
			lg.Debug("adding active vertex")
			for i, inp := range v.Inputs() {
				lg.WithField("input_index", i).
					WithField("input_vertex_name", inp.Vertex.Name()).
					WithField("input_vertex_digest", inp.Vertex.Digest()).
					WithField("input_edge_index", inp.Index).
					Debug("new active vertex input")
			}
		}
	} else if debugScheduler {
		lg := bklog.G(ctx).
			WithField("vertex_name", v.Name()).
			WithField("vertex_digest", v.Digest()).
			WithField("actives_digest_key", dgst)
		if j != nil {
			lg = lg.WithField("job", j.id)
		}
		lg.Debug("reusing active vertex")
	}

	st.mu.Lock()
	for _, cache := range v.Options().CacheSources {
		if cache.ID() != st.mainCache.ID() {
			if _, ok := st.cache[cache.ID()]; !ok {
				st.cache[cache.ID()] = cache
			}
		}
	}
	if md := v.Options().Metadata; md != nil {
		if st.metadata == nil {
			st.metadata = md
		} else {
			st.metadata = st.metadata.Merge(md)
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

			maps.Copy(st.cache, parentState.cache)
		}
	}

	jl.connectProgressFromState(st, st)
	cache[origVtx] = v
	return v, nil
}

func (jl *Solver) connectProgressFromState(target, src *state) {
	for j := range src.jobs {
		j.mu.Lock()
		pw := j.pw
		span := j.span
		j.mu.Unlock()
		target.allPwMu.Lock()
		if _, ok := target.allPw[pw]; !ok {
			target.mpw.Add(pw)
			target.allPw[pw] = struct{}{}
			target.writeVertexSnapshot(pw)
			if span != nil && span.SpanContext().IsValid() {
				target.mspan.Add(span)
			}
		}
		target.allPwMu.Unlock()
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
	pw, _, _ := progress.NewFromContext(ctx) // TODO: expose progress.Pipe()

	_, span := noop.NewTracerProvider().Tracer("").Start(ctx, "")
	j := &Job{
		list:           jl,
		pr:             progress.NewMultiReader(pr),
		pw:             pw,
		progressCloser: progressCloser,
		span:           span,
		id:             id,
		startedTime:    time.Now(),
		uniqueID:       identity.NewID(),
		resolverCache:  newResolverCache(),
	}
	jl.jobs[id] = j

	jl.updateCond.Broadcast()

	return j, nil
}

func (jl *Solver) Get(id string) (*Job, error) {
	ctx, cancel := context.WithCancelCause(context.Background())
	ctx, _ = context.WithTimeoutCause(ctx, 6*time.Second, errors.WithStack(context.DeadlineExceeded)) //nolint:govet
	defer func() { cancel(errors.WithStack(context.Canceled)) }()

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
			return nil, errdefs.NewUnknownJobError(id)
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
		if debugScheduler {
			bklog.G(context.TODO()).
				WithField("vertex_name", st.vtx.Name()).
				WithField("vertex_digest", st.vtx.Digest()).
				WithField("actives_key", k).
				Debug("deleting unreferenced active vertex")
			for _, e := range st.edges {
				bklog.G(context.TODO()).
					WithField("vertex_name", e.edge.Vertex.Name()).
					WithField("vertex_digest", e.edge.Vertex.Digest()).
					WithField("index", e.edge.Index).
					WithField("state", e.state).
					Debug("edge in deleted unreferenced state")
			}
		}
		for chKey := range st.childVtx {
			chState := jl.actives[chKey]
			delete(chState.parents, k)
			jl.deleteIfUnreferenced(chKey, chState)
		}
		st.Release()
		delete(jl.actives, k)
	} else if debugScheduler {
		var jobIDs []string
		for j := range st.jobs {
			jobIDs = append(jobIDs, j.id)
		}
		bklog.G(context.TODO()).
			WithField("vertex_name", st.vtx.Name()).
			WithField("vertex_digest", st.vtx.Digest()).
			WithField("actives_key", k).
			WithField("jobs", jobIDs).
			Debug("not deleting referenced active vertex")
	}
}

func (j *Job) Build(ctx context.Context, e Edge) (CachedResultWithProvenance, error) {
	j.mu.Lock()
	if j.discarded {
		j.mu.Unlock()
		return nil, errors.Errorf("build on discarded job %s", j.id)
	}
	j.builds++
	if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		j.span = span
	}
	j.mu.Unlock()
	defer j.buildDone()

	v, err := j.list.load(ctx, e.Vertex, nil, j)
	if err != nil {
		return nil, err
	}
	e.Vertex = v

	res, err := j.list.s.build(ctx, e)
	if err != nil {
		return nil, err
	}

	return &withProvenance{CachedResult: res, j: j, e: e}, nil
}

type withProvenance struct {
	CachedResult
	j *Job
	e Edge
}

func (wp *withProvenance) WalkProvenance(ctx context.Context, f func(ProvenanceProvider) error) error {
	if wp.j == nil {
		return nil
	}
	wp.j.list.mu.RLock()
	defer wp.j.list.mu.RUnlock()
	m := map[digest.Digest]struct{}{}
	return wp.j.walkProvenance(ctx, wp.e, f, m)
}

// called with solver lock
func (j *Job) walkProvenance(ctx context.Context, e Edge, f func(ProvenanceProvider) error, visited map[digest.Digest]struct{}) error {
	if _, ok := visited[e.Vertex.Digest()]; ok {
		return nil
	}
	visited[e.Vertex.Digest()] = struct{}{}
	// Walk via the resolved state's inputs when available, so the recursion
	// follows the chain that was actually scheduled rather than the caller's
	// wrapper graph (which can reference orphan states via the
	// dgstWithoutCache shift in loadUnlocked).
	inputs := e.Vertex.Inputs()
	if st, ok := j.list.actives[e.Vertex.Digest()]; ok {
		st.mu.Lock()
		if st.op != nil && st.op.op != nil {
			if wp, ok := st.op.op.(ProvenanceProvider); ok {
				if err := f(wp); err != nil {
					st.mu.Unlock()
					return err
				}
			}
		}
		inputs = st.vtx.Inputs()
		st.mu.Unlock()
	}
	for _, inp := range inputs {
		if err := j.walkProvenance(ctx, inp, f, visited); err != nil {
			return err
		}
	}
	return nil
}

func (j *Job) CloseProgress() {
	j.progressCloser(errors.WithStack(context.Canceled))
	j.pw.Close()
}

// buildDone finishes a pending Discard when the last running build returns.
func (j *Job) buildDone() {
	j.mu.Lock()
	j.builds--
	last := j.discarded && j.builds == 0
	j.mu.Unlock()
	if last {
		j.discard()
	}
}

// Discard releases the job's states. While builds are still running the states
// are released when the last one returns, so results that complete after
// Discard are released with their state.
func (j *Job) Discard() error {
	j.mu.Lock()
	j.discarded = true
	running := j.builds > 0
	j.mu.Unlock()
	if running {
		return nil
	}
	return j.discard()
}

func (j *Job) discard() error {
	j.list.mu.Lock()
	defer j.list.mu.Unlock()

	j.pw.Close()

	for k, st := range j.list.actives {
		st.mu.Lock()
		if _, ok := st.jobs[j]; ok {
			if debugScheduler {
				bklog.G(context.TODO()).
					WithField("job", j.id).
					WithField("vertex_name", st.vtx.Name()).
					WithField("vertex_digest", st.vtx.Digest()).
					WithField("actives_key", k).
					Debug("deleting job from state")
			}
			delete(st.jobs, j)
			j.list.deleteIfUnreferenced(k, st)
		}
		delete(st.allPw, j.pw)
		st.mu.Unlock()
	}

	for _, r := range j.releasers {
		if err := r(); err != nil {
			bklog.G(context.TODO()).WithError(err).Error("failed to cleanup job resources")
		}
	}
	j.releasers = nil

	go func() {
		// don't clear job right away. there might still be a status request coming to read progress
		time.Sleep(10 * time.Second)
		j.list.mu.Lock()
		defer j.list.mu.Unlock()
		delete(j.list.jobs, j.id)
	}()
	return nil
}

func (j *Job) StartedTime() time.Time {
	return j.startedTime
}

func (j *Job) RegisterCompleteTime() time.Time {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.completedTime.IsZero() {
		j.completedTime = time.Now()
	}
	return j.completedTime
}

func (j *Job) UniqueID() string {
	return j.uniqueID
}

func (j *Job) CompatibilityVersion() (int, error) {
	version := compat.CompatibilityVersionCurrent
	if err := j.EachValue(context.TODO(), compat.JobValueKey, func(v any) error {
		parsed, ok := v.(int)
		if !ok {
			return errors.Errorf("invalid compatibility version %T", v)
		}
		version = parsed
		return nil
	}); err != nil {
		return 0, err
	}
	return version, nil
}

func (j *Job) InContext(ctx context.Context, f func(context.Context, JobContext) error) error {
	return f(progress.WithProgress(ctx, j.pw), j)
}

func (j *Job) Session() session.Group {
	return session.NewGroup(j.SessionID)
}

func (j *Job) Cleanup(fn func() error) error {
	j.mu.Lock()
	j.releasers = append(j.releasers, fn)
	j.mu.Unlock()
	return nil
}

func (j *Job) ResolverCache() ResolverCache {
	return j.resolverCache
}

func (j *Job) SetValue(key string, v any) {
	j.values.Store(key, v)
}

func (j *Job) EachValue(ctx context.Context, key string, fn func(any) error) error {
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
	LoadCache(ctx context.Context, rec *CacheRecord) (Result, func(context.Context) context.Context, error)
	Exec(ctx context.Context, inputs []Result) (outputs []Result, exporters []ExportableCacheKey, ctxOpts func(context.Context) context.Context, err error)
	IgnoreCache() bool
	Cache() CacheManager
	CalcSlowCache(context.Context, Index, PreprocessFunc, ResultBasedCacheFunc, Result) (digest.Digest, error)
}

func newSharedOp(resolver ResolveOpFunc, st *state) *sharedOp {
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
	resolver  ResolveOpFunc
	st        *state
	gDigest   flightcontrol.Group[digest.Digest]
	gCacheRes flightcontrol.Group[[]*CacheMap]
	gExecRes  flightcontrol.Group[*execRes]

	opOnce     sync.Once
	op         Op
	subBuilder *subBuilder
	err        error

	execRes  *execRes
	execDone bool
	execErr  error

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
	return &cacheWithCacheOpts{s.st.combinedCacheManager(), s.st}
}

type cacheWithCacheOpts struct {
	CacheManager
	st *state
}

func (c cacheWithCacheOpts) Records(ctx context.Context, ck *CacheKey) ([]*CacheRecord, error) {
	// Allow Records accessing to cache opts through ctx. This enable to use remote provider
	// during checking the cache existence.
	return c.CacheManager.Records(withAncestorCacheOpts(ctx, c.st), ck)
}

func (s *sharedOp) LoadCache(ctx context.Context, rec *CacheRecord) (Result, func(context.Context) context.Context, error) {
	ctx = progress.WithProgress(ctx, s.st.mpw)
	if s.st.mspan.Span != nil {
		ctx = trace.ContextWithSpan(ctx, s.st.mspan)
	}
	// no cache hit. start evaluating the node
	span, ctx := tracing.StartSpan(ctx, "load cache: "+s.st.vtx.Name(), trace.WithAttributes(attribute.String("vertex", s.st.vtx.Digest().String())))
	s.st.execSpan = span
	notifyCompleted := s.st.notifyStarted(ctx, true)
	res, err := s.Cache().Load(withAncestorCacheOpts(ctx, s.st), rec)
	tracing.FinishWithError(span, err)
	notifyCompleted(err, true)
	return res, func(ctx context.Context) context.Context {
		return withAncestorCacheOpts(ctx, s.st)
	}, err
}

// CalcSlowCache computes the digest of an input that is ready and has been
// evaluated, hence "slow" cache.
func (s *sharedOp) CalcSlowCache(ctx context.Context, index Index, p PreprocessFunc, f ResultBasedCacheFunc, res Result) (dgst digest.Digest, err error) {
	defer func() {
		err = errdefs.WithOp(err, s.st.vtx.Sys(), s.st.vtx.Options().Description)
		err = errdefs.WrapVertex(err, s.st.origDigest)
	}()
	flightControlKey := fmt.Sprintf("slow-compute-%d", index)
	key, err := s.gDigest.Do(ctx, flightControlKey, func(ctx context.Context) (digest.Digest, error) {
		s.slowMu.Lock()
		// TODO: add helpers for these stored values
		if res, ok := s.slowCacheRes[index]; ok {
			s.slowMu.Unlock()
			return res, nil
		}
		if err := s.slowCacheErr[index]; err != nil {
			s.slowMu.Unlock()
			return "", err
		}
		s.slowMu.Unlock()

		complete := true
		if p != nil {
			st := s.st.solver.getState(s.st.vtx.Inputs()[index])
			if st == nil {
				return "", errors.Errorf("failed to get state for index %d on %v", index, s.st.vtx.Name())
			}
			ctx2 := progress.WithProgress(ctx, st.mpw)
			if st.execSpan != nil {
				ctx2 = trace.ContextWithSpan(ctx2, st.execSpan)
			} else if st.mspan.Span != nil {
				ctx2 = trace.ContextWithSpan(ctx2, st.mspan)
			}
			err = p(ctx2, res, st)
			if err != nil {
				f = nil
				ctx = ctx2
			}
		}

		var key digest.Digest
		if f != nil {
			ctx = progress.WithProgress(ctx, s.st.mpw)
			if s.st.mspan.Span != nil {
				ctx = trace.ContextWithSpan(ctx, s.st.mspan)
			}
			key, err = f(withAncestorCacheOpts(ctx, s.st), res, s.st)
		}
		if err != nil {
			select {
			case <-ctx.Done():
				if errdefs.IsCanceled(ctx, err) {
					complete = false
					releaseError(err)
					err = errors.Wrap(context.Cause(ctx), err.Error())
				}
			default:
			}
		}
		s.slowMu.Lock()
		defer s.slowMu.Unlock()
		if complete {
			if err == nil {
				s.slowCacheRes[index] = key
			} else if errResult := s.st.cloneUntilRelease(res); errResult != nil {
				// The error is memoized, so the input is cloned only once and
				// kept available until the solver state is discarded.
				err = WrapSlowCache(err, index, errResult)
			}
			s.slowCacheErr[index] = err
		}
		return key, err
	})
	if err != nil {
		ctx = progress.WithProgress(ctx, s.st.mpw)
		if s.st.mspan.Span != nil {
			ctx = trace.ContextWithSpan(ctx, s.st.mspan)
		}
		notifyCompleted := s.st.notifyStarted(ctx, false)
		notifyCompleted(err, false)
		return "", err
	}
	return key, nil
}

func (s *sharedOp) CacheMap(ctx context.Context, index int) (resp *cacheMapResp, err error) {
	defer func() {
		err = errdefs.WithOp(err, s.st.vtx.Sys(), s.st.vtx.Options().Description)
		err = errdefs.WrapVertex(err, s.st.origDigest)
	}()
	op, err := s.getOp()
	if err != nil {
		return nil, err
	}
	flightControlKey := fmt.Sprintf("cachemap-%d", index)
	res, err := s.gCacheRes.Do(ctx, flightControlKey, func(ctx context.Context) (ret []*CacheMap, retErr error) {
		if s.cacheRes != nil && s.cacheDone || index < len(s.cacheRes) {
			return s.cacheRes, nil
		}
		if s.cacheErr != nil {
			return nil, s.cacheErr
		}
		ctx = progress.WithProgress(ctx, s.st.mpw)
		if s.st.mspan.Span != nil {
			ctx = trace.ContextWithSpan(ctx, s.st.mspan)
		}
		ctx = withAncestorCacheOpts(ctx, s.st)
		if len(s.st.vtx.Inputs()) == 0 {
			// no cache hit. start evaluating the node
			span, ctx := tracing.StartSpan(ctx, "cache request: "+s.st.vtx.Name(), trace.WithAttributes(attribute.String("vertex", s.st.vtx.Digest().String())))
			notifyCompleted := s.st.notifyStarted(ctx, false)
			defer func() {
				tracing.FinishWithError(span, retErr)
				notifyCompleted(retErr, false)
			}()
		}
		res, done, err := op.CacheMap(ctx, s.st, len(s.cacheRes))
		if err == nil && res == nil {
			err = errors.New("invalid nil cache map")
		}
		if err == nil && len(res.Deps) != len(s.st.vtx.Inputs()) {
			err = errors.Errorf("invalid cache map: expected dependency count %d, got %d", len(s.st.vtx.Inputs()), len(res.Deps))
		}
		complete := true
		if err != nil {
			select {
			case <-ctx.Done():
				if errdefs.IsCanceled(ctx, err) {
					complete = false
					releaseError(err)
					err = errors.Wrap(context.Cause(ctx), err.Error())
				}
			default:
			}
		}
		if complete {
			if err == nil {
				if res.Opts == nil {
					res.Opts = CacheOpts(make(map[any]any))
				}
				res.Opts[progressKey{}] = &controller.Controller{
					WriterFactory: progress.FromContext(ctx),
					Digest:        s.st.vtx.Digest(),
					Name:          s.st.vtx.Name(),
					ProgressGroup: s.st.vtx.Options().ProgressGroup,
				}
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

	if len(res) <= index {
		return s.CacheMap(ctx, index)
	}

	return &cacheMapResp{CacheMap: res[index], complete: s.cacheDone}, nil
}

func (s *sharedOp) Exec(ctx context.Context, inputs []Result) (outputs []Result, exporters []ExportableCacheKey, ctxOpts func(context.Context) context.Context, err error) {
	defer func() {
		err = errdefs.WithOp(err, s.st.vtx.Sys(), s.st.vtx.Options().Description)
		err = errdefs.WrapVertex(err, s.st.origDigest)
	}()
	op, err := s.getOp()
	if err != nil {
		return nil, nil, nil, err
	}
	flightControlKey := "exec"
	res, err := s.gExecRes.Do(ctx, flightControlKey, func(ctx context.Context) (ret *execRes, retErr error) {
		if s.execDone {
			if s.execErr != nil {
				return nil, s.execErr
			}
			return s.execRes, nil
		}
		release, err := op.Acquire(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "acquire op resources")
		}
		defer release()

		ctx = progress.WithProgress(ctx, s.st.mpw)
		if s.st.mspan.Span != nil {
			ctx = trace.ContextWithSpan(ctx, s.st.mspan)
		}
		ctx = withAncestorCacheOpts(ctx, s.st)

		// no cache hit. start evaluating the node
		span, ctx := tracing.StartSpan(ctx, s.st.vtx.Name(), trace.WithAttributes(attribute.String("vertex", s.st.vtx.Digest().String())))
		s.st.execSpan = span
		notifyCompleted := s.st.notifyStarted(ctx, false)
		defer func() {
			tracing.FinishWithError(span, retErr)
			notifyCompleted(retErr, false)
		}()

		res, err := op.Exec(ctx, s.st, inputs)
		complete := true
		if err != nil {
			select {
			case <-ctx.Done():
				if errdefs.IsCanceled(ctx, err) {
					complete = false
					releaseError(err)
					err = errors.Wrap(context.Cause(ctx), err.Error())
				}
			default:
			}
		}
		if complete {
			s.execDone = true
			if res != nil {
				var subExporters []ExportableCacheKey
				s.subBuilder.mu.Lock()
				if len(s.subBuilder.exporters) > 0 {
					subExporters = append(subExporters, s.subBuilder.exporters...)
				}
				s.subBuilder.mu.Unlock()

				s.execRes = &execRes{execRes: wrapShared(res), execExporters: subExporters}
			}
			s.st.cloneErrorRefs(err)
			s.execErr = err
		}
		if s.execRes == nil || err != nil {
			return nil, err
		}
		return s.execRes, nil
	})
	if res == nil || err != nil {
		return nil, nil, nil, err
	}
	return unwrapShared(res.execRes), res.execExporters, func(ctx context.Context) context.Context {
		return withAncestorCacheOpts(ctx, s.st)
	}, nil
}

func (s *sharedOp) getOp() (Op, error) {
	s.opOnce.Do(func() {
		s.subBuilder = s.st.builder()
		s.st.mu.RLock()
		metadata := s.st.metadata
		s.st.mu.RUnlock()
		v := s.st.vtx
		if metadata != nil {
			v = &vertexWithMetadata{Vertex: v, metadata: metadata}
		}
		s.op, s.err = s.resolver(v, s.subBuilder)
	})
	if s.err != nil {
		return nil, s.err
	}
	return s.op, nil
}

type vertexWithMetadata struct {
	Vertex
	metadata VertexMetadata
}

func (v *vertexWithMetadata) Options() VertexOptions {
	opts := v.Vertex.Options()
	opts.Metadata = v.metadata
	return opts
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
		Inputs:        inputDigests,
		Name:          v.Name(),
		Digest:        v.Digest(),
		ProgressGroup: v.Options().ProgressGroup,
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

type SlowCacheError struct {
	error
	Index Index
	// Result is owned by the solver state that produced the error. Callers
	// must not release it and need to Clone it while the job is still active
	// if they want to keep a reference.
	Result Result
}

func (e *SlowCacheError) Unwrap() error {
	return e.error
}

func (e *SlowCacheError) ToSubject() errdefs.IsSolve_Subject {
	return &errdefs.Solve_Cache{
		Cache: &errdefs.ContentCache{
			Index: int64(e.Index),
		},
	}
}

func WrapSlowCache(err error, index Index, res Result) error {
	if err == nil {
		return nil
	}
	return &SlowCacheError{Index: index, Result: res, error: err}
}

// cloneErrorRefs retains a cached error's refs until the state is released.
func (s *state) cloneErrorRefs(err error) {
	for ; err != nil; err = errors.Unwrap(err) {
		if re, ok := err.(interface {
			EachRef(func(Result) error) error
		}); ok {
			re.EachRef(func(r Result) error {
				s.cloneUntilRelease(r)
				return nil
			})
		}
	}
}

func releaseError(err error) {
	if err == nil {
		return
	}
	if re, ok := err.(interface {
		Release() error
	}); ok {
		re.Release()
	}
	releaseError(errors.Unwrap(err))
}
