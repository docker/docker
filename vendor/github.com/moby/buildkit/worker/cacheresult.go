package worker

import (
	"context"
	"strings"
	"time"

	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/solver"
	"github.com/pkg/errors"
)

func NewCacheResultStorage(wc *Controller) solver.CacheResultStorage {
	return &cacheResultStorage{
		wc: wc,
	}
}

type cacheResultStorage struct {
	wc *Controller
}

func (s *cacheResultStorage) Save(res solver.Result, createdAt time.Time) (solver.CacheResult, error) {
	ref, ok := res.Sys().(*WorkerRef)
	if !ok {
		return solver.CacheResult{}, errors.Errorf("invalid result: %T", res.Sys())
	}
	if ref.ImmutableRef != nil {
		if !cache.HasCachePolicyRetain(ref.ImmutableRef) {
			if err := cache.CachePolicyRetain(ref.ImmutableRef); err != nil {
				return solver.CacheResult{}, err
			}
			ref.ImmutableRef.Metadata().Commit()
		}
	}
	return solver.CacheResult{ID: ref.ID(), CreatedAt: createdAt}, nil
}
func (s *cacheResultStorage) Load(ctx context.Context, res solver.CacheResult) (solver.Result, error) {
	return s.load(res.ID, false)
}

func (s *cacheResultStorage) getWorkerRef(id string) (Worker, string, error) {
	workerID, refID, err := parseWorkerRef(id)
	if err != nil {
		return nil, "", err
	}
	w, err := s.wc.Get(workerID)
	if err != nil {
		return nil, "", err
	}
	return w, refID, nil
}

func (s *cacheResultStorage) load(id string, hidden bool) (solver.Result, error) {
	w, refID, err := s.getWorkerRef(id)
	if err != nil {
		return nil, err
	}
	if refID == "" {
		return NewWorkerRefResult(nil, w), nil
	}
	ref, err := w.LoadRef(refID, hidden)
	if err != nil {
		return nil, err
	}
	return NewWorkerRefResult(ref, w), nil
}

func (s *cacheResultStorage) LoadRemote(ctx context.Context, res solver.CacheResult) (*solver.Remote, error) {
	w, refID, err := s.getWorkerRef(res.ID)
	if err != nil {
		return nil, err
	}
	ref, err := w.LoadRef(refID, true)
	if err != nil {
		return nil, err
	}
	defer ref.Release(context.TODO())
	remote, err := w.GetRemote(ctx, ref, false)
	if err != nil {
		return nil, nil // ignore error. loadRemote is best effort
	}
	return remote, nil
}
func (s *cacheResultStorage) Exists(id string) bool {
	ref, err := s.load(id, true)
	if err != nil {
		return false
	}
	ref.Release(context.TODO())
	return true
}

func parseWorkerRef(id string) (string, string, error) {
	parts := strings.Split(id, "::")
	if len(parts) != 2 {
		return "", "", errors.Errorf("invalid workerref id: %s", id)
	}
	return parts[0], parts[1], nil
}
