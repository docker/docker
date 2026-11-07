package solver

import (
	"sync"

	"github.com/moby/buildkit/identity"
)

// edgeIndex is a synchronous map for detecting edge collisions.
type edgeIndex struct {
	mu sync.Mutex

	items    map[string]*indexItem
	backRefs map[*edge]map[string]struct{}
}

type indexItem struct {
	edge  *edge
	links map[CacheInfoLink]map[string]struct{}
	deps  map[string]struct{}
}

func newEdgeIndex() *edgeIndex {
	return &edgeIndex{
		items:    map[string]*indexItem{},
		backRefs: map[*edge]map[string]struct{}{},
	}
}

func (ei *edgeIndex) Release(e *edge) {
	ei.mu.Lock()
	defer ei.mu.Unlock()

	for id := range ei.backRefs[e] {
		ei.releaseEdge(id)
	}
	delete(ei.backRefs, e)
}

func (ei *edgeIndex) releaseEdge(id string) {
	item, ok := ei.items[id]
	if !ok {
		return
	}

	item.edge = nil

	if len(item.links) == 0 {
		for d := range item.deps {
			ei.releaseLink(d, id)
		}
		delete(ei.items, id)
	}
}

func (ei *edgeIndex) releaseLink(id, target string) {
	item, ok := ei.items[id]
	if !ok {
		return
	}

	for lid, links := range item.links {
		for check := range links {
			if check == target {
				delete(links, check)
			}
		}
		if len(links) == 0 {
			delete(item.links, lid)
		}
	}

	if item.edge == nil && len(item.links) == 0 {
		for d := range item.deps {
			ei.releaseLink(d, id)
		}
		delete(ei.items, id)
	}
}

func (ei *edgeIndex) LoadOrStore(k *CacheKey, e *edge) *edge {
	ei.mu.Lock()
	defer ei.mu.Unlock()

	// get all current edges that match the cachekey
	ids := ei.getAllMatches(k)

	var oldID string
	var old *edge

	for _, id := range ids {
		if item, ok := ei.items[id]; ok {
			if item.edge != e {
				oldID = id
				old = item.edge
			}
		}
	}

	if old != nil && (isIgnoreCache(old) || !isIgnoreCache(e)) {
		ei.enforceLinked(oldID, k)
		return old
	}

	id := identity.NewID()
	if len(ids) > 0 {
		id = ids[0]
	}

	ei.enforceLinked(id, k)

	ei.items[id].edge = e
	backRefs, ok := ei.backRefs[e]
	if !ok {
		backRefs = map[string]struct{}{}
		ei.backRefs[e] = backRefs
	}
	backRefs[id] = struct{}{}

	return nil
}

// enforceLinked adds links from current ID to all dep keys
func (ei *edgeIndex) enforceLinked(id string, k *CacheKey) {
	main, ok := ei.items[id]
	if !ok {
		main = &indexItem{
			links: map[CacheInfoLink]map[string]struct{}{},
			deps:  map[string]struct{}{},
		}
		ei.items[id] = main
	}

	deps := k.Deps()

	for i, dd := range deps {
		for _, d := range dd {
			ck := d.CacheKey.CacheKey
			ei.enforceIndexID(ck)
			ll := CacheInfoLink{Input: Index(i), Digest: k.Digest(), Output: k.Output(), Selector: d.Selector}
			for _, ckID := range ck.indexIDs {
				if item, ok := ei.items[ckID]; ok {
					links, ok := item.links[ll]
					if !ok {
						links = map[string]struct{}{}
						item.links[ll] = links
					}
					links[id] = struct{}{}
					main.deps[ckID] = struct{}{}
				}
			}
		}
	}
}

func (ei *edgeIndex) enforceIndexID(k *CacheKey) {
	if len(k.indexIDs) > 0 {
		return
	}

	matches := ei.getAllMatches(k)

	// TraceFields reads indexIDs under the cache-key lock.
	k.mu.Lock()
	if len(matches) > 0 {
		k.indexIDs = matches
	} else {
		k.indexIDs = []string{identity.NewID()}
	}
	k.mu.Unlock()

	for _, id := range k.indexIDs {
		ei.enforceLinked(id, k)
	}
}

func (ei *edgeIndex) getAllMatches(k *CacheKey) []string {
	deps := k.Deps()

	if len(deps) == 0 {
		return []string{rootKey(k.Digest(), k.Output()).String()}
	}

	for _, dd := range deps {
		for _, k := range dd {
			ei.enforceIndexID(k.CacheKey.CacheKey)
		}
	}

	matches := map[string]struct{}{}

	for i, dd := range deps {
		if i == 0 {
			for _, d := range dd {
				ll := CacheInfoLink{Input: Index(i), Digest: k.Digest(), Output: k.Output(), Selector: d.Selector}
				for _, ckID := range d.CacheKey.indexIDs {
					item, ok := ei.items[ckID]
					if ok {
						for l := range item.links[ll] {
							matches[l] = struct{}{}
						}
					}
				}
			}
			continue
		}

		if len(matches) == 0 {
			break
		}

		for m := range matches {
			found := false
			for _, d := range dd {
				ll := CacheInfoLink{Input: Index(i), Digest: k.Digest(), Output: k.Output(), Selector: d.Selector}
				for _, ckID := range d.CacheKey.indexIDs {
					if item, ok := ei.items[ckID]; ok {
						if l, ok := item.links[ll]; ok {
							if _, ok := l[m]; ok {
								found = true
								break
							}
						}
					}
				}
			}

			if !found {
				delete(matches, m)
			}
		}
	}

	out := make([]string, 0, len(matches))

	for m := range matches {
		out = append(out, m)
	}

	return out
}

func isIgnoreCache(e *edge) bool {
	if e.edge.Vertex == nil {
		return false
	}
	return e.edge.Vertex.Options().IgnoreCache
}
