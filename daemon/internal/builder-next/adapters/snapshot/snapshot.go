package snapshot

import (
	"context"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/util/leaseutil"
	"github.com/moby/locker"
	"github.com/moby/moby/v2/daemon/graphdriver"
	"github.com/moby/moby/v2/daemon/internal/layer"
	"github.com/moby/sys/user"
	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	bolt "go.etcd.io/bbolt"
)

var (
	keyParent      = []byte("parent")
	keyCommitted   = []byte("committed")
	keyIsCommitted = []byte("iscommitted")
	keyChainID     = []byte("chainid")
	keySize        = []byte("size")
)

// Opt defines options for creating the snapshotter
type Opt struct {
	GraphDriver     graphdriver.Driver
	LayerStore      layer.Store
	Root            string
	IdentityMapping user.IdentityMapping
}

type graphIDRegistrar interface {
	RegisterByGraphID(string, layer.ChainID, layer.DiffID, string, int64) (layer.Layer, error)
	Release(layer.Layer) ([]layer.Metadata, error)
	checksumCalculator
}

type checksumCalculator interface {
	ChecksumForGraphID(id, parent, newTarDataPath string) (diffID layer.DiffID, size int64, err error)
}

type snapshotter struct {
	opt Opt

	refs              map[string]layer.Layer
	db                *bolt.DB
	mu                sync.Mutex
	reg               graphIDRegistrar
	layerCreateLocker *locker.Locker
}

// NewSnapshotter creates a new snapshotter
func NewSnapshotter(opt Opt, prevLM leases.Manager, ns string) (_ snapshot.Snapshotter, _ *leaseutil.Manager, retErr error) {
	dbPath := filepath.Join(opt.Root, "snapshots.db")
	db, err := bolt.Open(dbPath, 0o600, &bolt.Options{
		FreelistType: bolt.FreelistMapType,
	})
	if err != nil {
		return nil, nil, errors.Wrapf(err, "failed to open database file %s", dbPath)
	}
	defer func() {
		if retErr != nil {
			_ = db.Close()
		}
	}()

	reg, ok := opt.LayerStore.(graphIDRegistrar)
	if !ok {
		return nil, nil, errors.Errorf("layerstore doesn't support graphID registration")
	}

	s := &snapshotter{
		opt:               opt,
		db:                db,
		refs:              map[string]layer.Layer{},
		reg:               reg,
		layerCreateLocker: locker.New(),
	}

	slm := newLeaseManager(s, prevLM)
	lm := leaseutil.WithNamespace(slm, ns)

	ll, err := lm.List(context.TODO())
	if err != nil {
		return nil, nil, err
	}
	for _, l := range ll {
		rr, err := lm.ListResources(context.TODO(), l)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range rr {
			if r.Type == "snapshots/default" {
				slm.addRef(l.ID, r.ID)
			}
		}
	}

	return s, lm, nil
}

func (s *snapshotter) Name() string {
	return "default"
}

func (s *snapshotter) IdentityMapping() *user.IdentityMapping {
	// Returning a non-nil but empty *IdentityMapping breaks BuildKit:
	// https://github.com/moby/moby/pull/39444
	if s.opt.IdentityMapping.Empty() {
		return nil
	}
	return &s.opt.IdentityMapping
}

