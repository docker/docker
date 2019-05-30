package client

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/containerd/content"
	contentlocal "github.com/containerd/containerd/content/local"
	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/ociindex"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	sessioncontent "github.com/moby/buildkit/session/content"
	"github.com/moby/buildkit/session/filesync"
	"github.com/moby/buildkit/session/grpchijack"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/entitlements"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	fstypes "github.com/tonistiigi/fsutil/types"
	"golang.org/x/sync/errgroup"
)

type SolveOpt struct {
	Exports               []ExportEntry
	LocalDirs             map[string]string
	SharedKey             string
	Frontend              string
	FrontendAttrs         map[string]string
	CacheExports          []CacheOptionsEntry
	CacheImports          []CacheOptionsEntry
	Session               []session.Attachable
	AllowedEntitlements   []entitlements.Entitlement
	SharedSession         *session.Session // TODO: refactor to better session syncing
	SessionPreInitialized bool             // TODO: refactor to better session syncing
}

type ExportEntry struct {
	Type      string
	Attrs     map[string]string
	Output    io.WriteCloser // for ExporterOCI and ExporterDocker
	OutputDir string         // for ExporterLocal
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

type runGatewayCB func(ref string, s *session.Session) error

func (c *Client) solve(ctx context.Context, def *llb.Definition, runGateway runGatewayCB, opt SolveOpt, statusChan chan *SolveStatus) (*SolveResponse, error) {
	if def != nil && runGateway != nil {
		return nil, errors.New("invalid with def and cb")
	}

	syncedDirs, err := prepareSyncedDirs(def, opt.LocalDirs)
	if err != nil {
		return nil, err
	}

	ref := identity.NewID()
	eg, ctx := errgroup.WithContext(ctx)

	statusContext, cancelStatus := context.WithCancel(context.Background())
	defer cancelStatus()

	if span := opentracing.SpanFromContext(ctx); span != nil {
		statusContext = opentracing.ContextWithSpan(statusContext, span)
	}

	s := opt.SharedSession

	if s == nil {
		if opt.SessionPreInitialized {
			return nil, errors.Errorf("no session provided for preinitialized option")
		}
		s, err = session.NewSession(statusContext, defaultSessionName(), opt.SharedKey)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create session")
		}
	}

	cacheOpt, err := parseCacheOptions(opt)
	if err != nil {
		return nil, err
	}

	var ex ExportEntry

	if !opt.SessionPreInitialized {
		if len(syncedDirs) > 0 {
			s.Allow(filesync.NewFSSyncProvider(syncedDirs))
		}

		for _, a := range opt.Session {
			s.Allow(a)
		}

		if len(opt.Exports) > 1 {
			return nil, errors.New("currently only single Exports can be specified")
		}
		if len(opt.Exports) == 1 {
			ex = opt.Exports[0]
		}

		switch ex.Type {
		case ExporterLocal:
			if ex.Output != nil {
				return nil, errors.New("output file writer is not supported by local exporter")
			}
			if ex.OutputDir == "" {
				return nil, errors.New("output directory is required for local exporter")
			}
			s.Allow(filesync.NewFSSyncTargetDir(ex.OutputDir))
		case ExporterOCI, ExporterDocker, ExporterTar:
			if ex.OutputDir != "" {
				return nil, errors.Errorf("output directory %s is not supported by %s exporter", ex.OutputDir, ex.Type)
			}
			if ex.Output == nil {
				return nil, errors.Errorf("output file writer is required for %s exporter", ex.Type)
			}
			s.Allow(filesync.NewFSSyncTarget(ex.Output))
		default:
			if ex.Output != nil {
				return nil, errors.Errorf("output file writer is not supported by %s exporter", ex.Type)
			}
			if ex.OutputDir != "" {
				return nil, errors.Errorf("output directory %s is not supported by %s exporter", ex.OutputDir, ex.Type)
			}
		}

		if len(cacheOpt.contentStores) > 0 {
			s.Allow(sessioncontent.NewAttachable(cacheOpt.contentStores))
		}

		eg.Go(func() error {
			return s.Run(statusContext, grpchijack.Dialer(c.controlClient()))
		})
	}

	for k, v := range cacheOpt.frontendAttrs {
		opt.FrontendAttrs[k] = v
	}

	solveCtx, cancelSolve := context.WithCancel(ctx)
	var res *SolveResponse
	eg.Go(func() error {
		ctx := solveCtx
		defer cancelSolve()

		defer func() { // make sure the Status ends cleanly on build errors
			go func() {
				<-time.After(3 * time.Second)
				cancelStatus()
			}()
			logrus.Debugf("stopping session")
			s.Close()
		}()
		var pbd *pb.Definition
		if def != nil {
			pbd = def.ToPB()
		}
		resp, err := c.controlClient().Solve(ctx, &controlapi.SolveRequest{
			Ref:           ref,
			Definition:    pbd,
			Exporter:      ex.Type,
			ExporterAttrs: ex.Attrs,
			Session:       s.ID(),
			Frontend:      opt.Frontend,
			FrontendAttrs: opt.FrontendAttrs,
			Cache:         cacheOpt.options,
			Entitlements:  opt.AllowedEntitlements,
		})
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
			err := runGateway(ref, s)
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
				cancelSolve()
			}

			return err
		})
	}

	eg.Go(func() error {
		stream, err := c.controlClient().Status(statusContext, &controlapi.StatusRequest{
			Ref: ref,
		})
		if err != nil {
			return errors.Wrap(err, "failed to get status")
		}
		for {
			resp, err := stream.Recv()
			if err != nil {
				if err == io.EOF {
					return nil
				}
				return errors.Wrap(err, "failed to receive status")
			}
			s := SolveStatus{}
			for _, v := range resp.Vertexes {
				s.Vertexes = append(s.Vertexes, &Vertex{
					Digest:    v.Digest,
					Inputs:    v.Inputs,
					Name:      v.Name,
					Started:   v.Started,
					Completed: v.Completed,
					Error:     v.Error,
					Cached:    v.Cached,
				})
			}
			for _, v := range resp.Statuses {
				s.Statuses = append(s.Statuses, &VertexStatus{
					ID:        v.ID,
					Vertex:    v.Vertex,
					Name:      v.Name,
					Total:     v.Total,
					Current:   v.Current,
					Timestamp: v.Timestamp,
					Started:   v.Started,
					Completed: v.Completed,
				})
			}
			for _, v := range resp.Logs {
				s.Logs = append(s.Logs, &VertexLog{
					Vertex:    v.Vertex,
					Stream:    int(v.Stream),
					Data:      v.Msg,
					Timestamp: v.Timestamp,
				})
			}
			if statusChan != nil {
				statusChan <- &s
			}
		}
	})

	if err := eg.Wait(); err != nil {
		return nil, err
	}
	// Update index.json of exported cache content store
	// FIXME(AkihiroSuda): dedupe const definition of cache/remotecache.ExporterResponseManifestDesc = "cache.manifest"
	if manifestDescJSON := res.ExporterResponse["cache.manifest"]; manifestDescJSON != "" {
		var manifestDesc ocispec.Descriptor
		if err = json.Unmarshal([]byte(manifestDescJSON), &manifestDesc); err != nil {
			return nil, err
		}
		for indexJSONPath, tag := range cacheOpt.indicesToUpdate {
			if err = ociindex.PutDescToIndexJSONFileLocked(indexJSONPath, manifestDesc, tag); err != nil {
				return nil, err
			}
		}
	}
	return res, nil
}

