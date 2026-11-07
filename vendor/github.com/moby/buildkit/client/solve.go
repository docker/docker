package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	contentlocal "github.com/containerd/containerd/v2/plugins/content/local"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/gofrs/flock"
	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/ociindex"
	"github.com/moby/buildkit/exporter/containerimage/exptypes"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	sessioncontent "github.com/moby/buildkit/session/content"
	"github.com/moby/buildkit/session/filesync"
	"github.com/moby/buildkit/session/grpchijack"
	"github.com/moby/buildkit/solver/pb"
	spb "github.com/moby/buildkit/sourcepolicy/pb"
	"github.com/moby/buildkit/util/bklog"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/tonistiigi/fsutil"
	fstypes "github.com/tonistiigi/fsutil/types"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"
)

type SolveOpt struct {
	Exports               []ExportEntry
	CompatibilityVersion  int
	EnableSessionExporter bool
	LocalMounts           map[string]fsutil.FS
	LocalFilterOpt        filesync.FilterOptFunc
	OCIStores             map[string]content.Store
	SharedKey             string
	Frontend              string
	FrontendAttrs         map[string]string
	FrontendInputs        map[string]llb.State
	CacheExports          []CacheOptionsEntry
	CacheImports          []CacheOptionsEntry
	Session               []session.Attachable
	AllowedEntitlements   []string
	SharedSession         *session.Session // TODO: refactor to better session syncing
	SessionPreInitialized bool             // TODO: refactor to better session syncing
	Internal              bool
	SourcePolicy          *spb.Policy
	SourcePolicyProvider  session.Attachable
	ProxyNetwork          bool
	Ref                   string
}

type ExportEntry struct {
	Type        string
	Attrs       map[string]string
	Output      filesync.FileOutputFunc // for ExporterOCI and ExporterDocker
	OutputDir   string                  // for ExporterLocal
	OutputStore content.Store
}

type CacheOptionsEntry struct {
	Type  string
	Attrs map[string]string
}

// Solve calls Solve on the controller.
// def must be nil if (and only if) opt.Frontend is set.
func (c *Client) Solve(ctx context.Context, def *llb.Definition, opt SolveOpt, statusChan chan *SolveStatus) (*SolveResponse, error) {
	defer func() {
		if statusChan != nil {
			close(statusChan)
		}
	}()

	if opt.Frontend == "" && def == nil {
		return nil, errors.New("invalid empty definition")
	}
	if opt.Frontend != "" && def != nil {
		return nil, errors.Errorf("invalid definition for frontend %s", opt.Frontend)
	}

	return c.solve(ctx, def, nil, opt, statusChan)
}

type runGatewayCB func(ref string, s *session.Session, opts map[string]string) error

