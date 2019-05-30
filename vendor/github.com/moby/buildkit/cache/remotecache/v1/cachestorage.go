package cacheimport

import (
	"context"
	"time"

	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/worker"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

func NewCacheKeyStorage(cc *CacheChains, w worker.Worker) (solver.CacheKeyStorage, solver.CacheResultStorage, error) {
	storage := &cacheKeyStorage{
		byID:     map[string]*itemWithOutgoingLinks{},
		byItem:   map[*item]string{},
		byResult: map[string]map[string]struct{}{},
	}

	for _, it := range cc.items {
		if _, err := addItemToStorage(storage, it); err != nil {
			return nil, nil, err
		}
	}

	results := &cacheResultStorage{
		w:        w,
		byID:     storage.byID,
		byItem:   storage.byItem,
		byResult: storage.byResult,
	}

	return storage, results, nil
}

func addItemToStorage(k *cacheKeyStorage, it *item) (*itemWithOutgoingLinks, error) {
	if id, ok := k.byItem[it]; ok {
		if id == "" {
			return nil, errors.Errorf("invalid loop")
		}
		return k.byID[id], nil
	}

	var id string
	if len(it.links) == 0 {
		id = it.dgst.String()
	} else {
		id = identity.NewID()
	}

	k.byItem[it] = ""

	for i, m := range it.links {
		for l := range m {
			src, err := addItemToStorage(k, l.src)
			if err != nil {
				return nil, err
			}
			cl := nlink{
				input:    i,
				dgst:     it.dgst,
				selector: l.selector,
			}
			src.links[cl] = append(src.links[cl], id)
		}
	}

	k.byItem[it] = id

	itl := &itemWithOutgoingLinks{
		item:  it,
		links: map[nlink][]string{},
	}

	k.byID[id] = itl

	if res := it.result; res != nil {
		resultID := remoteID(res)
		ids, ok := k.byResult[resultID]
		if !ok {
			ids = map[string]struct{}{}
			k.byResult[resultID] = ids
		}
		ids[id] = struct{}{}
	}
	return itl, nil
}

type cacheKeyStorage struct {
	byID     map[string]*itemWithOutgoingLinks
	byItem   map[*item]string
	byResult map[string]map[string]struct{}
}

type itemWithOutgoingLinks struct {
	*item
	links map[nlink][]string
}

func (cs *cacheKeyStorage) Exists(id string) bool {
	_, ok := cs.byID[id]
	return ok
}

func (cs *cacheKeyStorage) Walk(func(id string) error) error {
	return nil
}

func (cs *cacheKeyStorage) WalkResults(id string, fn func(solver.CacheResult) error) error {
	it, ok := cs.byID[id]
	if !ok {
		return nil
	}
	if res := it.result; res != nil {
		return fn(solver.CacheResult{ID: remoteID(res), CreatedAt: it.resultTime})
	}
	return nil
}

func (cs *cacheKeyStorage) Load(id string, resultID string) (solver.CacheResult, error) {
	it, ok := cs.byID[id]
	if !ok {
		return solver.CacheResult{}, nil
	}
	if res := it.result; res != nil {
		return solver.CacheResult{ID: remoteID(res), CreatedAt: it.resultTime}, nil
	}
	return solver.CacheResult{}, nil
}

func (cs *cacheKeyStorage) AddResult(id string, res solver.CacheResult) error {
	return nil
}

func (cs *cacheKeyStorage) Release(resultID string) error {
	return nil
}
func (cs *cacheKeyStorage) AddLink(id string, link solver.CacheInfoLink, target string) error {
	return nil
}
func (cs *cacheKeyStorage) WalkLinks(id string, link solver.CacheInfoLink, fn func(id string) error) error {
	it, ok := cs.byID[id]
	if !ok {
		return nil
	}
	for _, id := range it.links[nlink{
		dgst:     outputKey(link.Digest, int(link.Output)),
		input:    int(link.Input),
		selector: link.Selector.String(),
	}] {
		if err := fn(id); err != nil {
			return err
		}
	}
	return nil
}

func (cs *cacheKeyStorage) WalkBacklinks(id string, fn func(id string, link solver.CacheInfoLink) error) error {
	for k, it := range cs.byID {
		for nl, ids := range it.links {
			for _, id2 := range ids {
				if id == id2 {
					if err := fn(k, solver.CacheInfoLink{
						Input:    solver.Index(nl.input),
						Selector: digest.Digest(nl.selector),
						Digest:   nl.dgst,
					}); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (cs *cacheKeyStorage) WalkIDsByResult(id string, fn func(id string) error) error {
	ids := cs.byResult[id]
	for id := range ids {
		if err := fn(id); err != nil {
			return err
		}
	}
	return nil
}

func (cs *cacheKeyStorage) HasLink(id string, link solver.CacheInfoLink, target string) bool {
	l := nlink{
		dgst:     outputKey(link.Digest, int(link.Output)),
		input:    int(link.Input),
		selector: link.Selector.String(),
	}
	if it, ok := cs.byID[id]; ok {
		for _, id := range it.links[l] {
			if id == target {
				return true
			}
		}
	}
	return false
}

type cacheResultStorage struct {
	w        worker.Worker
	byID     map[string]*itemWithOutgoingLinks
	byResult map[string]map[string]struct{}
	byItem   map[*item]string
}

func (cs *cacheResultStorage) Save(res solver.Result, createdAt time.Time) (solver.CacheResult, error) {
	return solver.CacheResult{}, errors.Errorf("importer is immutable")
}

func (cs *cacheResultStorage) LoadWithParents(ctx context.Context, res solver.CacheResult) (map[string]solver.Result, error) {
	v := cs.byResultID(res.ID)
	if v == nil || v.result == nil {
		return nil, errors.WithStack(solver.ErrNotFound)
	}

	m := map[string]solver.Result{}

	if err := v.walkAllResults(func(i *item) error {
		if i.result == nil {
			return nil
		}
		id, ok := cs.byItem[i]
		if !ok {
			return nil
		}
		if isSubRemote(*i.result, *v.result) {
			ref, err := cs.w.FromRemote(ctx, i.result)
			if err != nil {
				return err
			}
			m[id] = worker.NewWorkerRefResult(ref, cs.w)
		}
		return nil
	}); err != nil {
		for _, v := range m {
			v.Release(context.TODO())
		}
		return nil, err
	}

	return m, nil
}

func (cs *cacheResultStorage) Load(ctx context.Context, res solver.CacheResult) (solver.Result, error) {
	item := cs.byResultID(res.ID)
	if item == nil || item.result == nil {
		return nil, errors.WithStack(solver.ErrNotFound)
	}

	ref, err := cs.w.FromRemote(ctx, item.result)
	if err != nil {
		return nil, err
	}
	return worker.NewWorkerRefResult(ref, cs.w), nil
}

func (cs *cacheResultStorage) LoadRemote(ctx context.Context, res solver.CacheResult) (*solver.Remote, error) {
	if r := cs.byResultID(res.ID); r != nil && r.result != nil {
		return r.result, nil
	}
	return nil, errors.WithStack(solver.ErrNotFound)
}

func (cs *cacheResultStorage) Exists(id string) bool {
	return cs.byResultID(id) != nil
}

func (cs *cacheResultStorage) byResultID(resultID string) *itemWithOutgoingLinks {
	m, ok := cs.byResult[resultID]
	if !ok || len(m) == 0 {
		return nil
	}

	for id := range m {
		it, ok := cs.byID[id]
		if ok {
			return it
		}
	}

	return nil
}

// unique ID per remote. this ID is not stable.
func remoteID(r *solver.Remote) string {
	dgstr := digest.Canonical.Digester()
	for _, desc := range r.Descriptors {
		dgstr.Hash().Write([]byte(desc.Digest))
	}
	return dgstr.Digest().String()
}
