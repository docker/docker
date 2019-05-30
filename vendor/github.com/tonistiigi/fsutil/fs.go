package fsutil

import (
	"context"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pkg/errors"
	"github.com/tonistiigi/fsutil/types"
)

type FS interface {
	Walk(context.Context, filepath.WalkFunc) error
	Open(string) (io.ReadCloser, error)
}

func NewFS(root string, opt *WalkOpt) FS {
	return &fs{
		root: root,
		opt:  opt,
	}
}

type fs struct {
	root string
	opt  *WalkOpt
}

func (fs *fs) Walk(ctx context.Context, fn filepath.WalkFunc) error {
	return Walk(ctx, fs.root, fs.opt, fn)
}

func (fs *fs) Open(p string) (io.ReadCloser, error) {
	return os.Open(filepath.Join(fs.root, p))
}

type Dir struct {
	Stat types.Stat
	FS   FS
}

func SubDirFS(dirs []Dir) (FS, error) {
	sort.Slice(dirs, func(i, j int) bool {
		return dirs[i].Stat.Path < dirs[j].Stat.Path
	})
	m := map[string]Dir{}
	for _, d := range dirs {
		if path.Base(d.Stat.Path) != d.Stat.Path {
			return nil, errors.Errorf("subdir %s must be single file", d.Stat.Path)
		}
		if _, ok := m[d.Stat.Path]; ok {
			return nil, errors.Errorf("invalid path %s", d.Stat.Path)
		}
		m[d.Stat.Path] = d
	}
	return &subDirFS{m: m, dirs: dirs}, nil
}

type subDirFS struct {
	m    map[string]Dir
	dirs []Dir
}

func (fs *subDirFS) Walk(ctx context.Context, fn filepath.WalkFunc) error {
	for _, d := range fs.dirs {
		fi := &StatInfo{Stat: &d.Stat}
		if !fi.IsDir() {
			return errors.Errorf("fs subdir %s not mode directory", d.Stat.Path)
		}
		if err := fn(d.Stat.Path, fi, nil); err != nil {
			return err
		}
		if err := d.FS.Walk(ctx, func(p string, fi os.FileInfo, err error) error {
			stat, ok := fi.Sys().(*types.Stat)
			if !ok {
				return errors.Wrapf(err, "invalid fileinfo without stat info: %s", p)
			}
			stat.Path = path.Join(d.Stat.Path, stat.Path)
			if stat.Linkname != "" {
				if fi.Mode()&os.ModeSymlink != 0 {
					if strings.HasPrefix(stat.Linkname, "/") {
						stat.Linkname = path.Join("/"+d.Stat.Path, stat.Linkname)
					}
				} else {
					stat.Linkname = path.Join(d.Stat.Path, stat.Linkname)
				}
			}
			return fn(filepath.Join(d.Stat.Path, p), &StatInfo{stat}, nil)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (fs *subDirFS) Open(p string) (io.ReadCloser, error) {
	parts := strings.SplitN(filepath.Clean(p), string(filepath.Separator), 2)
	if len(parts) == 0 {
		return ioutil.NopCloser(&emptyReader{}), nil
	}
	d, ok := fs.m[parts[0]]
	if !ok {
		return nil, os.ErrNotExist
	}
	return d.FS.Open(parts[1])
}

type emptyReader struct {
}

func (*emptyReader) Read([]byte) (int, error) {
	return 0, io.EOF
}