func (c *Client) solve(ctx context.Context, def *llb.Definition, runGateway runGatewayCB, opt SolveOpt, statusChan chan *SolveStatus) (*SolveResponse, error) {
	if def != nil && runGateway != nil {
		return nil, errors.New("invalid with def and cb")
	}

	syncedDirs, err := prepareSyncedFiles(def, opt.LocalMounts)
	if err != nil {
		return nil, err
	}

	ref := identity.NewID()
	if opt.Ref != "" {
		ref = opt.Ref
	}

	callerCtx := ctx
	eg, ctx := errgroup.WithContext(ctx)

	statusContext, cancelStatus := context.WithCancelCause(context.Background())
	defer cancelStatus(errors.WithStack(context.Canceled))

	if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		statusContext = trace.ContextWithSpan(statusContext, span)
	}

	s := opt.SharedSession

	if s == nil {
		if opt.SessionPreInitialized {
			return nil, errors.New("no session provided for preinitialized option")
		}
		s, err = session.NewSession(statusContext, opt.SharedKey)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create session")
		}
	}

	cacheOpt, err := parseCacheOptions(ctx, runGateway != nil, opt)
	if err != nil {
		return nil, err
	}

	cacheStores := make(map[string]*cacheExportStore)
	for storePath := range cacheOpt.storesToUpdate {
		key := "local:" + storePath
		store := &cacheExportStore{
			Store: cacheOpt.contentStores[key],
			lock:  flock.New(filepath.Join(storePath, "cache.lock")),
		}
		// Defer until solve returns: export locks must outlive index publication,
		// and also be released on error. Closing again after explicit cleanup is safe.
		defer store.Close()
		cacheOpt.contentStores[key] = store
		cacheStores[storePath] = store
	}

	storesToUpdate := []string{}

	if !opt.SessionPreInitialized {
		if len(syncedDirs) > 0 {
			s.Allow(filesync.NewFSSyncProvider(syncedDirs, opt.LocalFilterOpt))
		}

		for _, a := range opt.Session {
			s.Allow(a)
		}

		contentStores := map[string]content.Store{}
		maps.Copy(contentStores, cacheOpt.contentStores)
		for key, store := range opt.OCIStores {
			key2 := "oci:" + key
			if _, ok := contentStores[key2]; ok {
				return nil, errors.Errorf("oci store key %q already exists", key)
			}
			contentStores[key2] = store
		}

		var syncTargets []filesync.FSSyncTarget
		for exID, ex := range opt.Exports {
			var supportFile, supportDir, supportStore bool
			switch ex.Type {
			case ExporterLocal:
				supportDir = true
			case ExporterTar:
				supportFile = true
			case ExporterOCI, ExporterDocker:
				supportFile = ex.Output != nil
				supportStore = ex.OutputStore != nil || ex.OutputDir != ""
				if supportFile && supportStore {
					return nil, errors.Errorf("both file and store output is not supported by %s exporter", ex.Type)
				}
			}
			if !supportFile && ex.Output != nil {
				return nil, errors.Errorf("output file writer is not supported by %s exporter", ex.Type)
			}
			if !supportDir && !supportStore && ex.OutputDir != "" {
				return nil, errors.Errorf("output directory is not supported by %s exporter", ex.Type)
			}
			if !supportStore && ex.OutputStore != nil {
				return nil, errors.Errorf("output store is not supported by %s exporter", ex.Type)
			}
			if supportFile {
				if ex.Output == nil {
					return nil, errors.Errorf("output file writer is required for %s exporter", ex.Type)
				}
				syncTargets = append(syncTargets, filesync.WithFSSync(exID, ex.Output))
			}
			if supportDir {
				if ex.OutputDir == "" {
					return nil, errors.Errorf("output directory is required for %s exporter", ex.Type)
				}
				if ex.Type == ExporterLocal {
					mode := LocalExporterModeCopy
					if ex.Attrs != nil {
						mode, err = ParseLocalExporterMode(ex.Attrs["mode"])
						if err != nil {
							return nil, err
						}
					}
					if mode == LocalExporterModeDelete {
						syncTargets = append(syncTargets, filesync.WithFSSyncDirDelete(exID, ex.OutputDir))
					} else {
						syncTargets = append(syncTargets, filesync.WithFSSyncDir(exID, ex.OutputDir))
					}
				} else {
					syncTargets = append(syncTargets, filesync.WithFSSyncDir(exID, ex.OutputDir))
				}
			}
			if supportStore {
				store := ex.OutputStore
				if store == nil {
					if err := os.MkdirAll(ex.OutputDir, 0755); err != nil {
						return nil, err
					}
					store, err = contentlocal.NewStore(ex.OutputDir)
					if err != nil {
						return nil, err
					}
					storesToUpdate = append(storesToUpdate, ex.OutputDir)
				}

				// TODO: this should be dependent on the exporter id (to allow multiple oci exporters)
				storeName := "export"
				if _, ok := contentStores[storeName]; ok {
					return nil, errors.Errorf("oci store key %q already exists", storeName)
				}
				contentStores[storeName] = store
			}
		}

		if len(contentStores) > 0 {
			s.Allow(sessioncontent.NewAttachable(contentStores))
		}

		if len(syncTargets) > 0 {
			s.Allow(filesync.NewFSSyncTarget(syncTargets...))
		}

		if opt.SourcePolicyProvider != nil {
			s.Allow(opt.SourcePolicyProvider)
		}

		eg.Go(func() error {
			sd := c.sessionDialer
			if sd == nil {
				sd = grpchijack.Dialer(c.ControlClient())
			}
			return s.Run(statusContext, sd)
		})
	}

	frontendAttrs := maps.Clone(opt.FrontendAttrs)
	maps.Copy(frontendAttrs, cacheOpt.frontendAttrs)

	const statusInactivityTimeout = 5 * time.Second
	statusActivity := make(chan struct{}, 1)

	solveCtx, cancelSolve := context.WithCancelCause(ctx)
	var res *SolveResponse
	eg.Go(func() error {
		ctx := solveCtx
		defer cancelSolve(errors.WithStack(context.Canceled))

		defer func() { // make sure the Status ends cleanly on build errors
			go func() {
				// Start inactivity monitoring after solve completes
				statusInactivityTimer := time.NewTimer(statusInactivityTimeout)
				defer statusInactivityTimer.Stop()
				for {
					select {
					case <-statusContext.Done():
						return
					case <-statusActivity:
						// Reset timer on activity
						statusInactivityTimer.Reset(statusInactivityTimeout)
					case <-statusInactivityTimer.C:
						cancelStatus(errors.WithStack(context.Canceled))
						return
					}
				}
			}()
			if !opt.SessionPreInitialized {
				bklog.G(ctx).Debugf("stopping session")
				s.Close()
			}
		}()
		var pbd *pb.Definition
		if def != nil {
			pbd = def.ToPB()
		}

		frontendInputs := make(map[string]*pb.Definition)
		for key, st := range opt.FrontendInputs {
			def, err := st.Marshal(ctx)
			if err != nil {
				return err
			}
			frontendInputs[key] = def.ToPB()
		}

		exports := make([]*controlapi.Exporter, 0, len(opt.Exports))
		exportDeprecated := ""
		exportAttrDeprecated := map[string]string{}
		for i, exp := range opt.Exports {
			if i == 0 {
				exportDeprecated = exp.Type
				exportAttrDeprecated = exp.Attrs
			}
			exports = append(exports, &controlapi.Exporter{
				Type:  exp.Type,
				Attrs: exp.Attrs,
			})
		}

		sopt := &controlapi.SolveRequest{
			Ref:                     ref,
			Definition:              pbd,
			Exporters:               exports,
			ExporterDeprecated:      exportDeprecated,
			ExporterAttrsDeprecated: exportAttrDeprecated,
			EnableSessionExporter:   opt.EnableSessionExporter,
			Session:                 s.ID(),
			Frontend:                opt.Frontend,
			FrontendAttrs:           frontendAttrs,
			FrontendInputs:          frontendInputs,
			Cache:                   &cacheOpt.options,
			Entitlements:            slices.Clone(opt.AllowedEntitlements),
			Internal:                opt.Internal,
			CompatibilityVersion:    int64(opt.CompatibilityVersion),
			SourcePolicy:            opt.SourcePolicy,
			ProxyNetwork:            opt.ProxyNetwork,
		}
		if opt.SourcePolicyProvider != nil {
			sopt.SourcePolicySession = s.ID()
		}

		resp, err := c.ControlClient().Solve(ctx, sopt)
		if err != nil {
			return errors.Wrap(err, "failed to solve")
		}
		res = &SolveResponse{
			ExporterResponse: resp.ExporterResponse,
		}
		return nil
	})

	if runGateway != nil {
		eg.Go(func() error {
			err := runGateway(ref, s, frontendAttrs)
			if err == nil {
				return nil
			}

			// If the callback failed then the main
			// `Solve` (called above) should error as
			// well. However as a fallback we wait up to
			// 5s for that to happen before failing this
			// goroutine.
			select {
			case <-solveCtx.Done():
			case <-time.After(5 * time.Second):
				cancelSolve(errors.WithStack(context.Canceled))
			}

			return err
		})
	}

	eg.Go(func() error {
		stream, err := c.ControlClient().Status(statusContext, &controlapi.StatusRequest{
			Ref: ref,
		})
		if err != nil {
			return errors.Wrap(err, "failed to get status")
		}
		for {
			resp, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				// Ignore context canceled, triggered after inactivity timeout
				if errors.Is(err, context.Canceled) || statusContext.Err() != nil {
					return nil
				}
				return errors.Wrap(err, "failed to receive status")
			}
			// Signal activity (non-blocking)
			select {
			case statusActivity <- struct{}{}:
			default:
			}
			if statusChan != nil {
				statusChan <- NewSolveStatus(resp)
			}
		}
	})

	if err := eg.Wait(); err != nil {
		return nil, err
	}
	// Update index.json of exported cache content store
	// FIXME(AkihiroSuda): dedupe const definition of cache/remotecache.ExporterResponseManifestDesc = "cache.manifest"
	if manifestDescJSON := res.ExporterResponse["cache.manifest"]; manifestDescJSON != "" {
		var manifestDesc ocispecs.Descriptor
		if err = json.Unmarshal([]byte(manifestDescJSON), &manifestDesc); err != nil {
			return nil, err
		}
		for storePath, store := range cacheOpt.storesToUpdate {
			idx := ociindex.NewStoreIndex(storePath)
			if err := idx.Put(manifestDesc, ociindex.Tag(store.tag)); err != nil {
				if store.ignoreError {
					bklog.G(callerCtx).WithError(err).WithField("cache_store_path", storePath).Warn("failed to update cache index, ignoring")
					continue
				}
				return nil, err
			}
		}
	}
	if manifestDescDt := res.ExporterResponse[exptypes.ExporterImageDescriptorKey]; manifestDescDt != "" {
		manifestDescDt, err := base64.StdEncoding.DecodeString(manifestDescDt)
		if err != nil {
			return nil, err
		}
		var manifestDesc ocispecs.Descriptor
		if err = json.Unmarshal(manifestDescDt, &manifestDesc); err != nil {
			return nil, err
		}
		for _, storePath := range storesToUpdate {
			names := []ociindex.NameOrTag{ociindex.Tag("latest")}
			if t, ok := res.ExporterResponse[exptypes.ExporterImageNameKey]; ok {
				inp := strings.Split(t, ",")
				names = make([]ociindex.NameOrTag, len(inp))
				for i, n := range inp {
					names[i] = ociindex.Name(n)
				}
			}
			idx := ociindex.NewStoreIndex(storePath)
			if err := idx.Put(manifestDesc, names...); err != nil {
				return nil, err
			}
		}
	}
	// Release all export locks before attempting an exclusive reset lock.
	for _, store := range cacheStores {
		if err := store.Close(); err != nil {
			bklog.G(callerCtx).WithError(err).WithField("cache_store_path", filepath.Dir(store.lock.Path())).Warn("failed to unlock cache store")
		}
	}
	// Reset cache stores that have reset=true — delete unreferenced blobs
	for _, ref := range cacheOpt.storesToReset {
		if err := resetCacheStore(callerCtx, ref.store, ref.path, cacheStores[ref.path].lock); err != nil {
			bklog.G(callerCtx).WithError(err).WithField("cache_store_path", ref.path).Warn("failed to reset cache store")
		}
	}
	return res, nil
}

