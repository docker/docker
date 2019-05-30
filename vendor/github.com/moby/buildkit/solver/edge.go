package solver

import (
	"context"
	"sync"
	"time"

	"github.com/moby/buildkit/solver/internal/pipe"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
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
		edge:         ed,
		op:           op,
		depRequests:  map[pipe.Receiver]*dep{},
		keyMap:       map[string]struct{}{},
		cacheRecords: map[string]*CacheRecord{},
		index:        index,
	}
	return e
}

type edge struct {
	edge Edge
	op   activeOp

	edgeState
	depRequests map[pipe.Receiver]*dep
	deps        []*dep

	cacheMapReq     pipe.Receiver
	cacheMapDone    bool
	cacheMapIndex   int
	cacheMapDigests []digest.Digest
	execReq         pipe.Receiver
	err             error
	cacheRecords    map[string]*CacheRecord
	keyMap          map[string]struct{}

	noCacheMatchPossible      bool
	allDepsCompletedCacheFast bool
	allDepsCompletedCacheSlow bool
	allDepsStateCacheSlow     bool
	allDepsCompleted          bool
	hasActiveOutgoing         bool

	releaserCount int
	keysDidChange bool
	index         *edgeIndex

	secondaryExporters []expDep
}

// dep holds state for a dependant edge
type dep struct {
	req pipe.Receiver
	edgeState
	index             Index
	keyMap            map[string]*CacheKey
	desiredState      edgeStatusType
	e                 *edge
	slowCacheReq      pipe.Receiver
	slowCacheComplete bool
	slowCacheFoundKey bool
	slowCacheKey      *ExportableCacheKey
	err               error
}

// expDep holds secorndary exporter info for dependency
type expDep struct {
	index    int
	cacheKey CacheKeyWithSelector
}

func newDep(i Index) *dep {
	return &dep{index: i, keyMap: map[string]*CacheKey{}}
}

