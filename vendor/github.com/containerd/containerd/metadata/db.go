/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package metadata

import (
	"context"
	"encoding/binary"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/gc"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/snapshots"
	"github.com/pkg/errors"
	bolt "go.etcd.io/bbolt"
)

const (
	// schemaVersion represents the schema version of
	// the database. This schema version represents the
	// structure of the data in the database. The schema
	// can envolve at any time but any backwards
	// incompatible changes or structural changes require
	// bumping the schema version.
	schemaVersion = "v1"

	// dbVersion represents updates to the schema
	// version which are additions and compatible with
	// prior version of the same schema.
	dbVersion = 3
)

// DBOpt configures how we set up the DB
type DBOpt func(*dbOptions)

// WithPolicyIsolated isolates contents between namespaces
func WithPolicyIsolated(o *dbOptions) {
	o.shared = false
}

// dbOptions configure db options.
type dbOptions struct {
	shared bool
}

// DB represents a metadata database backed by a bolt
// database. The database is fully namespaced and stores
// image, container, namespace, snapshot, and content data
// while proxying data shared across namespaces to backend
// datastores for content and snapshots.
type DB struct {
	db *bolt.DB
	ss map[string]*snapshotter
	cs *contentStore

	// wlock is used to protect access to the data structures during garbage
	// collection. While the wlock is held no writable transactions can be
	// opened, preventing changes from occurring between the mark and
	// sweep phases without preventing read transactions.
	wlock sync.RWMutex

	// dirty flags and lock keeps track of datastores which have had deletions
	// since the last garbage collection. These datastores will will be garbage
	// collected during the next garbage collection.
	dirtyL  sync.Mutex
	dirtySS map[string]struct{}
	dirtyCS bool

	// mutationCallbacks are called after each mutation with the flag
	// set indicating whether any dirty flags are set
	mutationCallbacks []func(bool)

	dbopts dbOptions
}

// NewDB creates a new metadata database using the provided
// bolt database, content store, and snapshotters.
func NewDB(db *bolt.DB, cs content.Store, ss map[string]snapshots.Snapshotter, opts ...DBOpt) *DB {
	m := &DB{
		db:      db,
		ss:      make(map[string]*snapshotter, len(ss)),
		dirtySS: map[string]struct{}{},
		dbopts: dbOptions{
			shared: true,
		},
	}

	for _, opt := range opts {
		opt(&m.dbopts)
	}

	// Initialize data stores
	m.cs = newContentStore(m, m.dbopts.shared, cs)
	for name, sn := range ss {
		m.ss[name] = newSnapshotter(m, name, sn)
	}

	return m
}

// Init ensures the database is at the correct version
// and performs any needed migrations.
func (m *DB) Init(ctx context.Context) error {
	// errSkip is used when no migration or version needs to be written
	// to the database and the transaction can be immediately rolled
	// back rather than performing a much slower and unnecessary commit.
	var errSkip = errors.New("skip update")

	err := m.db.Update(func(tx *bolt.Tx) error {
		var (
			// current schema and version
			schema  = "v0"
			version = 0
		)

		// i represents the index of the first migration
		// which must be run to get the database up to date.
		// The migration's version will be checked in reverse
		// order, decrementing i for each migration which
		// represents a version newer than the current
		// database version
		i := len(migrations)

		for ; i > 0; i-- {
			migration := migrations[i-1]

			bkt := tx.Bucket([]byte(migration.schema))
			if bkt == nil {
				// Hasn't encountered another schema, go to next migration
				if schema == "v0" {
					continue
				}
				break
			}
			if schema == "v0" {
				schema = migration.schema
				vb := bkt.Get(bucketKeyDBVersion)
				if vb != nil {
					v, _ := binary.Varint(vb)
					version = int(v)
				}
			}

			if version >= migration.version {
				break
			}
		}

		// Previous version fo database found
		if schema != "v0" {
			updates := migrations[i:]

			// No migration updates, return immediately
			if len(updates) == 0 {
				return errSkip
			}

			for _, m := range updates {
				t0 := time.Now()
				if err := m.migrate(tx); err != nil {
					return errors.Wrapf(err, "failed to migrate to %s.%d", m.schema, m.version)
				}
				log.G(ctx).WithField("d", time.Since(t0)).Debugf("finished database migration to %s.%d", m.schema, m.version)
			}
		}

		bkt, err := tx.CreateBucketIfNotExists(bucketKeyVersion)
		if err != nil {
			return err
		}

		versionEncoded, err := encodeInt(dbVersion)
		if err != nil {
			return err
		}

		return bkt.Put(bucketKeyDBVersion, versionEncoded)
	})
	if err == errSkip {
		err = nil
	}
	return err
}

// ContentStore returns a namespaced content store
// proxied to a content store.
func (m *DB) ContentStore() content.Store {
	if m.cs == nil {
		return nil
	}
	return m.cs
}

// Snapshotter returns a namespaced content store for
// the requested snapshotter name proxied to a snapshotter.
func (m *DB) Snapshotter(name string) snapshots.Snapshotter {
	sn, ok := m.ss[name]
	if !ok {
		return nil
	}
	return sn
}

// Snapshotters returns all available snapshotters.
func (m *DB) Snapshotters() map[string]snapshots.Snapshotter {
	ss := make(map[string]snapshots.Snapshotter, len(m.ss))
	for n, sn := range m.ss {
		ss[n] = sn
	}
	return ss
}

