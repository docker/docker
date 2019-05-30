package file

import (
	"archive/tar"
	"context"
	"os"
	"time"

	"github.com/containerd/continuity/fs"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/chrootarchive"
	copy "github.com/tonistiigi/fsutil/copy"
)

func unpack(ctx context.Context, srcRoot string, src string, destRoot string, dest string, user *copy.ChownOpt, tm *time.Time) (bool, error) {
	src, err := fs.RootPath(srcRoot, src)
	if err != nil {
		return false, err
	}
	if !isArchivePath(src) {
		return false, nil
	}

	dest, err = fs.RootPath(destRoot, dest)
	if err != nil {
		return false, err
	}
	if err := copy.MkdirAll(dest, 0755, user, tm); err != nil {
		return false, err
	}

	file, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer file.Close()

	return true, chrootarchive.Untar(file, dest, nil)
}

func isArchivePath(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if fi.Mode()&os.ModeType != 0 {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	rdr, err := archive.DecompressStream(file)
	if err != nil {
		return false
	}
	r := tar.NewReader(rdr)
	_, err = r.Next()
	return err == nil
}
