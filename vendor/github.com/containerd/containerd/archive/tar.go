/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package archive

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/containerd/continuity/fs"
	"github.com/pkg/errors"
)

var bufPool = &sync.Pool{
	New: func() interface{} {
		buffer := make([]byte, 32*1024)
		return &buffer
	},
}

var errInvalidArchive = errors.New("invalid archive")

// Diff returns a tar stream of the computed filesystem
// difference between the provided directories.
//
// Produces a tar using OCI style file markers for deletions. Deleted
// files will be prepended with the prefix ".wh.". This style is
// based off AUFS whiteouts.
// See https://github.com/opencontainers/image-spec/blob/master/layer.md
func Diff(ctx context.Context, a, b string) io.ReadCloser {
	r, w := io.Pipe()

	go func() {
		err := WriteDiff(ctx, w, a, b)
		if err = w.CloseWithError(err); err != nil {
			log.G(ctx).WithError(err).Debugf("closing tar pipe failed")
		}
	}()

	return r
}

// WriteDiff writes a tar stream of the computed difference between the
// provided directories.
//
// Produces a tar using OCI style file markers for deletions. Deleted
// files will be prepended with the prefix ".wh.". This style is
// based off AUFS whiteouts.
// See https://github.com/opencontainers/image-spec/blob/master/layer.md
func WriteDiff(ctx context.Context, w io.Writer, a, b string) error {
	cw := newChangeWriter(w, b)
	err := fs.Changes(ctx, a, b, cw.HandleChange)
	if err != nil {
		return errors.Wrap(err, "failed to create diff tar stream")
	}
	return cw.Close()
}

const (
	// whiteoutPrefix prefix means file is a whiteout. If this is followed by a
	// filename this means that file has been removed from the base layer.
	// See https://github.com/opencontainers/image-spec/blob/master/layer.md#whiteouts
	whiteoutPrefix = ".wh."

	// whiteoutMetaPrefix prefix means whiteout has a special meaning and is not
	// for removing an actual file. Normally these files are excluded from exported
	// archives.
	whiteoutMetaPrefix = whiteoutPrefix + whiteoutPrefix

	// whiteoutLinkDir is a directory AUFS uses for storing hardlink links to other
	// layers. Normally these should not go into exported archives and all changed
	// hardlinks should be copied to the top layer.
	whiteoutLinkDir = whiteoutMetaPrefix + "plnk"

	// whiteoutOpaqueDir file means directory has been made opaque - meaning
	// readdir calls to this directory do not follow to lower layers.
	whiteoutOpaqueDir = whiteoutMetaPrefix + ".opq"

	paxSchilyXattr = "SCHILY.xattr."
)

// Apply applies a tar stream of an OCI style diff tar.
// See https://github.com/opencontainers/image-spec/blob/master/layer.md#applying-changesets
func Apply(ctx context.Context, root string, r io.Reader, opts ...ApplyOpt) (int64, error) {
	root = filepath.Clean(root)

	var options ApplyOptions
	for _, opt := range opts {
		if err := opt(&options); err != nil {
			return 0, errors.Wrap(err, "failed to apply option")
		}
	}
	if options.Filter == nil {
		options.Filter = all
	}

	return apply(ctx, root, tar.NewReader(r), options)
}