// cacheExportStore protects blobs from reset starting at the first write attempt,
// including attempts that reuse existing blobs. The caller releases the lock only
// after publishing the export in index.json, or when the solve fails.
type cacheExportStore struct {
	content.Store
	mu     sync.RWMutex
	lock   *flock.Flock
	closed bool
}

func (s *cacheExportStore) Writer(ctx context.Context, opts ...content.WriterOpt) (content.Writer, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, errors.WithStack(os.ErrClosed)
	}
	if !s.lock.RLocked() {
		_, err := s.lock.TryRLockContext(ctx, 100*time.Millisecond)
		if err != nil {
			return nil, errors.Wrap(err, "failed to lock cache store for export")
		}
	}
	return s.Store.Writer(ctx, opts...)
}

// Close is called after session requests have finished or been canceled. Prevent
// late handlers from reacquiring the lock after the solve has returned.
func (s *cacheExportStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return s.lock.Close()
}

// resetCacheStore deletes all blobs not referenced by any manifest in
// index.json. Referenced blobs are always preserved.
func resetCacheStore(ctx context.Context, cs content.Store, storePath string, lock *flock.Flock) error {
	// Keep the lock file: unlinking it could allow another process to lock a
	// different inode while an exporter is still using the original one.
	defer lock.Close()
	locked, err := lock.TryLock()
	if err != nil {
		return errors.Wrap(err, "reset: failed to lock cache store")
	}
	if !locked {
		return errors.New("reset: cache store is in use, skipping cleanup")
	}

	idx := ociindex.NewStoreIndex(storePath)
	index, err := idx.Read()
	if err != nil {
		return errors.Wrap(err, "reset: failed to read index.json")
	}

	var mu sync.Mutex
	referenced := make(map[digest.Digest]struct{})
	childrenHandler := images.ChildrenHandler(cs)
	handler := images.HandlerFunc(func(ctx context.Context, desc ocispecs.Descriptor) ([]ocispecs.Descriptor, error) {
		mu.Lock()
		referenced[desc.Digest] = struct{}{}
		mu.Unlock()
		return childrenHandler(ctx, desc)
	})
	if err := images.Dispatch(ctx, handler, nil, index.Manifests...); err != nil {
		return errors.Wrap(err, "reset: failed to collect referenced blobs")
	}

	var toDelete []digest.Digest
	if err := cs.Walk(ctx, func(info content.Info) error {
		if _, ok := referenced[info.Digest]; !ok {
			toDelete = append(toDelete, info.Digest)
		}
		return nil
	}); err != nil {
		return errors.Wrap(err, "reset: failed to walk content store")
	}

	for _, dgst := range toDelete {
		if err := cs.Delete(ctx, dgst); err != nil {
			bklog.G(ctx).WithError(err).Warnf("reset: failed to delete blob %s", dgst)
		}
	}
	return nil
}

