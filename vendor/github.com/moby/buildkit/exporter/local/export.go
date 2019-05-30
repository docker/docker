package local

import (
	"context"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/docker/docker/pkg/idtools"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/exporter"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/filesync"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/util/progress"
	"github.com/pkg/errors"
	"github.com/tonistiigi/fsutil"
	fstypes "github.com/tonistiigi/fsutil/types"
	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"
)

type Opt struct {
	SessionManager *session.Manager
}

type localExporter struct {
	opt Opt
	// session manager
}

func New(opt Opt) (exporter.Exporter, error) {
	le := &localExporter{opt: opt}
	return le, nil
}

func (e *localExporter) Resolve(ctx context.Context, opt map[string]string) (exporter.ExporterInstance, error) {
	id := session.FromContext(ctx)
	if id == "" {
		return nil, errors.New("could not access local files without session")
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	caller, err := e.opt.SessionManager.Get(timeoutCtx, id)
	if err != nil {
		return nil, err
	}

	li := &localExporterInstance{localExporter: e, caller: caller}
	return li, nil
}

type localExporterInstance struct {
	*localExporter
	caller session.Caller
}

func (e *localExporterInstance) Name() string {
	return "exporting to client"
}

func (e *localExporterInstance) Export(ctx context.Context, inp exporter.Source) (map[string]string, error) {
	isMap := len(inp.Refs) > 0

	export := func(ctx context.Context, k string, ref cache.ImmutableRef) func() error {
		return func() error {
			var src string
			var err error
			var idmap *idtools.IdentityMapping
			if ref == nil {
				src, err = ioutil.TempDir("", "buildkit")
				if err != nil {
					return err
				}
				defer os.RemoveAll(src)
			} else {
				mount, err := ref.Mount(ctx, true)
				if err != nil {
					return err
				}

				lm := snapshot.LocalMounter(mount)

				src, err = lm.Mount()
				if err != nil {
					return err
				}

				idmap = mount.IdentityMapping()

				defer lm.Unmount()
			}

			walkOpt := &fsutil.WalkOpt{}

			if idmap != nil {
				walkOpt.Map = func(p string, st *fstypes.Stat) bool {
					uid, gid, err := idmap.ToContainer(idtools.Identity{
						UID: int(st.Uid),
						GID: int(st.Gid),
					})
					if err != nil {
						return false
					}
					st.Uid = uint32(uid)
					st.Gid = uint32(gid)
					return true
				}
			}

			fs := fsutil.NewFS(src, walkOpt)
			lbl := "copying files"
			if isMap {
				lbl += " " + k
				fs, err = fsutil.SubDirFS([]fsutil.Dir{{FS: fs, Stat: fstypes.Stat{
					Mode: uint32(os.ModeDir | 0755),
					Path: strings.Replace(k, "/", "_", -1),
				}}})
				if err != nil {
					return err
				}
			}

			progress := newProgressHandler(ctx, lbl)
			if err := filesync.CopyToCaller(ctx, fs, e.caller, progress); err != nil {
				return err
			}
			return nil
		}
	}

	eg, ctx := errgroup.WithContext(ctx)

	if isMap {
		for k, ref := range inp.Refs {
			eg.Go(export(ctx, k, ref))
		}
	} else {
		eg.Go(export(ctx, "", inp.Ref))
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return nil, nil
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
