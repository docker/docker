package fsutil

import (
	"context"
	"hash"
	"io"
	gofs "io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/tonistiigi/fsutil/types"
	"golang.org/x/sync/errgroup"
)

type WriteToFunc func(context.Context, string, io.WriteCloser) error

type DiskWriterOpt struct {
	AsyncDataCb   WriteToFunc
	SyncDataCb    WriteToFunc
	NotifyCb      func(ChangeKind, string, os.FileInfo, error) error
	ContentHasher ContentHasher
	Filter        FilterFunc
}

type FilterFunc func(string, *types.Stat) bool

type DiskWriter struct {
	opt  DiskWriterOpt
	dest string

	ctx         context.Context
	cancel      func()
	eg          *errgroup.Group
	egCtx       context.Context
	filter      FilterFunc
	dirModTimes map[string]int64
}

func NewDiskWriter(ctx context.Context, dest string, opt DiskWriterOpt) (*DiskWriter, error) {
	if opt.SyncDataCb == nil && opt.AsyncDataCb == nil {
		return nil, errors.New("no data callback specified")
	}
	if opt.SyncDataCb != nil && opt.AsyncDataCb != nil {
		return nil, errors.New("can't specify both sync and async data callbacks")
	}

	ctx, cancel := context.WithCancel(ctx)
	eg, egCtx := errgroup.WithContext(ctx)

	return &DiskWriter{
		opt:         opt,
		dest:        dest,
		eg:          eg,
		ctx:         ctx,
		egCtx:       egCtx,
		cancel:      cancel,
		filter:      opt.Filter,
		dirModTimes: map[string]int64{},
	}, nil
}

func (dw *DiskWriter) Wait(ctx context.Context) error {
	if err := dw.eg.Wait(); err != nil {
		return err
	}
	return filepath.WalkDir(dw.dest, func(path string, d gofs.DirEntry, prevErr error) error {
		if prevErr != nil {
			return prevErr
		}
		if !d.IsDir() {
			return nil
		}
		if mtime, ok := dw.dirModTimes[path]; ok {
			return chtimes(path, mtime)
		}
		return nil
	})
}

func (dw *DiskWriter) HandleChange(kind ChangeKind, p string, fi os.FileInfo, err error) (retErr error) {
	if err != nil {
		return err
	}

	select {
	case <-dw.ctx.Done():
		return dw.ctx.Err()
	default:
	}

	defer func() {
		if retErr != nil {
			dw.cancel()
		}
	}()

	destPath := filepath.Join(dw.dest, p)

	if kind == ChangeKindDelete {
		if dw.filter != nil {
			var empty types.Stat
			if ok := dw.filter(p, &empty); !ok {
				return nil
			}
		}
		// todo: no need to validate if diff is trusted but is it always?
		if err := os.RemoveAll(destPath); err != nil {
			return errors.Wrapf(err, "failed to remove: %s", destPath)
		}
		if dw.opt.NotifyCb != nil {
			if err := dw.opt.NotifyCb(kind, p, nil, nil); err != nil {
				return err
			}
		}
		return nil
	}

	stat, ok := fi.Sys().(*types.Stat)
	if !ok {
		return errors.WithStack(&os.PathError{Path: p, Err: syscall.EBADMSG, Op: "change without stat info"})
	}

	statCopy := stat.Clone()

	if dw.filter != nil {
		if ok := dw.filter(p, statCopy); !ok {
			return nil
		}
	}

	rename := true
	oldFi, err := os.Lstat(destPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if kind != ChangeKindAdd {
				return errors.Wrap(err, "modify/rm")
			}
			rename = false
		} else {
			return errors.WithStack(err)
		}
	}

	if oldFi != nil && fi.IsDir() && oldFi.IsDir() {
		if err := rewriteMetadata(destPath, statCopy); err != nil {
			return errors.Wrapf(err, "error setting dir metadata for %s", destPath)
		}
		return nil
	}

	newPath := destPath
	if rename {
		newPath = filepath.Join(filepath.Dir(destPath), ".tmp."+nextSuffix())
	}

	isRegularFile := false

	switch {
	case fi.IsDir():
		if err := os.Mkdir(newPath, fi.Mode()); err != nil {
			if errors.Is(err, syscall.EEXIST) {
				// we saw a race to create this directory, so try again
				return dw.HandleChange(kind, p, fi, nil)
			}
			return errors.Wrapf(err, "failed to create dir %s", newPath)
		}
		dw.dirModTimes[destPath] = statCopy.ModTime
	case fi.Mode()&os.ModeDevice != 0 || fi.Mode()&os.ModeNamedPipe != 0:
		if err := handleTarTypeBlockCharFifo(newPath, statCopy); err != nil {
			return errors.Wrapf(err, "failed to create device %s", newPath)
		}
	case fi.Mode()&os.ModeSymlink != 0:
		if err := os.Symlink(statCopy.Linkname, newPath); err != nil {
			return errors.Wrapf(err, "failed to symlink %s", newPath)
		}
	case statCopy.Linkname != "":
		if err := os.Link(filepath.Join(dw.dest, statCopy.Linkname), newPath); err != nil {
			return errors.Wrapf(err, "failed to link %s to %s", newPath, statCopy.Linkname)
		}
	default:
		isRegularFile = true
		file, err := os.OpenFile(newPath, os.O_CREATE|os.O_WRONLY, fi.Mode())
		if err != nil {
			return errors.Wrapf(err, "failed to create %s", newPath)
		}
		if dw.opt.SyncDataCb != nil {
			if err := dw.processChange(dw.ctx, ChangeKindAdd, p, fi, file); err != nil {
				file.Close()
				return err
			}
		}
		if err := file.Close(); err != nil {
			return errors.Wrapf(err, "failed to close %s", newPath)
		}
	}

	if err := rewriteMetadata(newPath, statCopy); err != nil {
		return errors.Wrapf(err, "error setting metadata for %s", newPath)
	}

	if rename {
		if oldFi.IsDir() != fi.IsDir() {
			if err := os.RemoveAll(destPath); err != nil {
				return errors.Wrapf(err, "failed to remove %s", destPath)
			}
		}

		if err := renameFile(newPath, destPath); err != nil {
			return errors.Wrapf(err, "failed to rename %s to %s", newPath, destPath)
		}
	}

	if isRegularFile {
		if dw.opt.AsyncDataCb != nil {
			dw.requestAsyncFileData(p, destPath, fi, statCopy)
		}
	} else {
		return dw.processChange(dw.ctx, kind, p, fi, nil)
	}

	return nil
}

