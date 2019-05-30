package snapshot

import (
	"context"
	"os"
	"path/filepath"

	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/pkg/errors"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/sync/errgroup"
)

func (s *snapshotter) GetDiffIDs(ctx context.Context, key string) ([]layer.DiffID, error) {
	if l, err := s.getLayer(key, true); err != nil {
		return nil, err
	} else if l != nil {
		return getDiffChain(l), nil
	}
	return nil, nil
}

func (s *snapshotter) EnsureLayer(ctx context.Context, key string) ([]layer.DiffID, error) {
	diffIDs, err := s.GetDiffIDs(ctx, key)
	if err != nil {
		return nil, err
	} else if diffIDs != nil {
		return diffIDs, nil
	}

	id, committed := s.getGraphDriverID(key)
	if !committed {
		return nil, errors.Errorf("can not convert active %s to layer", key)
	}

	info, err := s.Stat(ctx, key)
	if err != nil {
		return nil, err
	}

	eg, gctx := errgroup.WithContext(ctx)

	// TODO: add flightcontrol

	var parentChainID layer.ChainID
	if info.Parent != "" {
		eg.Go(func() error {
			diffIDs, err := s.EnsureLayer(gctx, info.Parent)
			if err != nil {
				return err
			}
			parentChainID = layer.CreateChainID(diffIDs)
			return nil
		})
	}

	tmpDir, err := ioutils.TempDir("", "docker-tarsplit")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)
	tarSplitPath := filepath.Join(tmpDir, "tar-split")

	var diffID layer.DiffID
	var size int64
	eg.Go(func() error {
		parent := ""
		if p := info.Parent; p != "" {
			if l, err := s.getLayer(p, true); err != nil {
				return err
			} else if l != nil {
				parent, err = getGraphID(l)
				if err != nil {
					return err
				}
			} else {
				parent, _ = s.getGraphDriverID(info.Parent)
			}
		}
		diffID, size, err = s.reg.ChecksumForGraphID(id, parent, "", tarSplitPath)
		return err
	})

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	l, err := s.reg.RegisterByGraphID(id, parentChainID, diffID, tarSplitPath, size)
	if err != nil {
		return nil, err
	}

	if err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(key))
		b.Put(keyChainID, []byte(l.ChainID()))
		return nil
	}); err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.refs[key] = l
	s.mu.Unlock()

	return getDiffChain(l), nil
}

func getDiffChain(l layer.Layer) []layer.DiffID {
	if p := l.Parent(); p != nil {
		return append(getDiffChain(p), l.DiffID())
	}
	return []layer.DiffID{l.DiffID()}
}

func getGraphID(l layer.Layer) (string, error) {
	if l, ok := l.(interface {
		CacheID() string
	}); ok {
		return l.CacheID(), nil
	}
	return "", errors.Errorf("couldn't access cacheID for %s", l.ChainID())
}