// applyNaive applies a tar stream of an OCI style diff tar.
// See https://github.com/opencontainers/image-spec/blob/master/layer.md#applying-changesets
func applyNaive(ctx context.Context, root string, tr *tar.Reader, options ApplyOptions) (size int64, err error) {
	var (
		dirs []*tar.Header

		// Used for handling opaque directory markers which
		// may occur out of order
		unpackedPaths = make(map[string]struct{})

		// Used for aufs plink directory
		aufsTempdir   = ""
		aufsHardlinks = make(map[string]*tar.Header)
	)

	// Iterate through the files in the archive.
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}

		hdr, err := tr.Next()
		if err == io.EOF {
			// end of tar archive
			break
		}
		if err != nil {
			return 0, err
		}

		size += hdr.Size

		// Normalize name, for safety and for a simple is-root check
		hdr.Name = filepath.Clean(hdr.Name)

		accept, err := options.Filter(hdr)
		if err != nil {
			return 0, err
		}
		if !accept {
			continue
		}

		if skipFile(hdr) {
			log.G(ctx).Warnf("file %q ignored: archive may not be supported on system", hdr.Name)
			continue
		}

		// Split name and resolve symlinks for root directory.
		ppath, base := filepath.Split(hdr.Name)
		ppath, err = fs.RootPath(root, ppath)
		if err != nil {
			return 0, errors.Wrap(err, "failed to get root path")
		}

		// Join to root before joining to parent path to ensure relative links are
		// already resolved based on the root before adding to parent.
		path := filepath.Join(ppath, filepath.Join("/", base))
		if path == root {
			log.G(ctx).Debugf("file %q ignored: resolved to root", hdr.Name)
			continue
		}

		// If file is not directly under root, ensure parent directory
		// exists or is created.
		if ppath != root {
			parentPath := ppath
			if base == "" {
				parentPath = filepath.Dir(path)
			}
			if _, err := os.Lstat(parentPath); err != nil && os.IsNotExist(err) {
				err = mkdirAll(parentPath, 0755)
				if err != nil {
					return 0, err
				}
			}
		}

		// Skip AUFS metadata dirs
		if strings.HasPrefix(hdr.Name, whiteoutMetaPrefix) {
			// Regular files inside /.wh..wh.plnk can be used as hardlink targets
			// We don't want this directory, but we need the files in them so that
			// such hardlinks can be resolved.
			if strings.HasPrefix(hdr.Name, whiteoutLinkDir) && hdr.Typeflag == tar.TypeReg {
				basename := filepath.Base(hdr.Name)
				aufsHardlinks[basename] = hdr
				if aufsTempdir == "" {
					if aufsTempdir, err = ioutil.TempDir(os.Getenv("XDG_RUNTIME_DIR"), "dockerplnk"); err != nil {
						return 0, err
					}
					defer os.RemoveAll(aufsTempdir)
				}
				p, err := fs.RootPath(aufsTempdir, basename)
				if err != nil {
					return 0, err
				}
				if err := createTarFile(ctx, p, root, hdr, tr); err != nil {
					return 0, err
				}
			}

			if hdr.Name != whiteoutOpaqueDir {
				continue
			}
		}

		if strings.HasPrefix(base, whiteoutPrefix) {
			dir := filepath.Dir(path)
			if base == whiteoutOpaqueDir {
				_, err := os.Lstat(dir)
				if err != nil {
					return 0, err
				}
				err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
					if err != nil {
						if os.IsNotExist(err) {
							err = nil // parent was deleted
						}
						return err
					}
					if path == dir {
						return nil
					}
					if _, exists := unpackedPaths[path]; !exists {
						err := os.RemoveAll(path)
						return err
					}
					return nil
				})
				if err != nil {
					return 0, err
				}
				continue
			}

			originalBase := base[len(whiteoutPrefix):]
			originalPath := filepath.Join(dir, originalBase)

			// Ensure originalPath is under dir
			if dir[len(dir)-1] != filepath.Separator {
				dir += string(filepath.Separator)
			}
			if !strings.HasPrefix(originalPath, dir) {
				return 0, errors.Wrapf(errInvalidArchive, "invalid whiteout name: %v", base)
			}

			if err := os.RemoveAll(originalPath); err != nil {
				return 0, err
			}
			continue
		}
		// If path exits we almost always just want to remove and replace it.
		// The only exception is when it is a directory *and* the file from
		// the layer is also a directory. Then we want to merge them (i.e.
		// just apply the metadata from the layer).
		if fi, err := os.Lstat(path); err == nil {
			if !(fi.IsDir() && hdr.Typeflag == tar.TypeDir) {
				if err := os.RemoveAll(path); err != nil {
					return 0, err
				}
			}
		}

		srcData := io.Reader(tr)
		srcHdr := hdr

		// Hard links into /.wh..wh.plnk don't work, as we don't extract that directory, so
		// we manually retarget these into the temporary files we extracted them into
		if hdr.Typeflag == tar.TypeLink && strings.HasPrefix(filepath.Clean(hdr.Linkname), whiteoutLinkDir) {
			linkBasename := filepath.Base(hdr.Linkname)
			srcHdr = aufsHardlinks[linkBasename]
			if srcHdr == nil {
				return 0, fmt.Errorf("invalid aufs hardlink")
			}
			p, err := fs.RootPath(aufsTempdir, linkBasename)
			if err != nil {
				return 0, err
			}
			tmpFile, err := os.Open(p)
			if err != nil {
				return 0, err
			}
			defer tmpFile.Close()
			srcData = tmpFile
		}

		if err := createTarFile(ctx, path, root, srcHdr, srcData); err != nil {
			return 0, err
		}

		// Directory mtimes must be handled at the end to avoid further
		// file creation in them to modify the directory mtime
		if hdr.Typeflag == tar.TypeDir {
			dirs = append(dirs, hdr)
		}
		unpackedPaths[path] = struct{}{}
	}

	for _, hdr := range dirs {
		path, err := fs.RootPath(root, hdr.Name)
		if err != nil {
			return 0, err
		}
		if err := chtimes(path, boundTime(latestTime(hdr.AccessTime, hdr.ModTime)), boundTime(hdr.ModTime)); err != nil {
			return 0, err
		}
	}

	return size, nil
}

