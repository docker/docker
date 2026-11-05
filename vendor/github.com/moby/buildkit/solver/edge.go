package solver

import (
	"context"
	"sync"
	"time"

	"github.com/moby/buildkit/solver/internal/pipe"
	"github.com/moby/buildkit/util/bklog"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

type edgeStatusType int

const (
	edgeStatusInitial edgeStatusType = iota
	edgeStatusCacheFast
	edgeStatusCacheSlow
	edgeStatusComplete
)

func (t edgeStatusType) String() string {
	return []string{"initial", "cache-fast", "cache-slow", "complete"}[t]
}

func newEdge(ed Edge, op activeOp, index *edgeIndex) *edge {
	e := &edge{
		edge:               ed,
		op:                 op,
		depRequests:        map[pipeReceiver]*dep{},
		keyMap:             map[string]struct{}{},
		cacheRecords:       map[string]*CacheRecord{},
		cacheRecordsLoaded: map[string]struct{}{},
		index:              index,
	}
	e.debug = debugSchedulerCheckEdge(e)
	return e
}

type edge struct {
	edge Edge
	op   activeOp

	edgeState
	depRequests map[pipeReceiver]*dep
	deps        []*dep

	cacheMapReq        pipeReceiver
	cacheMapDone       bool
	cacheMapIndex      int
	cacheMapDigests    []digest.Digest
	execReq            pipeReceiver
	execCacheLoad      bool
	err                error
	cacheRecords       map[string]*CacheRecord
	cacheRecordsLoaded map[string]struct{}
	keyMap             map[string]struct{}

	noCacheMatchPossible      bool
	allDepsCompletedCacheFast bool
	allDepsCompletedCacheSlow bool
	allDepsStateCacheSlow     bool
	allDepsCompleted          bool
	hasActiveOutgoing         bool

	releaserCount int
	owner         *edge
	keysDidChange bool
	index         *edgeIndex

	// secondaryExportersMu protects secondaryExporters, which the scheduler
	// appends to on merge while cache exporters read it from other goroutines
	secondaryExportersMu sync.Mutex
	secondaryExporters   []expDep

	failedOnce sync.Once
	debug      bool
}

// dep holds state for a dependant edge
type dep struct {
	req pipeReceiver
	edgeState
	index             Index
	keyMap            map[string]*CacheKey
	slowCacheReq      pipeReceiver
	slowCacheComplete bool
	slowCacheFoundKey bool
	slowCacheKey      *ExportableCacheKey
	err               error
}

// expDep holds secondary exporter info for dependency
type expDep struct {
	index    int
	cacheKey CacheKeyWithSelector
}

func newDep(i Index) *dep {
	return &dep{index: i, keyMap: map[string]*CacheKey{}}
}

// edgePipe is a pipe for requests between two edges
type edgePipe struct {
	*pipe.Pipe[*edgeRequest, any]
	From, Target *edge
	mu           sync.Mutex
}

// edgeState hold basic mutable state info for an edge
type edgeState struct {
	state    edgeStatusType
	result   *SharedCachedResult
	cacheMap *CacheMap
	keys     []ExportableCacheKey
}

type edgeRequest struct {
	desiredState edgeStatusType
	currentState edgeState
	currentKeys  int
}

// takeOwnership increases the number of times release needs to be
// called to release the edge. Called on merging edges.
func (e *edge) takeOwnership(old *edge) {
	e.releaserCount += old.releaserCount + 1
	old.owner = e
	old.releaseResult()
}

// release releases the edge resources
func (e *edge) release() {
	if e.releaserCount > 0 {
		e.releaserCount--
		return
	}
	e.releaseResult()
}

func (e *edge) releaseResult() {
	e.index.Release(e)
	if e.result != nil {
		go e.result.Release(context.TODO())
	}
}

// commitOptions returns parameters for the op execution
func (e *edge) commitOptions() ([]*CacheKey, []CachedResult) {
	k := NewCacheKey(e.cacheMap.Digest, e.edge.Vertex.Digest(), e.edge.Index)
	if len(e.deps) == 0 {
		keys := make([]*CacheKey, 0, len(e.cacheMapDigests))
		for _, dgst := range e.cacheMapDigests {
			keys = append(keys, NewCacheKey(dgst, e.edge.Vertex.Digest(), e.edge.Index))
		}
		return keys, nil
	}

	inputs := make([][]CacheKeyWithSelector, len(e.deps))
	results := make([]CachedResult, len(e.deps))
	for i, dep := range e.deps {
		for _, k := range dep.result.CacheKeys() {
			inputs[i] = append(inputs[i], CacheKeyWithSelector{CacheKey: k, Selector: e.cacheMap.Deps[i].Selector})
		}
		if dep.slowCacheKey != nil {
			inputs[i] = append(inputs[i], CacheKeyWithSelector{CacheKey: *dep.slowCacheKey})
		}
		results[i] = dep.result
	}

	k.deps = inputs
	return []*CacheKey{k}, results
}

// isComplete returns true if edge state is final and will never change
func (e *edge) isComplete() bool {
	return e.err != nil || e.result != nil
}

// finishIncoming finalizes the incoming pipe request
func (e *edge) finishIncoming(req pipeSender) {
	err := e.err
	if req.Request().Canceled && err == nil {
		err = context.Canceled
	}
	debugSchedulerFinishIncoming(e, err, req)
	req.Finalize(&e.edgeState, err)
}

// updateIncoming updates the current value of incoming pipe request
func (e *edge) updateIncoming(req pipeSender) {
	debugSchedulerUpdateIncoming(e, req)
	req.Update(&e.edgeState)
}

// probeCache is called with unprocessed cache keys for dependency
// if the key could match the edge, the cacheRecords for dependency are filled
func (e *edge) probeCache(d *dep, depKeys []CacheKeyWithSelector) bool {
	if len(depKeys) == 0 {
		return false
	}
	if e.op.IgnoreCache() {
		return false
	}
	keys, err := e.op.Cache().Query(depKeys, d.index, e.cacheMap.Digest, e.edge.Index)
	if err != nil {
		e.err = errors.Wrap(err, "error on cache query")
	}
	found := false
	for _, k := range keys {
		k.vtx = e.edge.Vertex.Digest()
		if _, ok := d.keyMap[k.ID]; !ok {
			d.keyMap[k.ID] = k
			found = true
		}
	}
	return found
}

// checkDepMatchPossible checks if any cache matches are possible past this point
func (e *edge) checkDepMatchPossible(dep *dep) {
	depHasSlowCache := e.cacheMap.Deps[dep.index].ComputeDigestFunc != nil
	if !e.noCacheMatchPossible && (((!dep.slowCacheFoundKey && dep.slowCacheComplete && depHasSlowCache) || (!depHasSlowCache && dep.state >= edgeStatusCacheSlow)) && len(dep.keyMap) == 0) {
		e.noCacheMatchPossible = true
		debugSchedulerNoCacheMatchPossible(e, dep, depHasSlowCache)
	}
}

// slowCacheFunc returns the result based cache func for dependency if it exists
func (e *edge) slowCacheFunc(dep *dep) ResultBasedCacheFunc {
	if e.cacheMap == nil {
		return nil
	}
	return e.cacheMap.Deps[int(dep.index)].ComputeDigestFunc
}

// preprocessFunc returns result based cache func
func (e *edge) preprocessFunc(dep *dep) PreprocessFunc {
	if e.cacheMap == nil {
		return nil
	}
	return e.cacheMap.Deps[int(dep.index)].PreprocessFunc
}

// allDepsHaveKeys checks if all dependencies have at least one key. used for
// determining if there is enough data for combining cache key for edge
func (e *edge) allDepsHaveKeys(matching bool) bool {
	if e.cacheMap == nil {
		return false
	}
	for _, d := range e.deps {
		cond := len(d.keys) == 0
		if matching {
			cond = len(d.keyMap) == 0
		}
		if cond && d.slowCacheKey == nil && d.result == nil {
			return false
		}
	}
	return true
}

// depKeys returns all current dependency cache keys
func (e *edge) currentIndexKey() *CacheKey {
	if e.cacheMap == nil {
		return nil
	}

	keys := make([][]CacheKeyWithSelector, len(e.deps))
	for i, d := range e.deps {
		if len(d.keys) == 0 && d.result == nil {
			return nil
		}
		for _, k := range d.keys {
			keys[i] = append(keys[i], CacheKeyWithSelector{Selector: e.cacheMap.Deps[i].Selector, CacheKey: k})
		}
		if d.result != nil {
			for _, rk := range d.result.CacheKeys() {
				keys[i] = append(keys[i], CacheKeyWithSelector{Selector: e.cacheMap.Deps[i].Selector, CacheKey: rk})
			}
			if d.slowCacheKey != nil {
				keys[i] = append(keys[i], CacheKeyWithSelector{CacheKey: ExportableCacheKey{CacheKey: d.slowCacheKey.CacheKey, Exporter: &exporter{k: d.slowCacheKey.CacheKey}}})
			}
		}
	}

	k := NewCacheKey(e.cacheMap.Digest, e.edge.Vertex.Digest(), e.edge.Index)
	k.deps = keys

	return k
}

// slow cache keys can be computed in 2 phases if there are multiple deps.
// first evaluate ones that didn't match any definition based keys
func (e *edge) skipPhase2SlowCache(dep *dep) bool {
	isPhase1 := false
	for _, dep := range e.deps {
		if (!dep.slowCacheComplete && e.slowCacheFunc(dep) != nil || dep.state < edgeStatusCacheSlow) && len(dep.keyMap) == 0 {
			isPhase1 = true
			break
		}
	}

	if isPhase1 && !dep.slowCacheComplete && e.slowCacheFunc(dep) != nil && len(dep.keyMap) > 0 {
		return true
	}
	return false
}

func (e *edge) skipPhase2FastCache(dep *dep) bool {
	isPhase1 := false
	for _, dep := range e.deps {
		if e.cacheMap == nil || len(dep.keyMap) == 0 && ((!dep.slowCacheComplete && e.slowCacheFunc(dep) != nil) || (dep.state < edgeStatusComplete && e.slowCacheFunc(dep) == nil)) {
			isPhase1 = true
			break
		}
	}

	if isPhase1 && len(dep.keyMap) > 0 {
		return true
	}
	return false
}

// unpark is called by the scheduler with incoming requests and updates for
// previous calls.
// To avoid deadlocks and resource leaks this function needs to follow
// following rules:
//  1. this function needs to return unclosed outgoing requests if some incoming
//     requests were not completed
//  2. this function may not return outgoing requests if it has completed all
//     incoming requests
func (e *edge) unpark(incoming []pipeSender, updates, allPipes []pipeReceiver, f *pipeFactory) {
	// process all incoming changes
	e.processUpdates(updates)

	desiredState, done := e.respondToIncoming(incoming, allPipes)
	if done {
		return
	}

	cacheMapReq := false
	// set up new outgoing requests if needed
	if e.cacheMapReq == nil && (e.cacheMap == nil || len(e.cacheRecords) == 0) {
		index := e.cacheMapIndex
		e.cacheMapReq = f.NewFuncRequest(func(ctx context.Context) (any, error) {
			cm, err := e.op.CacheMap(ctx, index)
			return cm, errors.Wrap(err, "failed to load cache key")
		})
		cacheMapReq = true
	}

	// execute op
	if e.execReq == nil && desiredState == edgeStatusComplete {
		if ok := e.execIfPossible(f); ok {
			return
		}
	}

	if e.execReq == nil {
		if added := e.createInputRequests(desiredState, f, false); !added && !e.hasActiveOutgoing && !cacheMapReq {
			bklog.G(context.TODO()).Errorf("buildkit scheduling error: leaving incoming open. forcing solve. Please report this with BUILDKIT_SCHEDULER_DEBUG=1")
			debugSchedulerPreUnpark(e, incoming, updates, allPipes)
			e.createInputRequests(desiredState, f, true)
		}
	}
}

func (e *edge) makeExportable(k *CacheKey, records []*CacheRecord) ExportableCacheKey {
	return ExportableCacheKey{
		CacheKey: k,
		Exporter: &exporter{k: k, records: records, override: e.edge.Vertex.Options().ExportCache},
	}
}

func (e *edge) markFailed(f *pipeFactory, err error) {
	e.err = err
	e.failedOnce.Do(func() {
		e.postpone(f)
	})
}

func (e *edge) processUpdates(updates []pipe.Receiver[*edgeRequest, any]) {
	depChanged := false
	for _, upt := range updates {
		depChanged = e.processUpdate(upt) || depChanged
	}

	if depChanged {
		e.recalcCurrentState()
	}
}

// processUpdate is called by unpark for every updated pipe request
func (e *edge) processUpdate(upt pipeReceiver) (depChanged bool) {
	// response for cachemap request
	if upt == e.cacheMapReq && upt.Status().Completed {
		e.processCacheMapReq()
		return true
	}

	// response for exec request
	if upt == e.execReq && upt.Status().Completed {
		e.processExecReq()
		return true
	}

	// response for requests to dependencies
	if dep, ok := e.depRequests[upt]; ok {
		return e.processDepReq(dep)
	}

	// response for result based cache function
	for i, dep := range e.deps {
		if upt == dep.slowCacheReq && upt.Status().Completed {
			e.processDepSlowCacheReq(i, dep)
			return true
		}
	}

	return false
}

// recalcCurrentState is called by unpark to recompute internal state after
// the state of dependencies has changed
func (e *edge) recalcCurrentState() {
	// TODO: fast pass to detect incomplete results
	newKeys := map[string]*CacheKey{}

	for i, dep := range e.deps {
		if i == 0 {
			for id, k := range dep.keyMap {
				if _, ok := e.keyMap[id]; ok {
					continue
				}
				newKeys[id] = k
			}
		} else {
			for id := range newKeys {
				if _, ok := dep.keyMap[id]; !ok {
					delete(newKeys, id)
				}
			}
		}
		if len(newKeys) == 0 {
			break
		}
	}

	for key := range newKeys {
		e.keyMap[key] = struct{}{}
	}

	for _, r := range newKeys {
		// TODO: add all deps automatically
		mergedKey := r.clone()
		mergedKey.deps = make([][]CacheKeyWithSelector, len(e.deps))
		for i, dep := range e.deps {
			if dep.result != nil {
				for _, dk := range dep.result.CacheKeys() {
					mergedKey.deps[i] = append(mergedKey.deps[i], CacheKeyWithSelector{Selector: e.cacheMap.Deps[i].Selector, CacheKey: dk})
				}
				if dep.slowCacheKey != nil {
					mergedKey.deps[i] = append(mergedKey.deps[i], CacheKeyWithSelector{CacheKey: *dep.slowCacheKey})
				}
			} else {
				for _, k := range dep.keys {
					mergedKey.deps[i] = append(mergedKey.deps[i], CacheKeyWithSelector{Selector: e.cacheMap.Deps[i].Selector, CacheKey: k})
				}
			}
		}

		records, err := e.op.Cache().Records(context.Background(), mergedKey)
		if err != nil {
			bklog.G(context.TODO()).Errorf("error receiving cache records: %v", err)
			continue
		}

		for _, r := range records {
			if _, ok := e.cacheRecordsLoaded[r.ID]; !ok {
				e.cacheRecords[r.ID] = r
			}
		}

		e.keys = append(e.keys, e.makeExportable(mergedKey, records))
	}

	// detect lower/upper bound for current state
	allDepsCompletedCacheFast := e.cacheMap != nil
	allDepsCompletedCacheSlow := e.cacheMap != nil
	allDepsStateCacheSlow := true
	allDepsCompleted := true
	stLow := edgeStatusInitial    // minimal possible state
	stHigh := edgeStatusCacheSlow // maximum possible state
	if e.cacheMap != nil {
		for _, dep := range e.deps {
			isSlowCacheIncomplete := e.slowCacheFunc(dep) != nil && (dep.state == edgeStatusCacheSlow || (dep.state == edgeStatusComplete && !dep.slowCacheComplete))
			isSlowIncomplete := (e.slowCacheFunc(dep) != nil || e.preprocessFunc(dep) != nil) && (dep.state == edgeStatusCacheSlow || (dep.state == edgeStatusComplete && !dep.slowCacheComplete))

			if dep.state > stLow && len(dep.keyMap) == 0 && !isSlowIncomplete {
				stLow = min(dep.state, edgeStatusCacheSlow)
			}
			effectiveState := dep.state
			if dep.state == edgeStatusCacheSlow && isSlowCacheIncomplete {
				effectiveState = edgeStatusCacheFast
			}
			if dep.state == edgeStatusComplete && isSlowCacheIncomplete {
				effectiveState = edgeStatusCacheFast
			}
			if effectiveState < stHigh {
				stHigh = effectiveState
			}
			if isSlowIncomplete || dep.state < edgeStatusComplete {
				allDepsCompleted = false
			}
			if dep.state < edgeStatusCacheFast {
				allDepsCompletedCacheFast = false
			}
			if isSlowCacheIncomplete || dep.state < edgeStatusCacheSlow {
				allDepsCompletedCacheSlow = false
			}
			if dep.state < edgeStatusCacheSlow && len(dep.keyMap) == 0 {
				allDepsStateCacheSlow = false
			}
		}
		if stLow > e.state {
			e.state = stLow
		}
		if stHigh > e.state {
			e.state = stHigh
		}
		if !e.cacheMapDone && len(e.keys) == 0 {
			e.state = edgeStatusInitial
		}

		e.allDepsCompletedCacheFast = e.cacheMapDone && allDepsCompletedCacheFast
		e.allDepsCompletedCacheSlow = e.cacheMapDone && allDepsCompletedCacheSlow
		e.allDepsStateCacheSlow = e.cacheMapDone && allDepsStateCacheSlow
		e.allDepsCompleted = e.cacheMapDone && allDepsCompleted

		if e.allDepsStateCacheSlow && len(e.cacheRecords) > 0 && e.state == edgeStatusCacheFast {
			openKeys := map[string]struct{}{}
			for _, dep := range e.deps {
				isSlowIncomplete := e.slowCacheFunc(dep) != nil && (dep.state == edgeStatusCacheSlow || (dep.state == edgeStatusComplete && !dep.slowCacheComplete))
				if !isSlowIncomplete {
					openDepKeys := map[string]struct{}{}
					for key := range dep.keyMap {
						if _, ok := e.keyMap[key]; !ok {
							openDepKeys[key] = struct{}{}
						}
					}
					if len(openKeys) != 0 {
						for k := range openKeys {
							if _, ok := openDepKeys[k]; !ok {
								delete(openKeys, k)
							}
						}
					} else {
						openKeys = openDepKeys
					}
					if len(openKeys) == 0 {
						e.state = edgeStatusCacheSlow
						debugSchedulerUpgradeCacheSlow(e)
					}
				}
			}
		}
	}
}

func (e *edge) processCacheMapReq() {
	upt := e.cacheMapReq
	if err := upt.Status().Err; err != nil {
		e.cacheMapReq = nil
		if !upt.Status().Canceled && e.err == nil {
			e.err = err
		}
		return
	}

	resp := upt.Status().Value.(*cacheMapResp)
	e.cacheMap = resp.CacheMap
	e.cacheMapDone = resp.complete
	e.cacheMapIndex++
	if len(e.deps) == 0 {
		e.cacheMapDigests = append(e.cacheMapDigests, e.cacheMap.Digest)
		if !e.op.IgnoreCache() {
			keys, err := e.op.Cache().Query(nil, 0, e.cacheMap.Digest, e.edge.Index)
			if err != nil {
				bklog.G(context.TODO()).Error(errors.Wrap(err, "invalid query response")) // make the build fail for this error
			} else {
				for _, k := range keys {
					k.vtx = e.edge.Vertex.Digest()
					records, err := e.op.Cache().Records(context.Background(), k)
					if err != nil {
						bklog.G(context.TODO()).Errorf("error receiving cache records: %v", err)
						continue
					}

					for _, r := range records {
						e.cacheRecords[r.ID] = r
					}

					e.keys = append(e.keys, e.makeExportable(k, records))
				}
			}
		}
		e.state = edgeStatusCacheSlow
	}
	if e.allDepsHaveKeys(false) {
		e.keysDidChange = true
	}
	// probe keys that were loaded before cache map
	for i, dep := range e.deps {
		e.probeCache(dep, withSelector(dep.keys, e.cacheMap.Deps[i].Selector))
		e.checkDepMatchPossible(dep)
	}
	if !e.cacheMapDone {
		e.cacheMapReq = nil
	}
}

func (e *edge) processExecReq() {
	upt := e.execReq
	if err := upt.Status().Err; err != nil {
		e.execReq = nil
		if e.execCacheLoad {
			for k := range e.cacheRecordsLoaded {
				delete(e.cacheRecords, k)
			}
		} else if !upt.Status().Canceled && e.err == nil {
			e.err = err
		}
		return
	}

	e.result = NewSharedCachedResult(upt.Status().Value.(CachedResult))
	e.state = edgeStatusComplete

	// The keys committed by the execution have so far only existed on the
	// result. Add them to e.keys so that a consumer subscribing after this
	// edge has completed (a shared edge kept alive by a concurrent build)
	// still receives them and can probe the cache. Skipped on cache load
	// where the loaded record's key is in e.keys already, and for
	// ignore-cache so that consumers cannot match cache records through a
	// dependency that was itself forced to re-run.
	if !e.execCacheLoad && !e.op.IgnoreCache() {
		e.keys = append(e.keys, e.result.CacheKeys()...)
	}
}

func (e *edge) processDepReq(dep *dep) (depChanged bool) {
	upt := dep.req
	if err := upt.Status().Err; !upt.Status().Canceled && upt.Status().Completed && err != nil {
		if e.err == nil {
			e.err = err
		}
		dep.err = err
	}

	if upt.Status().Value == nil {
		return false
	}

	state, isEdgeState := upt.Status().Value.(*edgeState)
	if !isEdgeState {
		bklog.G(context.TODO()).Warnf("invalid edgeState value for update: %T", state)
		return false
	}

	if state.state >= edgeStatusCacheSlow {
		debugSchedulerDepDelivery(e, dep, state)
	}

	if len(dep.keys) < len(state.keys) {
		newKeys := state.keys[len(dep.keys):]
		if e.cacheMap != nil {
			e.probeCache(dep, withSelector(newKeys, e.cacheMap.Deps[dep.index].Selector))
			dep.keys = state.keys
			if e.allDepsHaveKeys(false) {
				e.keysDidChange = true
			}
		}
		depChanged = true
	}

	if dep.state != edgeStatusComplete && state.state == edgeStatusComplete {
		e.keysDidChange = true
	}

	recheck := state.state != dep.state

	dep.edgeState = *state

	if recheck && e.cacheMap != nil {
		e.checkDepMatchPossible(dep)
		depChanged = true
	}
	return depChanged
}

func (e *edge) processDepSlowCacheReq(index int, dep *dep) {
	upt := dep.slowCacheReq
	if err := upt.Status().Err; err != nil {
		dep.slowCacheReq = nil
		if !upt.Status().Canceled && e.err == nil {
			e.err = upt.Status().Err
		}
	} else if !dep.slowCacheComplete {
		dgst := upt.Status().Value.(digest.Digest)
		if e.cacheMap.Deps[int(dep.index)].ComputeDigestFunc != nil && dgst != "" {
			k := NewCacheKey(dgst, "", -1)
			dep.slowCacheKey = &ExportableCacheKey{CacheKey: k, Exporter: &exporter{k: k}}
			slowKeyExp := CacheKeyWithSelector{CacheKey: *dep.slowCacheKey}
			defKeys := make([]CacheKeyWithSelector, 0, len(dep.result.CacheKeys()))
			for _, dk := range dep.result.CacheKeys() {
				defKeys = append(defKeys, CacheKeyWithSelector{CacheKey: dk, Selector: e.cacheMap.Deps[index].Selector})
			}
			dep.slowCacheFoundKey = e.probeCache(dep, []CacheKeyWithSelector{slowKeyExp})

			// connect def key to slow key
			e.op.Cache().Query(append(defKeys, slowKeyExp), dep.index, e.cacheMap.Digest, e.edge.Index)
		}

		dep.slowCacheComplete = true
		e.keysDidChange = true
		e.checkDepMatchPossible(dep) // not matching key here doesn't set nocachematch possible to true
	}
}

// respondToIncoming responds to all incoming requests. completing or
// updating them when possible
func (e *edge) respondToIncoming(incoming []pipeSender, allPipes []pipeReceiver) (edgeStatusType, bool) {
	// Requests can be canceled concurrently. Decide on one snapshot so a late
	// cancel cannot finish the last request while outgoing work is active; it
	// is handled on the next dispatch.
	canceled := make([]bool, len(incoming))
	for i, req := range incoming {
		canceled[i] = req.Request().Canceled
	}

	// detect the result state for the requests
	allIncomingCanComplete := true
	desiredState := e.state
	allCanceled := true

	// check incoming requests
	// check if all requests can be either answered or canceled
	if !e.isComplete() {
		for i, req := range incoming {
			if !canceled[i] {
				allCanceled = false
				if r := req.Request().Payload; desiredState < r.desiredState {
					desiredState = r.desiredState
					if e.hasActiveOutgoing || r.desiredState == edgeStatusComplete || r.currentKeys == len(e.keys) {
						allIncomingCanComplete = false
					}
				}
			}
		}
	}

	// do not set allIncomingCanComplete if active ongoing can modify the state
	if !allCanceled && e.state < edgeStatusComplete && len(e.keys) == 0 && e.hasActiveOutgoing {
		allIncomingCanComplete = false
	}

	debugSchedulerRespondToIncomingStatus(e, allIncomingCanComplete)

	if allIncomingCanComplete && e.hasActiveOutgoing {
		// cancel all current requests
		for _, p := range allPipes {
			p.Cancel()
		}

		// can close all but one requests
		var leaveOpen pipeSender
		for i, req := range incoming {
			if !canceled[i] {
				leaveOpen = req
				break
			}
		}
		for _, req := range incoming {
			if leaveOpen == nil || leaveOpen == req {
				leaveOpen = req
				continue
			}
			e.finishIncoming(req)
		}
		return desiredState, true
	}

	// can complete, finish and return
	if allIncomingCanComplete && !e.hasActiveOutgoing {
		for _, req := range incoming {
			e.finishIncoming(req)
		}
		return desiredState, true
	}

	// update incoming based on current state
	for i, req := range incoming {
		r := req.Request().Payload
		if canceled[i] {
			e.finishIncoming(req)
		} else if !e.hasActiveOutgoing && e.state >= r.desiredState {
			e.finishIncoming(req)
		} else if !isEqualState(r.currentState, e.edgeState) && !req.Request().Canceled {
			e.updateIncoming(req)
		}
	}
	return desiredState, false
}

// createInputRequests creates new requests for dependencies or async functions
// that need to complete to continue processing the edge
func (e *edge) createInputRequests(desiredState edgeStatusType, f *pipeFactory, force bool) (addedNew bool) {
	// initialize deps state
	e.ensureDepsInitialized()

	// cycle all dependencies. set up outgoing requests if needed
	for _, dep := range e.deps {
		desiredStateDep := e.desiredStateDep(dep, desiredState, force)

		// outgoing request is needed
		if dep.state < desiredStateDep {
			addedNew = e.createOutgoingRequest(dep, desiredStateDep, f) || addedNew
		} else {
			debugSchedulerSkipInputRequestBasedOnDepState(e, dep, desiredStateDep)
		}

		// initialize function to compute cache key based on dependency result
		addedNew = e.computeCacheKeyFromDep(dep, f) || addedNew
	}
	return addedNew
}

func (e *edge) ensureDepsInitialized() {
	if e.deps != nil {
		return
	}

	e.depRequests = make(map[pipeReceiver]*dep)
	e.deps = make([]*dep, 0, len(e.edge.Vertex.Inputs()))
	for i := range e.edge.Vertex.Inputs() {
		e.deps = append(e.deps, newDep(Index(i)))
	}
}

func (e *edge) desiredStateDep(dep *dep, desiredState edgeStatusType, force bool) edgeStatusType {
	if e.noCacheMatchPossible || force {
		return edgeStatusComplete
	}

	if dep.state == edgeStatusInitial && desiredState > dep.state {
		return edgeStatusCacheFast
	}

	if dep.state == edgeStatusCacheFast && desiredState > dep.state {
		// wait all deps to complete cache fast before continuing with slow cache
		if (e.allDepsCompletedCacheFast && len(e.keys) == 0) || len(dep.keyMap) == 0 || e.allDepsHaveKeys(true) {
			if !e.skipPhase2FastCache(dep) && e.cacheMap != nil {
				return edgeStatusCacheSlow
			}
		}
		return dep.state
	}

	if e.cacheMap != nil && dep.state == edgeStatusCacheSlow && desiredState == edgeStatusComplete {
		// if all deps have completed cache-slow or content based cache for input is available
		if (len(dep.keyMap) == 0 || e.allDepsCompletedCacheSlow || (!e.skipPhase2FastCache(dep) && e.slowCacheFunc(dep) != nil)) && (len(e.cacheRecords) == 0) {
			if len(dep.keyMap) == 0 || !e.skipPhase2SlowCache(dep) {
				return edgeStatusComplete
			}
		}
		return dep.state
	}

	if e.cacheMap != nil && dep.state == edgeStatusCacheSlow && e.slowCacheFunc(dep) != nil && desiredState == edgeStatusCacheSlow {
		if len(dep.keyMap) == 0 || !e.skipPhase2SlowCache(dep) {
			return edgeStatusComplete
		}
		return dep.state
	}

	return dep.state
}

func (e *edge) createOutgoingRequest(dep *dep, desiredStateDep edgeStatusType, f *pipeFactory) (addedNew bool) {
	if dep.req != nil && !dep.req.Status().Completed {
		if dep.req.Request().desiredState == desiredStateDep {
			debugSchedulerSkipInputRequestBasedOnExistingRequest(e, dep, desiredStateDep)
			return false
		}
		debugSchedulerCancelInputRequest(e, dep, desiredStateDep)
		dep.req.Cancel()
	}

	debugSchedulerAddInputRequest(e, dep, desiredStateDep)
	req := f.NewInputRequest(e.edge.Vertex.Inputs()[int(dep.index)], &edgeRequest{
		currentState: dep.edgeState,
		desiredState: desiredStateDep,
		currentKeys:  len(dep.keys),
	})
	e.depRequests[req] = dep
	dep.req = req
	return true
}

func (e *edge) computeCacheKeyFromDep(dep *dep, f *pipeFactory) (addedNew bool) {
	if dep.state != edgeStatusComplete || dep.slowCacheReq != nil || e.cacheMap == nil {
		return false
	}

	pfn := e.preprocessFunc(dep)
	fn := e.slowCacheFunc(dep)
	if pfn == nil && fn == nil {
		return false
	}

	res := dep.result
	index := dep.index
	dep.slowCacheReq = f.NewFuncRequest(func(ctx context.Context) (any, error) {
		v, err := e.op.CalcSlowCache(ctx, index, pfn, fn, res)
		return v, errors.Wrap(err, "failed to compute cache key")
	})
	return true
}

// execIfPossible creates a request for getting the edge result if there is
// enough state
func (e *edge) execIfPossible(f *pipeFactory) bool {
	if len(e.cacheRecords) > 0 {
		if e.keysDidChange {
			e.postpone(f)
			return true
		}
		e.execReq = f.NewFuncRequest(e.loadCache)
		e.execCacheLoad = true
		for req := range e.depRequests {
			req.Cancel()
		}
		return true
	} else if e.allDepsCompleted {
		if e.keysDidChange {
			e.postpone(f)
			return true
		}
		e.execReq = f.NewFuncRequest(e.execOp)
		e.execCacheLoad = false
		return true
	}
	return false
}

// postpone delays exec to next unpark invocation if we have unprocessed keys
func (e *edge) postpone(f *pipeFactory) {
	f.NewFuncRequest(func(context.Context) (any, error) {
		return nil, nil
	})
}

// loadCache creates a request to load edge result from cache
func (e *edge) loadCache(ctx context.Context) (any, error) {
	recs := make([]*CacheRecord, 0, len(e.cacheRecords))
	for _, r := range e.cacheRecords {
		recs = append(recs, r)
	}

	rec := getBestResult(recs)
	e.cacheRecordsLoaded[rec.ID] = struct{}{}

	bklog.G(ctx).Debugf("load cache for %s with %s", e.edge.Vertex.Name(), rec.ID)
	res, ctxOpts, err := e.op.LoadCache(ctx, rec)
	if err != nil {
		bklog.G(ctx).Debugf("load cache for %s err: %v", e.edge.Vertex.Name(), err)
		return nil, errors.Wrap(err, "failed to load cache")
	}

	return NewCachedResult(res, []ExportableCacheKey{{CacheKey: rec.key, Exporter: &exporter{k: rec.key, record: rec, edge: e, recordCtxOpts: ctxOpts}}}), nil
}

// execOp creates a request to execute the vertex operation
func (e *edge) execOp(ctx context.Context) (any, error) {
	cacheKeys, inputs := e.commitOptions()
	results, subExporters, ctxOpts, err := e.op.Exec(ctx, toResultSlice(inputs))
	if err != nil {
		return nil, errors.WithStack(err)
	}

	index := e.edge.Index
	if len(results) <= int(index) {
		return nil, errors.Errorf("invalid response from exec need %d index but %d results received", index, len(results))
	}

	res := results[int(index)]

	for i := range results {
		if i != int(index) {
			go results[i].Release(context.WithoutCancel(ctx))
		}
	}

	var exporters []CacheExporter

	for _, cacheKey := range cacheKeys {
		ck, err := e.op.Cache().Save(cacheKey, res, time.Now())
		if err != nil {
			return nil, err
		}

		if exp, ok := ck.Exporter.(*exporter); ok {
			exp.edge = e
			exp.recordCtxOpts = ctxOpts
		}

		exps := make([]CacheExporter, 0, len(subExporters))
		for _, exp := range subExporters {
			exps = append(exps, exp.Exporter)
		}

		exporters = append(exporters, ck.Exporter)
		exporters = append(exporters, exps...)
	}

	ek := make([]ExportableCacheKey, 0, len(cacheKeys))
	for _, ck := range cacheKeys {
		ek = append(ek, ExportableCacheKey{
			CacheKey: ck,
			Exporter: &mergedExporter{exporters: exporters},
		})
	}

	return NewCachedResult(res, ek), nil
}

func (e *edge) isDep(e2 *edge) bool {
	return isDep(e.edge.Vertex, e2.edge.Vertex)
}

func isDep(vtx, vtx2 Vertex) bool {
	if vtx.Digest() == vtx2.Digest() {
		return true
	}
	for _, e := range vtx.Inputs() {
		if isDep(e.Vertex, vtx2) {
			return true
		}
	}
	return false
}

func toResultSlice(cres []CachedResult) (out []Result) {
	out = make([]Result, len(cres))
	for i := range cres {
		out[i] = cres[i].(Result)
	}
	return out
}

func isEqualState(s1, s2 edgeState) bool {
	if s1.state != s2.state || s1.result != s2.result || s1.cacheMap != s2.cacheMap || len(s1.keys) != len(s2.keys) {
		return false
	}
	return true
}

func withSelector(keys []ExportableCacheKey, selector digest.Digest) []CacheKeyWithSelector {
	out := make([]CacheKeyWithSelector, len(keys))
	for i, k := range keys {
		out[i] = CacheKeyWithSelector{Selector: selector, CacheKey: k}
	}
	return out
}

type (
	pipeRequest  = pipe.Request[*edgeRequest]
	pipeSender   = pipe.Sender[*edgeRequest, any]
	pipeReceiver = pipe.Receiver[*edgeRequest, any]
)

func newPipe(req pipeRequest) *pipe.Pipe[*edgeRequest, any] {
	return pipe.New[*edgeRequest, any](req)
}