// edgePipe is a pipe for requests between two edges
type edgePipe struct {
	*pipe.Pipe
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

// incrementReferenceCount increases the number of times release needs to be
// called to release the edge. Called on merging edges.
func (e *edge) incrementReferenceCount() {
	e.releaserCount += 1
}

// release releases the edge resources
func (e *edge) release() {
	if e.releaserCount > 0 {
		e.releaserCount--
		return
	}
	e.index.Release(e)
	if e.result != nil {
		go e.result.Release(context.TODO())
	}
}

// commitOptions returns parameters for the op execution
func (e *edge) commitOptions() ([]*CacheKey, []CachedResult) {
	k := NewCacheKey(e.cacheMap.Digest, e.edge.Index)
	if len(e.deps) == 0 {
		keys := make([]*CacheKey, 0, len(e.cacheMapDigests))
		for _, dgst := range e.cacheMapDigests {
			keys = append(keys, NewCacheKey(dgst, e.edge.Index))
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
func (e *edge) finishIncoming(req pipe.Sender) {
	err := e.err
	if req.Request().Canceled && err == nil {
		err = context.Canceled
	}
	if debugScheduler {
		logrus.Debugf("finishIncoming %s %v %#v desired=%s", e.edge.Vertex.Name(), err, e.edgeState, req.Request().Payload.(*edgeRequest).desiredState)
	}
	req.Finalize(&e.edgeState, err)
}

// updateIncoming updates the current value of incoming pipe request
func (e *edge) updateIncoming(req pipe.Sender) {
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
	}
}

// slowCacheFunc returns the result based cache func for dependency if it exists
func (e *edge) slowCacheFunc(dep *dep) ResultBasedCacheFunc {
	if e.cacheMap == nil {
		return nil
	}
	return e.cacheMap.Deps[int(dep.index)].ComputeDigestFunc
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

	k := NewCacheKey(e.cacheMap.Digest, e.edge.Index)
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
// 1) this function needs to return unclosed outgoing requests if some incoming
//    requests were not completed
// 2) this function may not return outgoing requests if it has completed all
//    incoming requests
func (e *edge) unpark(incoming []pipe.Sender, updates, allPipes []pipe.Receiver, f *pipeFactory) {
	// process all incoming changes
	depChanged := false
	for _, upt := range updates {
		if changed := e.processUpdate(upt); changed {
			depChanged = true
		}
	}

	if depChanged {
		// the dep responses had changes. need to reevaluate edge state
		e.recalcCurrentState()
	}

	desiredState, done := e.respondToIncoming(incoming, allPipes)
	if done {
		return
	}

	cacheMapReq := false
	// set up new outgoing requests if needed
	if e.cacheMapReq == nil && (e.cacheMap == nil || len(e.cacheRecords) == 0) {
		index := e.cacheMapIndex
		e.cacheMapReq = f.NewFuncRequest(func(ctx context.Context) (interface{}, error) {
			return e.op.CacheMap(ctx, index)
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
			logrus.Errorf("buildkit scheluding error: leaving incoming open. forcing solve. Please report this with BUILDKIT_SCHEDULER_DEBUG=1")
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
	e.postpone(f)
}

// processUpdate is called by unpark for every updated pipe request
func (e *edge) processUpdate(upt pipe.Receiver) (depChanged bool) {
	// response for cachemap request
	if upt == e.cacheMapReq && upt.Status().Completed {
		if err := upt.Status().Err; err != nil {
			e.cacheMapReq = nil
			if !upt.Status().Canceled && e.err == nil {
				e.err = err
			}
		} else {
			resp := upt.Status().Value.(*cacheMapResp)
			e.cacheMap = resp.CacheMap
			e.cacheMapDone = resp.complete
			e.cacheMapIndex++
			if len(e.deps) == 0 {
				e.cacheMapDigests = append(e.cacheMapDigests, e.cacheMap.Digest)
				if !e.op.IgnoreCache() {
					keys, err := e.op.Cache().Query(nil, 0, e.cacheMap.Digest, e.edge.Index)
					if err != nil {
						logrus.Error(errors.Wrap(err, "invalid query response")) // make the build fail for this error
					} else {
						for _, k := range keys {
							records, err := e.op.Cache().Records(k)
							if err != nil {
								logrus.Errorf("error receiving cache records: %v", err)
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
		return true
	}

	// response for exec request
	if upt == e.execReq && upt.Status().Completed {
		if err := upt.Status().Err; err != nil {
			e.execReq = nil
			if !upt.Status().Canceled && e.err == nil {
				e.err = err
			}
		} else {
			e.result = NewSharedCachedResult(upt.Status().Value.(CachedResult))
			e.state = edgeStatusComplete
		}
		return true
	}

	// response for requests to dependencies
	if dep, ok := e.depRequests[upt]; ok {
		if err := upt.Status().Err; !upt.Status().Canceled && upt.Status().Completed && err != nil {
			if e.err == nil {
				e.err = err
			}
			dep.err = err
		}

		state := upt.Status().Value.(*edgeState)

		if len(dep.keys) < len(state.keys) {
			newKeys := state.keys[len(dep.keys):]
			if e.cacheMap != nil {
				e.probeCache(dep, withSelector(newKeys, e.cacheMap.Deps[dep.index].Selector))
				dep.edgeState.keys = state.keys
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

		return
	}

	// response for result based cache function
	for i, dep := range e.deps {
		if upt == dep.slowCacheReq && upt.Status().Completed {
			if err := upt.Status().Err; err != nil {
				dep.slowCacheReq = nil
				if !upt.Status().Canceled && e.err == nil {
					e.err = upt.Status().Err
				}
			} else if !dep.slowCacheComplete {
				k := NewCacheKey(upt.Status().Value.(digest.Digest), -1)
				dep.slowCacheKey = &ExportableCacheKey{CacheKey: k, Exporter: &exporter{k: k}}
				slowKeyExp := CacheKeyWithSelector{CacheKey: *dep.slowCacheKey}
				defKeys := make([]CacheKeyWithSelector, 0, len(dep.result.CacheKeys()))
				for _, dk := range dep.result.CacheKeys() {
					defKeys = append(defKeys, CacheKeyWithSelector{CacheKey: dk, Selector: e.cacheMap.Deps[i].Selector})
				}
				dep.slowCacheFoundKey = e.probeCache(dep, []CacheKeyWithSelector{slowKeyExp})

				// connect def key to slow key
				e.op.Cache().Query(append(defKeys, slowKeyExp), dep.index, e.cacheMap.Digest, e.edge.Index)

				dep.slowCacheComplete = true
				e.keysDidChange = true
				e.checkDepMatchPossible(dep) // not matching key here doesn't set nocachematch possible to true
			}
			return true
		}
	}

	return
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

		records, err := e.op.Cache().Records(mergedKey)
		if err != nil {
			logrus.Errorf("error receiving cache records: %v", err)
			continue
		}

		for _, r := range records {
			e.cacheRecords[r.ID] = r
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
			isSlowIncomplete := e.slowCacheFunc(dep) != nil && (dep.state == edgeStatusCacheSlow || (dep.state == edgeStatusComplete && !dep.slowCacheComplete))

			if dep.state > stLow && len(dep.keyMap) == 0 && !isSlowIncomplete {
				stLow = dep.state
				if stLow > edgeStatusCacheSlow {
					stLow = edgeStatusCacheSlow
				}
			}
			effectiveState := dep.state
			if dep.state == edgeStatusCacheSlow && isSlowIncomplete {
				effectiveState = edgeStatusCacheFast
			}
			if dep.state == edgeStatusComplete && isSlowIncomplete {
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
			if isSlowIncomplete || dep.state < edgeStatusCacheSlow {
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
						if debugScheduler {
							logrus.Debugf("upgrade to cache-slow because no open keys")
						}
					}
				}
			}
		}
	}
}

// respondToIncoming responds to all incoming requests. completing or
// updating them when possible
func (e *edge) respondToIncoming(incoming []pipe.Sender, allPipes []pipe.Receiver) (edgeStatusType, bool) {
	// detect the result state for the requests
	allIncomingCanComplete := true
	desiredState := e.state
	allCanceled := true

	// check incoming requests
	// check if all requests can be either answered or canceled
	if !e.isComplete() {
		for _, req := range incoming {
			if !req.Request().Canceled {
				allCanceled = false
				if r := req.Request().Payload.(*edgeRequest); desiredState < r.desiredState {
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

	if debugScheduler {
		logrus.Debugf("status state=%s cancomplete=%v hasouts=%v noPossibleCache=%v depsCacheFast=%v keys=%d cacheRecords=%d", e.state, allIncomingCanComplete, e.hasActiveOutgoing, e.noCacheMatchPossible, e.allDepsCompletedCacheFast, len(e.keys), len(e.cacheRecords))
	}

	if allIncomingCanComplete && e.hasActiveOutgoing {
		// cancel all current requests
		for _, p := range allPipes {
			p.Cancel()
		}

		// can close all but one requests
		var leaveOpen pipe.Sender
		for _, req := range incoming {
			if !req.Request().Canceled {
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
	for _, req := range incoming {
		r := req.Request().Payload.(*edgeRequest)
		if req.Request().Canceled {
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
func (e *edge) createInputRequests(desiredState edgeStatusType, f *pipeFactory, force bool) bool {
	addedNew := false

	// initialize deps state
	if e.deps == nil {
		e.depRequests = make(map[pipe.Receiver]*dep)
		e.deps = make([]*dep, 0, len(e.edge.Vertex.Inputs()))
		for i := range e.edge.Vertex.Inputs() {
			e.deps = append(e.deps, newDep(Index(i)))
		}
	}

	// cycle all dependencies. set up outgoing requests if needed
	for _, dep := range e.deps {
		desiredStateDep := dep.state

		if e.noCacheMatchPossible || force {
			desiredStateDep = edgeStatusComplete
		} else if dep.state == edgeStatusInitial && desiredState > dep.state {
			desiredStateDep = edgeStatusCacheFast
		} else if dep.state == edgeStatusCacheFast && desiredState > dep.state {
			// wait all deps to complete cache fast before continuing with slow cache
			if (e.allDepsCompletedCacheFast && len(e.keys) == 0) || len(dep.keyMap) == 0 || e.allDepsHaveKeys(true) {
				if !e.skipPhase2FastCache(dep) && e.cacheMap != nil {
					desiredStateDep = edgeStatusCacheSlow
				}
			}
		} else if e.cacheMap != nil && dep.state == edgeStatusCacheSlow && desiredState == edgeStatusComplete {
			// if all deps have completed cache-slow or content based cache for input is available
			if (len(dep.keyMap) == 0 || e.allDepsCompletedCacheSlow || (!e.skipPhase2FastCache(dep) && e.slowCacheFunc(dep) != nil)) && (len(e.cacheRecords) == 0) {
				if len(dep.keyMap) == 0 || !e.skipPhase2SlowCache(dep) {
					desiredStateDep = edgeStatusComplete
				}
			}
		} else if e.cacheMap != nil && dep.state == edgeStatusCacheSlow && e.slowCacheFunc(dep) != nil && desiredState == edgeStatusCacheSlow {
			if len(dep.keyMap) == 0 || !e.skipPhase2SlowCache(dep) {
				desiredStateDep = edgeStatusComplete
			}
		}

		// outgoing request is needed
		if dep.state < desiredStateDep {
			addNew := true
			if dep.req != nil && !dep.req.Status().Completed {
				if dep.req.Request().(*edgeRequest).desiredState != desiredStateDep {
					dep.req.Cancel()
				} else {
					addNew = false
				}
			}
			if addNew {
				req := f.NewInputRequest(e.edge.Vertex.Inputs()[int(dep.index)], &edgeRequest{
					currentState: dep.edgeState,
					desiredState: desiredStateDep,
					currentKeys:  len(dep.keys),
				})
				e.depRequests[req] = dep
				dep.req = req
				addedNew = true
			}
		}
		// initialize function to compute cache key based on dependency result
		if dep.state == edgeStatusComplete && dep.slowCacheReq == nil && e.slowCacheFunc(dep) != nil && e.cacheMap != nil {
			fn := e.slowCacheFunc(dep)
			res := dep.result
			func(fn ResultBasedCacheFunc, res Result, index Index) {
				dep.slowCacheReq = f.NewFuncRequest(func(ctx context.Context) (interface{}, error) {
					return e.op.CalcSlowCache(ctx, index, fn, res)
				})
			}(fn, res, dep.index)
			addedNew = true
		}
	}
	return addedNew
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
		return true
	}
	return false
}

// postpone delays exec to next unpark invocation if we have unprocessed keys
func (e *edge) postpone(f *pipeFactory) {
	f.NewFuncRequest(func(context.Context) (interface{}, error) {
		return nil, nil
	})
}

// loadCache creates a request to load edge result from cache
func (e *edge) loadCache(ctx context.Context) (interface{}, error) {
	recs := make([]*CacheRecord, 0, len(e.cacheRecords))
	for _, r := range e.cacheRecords {
		recs = append(recs, r)
	}

	rec := getBestResult(recs)

	logrus.Debugf("load cache for %s with %s", e.edge.Vertex.Name(), rec.ID)
	res, err := e.op.LoadCache(ctx, rec)
	if err != nil {
		return nil, err
	}

	return NewCachedResult(res, []ExportableCacheKey{{CacheKey: rec.key, Exporter: &exporter{k: rec.key, record: rec, edge: e}}}), nil
}

// execOp creates a request to execute the vertex operation
func (e *edge) execOp(ctx context.Context) (interface{}, error) {
	cacheKeys, inputs := e.commitOptions()
	results, subExporters, err := e.op.Exec(ctx, toResultSlice(inputs))
	if err != nil {
		return nil, err
	}

	index := e.edge.Index
	if len(results) <= int(index) {
		return nil, errors.Errorf("invalid response from exec need %d index but %d results received", index, len(results))
	}

	res := results[int(index)]

	for i := range results {
		if i != int(index) {
			go results[i].Release(context.TODO())
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