func createTarFile(ctx context.Context, path, extractDir string, hdr *tar.Header, reader io.Reader) error {
	// hdr.Mode is in linux format, which we can use for syscalls,
	// but for os.Foo() calls we need the mode converted to os.FileMode,
	// so use hdrInfo.Mode() (they differ for e.g. setuid bits)
	hdrInfo := hdr.FileInfo()

	switch hdr.Typeflag {
	case tar.TypeDir:
		// Create directory unless it exists as a directory already.
		// In that case we just want to merge the two
		if fi, err := os.Lstat(path); !(err == nil && fi.IsDir()) {
			if err := mkdir(path, hdrInfo.Mode()); err != nil {
				return err
			}
		}

	case tar.TypeReg, tar.TypeRegA:
		file, err := openFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, hdrInfo.Mode())
		if err != nil {
			return err
		}

		_, err = copyBuffered(ctx, file, reader)
		if err1 := file.Close(); err == nil {
			err = err1
		}
		if err != nil {
			return err
		}

	case tar.TypeBlock, tar.TypeChar:
		// Handle this is an OS-specific way
		if err := handleTarTypeBlockCharFifo(hdr, path); err != nil {
			return err
		}

	case tar.TypeFifo:
		// Handle this is an OS-specific way
		if err := handleTarTypeBlockCharFifo(hdr, path); err != nil {
			return err
		}

	case tar.TypeLink:
		targetPath, err := hardlinkRootPath(extractDir, hdr.Linkname)
		if err != nil {
			return err
		}

		if err := os.Link(targetPath, path); err != nil {
			return err
		}

	case tar.TypeSymlink:
		if err := os.Symlink(hdr.Linkname, path); err != nil {
			return err
		}

	case tar.TypeXGlobalHeader:
		log.G(ctx).Debug("PAX Global Extended Headers found and ignored")
		return nil

	default:
		return errors.Errorf("unhandled tar header type %d\n", hdr.Typeflag)
	}

	// Lchown is not supported on Windows.
	if runtime.GOOS != "windows" {
		if err := os.Lchown(path, hdr.Uid, hdr.Gid); err != nil {
			return err
		}
	}

	for key, value := range hdr.PAXRecords {
		if strings.HasPrefix(key, paxSchilyXattr) {
			key = key[len(paxSchilyXattr):]
			if err := setxattr(path, key, value); err != nil {
				if errors.Cause(err) == syscall.ENOTSUP {
					log.G(ctx).WithError(err).Warnf("ignored xattr %s in archive", key)
					continue
				}
				return err
			}
		}
	}

	// There is no LChmod, so ignore mode for symlink. Also, this
	// must happen after chown, as that can modify the file mode
	if err := handleLChmod(hdr, path, hdrInfo); err != nil {
		return err
	}

	return chtimes(path, boundTime(latestTime(hdr.AccessTime, hdr.ModTime)), boundTime(hdr.ModTime))
}

