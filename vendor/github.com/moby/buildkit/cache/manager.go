package cache

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/containerd/containerd/filters"
	"github.com/containerd/containerd/snapshots"
	"github.com/docker/docker/pkg/idtools"
	"github.com/moby/buildkit/cache/metadata"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/snapshot"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

var (
	ErrLocked   = errors.New("locked")
	errNotFound = errors.New("not found")
	errInvalid  = errors.New("invalid")
)

type ManagerOpt struct {
	Snapshotter     snapshot.SnapshotterBase
	MetadataStore   *metadata.Store
	PruneRefChecker ExternalRefCheckerFunc
}

type Accessor interface {
	Get(ctx context.Context, id string, opts ...RefOption) (ImmutableRef, error)
	GetFromSnapshotter(ctx context.Context, id string, opts ...RefOption) (ImmutableRef, error)
	New(ctx context.Context, s ImmutableRef, opts ...RefOption) (MutableRef, error)
	GetMutable(ctx context.Context, id string) (MutableRef, error) // Rebase?
	IdentityMapping() *idtools.IdentityMapping
}

type Controller interface {
	DiskUsage(ctx context.Context, info client.DiskUsageInfo) ([]*client.UsageInfo, error)
	Prune(ctx context.Context, ch chan client.UsageInfo, info ...client.PruneInfo) error
}

type Manager interface {
	Accessor
	Controller
	Close() error
}

type ExternalRefCheckerFunc func() (ExternalRefChecker, error)

type ExternalRefChecker interface {
	Exists(key string) bool
}

type cacheManager struct {
	records map[string]*cacheRecord
	mu      sync.Mutex
	ManagerOpt
	md *metadata.Store

	muPrune sync.Mutex // make sure parallel prune is not allowed so there will not be inconsistent results
}

func NewManager(opt ManagerOpt) (Manager, error) {
	cm := &cacheManager{
		ManagerOpt: opt,
		md:         opt.MetadataStore,
		records:    make(map[string]*cacheRecord),
	}

	if err := cm.init(context.TODO()); err != nil {
		return nil, err
	}

	// cm.scheduleGC(5 * time.Minute)

	return cm, nil
}

// init loads all snapshots from metadata state and tries to load the records
// from the snapshotter. If snaphot can't be found, metadata is deleted as well.
func (cm *cacheManager) init(ctx context.Context) error {
	items, err := cm.md.All()
	if err != nil {
		return err
	}

	for _, si := range items {
		if _, err := cm.getRecord(ctx, si.ID(), false); err != nil {
			logrus.Debugf("could not load snapshot %s: %v", si.ID(), err)
			cm.md.Clear(si.ID())
			// TODO: make sure content is deleted as well
		}
	}
	return nil
}

// IdentityMapping returns the userns remapping used for refs
func (cm *cacheManager) IdentityMapping() *idtools.IdentityMapping {
	return cm.Snapshotter.IdentityMapping()
}

// Close closes the manager and releases the metadata database lock. No other
// method should be called after Close.
func (cm *cacheManager) Close() error {
	// TODO: allocate internal context and cancel it here
	return cm.md.Close()
}

// Get returns an immutable snapshot reference for ID
func (cm *cacheManager) Get(ctx context.Context, id string, opts ...RefOption) (ImmutableRef, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.get(ctx, id, false, opts...)
}

// Get returns an immutable snapshot reference for ID
func (cm *cacheManager) GetFromSnapshotter(ctx context.Context, id string, opts ...RefOption) (ImmutableRef, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	return cm.get(ctx, id, true, opts...)
}