func prepareSyncedDirs(def *llb.Definition, localDirs map[string]string) ([]filesync.SyncedDir, error) {
	for _, d := range localDirs {
		fi, err := os.Stat(d)
		if err != nil {
			return nil, errors.Wrapf(err, "could not find %s", d)
		}
		if !fi.IsDir() {
			return nil, errors.Errorf("%s not a directory", d)
		}
	}
	resetUIDAndGID := func(p string, st *fstypes.Stat) bool {
		st.Uid = 0
		st.Gid = 0
		return true
	}

	dirs := make([]filesync.SyncedDir, 0, len(localDirs))
	if def == nil {
		for name, d := range localDirs {
			dirs = append(dirs, filesync.SyncedDir{Name: name, Dir: d, Map: resetUIDAndGID})
		}
	} else {
		for _, dt := range def.Def {
			var op pb.Op
			if err := (&op).Unmarshal(dt); err != nil {
				return nil, errors.Wrap(err, "failed to parse llb proto op")
			}
			if src := op.GetSource(); src != nil {
				if strings.HasPrefix(src.Identifier, "local://") { // TODO: just make a type property
					name := strings.TrimPrefix(src.Identifier, "local://")
					d, ok := localDirs[name]
					if !ok {
						return nil, errors.Errorf("local directory %s not enabled", name)
					}
					dirs = append(dirs, filesync.SyncedDir{Name: name, Dir: d, Map: resetUIDAndGID}) // TODO: excludes
				}
			}
		}
	}
	return dirs, nil
}

