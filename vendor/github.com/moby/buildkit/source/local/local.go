package local

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/docker/docker/pkg/idtools"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/cache/contenthash"
	"github.com/moby/buildkit/cache/metadata"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/filesync"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/source"
	"github.com/moby/buildkit/util/progress"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/tonistiigi/fsutil"
	fstypes "github.com/tonistiigi/fsutil/types"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/time/rate"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const keySharedKey = "local.sharedKey"

type Opt struct {
	CacheAccessor cache.Accessor
	MetadataStore *metadata.Store
}

func NewSource(opt Opt) (source.Source, error) {
	ls := &localSource{
		cm: opt.CacheAccessor,
		md: opt.MetadataStore,
	}
	return ls, nil
}

type localSource struct {
	cm cache.Accessor
	md *metadata.Store
}

func (ls *localSource) ID() string {
	return source.LocalScheme
}

func (ls *localSource) Resolve(ctx context.Context, id source.Identifier, sm *session.Manager) (source.SourceInstance, error) {
	localIdentifier, ok := id.(*source.LocalIdentifier)
	if !ok {
		return nil, errors.Errorf("invalid local identifier %v", id)
	}

	return &localSourceHandler{
		src:         *localIdentifier,
		sm:          sm,
		localSource: ls,
	}, nil
}

type localSourceHandler struct {
	src source.LocalIdentifier
	sm  *session.Manager
	*localSource
}

func (ls *localSourceHandler) CacheKey(ctx context.Context, index int) (string, bool, error) {
	sessionID := ls.src.SessionID

	if sessionID == "" {
		id := session.FromContext(ctx)
		if id == "" {
			return "", false, errors.New("could not access local files without session")
		}
		sessionID = id
	}
	dt, err := json.Marshal(struct {
		SessionID       string
		IncludePatterns []string
		ExcludePatterns []string
		FollowPaths     []string
	}{SessionID: sessionID, IncludePatterns: ls.src.IncludePatterns, ExcludePatterns: ls.src.ExcludePatterns, FollowPaths: ls.src.FollowPaths})
	if err != nil {
		return "", false, err
	}
	return "session:" + ls.src.Name + ":" + digest.FromBytes(dt).String(), true, nil
}

func (ls *localSourceHandler) Snapshot(ctx context.Context) (out cache.ImmutableRef, retErr error) {

	id := session.FromContext(ctx)
	if id == "" {
		return nil, errors.New("could not access local files without session")
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	caller, err := ls.sm.Get(timeoutCtx, id)
	if err != nil {
		return nil, err
	}

	sharedKey := keySharedKey + ":" + ls.src.Name + ":" + ls.src.SharedKeyHint + ":" + caller.SharedKey() // TODO: replace caller.SharedKey() with source based hint from client(absolute-path+nodeid)

	var mutable cache.MutableRef
	sis, err := ls.md.Search(sharedKey)
	if err != nil {
		return nil, err
	}
	for _, si := range sis {
		if m, err := ls.cm.GetMutable(ctx, si.ID()); err == nil {
			logrus.Debugf("reusing ref for local: %s", m.ID())
			mutable = m
			break
		}
	}

	if mutable == nil {
		m, err := ls.cm.New(ctx, nil, cache.CachePolicyRetain, cache.WithRecordType(client.UsageRecordTypeLocalSource), cache.WithDescription(fmt.Sprintf("local source for %s", ls.src.Name)))
		if err != nil {
			return nil, err
		}
		mutable = m
		logrus.Debugf("new ref for local: %s", mutable.ID())
	}

	defer func() {
		if retErr != nil && mutable != nil {
			go mutable.Release(context.TODO())
		}
	}()

	mount, err := mutable.Mount(ctx, false)
	if err != nil {
		return nil, err
	}

	lm := snapshot.LocalMounter(mount)

	dest, err := lm.Mount()
	if err != nil {
		return nil, err
	}

	defer func() {
		if retErr != nil && lm != nil {
			lm.Unmount()
		}
	}()

	cc, err := contenthash.GetCacheContext(ctx, mutable.Metadata(), mount.IdentityMapping())
	if err != nil {
		return nil, err
	}

	opt := filesync.FSSendRequestOpt{
		Name:             ls.src.Name,
		IncludePatterns:  ls.src.IncludePatterns,
		ExcludePatterns:  ls.src.ExcludePatterns,
		FollowPaths:      ls.src.FollowPaths,
		OverrideExcludes: false,
		DestDir:          dest,
		CacheUpdater:     &cacheUpdater{cc, mount.IdentityMapping()},
		ProgressCb:       newProgressHandler(ctx, "transferring "+ls.src.Name+":"),
	}

	if idmap := mount.IdentityMapping(); idmap != nil {
		opt.Filter = func(p string, stat *fstypes.Stat) bool {
			identity, err := idmap.ToHost(idtools.Identity{
				UID: int(stat.Uid),
				GID: int(stat.Gid),
			})
			if err != nil {
				return false
			}
			stat.Uid = uint32(identity.UID)
			stat.Gid = uint32(identity.GID)
			return true
		}
	}

	if err := filesync.FSSync(ctx, caller, opt); err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, errors.Errorf("local source %s not enabled from the client", ls.src.Name)
		}
		return nil, err
	}

	if err := lm.Unmount(); err != nil {
		return nil, err
	}
	lm = nil

	if err := contenthash.SetCacheContext(ctx, mutable.Metadata(), cc); err != nil {
		return nil, err
	}

	// skip storing snapshot by the shared key if it already exists
	skipStoreSharedKey := false
	si, _ := ls.md.Get(mutable.ID())
	if v := si.Get(keySharedKey); v != nil {
		var str string
		if err := v.Unmarshal(&str); err != nil {
			return nil, err
		}
		skipStoreSharedKey = str == sharedKey
	}
	if !skipStoreSharedKey {
		v, err := metadata.NewValue(sharedKey)
		if err != nil {
			return nil, err
		}
		v.Index = sharedKey
		if err := si.Update(func(b *bolt.Bucket) error {
			return si.SetValue(b, sharedKey, v)
		}); err != nil {
			return nil, err
		}
		logrus.Debugf("saved %s as %s", mutable.ID(), sharedKey)
	}

	snap, err := mutable.Commit(ctx)
	if err != nil {
		return nil, err
	}

	mutable = nil // avoid deferred cleanup

	return snap, nil
}

func newProgressHandler(ctx context.Context, id string) func(int, bool) {
	limiter := rate.NewLimiter(rate.Every(100*time.Millisecond), 1)
	pw, _, _ := progress.FromContext(ctx)
	now := time.Now()
	st := progress.Status{
		Started: &now,
		Action:  "transferring",
	}
	pw.Write(id, st)
	return func(s int, last bool) {
		if last || limiter.Allow() {
			st.Current = s
			if last {
				now := time.Now()
				st.Completed = &now
			}
			pw.Write(id, st)
			if last {
				pw.Close()
			}
		}
	}
}

type cacheUpdater struct {
	contenthash.CacheContext
	idmap *idtools.IdentityMapping
}

func (cu *cacheUpdater) MarkSupported(bool) {
}

func (cu *cacheUpdater) ContentHasher() fsutil.ContentHasher {
	return contenthash.NewFromStat
}