func prepareSyncedFiles(def *llb.Definition, localMounts map[string]fsutil.FS) (filesync.StaticDirSource, error) {
	resetUIDAndGID := func(p string, st *fstypes.Stat) fsutil.MapResult {
		st.Uid = 0
		st.Gid = 0
		return fsutil.MapResultKeep
	}

	result := make(filesync.StaticDirSource, len(localMounts))
	if def == nil {
		for name, mount := range localMounts {
			mount, err := fsutil.NewFilterFS(mount, &fsutil.FilterOpt{
				Map: resetUIDAndGID,
			})
			if err != nil {
				return nil, err
			}
			result[name] = mount
		}
	} else {
		for _, dt := range def.Def {
			var op pb.Op
			if err := op.UnmarshalVT(dt); err != nil {
				return nil, errors.Wrap(err, "failed to parse llb proto op")
			}
			if src := op.GetSource(); src != nil {
				if name, ok := strings.CutPrefix(src.Identifier, "local://"); ok {
					mount, ok := localMounts[name]
					if !ok {
						return nil, errors.Errorf("local directory %s not enabled", name)
					}
					mount, err := fsutil.NewFilterFS(mount, &fsutil.FilterOpt{
						Map: resetUIDAndGID,
					})
					if err != nil {
						return nil, err
					}
					result[name] = mount
				}
			}
		}
	}
	return result, nil
}