func defaultSessionName() string {
	wd, err := os.Getwd()
	if err != nil {
		return "unknown"
	}
	return filepath.Base(wd)
}

type cacheOptions struct {
	options         controlapi.CacheOptions
	contentStores   map[string]content.Store // key: ID of content store ("local:" + csDir)
	indicesToUpdate map[string]string        // key: index.JSON file name, value: tag
	frontendAttrs   map[string]string
}

func parseCacheOptions(opt SolveOpt) (*cacheOptions, error) {
	var (
		cacheExports []*controlapi.CacheOptionsEntry
		cacheImports []*controlapi.CacheOptionsEntry
		// legacy API is used for registry caches, because the daemon might not support the new API
		legacyExportRef  string
		legacyImportRefs []string
	)
	contentStores := make(map[string]content.Store)
	indicesToUpdate := make(map[string]string) // key: index.JSON file name, value: tag
	frontendAttrs := make(map[string]string)
	legacyExportAttrs := make(map[string]string)
	for _, ex := range opt.CacheExports {
		if ex.Type == "local" {
			csDir := ex.Attrs["dest"]
			if csDir == "" {
				return nil, errors.New("local cache exporter requires dest")
			}
			if err := os.MkdirAll(csDir, 0755); err != nil {
				return nil, err
			}
			cs, err := contentlocal.NewStore(csDir)
			if err != nil {
				return nil, err
			}
			contentStores["local:"+csDir] = cs
			// TODO(AkihiroSuda): support custom index JSON path and tag
			indexJSONPath := filepath.Join(csDir, "index.json")
			indicesToUpdate[indexJSONPath] = "latest"
		}
		if ex.Type == "registry" && legacyExportRef == "" {
			legacyExportRef = ex.Attrs["ref"]
			for k, v := range ex.Attrs {
				if k != "ref" {
					legacyExportAttrs[k] = v
				}
			}
		} else {
			cacheExports = append(cacheExports, &controlapi.CacheOptionsEntry{
				Type:  ex.Type,
				Attrs: ex.Attrs,
			})
		}
	}
	for _, im := range opt.CacheImports {
		attrs := im.Attrs
		if im.Type == "local" {
			csDir := im.Attrs["src"]
			if csDir == "" {
				return nil, errors.New("local cache importer requires src")
			}
			cs, err := contentlocal.NewStore(csDir)
			if err != nil {
				return nil, err
			}
			contentStores["local:"+csDir] = cs

			// if digest is not specified, load from "latest" tag
			if attrs["digest"] == "" {
				idx, err := ociindex.ReadIndexJSONFileLocked(filepath.Join(csDir, "index.json"))
				if err != nil {
					return nil, err
				}
				for _, m := range idx.Manifests {
					if m.Annotations[ocispec.AnnotationRefName] == "latest" {
						attrs["digest"] = string(m.Digest)
						break
					}
				}
				if attrs["digest"] == "" {
					return nil, errors.New("local cache importer requires either explicit digest or \"latest\" tag on index.json")
				}
			}
		}
		if im.Type == "registry" {
			legacyImportRef := attrs["ref"]
			legacyImportRefs = append(legacyImportRefs, legacyImportRef)
		} else {
			cacheImports = append(cacheImports, &controlapi.CacheOptionsEntry{
				Type:  im.Type,
				Attrs: attrs,
			})
		}
	}
	if opt.Frontend != "" {
		// use legacy API for registry importers, because the frontend might not support the new API
		if len(legacyImportRefs) > 0 {
			frontendAttrs["cache-from"] = strings.Join(legacyImportRefs, ",")
		}
		// use new API for other importers
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
			// old API (for registry caches, planned to be removed in early 2019)
			ExportRefDeprecated:   legacyExportRef,
			ExportAttrsDeprecated: legacyExportAttrs,
			ImportRefsDeprecated:  legacyImportRefs,
			// new API
			Exports: cacheExports,
			Imports: cacheImports,
		},
		contentStores:   contentStores,
		indicesToUpdate: indicesToUpdate,
		frontendAttrs:   frontendAttrs,
	}
	return &res, nil
}
