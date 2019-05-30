package fsutil

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	"github.com/tonistiigi/fsutil/types"
)

func WriteTar(ctx context.Context, fs FS, w io.Writer) error {
	tw := tar.NewWriter(w)
	err := fs.Walk(ctx, func(path string, fi os.FileInfo, err error) error {
		stat, ok := fi.Sys().(*types.Stat)
		if !ok {
			return errors.Wrapf(err, "invalid fileinfo without stat info: %s", path)
		}
		hdr, err := tar.FileInfoHeader(fi, stat.Linkname)
		if err != nil {
			return err
		}

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
			hdr.Typeflag = tar.TypeLink
		}

		if len(stat.Xattrs) > 0 {
			hdr.PAXRecords = map[string]string{}
		}
		for k, v := range stat.Xattrs {
			hdr.PAXRecords["SCHILY.xattr."+k] = string(v)
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return errors.Wrap(err, "failed to write file header")
		}

		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 && hdr.Linkname == "" {
			rc, err := fs.Open(path)
			if err != nil {
				return err
			}
			if _, err := io.Copy(tw, rc); err != nil {
				return err
			}
			if err := rc.Close(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}
