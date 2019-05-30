package solver

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/moby/buildkit/identity"
	digest "github.com/opencontainers/go-digest"
	"github.com/sirupsen/logrus"
)

// NewInMemoryCacheManager creates a new in-memory cache manager
func NewInMemoryCacheManager() CacheManager {
	return NewCacheManager(identity.NewID(), NewInMemoryCacheStorage(), NewInMemoryResultStorage())
}

// NewCacheManager creates a new cache manager with specific storage backend
func NewCacheManager(id string, storage CacheKeyStorage, results CacheResultStorage) CacheManager {
	cm := &cacheManager{
		id:      id,
		backend: storage,
		results: results,
	}

	if err := cm.ReleaseUnreferenced(); err != nil {
		logrus.Errorf("failed to release unreferenced cache metadata: %+v", err)
	}

	return cm
}

type cacheManager struct {
	mu sync.RWMutex
	id string

	backend CacheKeyStorage
	results CacheResultStorage
}

func (c *cacheManager) ReleaseUnreferenced() error {
	return c.backend.Walk(func(id string) error {
		return c.backend.WalkResults(id, func(cr CacheResult) error {
			if !c.results.Exists(cr.ID) {
				c.backend.Release(cr.ID)
			}
			return nil
		})
	})
}

func (c *cacheManager) ID() string {
	return c.id
}

