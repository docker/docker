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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/moby/sys/userns"

	"github.com/containerd/containerd/v2/pkg/archive/tarheader"
	"github.com/containerd/containerd/v2/pkg/epoch"
	"github.com/containerd/continuity/fs"
	"github.com/containerd/log"
	"github.com/opencontainers/runtime-spec/specs-go"
)

var bufPool = &sync.Pool{
	New: func() any {
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
// See https://github.com/opencontainers/image-spec/blob/main/layer.md
func Diff(ctx context.Context, a, b string, opts ...WriteDiffOpt) io.ReadCloser {
	r, w := io.Pipe()

	go func() {
		err := WriteDiff(ctx, w, a, b, opts...)
		if err != nil {
			log.G(ctx).WithError(err).Debugf("write diff failed")
		}
		if err = w.CloseWithError(err); err != nil {
			log.G(ctx).WithError(err).Debugf("closing tar pipe failed")
		}
	}()

	return r
}

// WriteDiff writes a tar stream of the computed difference between the
// provided paths.
//
// Produces a tar using OCI style file markers for deletions. Deleted
// files will be prepended with the prefix ".wh.". This style is
// based off AUFS whiteouts.
// See https://github.com/opencontainers/image-spec/blob/main/layer.md
func WriteDiff(ctx context.Context, w io.Writer, a, b string, opts ...WriteDiffOpt) error {
	var options WriteDiffOptions
	for _, opt := range opts {
		if err := opt(&options); err != nil {
			return fmt.Errorf("failed to apply option: %w", err)
		}
	}
	if tm := epoch.FromContext(ctx); tm != nil && options.SourceDateEpoch == nil {
		options.SourceDateEpoch = tm
	}

	if options.writeDiffFunc != nil && options.idMapping != nil {
		return errors.New("ID mapping is not supported with this diff function")
	}
	if options.writeDiffFunc == nil {
		options.writeDiffFunc = writeDiffNaive
	}

	return options.writeDiffFunc(ctx, w, a, b, options)
}

// writeDiffNaive writes a tar stream of the computed difference between the
// provided directories on disk.
//
// Produces a tar using OCI style file markers for deletions. Deleted
// files will be prepended with the prefix ".wh.". This style is
// based off AUFS whiteouts.
// See https://github.com/opencontainers/image-spec/blob/main/layer.md
func writeDiffNaive(ctx context.Context, w io.Writer, a, b string, o WriteDiffOptions) error {
	var opts []ChangeWriterOpt
	if o.SourceDateEpoch != nil {
		opts = append(opts, WithModTimeUpperBound(*o.SourceDateEpoch))
		// Since containerd v2.0, the whiteout timestamps are set to zero (1970-01-01),
		// not to the source date epoch
	}
	if o.idMapping != nil {
		opts = append(opts, func(cw *ChangeWriter) {
			cw.idMapping = o.idMapping
		})
	}
	cw := NewChangeWriter(w, b, opts...)
	err := fs.Changes(ctx, a, b, cw.HandleChange)
	if err != nil {
		return fmt.Errorf("failed to create diff tar stream: %w", err)
	}
	return cw.Close()
}

const (
	// whiteoutPrefix prefix means file is a whiteout. If this is followed by a
	// filename this means that file has been removed from the base layer.
	// See https://github.com/opencontainers/image-spec/blob/main/layer.md#whiteouts
	whiteoutPrefix = ".wh."

	// whiteoutMetaPrefix prefix means whiteout has a special meaning and is not
	// for removing an actual file. Normally these files are excluded from exported
	// archives.
	whiteoutMetaPrefix = whiteoutPrefix + whiteoutPrefix

	// whiteoutOpaqueDir file means directory has been made opaque - meaning
	// readdir calls to this directory do not follow to lower layers.
	whiteoutOpaqueDir = whiteoutMetaPrefix + ".opq"

	paxSchilyXattr = "SCHILY.xattr."

	userXattrPrefix = "user."
)

// Apply applies a tar stream of an OCI style diff tar.
// See https://github.com/opencontainers/image-spec/blob/main/layer.md#applying-changesets
func Apply(ctx context.Context, root string, r io.Reader, opts ...ApplyOpt) (int64, error) {
	root = filepath.Clean(root)

	var options ApplyOptions
	for _, opt := range opts {
		if err := opt(&options); err != nil {
			return 0, fmt.Errorf("failed to apply option: %w", err)
		}
	}
	if options.Filter == nil {
		options.Filter = all
	}
	if options.applyFunc != nil && options.idMapping != nil {
		return 0, errors.New("ID mapping is not supported with this apply function")
	}
	if options.applyFunc == nil {
		options.applyFunc = applyNaive
	}

	return options.applyFunc(ctx, root, r, options)
}

// applyNaive applies a tar stream of an OCI style diff tar to a directory
// applying each file as either a whole file or whiteout.
// See https://github.com/opencontainers/image-spec/blob/main/layer.md#applying-changesets
func applyNaive(ctx context.Context, root string, r io.Reader, options ApplyOptions) (size int64, err error) {
	var (
		dirs []*tar.Header

		tr = tar.NewReader(r)

		// Used for handling opaque directory markers which
		// may occur out of order
		unpackedPaths = make(map[string]struct{})
		// Used for tracking directories that have already processed an
		// opaque directory marker to avoid redundant directory walks
		opaqueDirs = make(map[string]struct{})

		convertWhiteout = options.ConvertWhiteout
	)

	if convertWhiteout == nil {
		// handle whiteouts by removing the target files
		convertWhiteout = func(hdr *tar.Header, path string) (bool, error) {
			base := filepath.Base(path)
			dir := filepath.Dir(path)
			if base == whiteoutOpaqueDir {
				if _, ok := opaqueDirs[dir]; ok {
					return false, nil
				}
				opaqueDirs[dir] = struct{}{}

				_, err := os.Lstat(dir)
				if err != nil {
					return false, err
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
						if err != nil {
							return err
						}
						if info.IsDir() {
							return filepath.SkipDir
						}
					}
					return nil
				})
				return false, err
			}

			if strings.HasPrefix(base, whiteoutPrefix) {
				originalBase := base[len(whiteoutPrefix):]
				originalPath := filepath.Join(dir, originalBase)

				return false, os.RemoveAll(originalPath)
			}

			return true, nil
		}
	}

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

		if options.idMapping != nil {
			if err := options.idMapping.toHost(hdr); err != nil {
				return 0, err
			}
		}

		if skipFile(hdr) {
			log.G(ctx).Warnf("file %q ignored: archive may not be supported on system", hdr.Name)
			continue
		}

		// Split name and resolve symlinks for root directory.
		ppath, base := filepath.Split(hdr.Name)
		ppath, err = fs.RootPath(root, ppath)
		if err != nil {
			return 0, fmt.Errorf("failed to get root path: %w", err)
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
			if err := mkparent(ctx, parentPath, root, options.Parents, options.idMapping); err != nil {
				return 0, err
			}
		}

		// Naive whiteout convert function which handles whiteout files by
		// removing the target files.
		if err := validateWhiteout(path); err != nil {
			return 0, err
		}
		writeFile, err := convertWhiteout(hdr, path)
		if err != nil {
			return 0, fmt.Errorf("failed to convert whiteout file %q: %w", hdr.Name, err)
		}
		if !writeFile {
			continue
		}
		// If path exits we almost always just want to remove and replace it.
		// The only exception is when it is a directory *and* the file from
		// the layer is also a directory. Then we want to merge them (i.e.
		// just apply the metadata from the layer).
		if fi, err := os.Lstat(path); err == nil {
			if !fi.IsDir() || hdr.Typeflag != tar.TypeDir {
				if err := os.RemoveAll(path); err != nil {
					return 0, err
				}
			}
		}

		srcData := io.Reader(tr)
		srcHdr := hdr

		if err := createTarFile(ctx, path, root, srcHdr, srcData, options.NoSameOwner); err != nil {
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

	if options.idMapping != nil {
		if err := options.idMapping.chownRoot(root); err != nil {
			return 0, err
		}
	}

	return size, nil
}

func createTarFile(ctx context.Context, path, extractDir string, hdr *tar.Header, reader io.Reader, noSameOwner bool) error {
	// hdr.Mode is in linux format, which we can use for syscalls,
	// but for os.Foo() calls we need the mode converted to os.FileMode,
	// so use hdrInfo.Mode() (they differ for e.g. setuid bits)
	hdrInfo := hdr.FileInfo()

	switch hdr.Typeflag {
	case tar.TypeDir:
		// Create directory unless it exists as a directory already.
		// In that case we just want to merge the two
		if fi, err := os.Lstat(path); err != nil || !fi.IsDir() {
			if err := mkdir(path, hdrInfo.Mode()); err != nil {
				return err
			}
		}

	//nolint:staticcheck // TypeRegA is deprecated but we may still receive an external tar with TypeRegA
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

		if err := link(targetPath, path); err != nil {
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
		return fmt.Errorf("unhandled tar header type %d", hdr.Typeflag)
	}

	if !noSameOwner {
		if err := os.Lchown(path, hdr.Uid, hdr.Gid); err != nil {
			err = fmt.Errorf("failed to Lchown %q for UID %d, GID %d: %w", path, hdr.Uid, hdr.Gid, err)
			if errors.Is(err, syscall.EINVAL) && userns.RunningInUserNS() {
				err = fmt.Errorf("%w (Hint: try increasing the number of subordinate IDs in /etc/subuid and /etc/subgid)", err)
			}
			return err
		}
	}

	for key, value := range hdr.PAXRecords {
		if strings.HasPrefix(key, paxSchilyXattr) {
			key = key[len(paxSchilyXattr):]
			if err := setxattr(path, key, value); err != nil {
				if errors.Is(err, syscall.EPERM) && strings.HasPrefix(key, userXattrPrefix) {
					// In the user.* namespace, only regular files and directories can have extended attributes.
					// See https://man7.org/linux/man-pages/man7/xattr.7.html for details.
					if fi, err := os.Lstat(path); err == nil && (!fi.Mode().IsRegular() && !fi.Mode().IsDir()) {
						log.G(ctx).WithError(err).Warnf("ignored xattr %s in archive", key)
						continue
					}
				}
				if errors.Is(err, syscall.ENOTSUP) {
					log.G(ctx).WithError(err).Warnf("ignored xattr %s in archive", key)
					continue
				}
				return fmt.Errorf("failed to setxattr %q for key %q: %w", path, key, err)
			}
		}
	}

	// call lchmod after lchown since lchown can modify the file mode
	if err := lchmod(path, hdrInfo.Mode()); err != nil {
		return err
	}

	return chtimes(path, boundTime(latestTime(hdr.AccessTime, hdr.ModTime)), boundTime(hdr.ModTime))
}

func mkparent(ctx context.Context, path, root string, parents []string, idMapping *idMapping) error {
	if dir, err := os.Lstat(path); err == nil {
		if dir.IsDir() {
			return nil
		}
		return &os.PathError{
			Op:   "mkparent",
			Path: path,
			Err:  syscall.ENOTDIR,
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	i := len(path)
	for i > len(root) && !os.IsPathSeparator(path[i-1]) {
		i--
	}

	if i > len(root)+1 {
		if err := mkparent(ctx, path[:i-1], root, parents, idMapping); err != nil {
			return err
		}
	}

	if err := mkdir(path, 0755); err != nil {
		// Check that still doesn't exist
		dir, err1 := os.Lstat(path)
		if err1 == nil && dir.IsDir() {
			return nil
		}
		return err
	}

	for _, p := range parents {
		ppath, err := fs.RootPath(p, path[len(root):])
		if err != nil {
			return err
		}

		dir, err := os.Lstat(ppath)
		if err == nil {
			if !dir.IsDir() {
				// Replaced, do not copy attributes
				break
			}
			if err := copyDirInfo(dir, path); err != nil {
				return err
			}
			return copyUpXAttrs(path, ppath)
		} else if !os.IsNotExist(err) {
			return err
		}
	}

	log.G(ctx).Debugf("parent directory %q not found: default permissions(0755) used", path)

	if idMapping != nil {
		return idMapping.chownRoot(path)
	}
	return nil
}

// ChangeWriter provides tar stream from filesystem change information.
// The privided tar stream is styled as an OCI layer. Change information
// (add/modify/delete/unmodified) for each file needs to be passed to this
// writer through HandleChange method.
//
// This should be used combining with continuity's diff computing functionality
// (e.g. `fs.Change` of github.com/containerd/continuity/fs).
//
// See also https://github.com/opencontainers/image-spec/blob/main/layer.md for details
// about OCI layers
type ChangeWriter struct {
	tw                *tar.Writer
	source            string
	modTimeUpperBound *time.Time
	idMapping         *idMapping
	inodeSrc          map[uint64]string
	inodeRefs         map[uint64][]string
	addedDirs         map[string]struct{}
}

// ChangeWriterOpt can be specified in NewChangeWriter.
type ChangeWriterOpt func(cw *ChangeWriter)

// WithModTimeUpperBound sets the mod time upper bound.
func WithModTimeUpperBound(tm time.Time) ChangeWriterOpt {
	return func(cw *ChangeWriter) {
		cw.modTimeUpperBound = &tm
	}
}

// WithChangeWriterIDMapping maps the owners of the files in the tar stream
// from host IDs to the container IDs of the user namespace mappings.
func WithChangeWriterIDMapping(uidMap, gidMap []specs.LinuxIDMapping) ChangeWriterOpt {
	return func(cw *ChangeWriter) {
		cw.idMapping = newIDMapping(uidMap, gidMap)
	}
}

// NewChangeWriter returns ChangeWriter that writes tar stream of the source directory
// to the privided writer. Change information (add/modify/delete/unmodified) for each
// file needs to be passed through HandleChange method.
func NewChangeWriter(w io.Writer, source string, opts ...ChangeWriterOpt) *ChangeWriter {
	cw := &ChangeWriter{
		tw:        tar.NewWriter(w),
		source:    source,
		inodeSrc:  map[uint64]string{},
		inodeRefs: map[uint64][]string{},
		addedDirs: map[string]struct{}{},
	}
	for _, o := range opts {
		o(cw)
	}
	return cw
}

// HandleChange receives filesystem change information and reflect that information to
// the result tar stream. This function implements `fs.ChangeFunc` of continuity
// (github.com/containerd/continuity/fs) and should be used with that package.
func (cw *ChangeWriter) HandleChange(k fs.ChangeKind, p string, f os.FileInfo, err error) error {
	if err != nil {
		return err
	}
	if k == fs.ChangeKindDelete {
		whiteOutDir := filepath.Dir(p)
		whiteOutBase := filepath.Base(p)
		whiteOut := filepath.Join(whiteOutDir, whiteoutPrefix+whiteOutBase)
		// Since containerd v2.0, the whiteout timestamps are set to zero (1970-01-01),
		// not to the source date epoch.
		whiteOutT := time.Unix(0, 0).UTC()
		hdr := &tar.Header{
			Typeflag:   tar.TypeReg,
			Name:       whiteOut[1:],
			Size:       0,
			ModTime:    whiteOutT,
			AccessTime: whiteOutT,
			ChangeTime: whiteOutT,
		}
		if err := cw.includeParents(hdr); err != nil {
			return err
		}
		if err := cw.tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("failed to write whiteout header: %w", err)
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

		// Use FileInfoHeaderNoLookups to avoid propagating user names and group names from the host
		hdr, err := tarheader.FileInfoHeaderNoLookups(f, link)
		if err != nil {
			return err
		}

		hdr.Mode = int64(chmodTarEntry(os.FileMode(hdr.Mode)))

		// truncate timestamp for compatibility. without PAX stdlib rounds timestamps instead
		hdr.Format = tar.FormatPAX
		if cw.modTimeUpperBound != nil && hdr.ModTime.After(*cw.modTimeUpperBound) {
			hdr.ModTime = *cw.modTimeUpperBound
		}
		hdr.ModTime = hdr.ModTime.Truncate(time.Second)
		hdr.AccessTime = time.Time{}
		hdr.ChangeTime = time.Time{}

		name := p
		if strings.HasPrefix(name, string(filepath.Separator)) {
			name, err = filepath.Rel(string(filepath.Separator), name)
			if err != nil {
				return fmt.Errorf("failed to make path relative: %w", err)
			}
		}
		// Canonicalize to POSIX-style paths using forward slashes. Directory
		// entries must end with a slash.
		name = filepath.ToSlash(name)
		if f.IsDir() && !strings.HasSuffix(name, "/") {
			name += "/"
		}
		hdr.Name = name

		if err := setHeaderForSpecialDevice(hdr, name, f); err != nil {
			return fmt.Errorf("failed to set device headers: %w", err)
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
			return fmt.Errorf("failed to get capabilities xattr: %w", err)
		} else if len(capability) > 0 {
			if hdr.PAXRecords == nil {
				hdr.PAXRecords = map[string]string{}
			}
			hdr.PAXRecords[paxSchilyXattr+"security.capability"] = string(capability)
		}

		if cw.idMapping != nil {
			if err := cw.idMapping.toContainer(hdr); err != nil {
				return err
			}
		}

		if err := cw.includeParents(hdr); err != nil {
			return err
		}
		if err := cw.tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("failed to write file header: %w", err)
		}

		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
			file, err := open(source)
			if err != nil {
				return fmt.Errorf("failed to open path: %v: %w", source, err)
			}
			defer file.Close()

			n, err := copyBuffered(context.TODO(), cw.tw, file)
			if err != nil {
				return fmt.Errorf("failed to copy: %v: %w", source, err)
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
					return fmt.Errorf("failed to write file header: %w", err)
				}
			}
		}
	}
	return nil
}

// Close closes this writer.
func (cw *ChangeWriter) Close() error {
	if err := cw.tw.Close(); err != nil {
		return fmt.Errorf("failed to close tar writer: %w", err)
	}
	return nil
}

func (cw *ChangeWriter) includeParents(hdr *tar.Header) error {
	if cw.addedDirs == nil {
		return nil
	}
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

func validateWhiteout(path string) error {
	base := filepath.Base(path)
	dir := filepath.Dir(path)

	if base == whiteoutOpaqueDir {
		return nil
	}

	if strings.HasPrefix(base, whiteoutPrefix) {
		originalBase := base[len(whiteoutPrefix):]
		originalPath := filepath.Join(dir, originalBase)

		// Ensure originalPath is under dir
		if dir[len(dir)-1] != filepath.Separator {
			dir += string(filepath.Separator)
		}
		if !strings.HasPrefix(originalPath, dir) {
			return fmt.Errorf("invalid whiteout name: %v: %w", base, errInvalidArchive)
		}
	}
	return nil
}
