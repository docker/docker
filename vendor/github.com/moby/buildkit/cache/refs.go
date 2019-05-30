package cache

import (
	"context"
	"sync"

	"github.com/containerd/containerd/mount"
	"github.com/docker/docker/pkg/idtools"
	"github.com/moby/buildkit/cache/metadata"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/util/flightcontrol"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// Ref is a reference to cacheable objects.
type Ref interface {
	Mountable
	ID() string
	Release(context.Context) error
	Size(ctx context.Context) (int64, error)
	Metadata() *metadata.StorageItem
	IdentityMapping() *idtools.IdentityMapping
}

type ImmutableRef interface {
	Ref
	Parent() ImmutableRef
	Finalize(ctx context.Context, commit bool) error // Make sure reference is flushed to driver
	Clone() ImmutableRef
}

type MutableRef interface {
	Ref
	Commit(context.Context) (ImmutableRef, error)
}

type Mountable interface {
	Mount(ctx context.Context, readonly bool) (snapshot.Mountable, error)
}

type ref interface {
	updateLastUsed() bool
}

type cacheRecord struct {
	cm *cacheManager
	mu *sync.Mutex // the mutex is shared by records sharing data

	mutable bool
	refs    map[ref]struct{}
	parent  ImmutableRef
	md      *metadata.StorageItem

	// dead means record is marked as deleted
	dead bool

	view      string
	viewMount snapshot.Mountable

	sizeG flightcontrol.Group

	// these are filled if multiple refs point to same data
	equalMutable   *mutableRef
	equalImmutable *immutableRef
}

// hold ref lock before calling
func (cr *cacheRecord) ref(triggerLastUsed bool) *immutableRef {
	ref := &immutableRef{cacheRecord: cr, triggerLastUsed: triggerLastUsed}
	cr.refs[ref] = struct{}{}
	return ref
}

// hold ref lock before calling
func (cr *cacheRecord) mref(triggerLastUsed bool) *mutableRef {
	ref := &mutableRef{cacheRecord: cr, triggerLastUsed: triggerLastUsed}
	cr.refs[ref] = struct{}{}
	return ref
}

// hold ref lock before calling
func (cr *cacheRecord) isDead() bool {
	return cr.dead || (cr.equalImmutable != nil && cr.equalImmutable.dead) || (cr.equalMutable != nil && cr.equalMutable.dead)
}

func (cr *cacheRecord) IdentityMapping() *idtools.IdentityMapping {
	return cr.cm.IdentityMapping()
}

func (cr *cacheRecord) Size(ctx context.Context) (int64, error) {
	// this expects that usage() is implemented lazily
	s, err := cr.sizeG.Do(ctx, cr.ID(), func(ctx context.Context) (interface{}, error) {
		cr.mu.Lock()
		s := getSize(cr.md)
		if s != sizeUnknown {
			cr.mu.Unlock()
			return s, nil
		}
		driverID := cr.ID()
		if cr.equalMutable != nil {
			driverID = cr.equalMutable.ID()
		}
		cr.mu.Unlock()
		usage, err := cr.cm.ManagerOpt.Snapshotter.Usage(ctx, driverID)
		if err != nil {
			cr.mu.Lock()
			isDead := cr.isDead()
			cr.mu.Unlock()
			if isDead {
				return int64(0), nil
			}
			return s, errors.Wrapf(err, "failed to get usage for %s", cr.ID())
		}
		cr.mu.Lock()
		setSize(cr.md, usage.Size)
		if err := cr.md.Commit(); err != nil {
			cr.mu.Unlock()
			return s, err
		}
		cr.mu.Unlock()
		return usage.Size, nil
	})
	return s.(int64), err
}

func (cr *cacheRecord) Parent() ImmutableRef {
	return cr.parentRef(true)
}

func (cr *cacheRecord) parentRef(hidden bool) ImmutableRef {
	if cr.parent == nil {
		return nil
	}
	p := cr.parent.(*immutableRef)
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ref(hidden)
}

func (cr *cacheRecord) Mount(ctx context.Context, readonly bool) (snapshot.Mountable, error) {
	cr.mu.Lock()
	defer cr.mu.Unlock()

	if cr.mutable {
		m, err := cr.cm.Snapshotter.Mounts(ctx, cr.ID())
		if err != nil {
			return nil, errors.Wrapf(err, "failed to mount %s", cr.ID())
		}
		if readonly {
			m = setReadonly(m)
		}
		return m, nil
	}

	if cr.equalMutable != nil && readonly {
		m, err := cr.cm.Snapshotter.Mounts(ctx, cr.equalMutable.ID())
		if err != nil {
			return nil, errors.Wrapf(err, "failed to mount %s", cr.equalMutable.ID())
		}
		return setReadonly(m), nil
	}

	if err := cr.finalize(ctx, true); err != nil {
		return nil, err
	}
	if cr.viewMount == nil { // TODO: handle this better
		cr.view = identity.NewID()
		m, err := cr.cm.Snapshotter.View(ctx, cr.view, cr.ID())
		if err != nil {
			cr.view = ""
			return nil, errors.Wrapf(err, "failed to mount %s", cr.ID())
		}
		cr.viewMount = m
	}
	return cr.viewMount, nil
}

// call when holding the manager lock
func (cr *cacheRecord) remove(ctx context.Context, removeSnapshot bool) error {
	delete(cr.cm.records, cr.ID())
	if cr.parent != nil {
		if err := cr.parent.(*immutableRef).release(ctx); err != nil {
			return err
		}
	}
	if removeSnapshot {
		if err := cr.cm.Snapshotter.Remove(ctx, cr.ID()); err != nil {
			return err
		}
	}
	if err := cr.cm.md.Clear(cr.ID()); err != nil {
		return err
	}
	return nil
}

func (cr *cacheRecord) ID() string {
	return cr.md.ID()
}

type immutableRef struct {
	*cacheRecord
	triggerLastUsed bool
}

type mutableRef struct {
	*cacheRecord
	triggerLastUsed bool
}

func (sr *immutableRef) Clone() ImmutableRef {
	sr.mu.Lock()
	ref := sr.ref(false)
	sr.mu.Unlock()
	return ref
}

func (sr *immutableRef) Release(ctx context.Context) error {
	sr.cm.mu.Lock()
	defer sr.cm.mu.Unlock()

	sr.mu.Lock()
	defer sr.mu.Unlock()

	return sr.release(ctx)
}

func (sr *immutableRef) updateLastUsed() bool {
	return sr.triggerLastUsed
}

func (sr *immutableRef) updateLastUsedNow() bool {
	if !sr.triggerLastUsed {
		return false
	}
	for r := range sr.refs {
		if r.updateLastUsed() {
			return false
		}
	}
	return true
}

func (sr *immutableRef) release(ctx context.Context) error {
	delete(sr.refs, sr)

	if sr.updateLastUsedNow() {
		updateLastUsed(sr.md)
		if sr.equalMutable != nil {
			sr.equalMutable.triggerLastUsed = true
		}
	}

	if len(sr.refs) == 0 {
		if sr.viewMount != nil { // TODO: release viewMount earlier if possible
			if err := sr.cm.Snapshotter.Remove(ctx, sr.view); err != nil {
				return err
			}
			sr.view = ""
			sr.viewMount = nil
		}

		if sr.equalMutable != nil {
			sr.equalMutable.release(ctx)
		}
		// go sr.cm.GC()
	}

	return nil
}

func (sr *immutableRef) Finalize(ctx context.Context, b bool) error {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	return sr.finalize(ctx, b)
}

func (cr *cacheRecord) Metadata() *metadata.StorageItem {
	return cr.md
}

func (cr *cacheRecord) finalize(ctx context.Context, commit bool) error {
	mutable := cr.equalMutable
	if mutable == nil {
		return nil
	}
	if !commit {
		if HasCachePolicyRetain(mutable) {
			CachePolicyRetain(mutable)
			return mutable.Metadata().Commit()
		}
		return nil
	}
	err := cr.cm.Snapshotter.Commit(ctx, cr.ID(), mutable.ID())
	if err != nil {
		return errors.Wrapf(err, "failed to commit %s", mutable.ID())
	}
	mutable.dead = true
	go func() {
		cr.cm.mu.Lock()
		defer cr.cm.mu.Unlock()
		if err := mutable.remove(context.TODO(), false); err != nil {
			logrus.Error(err)
		}
	}()
	cr.equalMutable = nil
	clearEqualMutable(cr.md)
	return cr.md.Commit()
}

func (sr *mutableRef) updateLastUsed() bool {
	return sr.triggerLastUsed
}

func (sr *mutableRef) commit(ctx context.Context) (ImmutableRef, error) {
	if !sr.mutable || len(sr.refs) == 0 {
		return nil, errors.Wrapf(errInvalid, "invalid mutable ref %p", sr)
	}

	id := identity.NewID()
	md, _ := sr.cm.md.Get(id)
	rec := &cacheRecord{
		mu:           sr.mu,
		cm:           sr.cm,
		parent:       sr.parentRef(false),
		equalMutable: sr,
		refs:         make(map[ref]struct{}),
		md:           md,
	}

	if descr := GetDescription(sr.md); descr != "" {
		if err := queueDescription(md, descr); err != nil {
			return nil, err
		}
	}

	if err := initializeMetadata(rec); err != nil {
		return nil, err
	}

	sr.cm.records[id] = rec

	if err := sr.md.Commit(); err != nil {
		return nil, err
	}

	setSize(md, sizeUnknown)
	setEqualMutable(md, sr.ID())
	if err := md.Commit(); err != nil {
		return nil, err
	}

	ref := rec.ref(true)
	sr.equalImmutable = ref
	return ref, nil
}

func (sr *mutableRef) updatesLastUsed() bool {
	return sr.triggerLastUsed
}

func (sr *mutableRef) Commit(ctx context.Context) (ImmutableRef, error) {
	sr.cm.mu.Lock()
	defer sr.cm.mu.Unlock()

	sr.mu.Lock()
	defer sr.mu.Unlock()

	return sr.commit(ctx)
}

func (sr *mutableRef) Release(ctx context.Context) error {
	sr.cm.mu.Lock()
	defer sr.cm.mu.Unlock()

	sr.mu.Lock()
	defer sr.mu.Unlock()

	return sr.release(ctx)
}

func (sr *mutableRef) release(ctx context.Context) error {
	delete(sr.refs, sr)
	if getCachePolicy(sr.md) != cachePolicyRetain {
		if sr.equalImmutable != nil {
			if getCachePolicy(sr.equalImmutable.md) == cachePolicyRetain {
				if sr.updateLastUsed() {
					updateLastUsed(sr.md)
					sr.triggerLastUsed = false
				}
				return nil
			}
			if err := sr.equalImmutable.remove(ctx, false); err != nil {
				return err
			}
		}
		if sr.parent != nil {
			if err := sr.parent.(*immutableRef).release(ctx); err != nil {
				return err
			}
		}
		return sr.remove(ctx, true)
	} else {
		if sr.updateLastUsed() {
			updateLastUsed(sr.md)
			sr.triggerLastUsed = false
		}
	}
	return nil
}

func setReadonly(mounts snapshot.Mountable) snapshot.Mountable {
	return &readOnlyMounter{mounts}
}

type readOnlyMounter struct {
	snapshot.Mountable
}

func (m *readOnlyMounter) Mount() ([]mount.Mount, error) {
	mounts, err := m.Mountable.Mount()
	if err != nil {
		return nil, err
	}
	for i, m := range mounts {
		opts := make([]string, 0, len(m.Options))
		for _, opt := range m.Options {
			if opt != "rw" {
				opts = append(opts, opt)
			}
		}
		opts = append(opts, "ro")
		mounts[i].Options = opts
	}
	return mounts, nil
}