type cacheStoreRef struct {
	path  string
	store content.Store
}

type cacheStoreUpdate struct {
	tag         string
	ignoreError bool
}

type cacheOptions struct {
	options        controlapi.CacheOptions
	contentStores  map[string]content.Store // key: ID of content store ("local:" + csDir)
	storesToUpdate map[string]cacheStoreUpdate
	storesToReset  []cacheStoreRef // cache stores with reset=true
	frontendAttrs  map[string]string
}

func parseCacheOptions(ctx context.Context, isGateway bool, opt SolveOpt) (*cacheOptions, error) {
	var (
		cacheExports []*controlapi.CacheOptionsEntry
		cacheImports []*controlapi.CacheOptionsEntry
	)
	var storesToReset []cacheStoreRef
	contentStores := make(map[string]content.Store)
	storesToUpdate := make(map[string]cacheStoreUpdate)
	frontendAttrs := make(map[string]string)
	for _, ex := range opt.CacheExports {
		if ex.Type == "local" {
			csDir := ex.Attrs["dest"]
			if csDir == "" {
				return nil, errors.New("local cache exporter requires dest")
			}
			if err := os.MkdirAll(csDir, 0755); err != nil {
				return nil, err
			}
			cs, err := newLocalCacheStore(csDir)
			if err != nil {
				return nil, err
			}
			contentStores["local:"+csDir] = cs

			tag := "latest"
			if t, ok := ex.Attrs["tag"]; ok {
				tag = t
			}
			update := cacheStoreUpdate{tag: tag}
			if v, ok := ex.Attrs["ignore-error"]; ok {
				if b, err := strconv.ParseBool(v); err == nil {
					update.ignoreError = b
				}
			}
			// TODO(AkihiroSuda): support custom index JSON path and tag
			storesToUpdate[csDir] = update

			if v, ok := ex.Attrs["reset"]; ok {
				b, err := strconv.ParseBool(v)
				if err != nil {
					return nil, errors.Wrapf(err, "failed to parse reset attribute")
				}
				if b {
					storesToReset = append(storesToReset, cacheStoreRef{path: csDir, store: cs})
				}
			}
		}
		if ex.Type == "registry" {
			regRef := ex.Attrs["ref"]
			if regRef == "" {
				return nil, errors.New("registry cache exporter requires ref")
			}
		}
		cacheExports = append(cacheExports, &controlapi.CacheOptionsEntry{
			Type:  ex.Type,
			Attrs: ex.Attrs,
		})
	}
	for _, im := range opt.CacheImports {
		if im.Type == "local" {
			csDir := im.Attrs["src"]
			if csDir == "" {
				return nil, errors.New("local cache importer requires src")
			}
			cs, err := newLocalCacheStore(csDir)
			if err != nil {
				bklog.G(ctx).Warning("local cache import at " + csDir + " not found due to err: " + err.Error())
				continue
			}
			dgst := im.Attrs["digest"]
			// if digest is not specified, attempt to load from tag
			if dgst == "" {
				tag := "latest"
				if t, ok := im.Attrs["tag"]; ok {
					tag = t
				}
				if tag == "" {
					return nil, errors.New("local cache importer requires either explicit digest, \"latest\" tag or custom tag on index.json")
				}

				idx := ociindex.NewStoreIndex(csDir)
				desc, err := idx.Get(tag)
				if err != nil {
					bklog.G(ctx).Warning("local cache import at " + csDir + " skipped due to err: " + err.Error())
					continue
				}
				if desc == nil {
					bklog.G(ctx).Warning("local cache import at " + csDir + " skipped: no digest found for tag " + tag)
					continue
				}
				im.Attrs["digest"] = desc.Digest.String()
			}
			if _, err := cs.Info(ctx, digest.Digest(im.Attrs["digest"])); err != nil {
				bklog.G(ctx).Warning("local cache import at " + csDir + " skipped: digest " + im.Attrs["digest"] + " unavailable: " + err.Error())
				continue
			}
			contentStores["local:"+csDir] = cs
		}
		if im.Type == "registry" {
			regRef := im.Attrs["ref"]
			if regRef == "" {
				return nil, errors.New("registry cache importer requires ref")
			}
		}
		cacheImports = append(cacheImports, &controlapi.CacheOptionsEntry{
			Type:  im.Type,
			Attrs: im.Attrs,
		})
	}
	if opt.Frontend != "" || isGateway {
		if len(cacheImports) > 0 {
			s, err := json.Marshal(cacheImports)
			if err != nil {
				return nil, err
			}
			frontendAttrs["cache-imports"] = string(s)
		}
	}
	res := cacheOptions{
		options: controlapi.CacheOptions{
			Exports: cacheExports,
			Imports: cacheImports,
		},
		contentStores:  contentStores,
		storesToUpdate: storesToUpdate,
		storesToReset:  storesToReset,
		frontendAttrs:  frontendAttrs,
	}
	return &res, nil
}