func (s *snapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) error {
	origParent := parent
	if parent != "" {
		if l, err := s.getLayer(parent, false); err != nil {
			return errors.Wrapf(err, "failed to get parent layer %s", parent)
		} else if l != nil {
			parent, err = getGraphID(l)
			if err != nil {
				return errors.Wrapf(err, "failed to get parent graphid %s", l.ChainID())
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
	if chainID, ok := snapshot.ParseLayerSnapshotID(key); ok {
		if chainID.Algorithm() != digest.SHA256 {
			return "", false
		}
		return chainID, true
	}

	chainID, err := digest.Parse(key)
	if err != nil || chainID.Algorithm() != digest.SHA256 {
		return "", false
	}
	return chainID, true
}

func layerParentSnapshotID(key string, chainID layer.ChainID) string {
	if _, ok := snapshot.ParseLayerSnapshotID(key); ok {
		return snapshot.LayerSnapshotID(chainID)
	}
	return chainID.String()
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
				return nil, errors.WithStack(err)
			}
			s.mu.Unlock()
			if id == "" {
				return nil, nil
			}
			return s.getLayer(string(id), withCommitted)
		}
		var err error
		l, err = s.opt.LayerStore.Get(id)
		if err != nil {
			s.mu.Unlock()
			return nil, errors.WithStack(err)
		}
		s.refs[key] = l
		if err := s.db.Update(func(tx *bolt.Tx) error {
			_, err := tx.CreateBucketIfNotExists([]byte(key))
			return errors.WithStack(err)
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
			return errors.Wrapf(cerrdefs.ErrNotFound, "key %s", key)
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
			inf.Parent = layerParentSnapshotID(key, p.ChainID())
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
			return errors.Wrapf(cerrdefs.ErrNotFound, "snapshot %s", id)
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
				inf.Parent = layerParentSnapshotID(key, p.ChainID())
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
			idmap: s.opt.IdentityMapping,
			acquire: func() ([]mount.Mount, func() error, error) {
				rwlayer, err = s.opt.LayerStore.CreateRWLayer(id, l.ChainID(), nil)
				if err != nil {
					return nil, nil, err
				}
				rootfs, err := rwlayer.Mount("")
				if err != nil {
					return nil, nil, err
				}
				return []mount.Mount{{
					Source:  rootfs,
					Type:    "bind",
					Options: []string{"rbind"},
				}}, func() error {
					_, err := s.opt.LayerStore.ReleaseRWLayer(rwlayer)
					return err
				}, nil
			},
		}, nil
	}

	id, _ := s.getGraphDriverID(key)

	return &mountable{
		idmap: s.opt.IdentityMapping,
		acquire: func() ([]mount.Mount, func() error, error) {
			rootfs, err := s.opt.GraphDriver.Get(id, "")
			if err != nil {
				return nil, nil, err
			}
			return []mount.Mount{{
				Source:  rootfs,
				Type:    "bind",
				Options: []string{"rbind"},
			}}, func() error {
				return s.opt.GraphDriver.Put(id)
			}, nil
		},
	}, nil
}

func (s *snapshotter) Remove(ctx context.Context, key string) error {
	return errors.Errorf("calling snapshot.remove is forbidden")
}

func (s *snapshotter) remove(ctx context.Context, key string) error {
	l, err := s.getLayer(key, true)
	if err != nil {
		return err
	}

	id, _ := s.getGraphDriverID(key)

	var found bool
	var alreadyCommitted bool
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(key))
		found = b != nil

		if b != nil {
			if b.Get(keyIsCommitted) != nil {
				alreadyCommitted = true
				return nil
			}
		}
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

	if alreadyCommitted {
		return nil
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
		if err := b.Put(keyCommitted, []byte(key)); err != nil {
			return err
		}
		b, err = tx.CreateBucketIfNotExists([]byte(key))
		if err != nil {
			return err
		}
		return b.Put(keyIsCommitted, []byte{})
	})
}

func (s *snapshotter) View(ctx context.Context, key, parent string, opts ...snapshots.Opt) (snapshot.Mountable, error) {
	return s.Mounts(ctx, parent)
}

func (s *snapshotter) Walk(context.Context, snapshots.WalkFunc, ...string) error {
	return nil
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
		usage.Size = l.DiffSize()
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

	diffSize, err := s.opt.GraphDriver.DiffSize(ctx, id, parent)
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
	acquire  func() ([]mount.Mount, func() error, error)
	release  func() error
	refCount int
	idmap    user.IdentityMapping
}

func (m *mountable) Mount() ([]mount.Mount, func() error, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.mounts != nil {
		m.refCount++
		return m.mounts, m.releaseMount, nil
	}

	mounts, release, err := m.acquire()
	if err != nil {
		return nil, nil, err
	}
	m.mounts = mounts
	m.release = release
	m.refCount = 1

	return m.mounts, m.releaseMount, nil
}

func (m *mountable) releaseMount() error {
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
	defer func() {
		m.release = nil
	}()
	return m.release()
}

func (m *mountable) IdentityMapping() *user.IdentityMapping {
	// Returning a non-nil but empty *IdentityMapping breaks BuildKit:
	// https://github.com/moby/moby/pull/39444
	if m.idmap.Empty() {
		return nil
	}
	return &m.idmap
}
