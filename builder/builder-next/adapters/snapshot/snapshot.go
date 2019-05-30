package snapshot

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots"
	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/idtools"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/snapshot"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	bolt "go.etcd.io/bbolt"
)

var keyParent = []byte("parent")
var keyCommitted = []byte("committed")
var keyChainID = []byte("chainid")
var keySize = []byte("size")

// Opt defines options for creating the snapshotter
type Opt struct {
	GraphDriver graphdriver.Driver
	LayerStore  layer.Store
	Root        string
}

type graphIDRegistrar interface {
	RegisterByGraphID(string, layer.ChainID, layer.DiffID, string, int64) (layer.Layer, error)
	Release(layer.Layer) ([]layer.Metadata, error)
	checksumCalculator
}

type checksumCalculator interface {
	ChecksumForGraphID(id, parent, oldTarDataPath, newTarDataPath string) (diffID layer.DiffID, size int64, err error)
}

type snapshotter struct {
	opt Opt

	refs map[string]layer.Layer
	db   *bolt.DB
	mu   sync.Mutex
	reg  graphIDRegistrar
}

var _ snapshot.SnapshotterBase = &snapshotter{}

// NewSnapshotter creates a new snapshotter
func NewSnapshotter(opt Opt) (snapshot.SnapshotterBase, error) {
	dbPath := filepath.Join(opt.Root, "snapshots.db")
	db, err := bolt.Open(dbPath, 0600, nil)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to open database file %s", dbPath)
	}

	reg, ok := opt.LayerStore.(graphIDRegistrar)
	if !ok {
		return nil, errors.Errorf("layerstore doesn't support graphID registration")
	}

	s := &snapshotter{
		opt:  opt,
		db:   db,
		refs: map[string]layer.Layer{},
		reg:  reg,
	}
	return s, nil
}

func (s *snapshotter) Name() string {
	return "default"
}

func (s *snapshotter) IdentityMapping() *idtools.IdentityMapping {
	return nil
}

func (s *snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) error {
	origParent := parent
	if parent != "" {
		if l, err := s.getLayer(parent, false); err != nil {
			return err
		} else if l != nil {
			parent, err = getGraphID(l)
			if err != nil {
				return err
			}
		} else {
			parent, _ = s.getGraphDriverID(parent)
		}
	}
	if err := s.opt.GraphDriver.Create(key, parent, nil); err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(key))
		if err != nil {
			return err
		}
		return b.Put(keyParent, []byte(origParent))
	})
}

func (s *snapshotter) chainID(key string) (layer.ChainID, bool) {
	if strings.HasPrefix(key, "sha256:") {
		dgst, err := digest.Parse(key)
		if err != nil {
			return "", false
		}
		return layer.ChainID(dgst), true
	}
	return "", false
}

func (s *snapshotter) GetLayer(key string) (layer.Layer, error) {
	return s.getLayer(key, true)
}

func (s *snapshotter) getLayer(key string, withCommitted bool) (layer.Layer, error) {
	s.mu.Lock()
	l, ok := s.refs[key]
	if !ok {
		id, ok := s.chainID(key)
		if !ok {
			if !withCommitted {
				s.mu.Unlock()
				return nil, nil
			}
			if err := s.db.View(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte(key))
				if b == nil {
					return nil
				}
				v := b.Get(keyChainID)
				if v != nil {
					id = layer.ChainID(v)
				}
				return nil
			}); err != nil {
				s.mu.Unlock()
				return nil, err
			}
			if id == "" {
				s.mu.Unlock()
				return nil, nil
			}
		}
		var err error
		l, err = s.opt.LayerStore.Get(id)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		s.refs[key] = l
		if err := s.db.Update(func(tx *bolt.Tx) error {
			_, err := tx.CreateBucketIfNotExists([]byte(key))
			return err
		}); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	s.mu.Unlock()

	return l, nil
}

func (s *snapshotter) getGraphDriverID(key string) (string, bool) {
	var gdID string
	if err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(key))
		if b == nil {
			return errors.Errorf("not found") // TODO: typed
		}
		v := b.Get(keyCommitted)
		if v != nil {
			gdID = string(v)
		}
		return nil
	}); err != nil || gdID == "" {
		return key, false
	}
	return gdID, true
}

func (s *snapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	inf := snapshots.Info{
		Kind: snapshots.KindActive,
	}

	l, err := s.getLayer(key, false)
	if err != nil {
		return snapshots.Info{}, err
	}
	if l != nil {
		if p := l.Parent(); p != nil {
			inf.Parent = p.ChainID().String()
		}
		inf.Kind = snapshots.KindCommitted
		inf.Name = key
		return inf, nil
	}

	l, err = s.getLayer(key, true)
	if err != nil {
		return snapshots.Info{}, err
	}

	id, committed := s.getGraphDriverID(key)
	if committed {
		inf.Kind = snapshots.KindCommitted
	}

	if err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(id))
		if b == nil && l == nil {
			return errors.Errorf("snapshot %s not found", id) // TODO: typed
		}
		inf.Name = key
		if b != nil {
			v := b.Get(keyParent)
			if v != nil {
				inf.Parent = string(v)
				return nil
			}
		}
		if l != nil {
			if p := l.Parent(); p != nil {
				inf.Parent = p.ChainID().String()
			}
			inf.Kind = snapshots.KindCommitted
		}
		return nil
	}); err != nil {
		return snapshots.Info{}, err
	}
	return inf, nil
}