func (c *cacheManager) Query(deps []CacheKeyWithSelector, input Index, dgst digest.Digest, output Index) ([]*CacheKey, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	type dep struct {
		results map[string]struct{}
		key     CacheKeyWithSelector
	}

	allDeps := make([]dep, 0, len(deps))
	for _, k := range deps {
		allDeps = append(allDeps, dep{key: k, results: map[string]struct{}{}})
	}

	allRes := map[string]*CacheKey{}
	for _, d := range allDeps {
		if err := c.backend.WalkLinks(c.getID(d.key.CacheKey.CacheKey), CacheInfoLink{input, output, dgst, d.key.Selector}, func(id string) error {
			d.results[id] = struct{}{}
			if _, ok := allRes[id]; !ok {
				allRes[id] = c.newKeyWithID(id, dgst, output)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}

	// link the results against the keys that didn't exist
	for id, key := range allRes {
		for _, d := range allDeps {
			if _, ok := d.results[id]; !ok {
				if err := c.backend.AddLink(c.getID(d.key.CacheKey.CacheKey), CacheInfoLink{
					Input:    input,
					Output:   output,
					Digest:   dgst,
					Selector: d.key.Selector,
				}, c.getID(key)); err != nil {
					return nil, err
				}
			}
		}
	}

	if len(deps) == 0 {
		if !c.backend.Exists(rootKey(dgst, output).String()) {
			return nil, nil
		}
		return []*CacheKey{c.newRootKey(dgst, output)}, nil
	}

	keys := make([]*CacheKey, 0, len(deps))
	for _, k := range allRes {
		keys = append(keys, k)
	}
	return keys, nil
}

func (c *cacheManager) Records(ck *CacheKey) ([]*CacheRecord, error) {
	outs := make([]*CacheRecord, 0)
	if err := c.backend.WalkResults(c.getID(ck), func(r CacheResult) error {
		if c.results.Exists(r.ID) {
			outs = append(outs, &CacheRecord{
				ID:           r.ID,
				cacheManager: c,
				key:          ck,
				CreatedAt:    r.CreatedAt,
			})
		} else {
			c.backend.Release(r.ID)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return outs, nil
}

func (c *cacheManager) Load(ctx context.Context, rec *CacheRecord) (Result, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	res, err := c.backend.Load(c.getID(rec.key), rec.ID)
	if err != nil {
		return nil, err
	}

	return c.results.Load(ctx, res)
}

type LoadedResult struct {
	Result      Result
	CacheResult CacheResult
	CacheKey    *CacheKey
}

func (c *cacheManager) filterResults(m map[string]Result, ck *CacheKey) (results []LoadedResult, err error) {
	id := c.getID(ck)
	if err := c.backend.WalkResults(id, func(cr CacheResult) error {
		res, ok := m[id]
		if ok {
			results = append(results, LoadedResult{
				Result:      res,
				CacheKey:    ck,
				CacheResult: cr,
			})
			delete(m, id)
		}
		return nil
	}); err != nil {
		for _, r := range results {
			r.Result.Release(context.TODO())
		}
	}
	for _, keys := range ck.Deps() {
		for _, key := range keys {
			res, err := c.filterResults(m, key.CacheKey.CacheKey)
			if err != nil {
				for _, r := range results {
					r.Result.Release(context.TODO())
				}
				return nil, err
			}
			results = append(results, res...)
		}
	}
	return
}

func (c *cacheManager) LoadWithParents(ctx context.Context, rec *CacheRecord) ([]LoadedResult, error) {
	lwp, ok := c.results.(interface {
		LoadWithParents(context.Context, CacheResult) (map[string]Result, error)
	})
	if !ok {
		res, err := c.Load(ctx, rec)
		if err != nil {
			return nil, err
		}
		return []LoadedResult{{Result: res, CacheKey: rec.key, CacheResult: CacheResult{ID: c.getID(rec.key), CreatedAt: rec.CreatedAt}}}, nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	cr, err := c.backend.Load(c.getID(rec.key), rec.ID)
	if err != nil {
		return nil, err
	}

	m, err := lwp.LoadWithParents(ctx, cr)
	if err != nil {
		return nil, err
	}

	results, err := c.filterResults(m, rec.key)
	if err != nil {
		for _, r := range m {
			r.Release(context.TODO())
		}
	}

	return results, nil
}

func (c *cacheManager) Save(k *CacheKey, r Result, createdAt time.Time) (*ExportableCacheKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	res, err := c.results.Save(r, createdAt)
	if err != nil {
		return nil, err
	}
	if err := c.backend.AddResult(c.getID(k), res); err != nil {
		return nil, err
	}

	if err := c.ensurePersistentKey(k); err != nil {
		return nil, err
	}

	rec := &CacheRecord{
		ID:           res.ID,
		cacheManager: c,
		key:          k,
		CreatedAt:    res.CreatedAt,
	}

	return &ExportableCacheKey{
		CacheKey: k,
		Exporter: &exporter{k: k, record: rec},
	}, nil
}

func newKey() *CacheKey {
	return &CacheKey{ids: map[*cacheManager]string{}}
}

func (c *cacheManager) newKeyWithID(id string, dgst digest.Digest, output Index) *CacheKey {
	k := newKey()
	k.digest = dgst
	k.output = output
	k.ID = id
	k.ids[c] = id
	return k
}

func (c *cacheManager) newRootKey(dgst digest.Digest, output Index) *CacheKey {
	return c.newKeyWithID(rootKey(dgst, output).String(), dgst, output)
}

func (c *cacheManager) getID(k *CacheKey) string {
	k.mu.Lock()
	id, ok := k.ids[c]
	if ok {
		k.mu.Unlock()
		return id
	}
	if len(k.deps) == 0 {
		k.ids[c] = k.ID
		k.mu.Unlock()
		return k.ID
	}
	id = c.getIDFromDeps(k)
	k.ids[c] = id
	k.mu.Unlock()
	return id
}

func (c *cacheManager) ensurePersistentKey(k *CacheKey) error {
	id := c.getID(k)
	for i, deps := range k.Deps() {
		for _, ck := range deps {
			l := CacheInfoLink{
				Input:    Index(i),
				Output:   Index(k.Output()),
				Digest:   k.Digest(),
				Selector: ck.Selector,
			}
			ckID := c.getID(ck.CacheKey.CacheKey)
			if !c.backend.HasLink(ckID, l, id) {
				if err := c.ensurePersistentKey(ck.CacheKey.CacheKey); err != nil {
					return err
				}
				if err := c.backend.AddLink(ckID, l, id); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (c *cacheManager) getIDFromDeps(k *CacheKey) string {
	matches := map[string]struct{}{}

	for i, deps := range k.deps {
		if i == 0 || len(matches) > 0 {
			for _, ck := range deps {
				m2 := make(map[string]struct{})
				if err := c.backend.WalkLinks(c.getID(ck.CacheKey.CacheKey), CacheInfoLink{
					Input:    Index(i),
					Output:   Index(k.Output()),
					Digest:   k.Digest(),
					Selector: ck.Selector,
				}, func(id string) error {
					if i == 0 {
						matches[id] = struct{}{}
					} else {
						m2[id] = struct{}{}
					}
					return nil
				}); err != nil {
					matches = map[string]struct{}{}
					break
				}
				if i != 0 {
					for id := range matches {
						if _, ok := m2[id]; !ok {
							delete(matches, id)
						}
					}
				}
			}
		}
	}

	for k := range matches {
		return k
	}

	return identity.NewID()
}

func rootKey(dgst digest.Digest, output Index) digest.Digest {
	if strings.HasPrefix(dgst.String(), "random:") {
		return digest.Digest("random:" + strings.TrimPrefix(digest.FromBytes([]byte(fmt.Sprintf("%s@%d", dgst, output))).String(), digest.Canonical.String()+":"))
	}
	return digest.FromBytes([]byte(fmt.Sprintf("%s@%d", dgst, output)))
}