func (dw *DiskWriter) requestAsyncFileData(p, dest string, fi os.FileInfo, st *types.Stat) {
	// todo: limit worker threads
	dw.eg.Go(func() error {
		if err := dw.processChange(dw.egCtx, ChangeKindAdd, p, fi, &lazyFileWriter{
			dest: dest,
		}); err != nil {
			return err
		}
		return chtimes(dest, st.ModTime) // TODO: parent dirs
	})
}

func (dw *DiskWriter) processChange(ctx context.Context, kind ChangeKind, p string, fi os.FileInfo, w io.WriteCloser) error {
	origw := w
	var hw *hashedWriter
	if dw.opt.NotifyCb != nil {
		var err error
		if hw, err = newHashWriter(dw.opt.ContentHasher, fi, w); err != nil {
			return err
		}
		w = hw
	}
	if origw != nil {
		fn := dw.opt.SyncDataCb
		if fn == nil && dw.opt.AsyncDataCb != nil {
			fn = dw.opt.AsyncDataCb
		}
		if err := fn(ctx, p, w); err != nil {
			return err
		}
	} else {
		if hw != nil {
			hw.Close()
		}
	}
	if hw != nil {
		return dw.opt.NotifyCb(kind, p, hw, nil)
	}
	return nil
}

type hashedWriter struct {
	os.FileInfo
	io.Writer
	h    hash.Hash
	w    io.WriteCloser
	dgst digest.Digest
}

func newHashWriter(ch ContentHasher, fi os.FileInfo, w io.WriteCloser) (*hashedWriter, error) {
	stat, ok := fi.Sys().(*types.Stat)
	if !ok {
		return nil, errors.Errorf("invalid change without stat information")
	}

	h, err := ch(stat)
	if err != nil {
		return nil, err
	}
	hw := &hashedWriter{
		FileInfo: fi,
		Writer:   io.MultiWriter(w, h),
		h:        h,
		w:        w,
	}
	return hw, nil
}

func (hw *hashedWriter) Close() error {
	hw.dgst = digest.NewDigest(digest.SHA256, hw.h)
	if hw.w != nil {
		return hw.w.Close()
	}
	return nil
}

func (hw *hashedWriter) Digest() digest.Digest {
	return hw.dgst
}

type lazyFileWriter struct {
	dest     string
	f        *os.File
	fileMode *os.FileMode
}

func (lfw *lazyFileWriter) Write(dt []byte) (int, error) {
	if lfw.f == nil {
		file, err := os.OpenFile(lfw.dest, os.O_WRONLY, 0)
		if os.IsPermission(err) {
			// retry after chmod
			fi, er := os.Stat(lfw.dest)
			if er == nil {
				mode := fi.Mode()
				lfw.fileMode = &mode
				er = os.Chmod(lfw.dest, mode|0222)
				if er == nil {
					file, err = os.OpenFile(lfw.dest, os.O_WRONLY, 0)
				}
			}
		}
		if err != nil {
			return 0, errors.Wrapf(err, "failed to open %s", lfw.dest)
		}
		lfw.f = file
	}
	return lfw.f.Write(dt)
}

func (lfw *lazyFileWriter) Close() error {
	var err error
	if lfw.f != nil {
		err = lfw.f.Close()
	}
	if err == nil && lfw.fileMode != nil {
		err = os.Chmod(lfw.dest, *lfw.fileMode)
	}
	return err
}

// Random number state.
// We generate random temporary file names so that there's a good
// chance the file doesn't exist yet - keeps the number of tries in
// TempFile to a minimum.
var (
	rand   uint32
	randmu sync.Mutex
)

func reseed() uint32 {
	return uint32(time.Now().UnixNano() + int64(os.Getpid()))
}

func nextSuffix() string {
	randmu.Lock()
	r := rand
	if r == 0 {
		r = reseed()
	}
	r = r*1664525 + 1013904223 // constants from Numerical Recipes
	rand = r
	randmu.Unlock()
	return strconv.Itoa(int(1e9 + r%1e9))[1:]
}