// localCacheStore coordinates ingests across clients sharing a cache directory.
// The underlying local store only locks references within one store instance.
type localCacheStore struct {
	content.Store
	dir string
}

func newLocalCacheStore(dir string) (content.Store, error) {
	store, err := contentlocal.NewStore(dir)
	if err != nil {
		return nil, err
	}
	return &localCacheStore{Store: store, dir: dir}, nil
}

func (s *localCacheStore) lock(ref string) (*flock.Flock, error) {
	dir := filepath.Join(s.dir, "ingest-locks")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	// Keep lock files after unlocking: deleting one could let another client
	// lock a different inode while an existing waiter still uses the old one.
	// This retains one empty file per ref, even after cache reset. The reset
	// lock cannot protect their removal: not all users of this store hold it.
	lock := flock.New(filepath.Join(dir, digest.FromString(ref).Encoded()))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, errors.Wrapf(cerrdefs.ErrUnavailable, "ingest ref %q is locked", ref)
	}
	return lock, nil
}

func (s *localCacheStore) Writer(ctx context.Context, opts ...content.WriterOpt) (content.Writer, error) {
	var wo content.WriterOpts
	for _, opt := range opts {
		if err := opt(&wo); err != nil {
			return nil, err
		}
	}
	if wo.Ref == "" {
		return nil, errors.Wrap(cerrdefs.ErrInvalidArgument, "ref must not be empty")
	}
	lock, err := s.lock(wo.Ref)
	if err != nil {
		return nil, err
	}
	w, err := s.Store.Writer(ctx, content.WithRef(wo.Ref), content.WithDescriptor(wo.Desc))
	if err != nil {
		lock.Close()
		return nil, err
	}
	return &localCacheWriter{Writer: w, lock: lock}, nil
}

func (s *localCacheStore) Abort(ctx context.Context, ref string) error {
	lock, err := s.lock(ref)
	if err != nil {
		return err
	}
	defer lock.Close()
	return s.Store.Abort(ctx, ref)
}

type localCacheWriter struct {
	content.Writer
	lock *flock.Flock
}

func (w *localCacheWriter) Commit(ctx context.Context, size int64, expected digest.Digest, opts ...content.Opt) error {
	err := w.Writer.Commit(ctx, size, expected, opts...)
	closeErr := w.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (w *localCacheWriter) Close() error {
	err := w.Writer.Close()
	lockErr := w.lock.Close()
	if err != nil {
		return err
	}
	return lockErr
}