// View runs a readonly transaction on the metadata store.
func (m *DB) View(fn func(*bolt.Tx) error) error {
	return m.db.View(fn)
}

// Update runs a writable transaction on the metadata store.
func (m *DB) Update(fn func(*bolt.Tx) error) error {
	m.wlock.RLock()
	defer m.wlock.RUnlock()
	err := m.db.Update(fn)
	if err == nil {
		m.dirtyL.Lock()
		dirty := m.dirtyCS || len(m.dirtySS) > 0
		for _, fn := range m.mutationCallbacks {
			fn(dirty)
		}
		m.dirtyL.Unlock()
	}

	return err
}

// RegisterMutationCallback registers a function to be called after a metadata
// mutations has been performed.
//
// The callback function is an argument for whether a deletion has occurred
// since the last garbage collection.
func (m *DB) RegisterMutationCallback(fn func(bool)) {
	m.dirtyL.Lock()
	m.mutationCallbacks = append(m.mutationCallbacks, fn)
	m.dirtyL.Unlock()
}

// GCStats holds the duration for the different phases of the garbage collector
type GCStats struct {
	MetaD     time.Duration
	ContentD  time.Duration
	SnapshotD map[string]time.Duration
}

// Elapsed returns the duration which elapsed during a collection
func (s GCStats) Elapsed() time.Duration {
	return s.MetaD
}

// GarbageCollect starts garbage collection
func (m *DB) GarbageCollect(ctx context.Context) (gc.Stats, error) {
	m.wlock.Lock()
	t1 := time.Now()

	marked, err := m.getMarked(ctx)
	if err != nil {
		m.wlock.Unlock()
		return nil, err
	}

	m.dirtyL.Lock()

	if err := m.db.Update(func(tx *bolt.Tx) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		rm := func(ctx context.Context, n gc.Node) error {
			if _, ok := marked[n]; ok {
				return nil
			}

			if n.Type == ResourceSnapshot {
				if idx := strings.IndexRune(n.Key, '/'); idx > 0 {
					m.dirtySS[n.Key[:idx]] = struct{}{}
				}
			} else if n.Type == ResourceContent || n.Type == ResourceIngest {
				m.dirtyCS = true
			}
			return remove(ctx, tx, n)
		}

		if err := scanAll(ctx, tx, rm); err != nil {
			return errors.Wrap(err, "failed to scan and remove")
		}

		return nil
	}); err != nil {
		m.dirtyL.Unlock()
		m.wlock.Unlock()
		return nil, err
	}

	var stats GCStats
	var wg sync.WaitGroup

	if len(m.dirtySS) > 0 {
		var sl sync.Mutex
		stats.SnapshotD = map[string]time.Duration{}
		wg.Add(len(m.dirtySS))
		for snapshotterName := range m.dirtySS {
			log.G(ctx).WithField("snapshotter", snapshotterName).Debug("schedule snapshotter cleanup")
			go func(snapshotterName string) {
				st1 := time.Now()
				m.cleanupSnapshotter(snapshotterName)

				sl.Lock()
				stats.SnapshotD[snapshotterName] = time.Since(st1)
				sl.Unlock()

				wg.Done()
			}(snapshotterName)
		}
		m.dirtySS = map[string]struct{}{}
	}

	if m.dirtyCS {
		wg.Add(1)
		log.G(ctx).Debug("schedule content cleanup")
		go func() {
			ct1 := time.Now()
			m.cleanupContent()
			stats.ContentD = time.Since(ct1)
			wg.Done()
		}()
		m.dirtyCS = false
	}

	m.dirtyL.Unlock()

	stats.MetaD = time.Since(t1)
	m.wlock.Unlock()

	wg.Wait()

	return stats, err
}

func (m *DB) getMarked(ctx context.Context) (map[gc.Node]struct{}, error) {
	var marked map[gc.Node]struct{}
	if err := m.db.View(func(tx *bolt.Tx) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		var (
			nodes []gc.Node
			wg    sync.WaitGroup
			roots = make(chan gc.Node)
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range roots {
				nodes = append(nodes, n)
			}
		}()
		// Call roots
		if err := scanRoots(ctx, tx, roots); err != nil {
			cancel()
			return err
		}
		close(roots)
		wg.Wait()

		refs := func(n gc.Node) ([]gc.Node, error) {
			var sn []gc.Node
			if err := references(ctx, tx, n, func(nn gc.Node) {
				sn = append(sn, nn)
			}); err != nil {
				return nil, err
			}
			return sn, nil
		}

		reachable, err := gc.Tricolor(nodes, refs)
		if err != nil {
			return err
		}
		marked = reachable
		return nil
	}); err != nil {
		return nil, err
	}
	return marked, nil
}

func (m *DB) cleanupSnapshotter(name string) (time.Duration, error) {
	ctx := context.Background()
	sn, ok := m.ss[name]
	if !ok {
		return 0, nil
	}

	d, err := sn.garbageCollect(ctx)
	logger := log.G(ctx).WithField("snapshotter", name)
	if err != nil {
		logger.WithError(err).Warn("snapshot garbage collection failed")
	} else {
		logger.WithField("d", d).Debugf("snapshot garbage collected")
	}
	return d, err
}

func (m *DB) cleanupContent() (time.Duration, error) {
	ctx := context.Background()
	if m.cs == nil {
		return 0, nil
	}

	d, err := m.cs.garbageCollect(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Warn("content garbage collection failed")
	} else {
		log.G(ctx).WithField("d", d).Debugf("content garbage collected")
	}

	return d, err
}