type changeWriter struct {
	tw        *tar.Writer
	source    string
	whiteoutT time.Time
	inodeSrc  map[uint64]string
	inodeRefs map[uint64][]string
	addedDirs map[string]struct{}
}

func newChangeWriter(w io.Writer, source string) *changeWriter {
	return &changeWriter{
		tw:        tar.NewWriter(w),
		source:    source,
		whiteoutT: time.Now(),
		inodeSrc:  map[uint64]string{},
		inodeRefs: map[uint64][]string{},
		addedDirs: map[string]struct{}{},
	}
}

func (cw *changeWriter) HandleChange(k fs.ChangeKind, p string, f os.FileInfo, err error) error {
	if err != nil {
		return err
	}
	if k == fs.ChangeKindDelete {
		whiteOutDir := filepath.Dir(p)
		whiteOutBase := filepath.Base(p)
		whiteOut := filepath.Join(whiteOutDir, whiteoutPrefix+whiteOutBase)
		hdr := &tar.Header{
			Typeflag:   tar.TypeReg,
			Name:       whiteOut[1:],
			Size:       0,
			ModTime:    cw.whiteoutT,
			AccessTime: cw.whiteoutT,
			ChangeTime: cw.whiteoutT,
		}
		if err := cw.includeParents(hdr); err != nil {
			return err
		}
		if err := cw.tw.WriteHeader(hdr); err != nil {
			return errors.Wrap(err, "failed to write whiteout header")
		}
	} else {
		var (
			link   string
			err    error
			source = filepath.Join(cw.source, p)
		)

		switch {
		case f.Mode()&os.ModeSocket != 0:
			return nil // ignore sockets
		case f.Mode()&os.ModeSymlink != 0:
			if link, err = os.Readlink(source); err != nil {
				return err
			}
		}

		hdr, err := tar.FileInfoHeader(f, link)
		if err != nil {
			return err
		}

		hdr.Mode = int64(chmodTarEntry(os.FileMode(hdr.Mode)))

		name := p
		if strings.HasPrefix(name, string(filepath.Separator)) {
			name, err = filepath.Rel(string(filepath.Separator), name)
			if err != nil {
				return errors.Wrap(err, "failed to make path relative")
			}
		}
		name, err = tarName(name)
		if err != nil {
			return errors.Wrap(err, "cannot canonicalize path")
		}
		// suffix with '/' for directories
		if f.IsDir() && !strings.HasSuffix(name, "/") {
			name += "/"
		}
		hdr.Name = name

		if err := setHeaderForSpecialDevice(hdr, name, f); err != nil {
			return errors.Wrap(err, "failed to set device headers")
		}

		// additionalLinks stores file names which must be linked to
		// this file when this file is added
		var additionalLinks []string
		inode, isHardlink := fs.GetLinkInfo(f)
		if isHardlink {
			// If the inode has a source, always link to it
			if source, ok := cw.inodeSrc[inode]; ok {
				hdr.Typeflag = tar.TypeLink
				hdr.Linkname = source
				hdr.Size = 0
			} else {
				if k == fs.ChangeKindUnmodified {
					cw.inodeRefs[inode] = append(cw.inodeRefs[inode], name)
					return nil
				}
				cw.inodeSrc[inode] = name
				additionalLinks = cw.inodeRefs[inode]
				delete(cw.inodeRefs, inode)
			}
		} else if k == fs.ChangeKindUnmodified {
			// Nothing to write to diff
			return nil
		}

		if capability, err := getxattr(source, "security.capability"); err != nil {
			return errors.Wrap(err, "failed to get capabilities xattr")
		} else if capability != nil {
			if hdr.PAXRecords == nil {
				hdr.PAXRecords = map[string]string{}
			}
			hdr.PAXRecords[paxSchilyXattr+"security.capability"] = string(capability)
		}

		if err := cw.includeParents(hdr); err != nil {
			return err
		}
		if err := cw.tw.WriteHeader(hdr); err != nil {
			return errors.Wrap(err, "failed to write file header")
		}

		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
			file, err := open(source)
			if err != nil {
				return errors.Wrapf(err, "failed to open path: %v", source)
			}
			defer file.Close()

			n, err := copyBuffered(context.TODO(), cw.tw, file)
			if err != nil {
				return errors.Wrap(err, "failed to copy")
			}
			if n != hdr.Size {
				return errors.New("short write copying file")
			}
		}

		if additionalLinks != nil {
			source = hdr.Name
			for _, extra := range additionalLinks {
				hdr.Name = extra
				hdr.Typeflag = tar.TypeLink
				hdr.Linkname = source
				hdr.Size = 0

				if err := cw.includeParents(hdr); err != nil {
					return err
				}
				if err := cw.tw.WriteHeader(hdr); err != nil {
					return errors.Wrap(err, "failed to write file header")
				}
			}
		}
	}
	return nil
}

