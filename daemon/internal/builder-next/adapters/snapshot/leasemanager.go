package snapshot

import (
	"context"
	"sync"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/log"
	bolt "go.etcd.io/bbolt"
)

type sLM struct {
	manager leases.Manager
	s       *snapshotter

	// Serialize durable lease mutations with the mirrored references and removal.
	// Otherwise a new owner can exist in the database while the last mirrored
	// reference is being removed, allowing GC to delete the shared snapshot.
	mu         sync.Mutex
	byLease    map[string]map[string]struct{}
	bySnapshot map[string]map[string]struct{}
}

func newLeaseManager(s *snapshotter, lm leases.Manager) *sLM {
	return &sLM{
		s:       s,
		manager: lm,

		byLease:    map[string]map[string]struct{}{},
		bySnapshot: map[string]map[string]struct{}{},
	}
}

func (l *sLM) Create(ctx context.Context, opts ...leases.Opt) (leases.Lease, error) {
	return l.manager.Create(ctx, opts...)
}

func (l *sLM) Delete(ctx context.Context, lease leases.Lease, opts ...leases.DeleteOpt) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.manager.Delete(ctx, lease, opts...); err != nil {
		return err
	}
	if snaps, ok := l.byLease[lease.ID]; ok {
		for sID := range snaps {
			l.delRef(lease.ID, sID)
		}
	}
	return nil
}

func (l *sLM) List(ctx context.Context, filters ...string) ([]leases.Lease, error) {
	return l.manager.List(ctx, filters...)
}

func (l *sLM) AddResource(ctx context.Context, lease leases.Lease, resource leases.Resource) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.manager.AddResource(ctx, lease, resource); err != nil {
		return err
	}
	if resource.Type == "snapshots/default" {
		l.addRef(lease.ID, resource.ID)
	}
	return nil
}

func (l *sLM) DeleteResource(ctx context.Context, lease leases.Lease, resource leases.Resource) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.manager.DeleteResource(ctx, lease, resource); err != nil {
		return err
	}
	if resource.Type == "snapshots/default" {
		l.delRef(lease.ID, resource.ID)
	}
	return nil
}

func (l *sLM) ListResources(ctx context.Context, lease leases.Lease) ([]leases.Resource, error) {
	return l.manager.ListResources(ctx, lease)
}

func (l *sLM) addRef(lID, sID string) {
	load := false
	snapshots, ok := l.byLease[lID]
	if !ok {
		snapshots = map[string]struct{}{}
		l.byLease[lID] = snapshots
	}
	if _, ok := snapshots[sID]; !ok {
		snapshots[sID] = struct{}{}
	}
	leases, ok := l.bySnapshot[sID]
	if !ok {
		leases = map[string]struct{}{}
		l.bySnapshot[sID] = leases
		load = true
	}
	if _, ok := leases[lID]; !ok {
		leases[lID] = struct{}{}
	}

	if load {
		l.s.getLayer(sID, true)
		if _, ok := l.s.chainID(sID); ok {
			l.s.db.Update(func(tx *bolt.Tx) error {
				b, err := tx.CreateBucketIfNotExists([]byte(lID))
				if err != nil {
					return err
				}
				return b.Put(keyChainID, []byte(sID))
			})
		}
	}
}

func (l *sLM) delRef(lID, sID string) {
	snapshots, ok := l.byLease[lID]
	if ok {
		delete(snapshots, sID)
		if len(snapshots) == 0 {
			delete(l.byLease, lID)
		}
	}
	leases, ok := l.bySnapshot[sID]
	if ok {
		delete(leases, lID)
		if len(leases) == 0 {
			delete(l.bySnapshot, sID)
			if err := l.s.remove(context.TODO(), sID); err != nil {
				log.G(context.TODO()).Warnf("failed to remove snapshot %v", sID)
			}
		}
	}
}