// get requires manager lock to be taken
func (cm *cacheManager) get(ctx context.Context, id string, fromSnapshotter bool, opts ...RefOption) (ImmutableRef, error) {
	rec, err := cm.getRecord(ctx, id, fromSnapshotter, opts...)
	if err != nil {
		return nil, err
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()

	triggerUpdate := true
	for _, o := range opts {
		if o == NoUpdateLastUsed {
			triggerUpdate = false
		}
	}

	if rec.mutable {
		if len(rec.refs) != 0 {
			return nil, errors.Wrapf(ErrLocked, "%s is locked", id)
		}
		if rec.equalImmutable != nil {
			return rec.equalImmutable.ref(triggerUpdate), nil
		}
		return rec.mref(triggerUpdate).commit(ctx)
	}

	return rec.ref(triggerUpdate), nil
}

// getRecord returns record for id. Requires manager lock.
func (cm *cacheManager) getRecord(ctx context.Context, id string, fromSnapshotter bool, opts ...RefOption) (cr *cacheRecord, retErr error) {
	if rec, ok := cm.records[id]; ok {
		if rec.isDead() {
			return nil, errNotFound
		}
		return rec, nil
	}

	md, ok := cm.md.Get(id)
	if !ok && !fromSnapshotter {
		return nil, errNotFound
	}
	if mutableID := getEqualMutable(md); mutableID != "" {
		mutable, err := cm.getRecord(ctx, mutableID, fromSnapshotter)
		if err != nil {
			// check loading mutable deleted record from disk
			if errors.Cause(err) == errNotFound {
				cm.md.Clear(id)
			}
			return nil, err
		}
		rec := &cacheRecord{
			mu:           &sync.Mutex{},
			cm:           cm,
			refs:         make(map[ref]struct{}),
			parent:       mutable.parentRef(false),
			md:           md,
			equalMutable: &mutableRef{cacheRecord: mutable},
		}
		mutable.equalImmutable = &immutableRef{cacheRecord: rec}
		cm.records[id] = rec
		return rec, nil
	}

	info, err := cm.Snapshotter.Stat(ctx, id)
	if err != nil {
		return nil, errors.Wrap(errNotFound, err.Error())
	}

	var parent ImmutableRef
	if info.Parent != "" {
		parent, err = cm.get(ctx, info.Parent, fromSnapshotter, append(opts, NoUpdateLastUsed)...)
		if err != nil {
			return nil, err
		}
		defer func() {
			if retErr != nil {
				parent.Release(context.TODO())
			}
		}()
	}

	rec := &cacheRecord{
		mu:      &sync.Mutex{},
		mutable: info.Kind != snapshots.KindCommitted,
		cm:      cm,
		refs:    make(map[ref]struct{}),
		parent:  parent,
		md:      md,
	}

	// the record was deleted but we crashed before data on disk was removed
	if getDeleted(md) {
		if err := rec.remove(ctx, true); err != nil {
			return nil, err
		}
		return nil, errNotFound
	}

	if err := initializeMetadata(rec, opts...); err != nil {
		if parent != nil {
			parent.Release(context.TODO())
		}
		return nil, err
	}

	cm.records[id] = rec
	return rec, nil
}

func (cm *cacheManager) New(ctx context.Context, s ImmutableRef, opts ...RefOption) (MutableRef, error) {
	id := identity.NewID()

	var parent ImmutableRef
	var parentID string
	if s != nil {
		var err error
		parent, err = cm.Get(ctx, s.ID(), NoUpdateLastUsed)
		if err != nil {
			return nil, err
		}
		if err := parent.Finalize(ctx, true); err != nil {
			return nil, err
		}
		parentID = parent.ID()
	}

	if err := cm.Snapshotter.Prepare(ctx, id, parentID); err != nil {
		if parent != nil {
			parent.Release(context.TODO())
		}
		return nil, errors.Wrapf(err, "failed to prepare %s", id)
	}

	md, _ := cm.md.Get(id)

	rec := &cacheRecord{
		mu:      &sync.Mutex{},
		mutable: true,
		cm:      cm,
		refs:    make(map[ref]struct{}),
		parent:  parent,
		md:      md,
	}

	if err := initializeMetadata(rec, opts...); err != nil {
		if parent != nil {
			parent.Release(context.TODO())
		}
		return nil, err
	}

	cm.mu.Lock()
	defer cm.mu.Unlock()

	cm.records[id] = rec // TODO: save to db

	return rec.mref(true), nil
}
func (cm *cacheManager) GetMutable(ctx context.Context, id string) (MutableRef, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	rec, err := cm.getRecord(ctx, id, false)
	if err != nil {
		return nil, err
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !rec.mutable {
		return nil, errors.Wrapf(errInvalid, "%s is not mutable", id)
	}

	if len(rec.refs) != 0 {
		return nil, errors.Wrapf(ErrLocked, "%s is locked", id)
	}

	if rec.equalImmutable != nil {
		if len(rec.equalImmutable.refs) != 0 {
			return nil, errors.Wrapf(ErrLocked, "%s is locked", id)
		}
		delete(cm.records, rec.equalImmutable.ID())
		if err := rec.equalImmutable.remove(ctx, false); err != nil {
			return nil, err
		}
		rec.equalImmutable = nil
	}

	return rec.mref(true), nil
}

func (cm *cacheManager) Prune(ctx context.Context, ch chan client.UsageInfo, opts ...client.PruneInfo) error {
	cm.muPrune.Lock()
	defer cm.muPrune.Unlock()

	for _, opt := range opts {
		if err := cm.pruneOnce(ctx, ch, opt); err != nil {
			return err
		}
	}
	return nil
}

func (cm *cacheManager) pruneOnce(ctx context.Context, ch chan client.UsageInfo, opt client.PruneInfo) error {
	filter, err := filters.ParseAll(opt.Filter...)
	if err != nil {
		return err
	}

	var check ExternalRefChecker
	if f := cm.PruneRefChecker; f != nil && (!opt.All || len(opt.Filter) > 0) {
		c, err := f()
		if err != nil {
			return err
		}
		check = c
	}

	totalSize := int64(0)
	if opt.KeepBytes != 0 {
		du, err := cm.DiskUsage(ctx, client.DiskUsageInfo{})
		if err != nil {
			return err
		}
		for _, ui := range du {
			if check != nil {
				if check.Exists(ui.ID) {
					continue
				}
			}
			totalSize += ui.Size
		}
	}

	return cm.prune(ctx, ch, pruneOpt{
		filter:       filter,
		all:          opt.All,
		checkShared:  check,
		keepDuration: opt.KeepDuration,
		keepBytes:    opt.KeepBytes,
		totalSize:    totalSize,
	})
}

func (cm *cacheManager) prune(ctx context.Context, ch chan client.UsageInfo, opt pruneOpt) error {
	var toDelete []*deleteRecord

	if opt.keepBytes != 0 && opt.totalSize < opt.keepBytes {
		return nil
	}

	cm.mu.Lock()

	gcMode := opt.keepBytes != 0
	cutOff := time.Now().Add(-opt.keepDuration)

	locked := map[*sync.Mutex]struct{}{}

	for _, cr := range cm.records {
		if _, ok := locked[cr.mu]; ok {
			continue
		}
		cr.mu.Lock()

		// ignore duplicates that share data
		if cr.equalImmutable != nil && len(cr.equalImmutable.refs) > 0 || cr.equalMutable != nil && len(cr.refs) == 0 {
			cr.mu.Unlock()
			continue
		}

		if cr.isDead() {
			cr.mu.Unlock()
			continue
		}

		if len(cr.refs) == 0 {
			recordType := GetRecordType(cr)
			if recordType == "" {
				recordType = client.UsageRecordTypeRegular
			}

			shared := false
			if opt.checkShared != nil {
				shared = opt.checkShared.Exists(cr.ID())
			}

			if !opt.all {
				if recordType == client.UsageRecordTypeInternal || recordType == client.UsageRecordTypeFrontend || shared {
					cr.mu.Unlock()
					continue
				}
			}

			c := &client.UsageInfo{
				ID:         cr.ID(),
				Mutable:    cr.mutable,
				RecordType: recordType,
				Shared:     shared,
			}

			usageCount, lastUsedAt := getLastUsed(cr.md)
			c.LastUsedAt = lastUsedAt
			c.UsageCount = usageCount

			if opt.keepDuration != 0 {
				if lastUsedAt != nil && lastUsedAt.After(cutOff) {
					cr.mu.Unlock()
					continue
				}
			}

			if opt.filter.Match(adaptUsageInfo(c)) {
				toDelete = append(toDelete, &deleteRecord{
					cacheRecord: cr,
					lastUsedAt:  c.LastUsedAt,
					usageCount:  c.UsageCount,
				})
				if !gcMode {
					cr.dead = true

					// mark metadata as deleted in case we crash before cleanup finished
					if err := setDeleted(cr.md); err != nil {
						cr.mu.Unlock()
						cm.mu.Unlock()
						return err
					}
				} else {
					locked[cr.mu] = struct{}{}
					continue // leave the record locked
				}
			}
		}
		cr.mu.Unlock()
	}

	if gcMode && len(toDelete) > 0 {
		sortDeleteRecords(toDelete)
		var err error
		for i, cr := range toDelete {
			// only remove single record at a time
			if i == 0 {
				cr.dead = true
				err = setDeleted(cr.md)
			}
			cr.mu.Unlock()
		}
		if err != nil {
			return err
		}
		toDelete = toDelete[:1]
	}

	cm.mu.Unlock()

	if len(toDelete) == 0 {
		return nil
	}

	var err error
	for _, cr := range toDelete {
		cr.mu.Lock()

		usageCount, lastUsedAt := getLastUsed(cr.md)

		c := client.UsageInfo{
			ID:          cr.ID(),
			Mutable:     cr.mutable,
			InUse:       len(cr.refs) > 0,
			Size:        getSize(cr.md),
			CreatedAt:   GetCreatedAt(cr.md),
			Description: GetDescription(cr.md),
			LastUsedAt:  lastUsedAt,
			UsageCount:  usageCount,
		}

		if cr.parent != nil {
			c.Parent = cr.parent.ID()
		}
		if c.Size == sizeUnknown && cr.equalImmutable != nil {
			c.Size = getSize(cr.equalImmutable.md) // benefit from DiskUsage calc
		}
		if c.Size == sizeUnknown {
			cr.mu.Unlock() // all the non-prune modifications already protected by cr.dead
			s, err := cr.Size(ctx)
			if err != nil {
				return err
			}
			c.Size = s
			cr.mu.Lock()
		}

		opt.totalSize -= c.Size

		if cr.equalImmutable != nil {
			if err1 := cr.equalImmutable.remove(ctx, false); err == nil {
				err = err1
			}
		}
		if err1 := cr.remove(ctx, true); err == nil {
			err = err1
		}

		if err == nil && ch != nil {
			ch <- c
		}
		cr.mu.Unlock()
	}
	if err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return cm.prune(ctx, ch, opt)
	}
}

func (cm *cacheManager) markShared(m map[string]*cacheUsageInfo) error {
	if cm.PruneRefChecker == nil {
		return nil
	}
	c, err := cm.PruneRefChecker()
	if err != nil {
		return err
	}

	var markAllParentsShared func(string)
	markAllParentsShared = func(id string) {
		if v, ok := m[id]; ok {
			v.shared = true
			if v.parent != "" {
				markAllParentsShared(v.parent)
			}
		}
	}

	for id := range m {
		if m[id].shared {
			continue
		}
		if b := c.Exists(id); b {
			markAllParentsShared(id)
		}
	}
	return nil
}

type cacheUsageInfo struct {
	refs        int
	parent      string
	size        int64
	mutable     bool
	createdAt   time.Time
	usageCount  int
	lastUsedAt  *time.Time
	description string
	doubleRef   bool
	recordType  client.UsageRecordType
	shared      bool
}

func (cm *cacheManager) DiskUsage(ctx context.Context, opt client.DiskUsageInfo) ([]*client.UsageInfo, error) {
	filter, err := filters.ParseAll(opt.Filter...)
	if err != nil {
		return nil, err
	}

	cm.mu.Lock()

	m := make(map[string]*cacheUsageInfo, len(cm.records))
	rescan := make(map[string]struct{}, len(cm.records))

	for id, cr := range cm.records {
		cr.mu.Lock()
		// ignore duplicates that share data
		if cr.equalImmutable != nil && len(cr.equalImmutable.refs) > 0 || cr.equalMutable != nil && len(cr.refs) == 0 {
			cr.mu.Unlock()
			continue
		}

		usageCount, lastUsedAt := getLastUsed(cr.md)
		c := &cacheUsageInfo{
			refs:        len(cr.refs),
			mutable:     cr.mutable,
			size:        getSize(cr.md),
			createdAt:   GetCreatedAt(cr.md),
			usageCount:  usageCount,
			lastUsedAt:  lastUsedAt,
			description: GetDescription(cr.md),
			doubleRef:   cr.equalImmutable != nil,
			recordType:  GetRecordType(cr),
		}
		if c.recordType == "" {
			c.recordType = client.UsageRecordTypeRegular
		}
		if cr.parent != nil {
			c.parent = cr.parent.ID()
		}
		if cr.mutable && c.refs > 0 {
			c.size = 0 // size can not be determined because it is changing
		}
		m[id] = c
		rescan[id] = struct{}{}
		cr.mu.Unlock()
	}
	cm.mu.Unlock()

	for {
		if len(rescan) == 0 {
			break
		}
		for id := range rescan {
			v := m[id]
			if v.refs == 0 && v.parent != "" {
				m[v.parent].refs--
				if v.doubleRef {
					m[v.parent].refs--
				}
				rescan[v.parent] = struct{}{}
			}
			delete(rescan, id)
		}
	}

	if err := cm.markShared(m); err != nil {
		return nil, err
	}

	var du []*client.UsageInfo
	for id, cr := range m {
		c := &client.UsageInfo{
			ID:          id,
			Mutable:     cr.mutable,
			InUse:       cr.refs > 0,
			Size:        cr.size,
			Parent:      cr.parent,
			CreatedAt:   cr.createdAt,
			Description: cr.description,
			LastUsedAt:  cr.lastUsedAt,
			UsageCount:  cr.usageCount,
			RecordType:  cr.recordType,
			Shared:      cr.shared,
		}
		if filter.Match(adaptUsageInfo(c)) {
			du = append(du, c)
		}
	}

	eg, ctx := errgroup.WithContext(ctx)

	for _, d := range du {
		if d.Size == sizeUnknown {
			func(d *client.UsageInfo) {
				eg.Go(func() error {
					ref, err := cm.Get(ctx, d.ID, NoUpdateLastUsed)
					if err != nil {
						d.Size = 0
						return nil
					}
					s, err := ref.Size(ctx)
					if err != nil {
						return err
					}
					d.Size = s
					return ref.Release(context.TODO())
				})
			}(d)
		}
	}

	if err := eg.Wait(); err != nil {
		return du, err
	}

	return du, nil
}

func IsLocked(err error) bool {
	return errors.Cause(err) == ErrLocked
}

func IsNotFound(err error) bool {
	return errors.Cause(err) == errNotFound
}

type RefOption interface{}

type cachePolicy int

const (
	cachePolicyDefault cachePolicy = iota
	cachePolicyRetain
)

type withMetadata interface {
	Metadata() *metadata.StorageItem
}

type noUpdateLastUsed struct{}

var NoUpdateLastUsed noUpdateLastUsed

func HasCachePolicyRetain(m withMetadata) bool {
	return getCachePolicy(m.Metadata()) == cachePolicyRetain
}

func CachePolicyRetain(m withMetadata) error {
	return queueCachePolicy(m.Metadata(), cachePolicyRetain)
}

func WithDescription(descr string) RefOption {
	return func(m withMetadata) error {
		return queueDescription(m.Metadata(), descr)
	}
}

func WithRecordType(t client.UsageRecordType) RefOption {
	return func(m withMetadata) error {
		return queueRecordType(m.Metadata(), t)
	}
}

func WithCreationTime(tm time.Time) RefOption {
	return func(m withMetadata) error {
		return queueCreatedAt(m.Metadata(), tm)
	}
}

func initializeMetadata(m withMetadata, opts ...RefOption) error {
	md := m.Metadata()
	if tm := GetCreatedAt(md); !tm.IsZero() {
		return nil
	}

	if err := queueCreatedAt(md, time.Now()); err != nil {
		return err
	}

	for _, opt := range opts {
		if fn, ok := opt.(func(withMetadata) error); ok {
			if err := fn(m); err != nil {
				return err
			}
		}
	}

	return md.Commit()
}

func adaptUsageInfo(info *client.UsageInfo) filters.Adaptor {
	return filters.AdapterFunc(func(fieldpath []string) (string, bool) {
		if len(fieldpath) == 0 {
			return "", false
		}

		switch fieldpath[0] {
		case "id":
			return info.ID, info.ID != ""
		case "parent":
			return info.Parent, info.Parent != ""
		case "description":
			return info.Description, info.Description != ""
		case "inuse":
			return "", info.InUse
		case "mutable":
			return "", info.Mutable
		case "immutable":
			return "", !info.Mutable
		case "type":
			return string(info.RecordType), info.RecordType != ""
		case "shared":
			return "", info.Shared
		case "private":
			return "", !info.Shared
		}

		// TODO: add int/datetime/bytes support for more fields

		return "", false
	})
}

type pruneOpt struct {
	filter       filters.Filter
	all          bool
	checkShared  ExternalRefChecker
	keepDuration time.Duration
	keepBytes    int64
	totalSize    int64
}

type deleteRecord struct {
	*cacheRecord
	lastUsedAt      *time.Time
	usageCount      int
	lastUsedAtIndex int
	usageCountIndex int
}

func sortDeleteRecords(toDelete []*deleteRecord) {
	sort.Slice(toDelete, func(i, j int) bool {
		if toDelete[i].lastUsedAt == nil {
			return true
		}
		if toDelete[j].lastUsedAt == nil {
			return false
		}
		return toDelete[i].lastUsedAt.Before(*toDelete[j].lastUsedAt)
	})

	maxLastUsedIndex := 0
	var val time.Time
	for _, v := range toDelete {
		if v.lastUsedAt != nil && v.lastUsedAt.After(val) {
			val = *v.lastUsedAt
			maxLastUsedIndex++
		}
		v.lastUsedAtIndex = maxLastUsedIndex
	}

	sort.Slice(toDelete, func(i, j int) bool {
		return toDelete[i].usageCount < toDelete[j].usageCount
	})

	maxUsageCountIndex := 0
	var count int
	for _, v := range toDelete {
		if v.usageCount != count {
			count = v.usageCount
			maxUsageCountIndex++
		}
		v.usageCountIndex = maxUsageCountIndex
	}

	sort.Slice(toDelete, func(i, j int) bool {
		return float64(toDelete[i].lastUsedAtIndex)/float64(maxLastUsedIndex)+
			float64(toDelete[i].usageCountIndex)/float64(maxUsageCountIndex) <
			float64(toDelete[j].lastUsedAtIndex)/float64(maxLastUsedIndex)+
				float64(toDelete[j].usageCountIndex)/float64(maxUsageCountIndex)
	})
}