func (cw *changeWriter) Close() error {
	if err := cw.tw.Close(); err != nil {
		return errors.Wrap(err, "failed to close tar writer")
	}
	return nil
}

func (cw *changeWriter) includeParents(hdr *tar.Header) error {
	name := strings.TrimRight(hdr.Name, "/")
	fname := filepath.Join(cw.source, name)
	parent := filepath.Dir(name)
	pname := filepath.Join(cw.source, parent)

	// Do not include root directory as parent
	if fname != cw.source && pname != cw.source {
		_, ok := cw.addedDirs[parent]
		if !ok {
			cw.addedDirs[parent] = struct{}{}
			fi, err := os.Stat(pname)
			if err != nil {
				return err
			}
			if err := cw.HandleChange(fs.ChangeKindModify, parent, fi, nil); err != nil {
				return err
			}
		}
	}
	if hdr.Typeflag == tar.TypeDir {
		cw.addedDirs[name] = struct{}{}
	}
	return nil
}

func copyBuffered(ctx context.Context, dst io.Writer, src io.Reader) (written int64, err error) {
	buf := bufPool.Get().(*[]byte)
	defer bufPool.Put(buf)

	for {
		select {
		case <-ctx.Done():
			err = ctx.Err()
			return
		default:
		}

		nr, er := src.Read(*buf)
		if nr > 0 {
			nw, ew := dst.Write((*buf)[0:nr])
			if nw > 0 {
				written += int64(nw)
			}
			if ew != nil {
				err = ew
				break
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
		}
		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}
	}
	return written, err

}

// hardlinkRootPath returns target linkname, evaluating and bounding any
// symlink to the parent directory.
//
// NOTE: Allow hardlink to the softlink, not the real one. For example,
//
//	touch /tmp/zzz
//	ln -s /tmp/zzz /tmp/xxx
//	ln /tmp/xxx /tmp/yyy
//
// /tmp/yyy should be softlink which be same of /tmp/xxx, not /tmp/zzz.
func hardlinkRootPath(root, linkname string) (string, error) {
	ppath, base := filepath.Split(linkname)
	ppath, err := fs.RootPath(root, ppath)
	if err != nil {
		return "", err
	}

	targetPath := filepath.Join(ppath, base)
	if !strings.HasPrefix(targetPath, root) {
		targetPath = root
	}
	return targetPath, nil
}