func (s *snapshotter) Mounts(ctx context.Context, key string) (snapshot.Mountable, error) {
	l, err := s.getLayer(key, true)
	if err != nil {
		return nil, err
	}
	if l != nil {
		id := identity.NewID()
		var rwlayer layer.RWLayer
		return &mountable{
			acquire: func() ([]mount.Mount, error) {
				rwlayer, err = s.opt.LayerStore.CreateRWLayer(id, l.ChainID(), nil)
				if err != nil {
					return nil, err
				}
				rootfs, err := rwlayer.Mount("")
				if err != nil {
					return nil, err
				}
				return []mount.Mount{{
					Source:  rootfs.Path(),
					Type:    "bind",
					Options: []string{"rbind"},
				}}, nil
			},
			release: func() error {
				_, err := s.opt.LayerStore.ReleaseRWLayer(rwlayer)
				return err
			},
		}, nil
	}

	id, _ := s.getGraphDriverID(key)

	return &mountable{
		acquire: func() ([]mount.Mount, error) {
			rootfs, err := s.opt.GraphDriver.Get(id, "")
			if err != nil {
				return nil, err
			}
			return []mount.Mount{{
				Source:  rootfs.Path(),
				Type:    "bind",
				Options: []string{"rbind"},
			}}, nil
		},
		release: func() error {
			return s.opt.GraphDriver.Put(id)
		},
	}, nil
}

func (s *snapshotter) Remove(ctx context.Context, key string) error {
	l, err := s.getLayer(key, true)
	if err != nil {
		return err
	}

	id, _ := s.getGraphDriverID(key)

	var found bool
	if err := s.db.Update(func(tx *bolt.Tx) error {
		found = tx.Bucket([]byte(key)) != nil
		if found {
			tx.DeleteBucket([]byte(key))
			if id != key {
				tx.DeleteBucket([]byte(id))
			}
		}
		return nil
	}); err != nil {
		return err
	}

	if l != nil {
		s.mu.Lock()
		delete(s.refs, key)
		s.mu.Unlock()
		_, err := s.opt.LayerStore.Release(l)
		return err
	}

	if !found { // this happens when removing views
		return nil
	}

	return s.opt.GraphDriver.Remove(id)
}

func (s *snapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(name))
		if err != nil {
			return err
		}
		return b.Put(keyCommitted, []byte(key))
	})
}

func (s *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) (snapshot.Mountable, error) {
	return s.Mounts(ctx, parent)
}

func (s *snapshotter) Walk(ctx context.Context, fn func(context.Context, snapshots.Info) error) error {
	return errors.Errorf("not-implemented")
}

func (s *snapshotter) Update(ctx context.Context, info snapshots.Info, fieldpaths ...string) (snapshots.Info, error) {
	// not implemented
	return s.Stat(ctx, info.Name)
}

func (s *snapshotter) Usage(ctx context.Context, key string) (us snapshots.Usage, retErr error) {
	usage := snapshots.Usage{}
	if l, err := s.getLayer(key, true); err != nil {
		return usage, err
	} else if l != nil {
		s, err := l.DiffSize()
		if err != nil {
			return usage, err
		}
		usage.Size = s
		return usage, nil
	}

	size := int64(-1)
	if err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(key))
		if b == nil {
			return nil
		}
		v := b.Get(keySize)
		if v != nil {
			s, err := strconv.Atoi(string(v))
			if err != nil {
				return err
			}
			size = int64(s)
		}
		return nil
	}); err != nil {
		return usage, err
	}

	if size != -1 {
		usage.Size = size
		return usage, nil
	}

	id, _ := s.getGraphDriverID(key)

	info, err := s.Stat(ctx, key)
	if err != nil {
		return usage, err
	}
	var parent string
	if info.Parent != "" {
		if l, err := s.getLayer(info.Parent, false); err != nil {
			return usage, err
		} else if l != nil {
			parent, err = getGraphID(l)
			if err != nil {
				return usage, err
			}
		} else {
			parent, _ = s.getGraphDriverID(info.Parent)
		}
	}

	diffSize, err := s.opt.GraphDriver.DiffSize(id, parent)
	if err != nil {
		return usage, err
	}

	if err := s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(key))
		if err != nil {
			return err
		}
		return b.Put(keySize, []byte(strconv.Itoa(int(diffSize))))
	}); err != nil {
		return usage, err
	}
	usage.Size = diffSize
	return usage, nil
}

func (s *snapshotter) Close() error {
	return s.db.Close()
}

type mountable struct {
	mu       sync.Mutex
	mounts   []mount.Mount
	acquire  func() ([]mount.Mount, error)
	release  func() error
	refCount int
}

func (m *mountable) Mount() ([]mount.Mount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.mounts != nil {
		m.refCount++
		return m.mounts, nil
	}

	mounts, err := m.acquire()
	if err != nil {
		return nil, err
	}
	m.mounts = mounts
	m.refCount = 1

	return m.mounts, nil
}

func (m *mountable) Release() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.refCount > 1 {
		m.refCount--
		return nil
	}

	m.refCount = 0
	if m.release == nil {
		return nil
	}

	m.mounts = nil
	return m.release()
}

func (m *mountable) IdentityMapping() *idtools.IdentityMapping {
	return nil
}
