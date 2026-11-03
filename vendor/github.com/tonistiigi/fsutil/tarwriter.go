package fsutil

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/pkg/errors"
	"github.com/tonistiigi/fsutil/types"
)

func WriteTar(ctx context.Context, fs FS, w io.Writer) error {
	tw := tar.NewWriter(w)
	err := fs.Walk(ctx, "", func(path string, entry os.DirEntry, err error) error {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}

		fi, err := entry.Info()
		if err != nil {
			return err
		}
		stat, ok := fi.Sys().(*types.Stat)
		if !ok {
			return errors.WithStack(&os.PathError{Path: path, Err: syscall.EBADMSG, Op: "fileinfo without stat info"})
		}
		hdr, err := tar.FileInfoHeader(fi, stat.Linkname)
		if err != nil {
			return err
		}

		// Tar paths use slash separators on the wire.
		name := filepath.ToSlash(path)
		if fi.IsDir() && !strings.HasSuffix(name, "/") {
			name += "/"
		}
		hdr.Name = name

		hdr.Uid = int(stat.Uid)
		hdr.Gid = int(stat.Gid)
		hdr.Devmajor = stat.Devmajor
		hdr.Devminor = stat.Devminor
		hdr.Linkname = stat.Linkname
		if hdr.Linkname != "" {
			hdr.Size = 0
			if fi.Mode()&os.ModeSymlink != 0 {
				hdr.Typeflag = tar.TypeSymlink
			} else {
				hdr.Typeflag = tar.TypeLink
			}
		}

		if len(stat.Xattrs) > 0 {
			hdr.PAXRecords = map[string]string{}
		}
		for k, v := range stat.Xattrs {
			hdr.PAXRecords["SCHILY.xattr."+k] = string(v)
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return errors.Wrapf(err, "failed to write file header %s", name)
		}

		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 && hdr.Linkname == "" {
			rc, err := fs.Open(path)
			if err != nil {
				return err
			}
			if _, err := io.Copy(tw, rc); err != nil {
				return errors.WithStack(err)
			}
			if err := rc.Close(); err != nil {
				return errors.WithStack(err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}
