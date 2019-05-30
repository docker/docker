package dockerfile // import "github.com/docker/docker/builder/dockerfile"

import (
	"archive/tar"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/builder"
	"github.com/docker/docker/builder/remotecontext"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/containerfs"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/progress"
	"github.com/docker/docker/pkg/streamformatter"
	"github.com/docker/docker/pkg/system"
	"github.com/docker/docker/pkg/urlutil"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

const unnamedFilename = "__unnamed__"

type pathCache interface {
	Load(key interface{}) (value interface{}, ok bool)
	Store(key, value interface{})
}

// copyInfo is a data object which stores the metadata about each source file in
// a copyInstruction
type copyInfo struct {
	root         containerfs.ContainerFS
	path         string
	hash         string
	noDecompress bool
}

func (c copyInfo) fullPath() (string, error) {
	return c.root.ResolveScopedPath(c.path, true)
}

func newCopyInfoFromSource(source builder.Source, path string, hash string) copyInfo {
	return copyInfo{root: source.Root(), path: path, hash: hash}
}

func newCopyInfos(copyInfos ...copyInfo) []copyInfo {
	return copyInfos
}

// copyInstruction is a fully parsed COPY or ADD command that is passed to
// Builder.performCopy to copy files into the image filesystem
type copyInstruction struct {
	cmdName                 string
	infos                   []copyInfo
	dest                    string
	chownStr                string
	allowLocalDecompression bool
	preserveOwnership       bool
}

// copier reads a raw COPY or ADD command, fetches remote sources using a downloader,
// and creates a copyInstruction
type copier struct {
	imageSource *imageMount
	source      builder.Source
	pathCache   pathCache
	download    sourceDownloader
	platform    *specs.Platform
	// for cleanup. TODO: having copier.cleanup() is error prone and hard to
	// follow. Code calling performCopy should manage the lifecycle of its params.
	// Copier should take override source as input, not imageMount.
	activeLayer builder.RWLayer
	tmpPaths    []string
}

func copierFromDispatchRequest(req dispatchRequest, download sourceDownloader, imageSource *imageMount) copier {
	platform := req.builder.platform
	if platform == nil {
		// May be nil if not explicitly set in API/dockerfile
		platform = &specs.Platform{}
	}
	if platform.OS == "" {
		// Default to the dispatch requests operating system if not explicit in API/dockerfile
		platform.OS = req.state.operatingSystem
	}
	if platform.OS == "" {
		// This is a failsafe just in case. Shouldn't be hit.
		platform.OS = runtime.GOOS
	}

	return copier{
		source:      req.source,
		pathCache:   req.builder.pathCache,
		download:    download,
		imageSource: imageSource,
		platform:    platform,
	}

}

func (o *copier) createCopyInstruction(args []string, cmdName string) (copyInstruction, error) {
	inst := copyInstruction{cmdName: cmdName}
	last := len(args) - 1

	// Work in platform-specific filepath semantics
	// TODO: This OS switch for paths is NOT correct and should not be supported.
	// Maintained for backwards compatibility
	pathOS := runtime.GOOS
	if o.platform != nil {
		pathOS = o.platform.OS
	}
	inst.dest = fromSlash(args[last], pathOS)
	separator := string(separator(pathOS))
	infos, err := o.getCopyInfosForSourcePaths(args[0:last], inst.dest)
	if err != nil {
		return inst, errors.Wrapf(err, "%s failed", cmdName)
	}
	if len(infos) > 1 && !strings.HasSuffix(inst.dest, separator) {
		return inst, errors.Errorf("When using %s with more than one source file, the destination must be a directory and end with a /", cmdName)
	}
	inst.infos = infos
	return inst, nil
}

// getCopyInfosForSourcePaths iterates over the source files and calculate the info
// needed to copy (e.g. hash value if cached)
// The dest is used in case source is URL (and ends with "/")
func (o *copier) getCopyInfosForSourcePaths(sources []string, dest string) ([]copyInfo, error) {
	var infos []copyInfo
	for _, orig := range sources {
		subinfos, err := o.getCopyInfoForSourcePath(orig, dest)
		if err != nil {
			return nil, err
		}
		infos = append(infos, subinfos...)
	}

	if len(infos) == 0 {
		return nil, errors.New("no source files were specified")
	}
	return infos, nil
}

func (o *copier) getCopyInfoForSourcePath(orig, dest string) ([]copyInfo, error) {
	if !urlutil.IsURL(orig) {
		return o.calcCopyInfo(orig, true)
	}

	remote, path, err := o.download(orig)
	if err != nil {
		return nil, err
	}
	// If path == "" then we are unable to determine filename from src
	// We have to make sure dest is available
	if path == "" {
		if strings.HasSuffix(dest, "/") {
			return nil, errors.Errorf("cannot determine filename for source %s", orig)
		}
		path = unnamedFilename
	}
	o.tmpPaths = append(o.tmpPaths, remote.Root().Path())

	hash, err := remote.Hash(path)
	ci := newCopyInfoFromSource(remote, path, hash)
	ci.noDecompress = true // data from http shouldn't be extracted even on ADD
	return newCopyInfos(ci), err
}

// Cleanup removes any temporary directories created as part of downloading
// remote files.
func (o *copier) Cleanup() {
	for _, path := range o.tmpPaths {
		os.RemoveAll(path)
	}
	o.tmpPaths = []string{}
	if o.activeLayer != nil {
		o.activeLayer.Release()
		o.activeLayer = nil
	}
}

// TODO: allowWildcards can probably be removed by refactoring this function further.
func (o *copier) calcCopyInfo(origPath string, allowWildcards bool) ([]copyInfo, error) {
	imageSource := o.imageSource

	// TODO: do this when creating copier. Requires validateCopySourcePath
	// (and other below) to be aware of the difference sources. Why is it only
	// done on image Source?
	if imageSource != nil && o.activeLayer == nil {
		// this needs to be protected against repeated calls as wildcard copy
		// will call it multiple times for a single COPY
		var err error
		rwLayer, err := imageSource.NewRWLayer()
		if err != nil {
			return nil, err
		}
		o.activeLayer = rwLayer

		o.source, err = remotecontext.NewLazySource(rwLayer.Root())
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create context for copy from %s", rwLayer.Root().Path())
		}
	}

	if o.source == nil {
		return nil, errors.Errorf("missing build context")
	}

	root := o.source.Root()

	if err := validateCopySourcePath(imageSource, origPath, root.OS()); err != nil {
		return nil, err
	}

	// Work in source OS specific filepath semantics
	// For LCOW, this is NOT the daemon OS.
	origPath = root.FromSlash(origPath)
	origPath = strings.TrimPrefix(origPath, string(root.Separator()))
	origPath = strings.TrimPrefix(origPath, "."+string(root.Separator()))

	// Deal with wildcards
	if allowWildcards && containsWildcards(origPath, root.OS()) {
		return o.copyWithWildcards(origPath)
	}

	if imageSource != nil && imageSource.ImageID() != "" {
		// return a cached copy if one exists
		if h, ok := o.pathCache.Load(imageSource.ImageID() + origPath); ok {
			return newCopyInfos(newCopyInfoFromSource(o.source, origPath, h.(string))), nil
		}
	}

	// Deal with the single file case
	copyInfo, err := copyInfoForFile(o.source, origPath)
	switch {
	case err != nil:
		return nil, err
	case copyInfo.hash != "":
		o.storeInPathCache(imageSource, origPath, copyInfo.hash)
		return newCopyInfos(copyInfo), err
	}

	// TODO: remove, handle dirs in Hash()
	subfiles, err := walkSource(o.source, origPath)
	if err != nil {
		return nil, err
	}

	hash := hashStringSlice("dir", subfiles)
	o.storeInPathCache(imageSource, origPath, hash)
	return newCopyInfos(newCopyInfoFromSource(o.source, origPath, hash)), nil
}

func containsWildcards(name, platform string) bool {
	isWindows := platform == "windows"
	for i := 0; i < len(name); i++ {
		ch := name[i]
		if ch == '\\' && !isWindows {
			i++
		} else if ch == '*' || ch == '?' || ch == '[' {
			return true
		}
	}
	return false
}

func (o *copier) storeInPathCache(im *imageMount, path string, hash string) {
	if im != nil {
		o.pathCache.Store(im.ImageID()+path, hash)
	}
}

func (o *copier) copyWithWildcards(origPath string) ([]copyInfo, error) {
	root := o.source.Root()
	var copyInfos []copyInfo
	if err := root.Walk(root.Path(), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := remotecontext.Rel(root, path)
		if err != nil {
			return err
		}

		if rel == "." {
			return nil
		}
		if match, _ := root.Match(origPath, rel); !match {
			return nil
		}

		// Note we set allowWildcards to false in case the name has
		// a * in it
		subInfos, err := o.calcCopyInfo(rel, false)
		if err != nil {
			return err
		}
		copyInfos = append(copyInfos, subInfos...)
		return nil
	}); err != nil {
		return nil, err
	}
	return copyInfos, nil
}

func copyInfoForFile(source builder.Source, path string) (copyInfo, error) {
	fi, err := remotecontext.StatAt(source, path)
	if err != nil {
		return copyInfo{}, err
	}

	if fi.IsDir() {
		return copyInfo{}, nil
	}
	hash, err := source.Hash(path)
	if err != nil {
		return copyInfo{}, err
	}
	return newCopyInfoFromSource(source, path, "file:"+hash), nil
}

// TODO: dedupe with copyWithWildcards()
func walkSource(source builder.Source, origPath string) ([]string, error) {
	fp, err := remotecontext.FullPath(source, origPath)
	if err != nil {
		return nil, err
	}
	// Must be a dir
	var subfiles []string
	err = source.Root().Walk(fp, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := remotecontext.Rel(source.Root(), path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hash, err := source.Hash(rel)
		if err != nil {
			return nil
		}
		// we already checked handleHash above
		subfiles = append(subfiles, hash)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(subfiles)
	return subfiles, nil
}

type sourceDownloader func(string) (builder.Source, string, error)

func newRemoteSourceDownloader(output, stdout io.Writer) sourceDownloader {
	return func(url string) (builder.Source, string, error) {
		return downloadSource(output, stdout, url)
	}
}

func errOnSourceDownload(_ string) (builder.Source, string, error) {
	return nil, "", errors.New("source can't be a URL for COPY")
}

func getFilenameForDownload(path string, resp *http.Response) string {
	// Guess filename based on source
	if path != "" && !strings.HasSuffix(path, "/") {
		if filename := filepath.Base(filepath.FromSlash(path)); filename != "" {
			return filename
		}
	}

	// Guess filename based on Content-Disposition
	if contentDisposition := resp.Header.Get("Content-Disposition"); contentDisposition != "" {
		if _, params, err := mime.ParseMediaType(contentDisposition); err == nil {
			if params["filename"] != "" && !strings.HasSuffix(params["filename"], "/") {
				if filename := filepath.Base(filepath.FromSlash(params["filename"])); filename != "" {
					return filename
				}
			}
		}
	}
	return ""
}

func downloadSource(output io.Writer, stdout io.Writer, srcURL string) (remote builder.Source, p string, err error) {
	u, err := url.Parse(srcURL)
	if err != nil {
		return
	}

	resp, err := remotecontext.GetWithStatusError(srcURL)
	if err != nil {
		return
	}

	filename := getFilenameForDownload(u.Path, resp)

	// Prepare file in a tmp dir
	tmpDir, err := ioutils.TempDir("", "docker-remote")
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			os.RemoveAll(tmpDir)
		}
	}()
	// If filename is empty, the returned filename will be "" but
	// the tmp filename will be created as "__unnamed__"
	tmpFileName := filename
	if filename == "" {
		tmpFileName = unnamedFilename
	}
	tmpFileName = filepath.Join(tmpDir, tmpFileName)
	tmpFile, err := os.OpenFile(tmpFileName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return
	}

	progressOutput := streamformatter.NewJSONProgressOutput(output, true)
	progressReader := progress.NewProgressReader(resp.Body, progressOutput, resp.ContentLength, "", "Downloading")
	// Download and dump result to tmp file
	// TODO: add filehash directly
	if _, err = io.Copy(tmpFile, progressReader); err != nil {
		tmpFile.Close()
		return
	}
	// TODO: how important is this random blank line to the output?
	fmt.Fprintln(stdout)

	// Set the mtime to the Last-Modified header value if present
	// Otherwise just remove atime and mtime
	mTime := time.Time{}

	lastMod := resp.Header.Get("Last-Modified")
	if lastMod != "" {
		// If we can't parse it then just let it default to 'zero'
		// otherwise use the parsed time value
		if parsedMTime, err := http.ParseTime(lastMod); err == nil {
			mTime = parsedMTime
		}
	}

	tmpFile.Close()

	if err = system.Chtimes(tmpFileName, mTime, mTime); err != nil {
		return
	}

	lc, err := remotecontext.NewLazySource(containerfs.NewLocalContainerFS(tmpDir))
	return lc, filename, err
}

type copyFileOptions struct {
	decompress bool
	identity   *idtools.Identity
	archiver   Archiver
}

type copyEndpoint struct {
	driver containerfs.Driver
	path   string
}

func performCopyForInfo(dest copyInfo, source copyInfo, options copyFileOptions) error {
	srcPath, err := source.fullPath()
	if err != nil {
		return err
	}

	destPath, err := dest.fullPath()
	if err != nil {
		return err
	}

	archiver := options.archiver

	srcEndpoint := &copyEndpoint{driver: source.root, path: srcPath}
	destEndpoint := &copyEndpoint{driver: dest.root, path: destPath}

	src, err := source.root.Stat(srcPath)
	if err != nil {
		return errors.Wrapf(err, "source path not found")
	}
	if src.IsDir() {
		return copyDirectory(archiver, srcEndpoint, destEndpoint, options.identity)
	}
	if options.decompress && isArchivePath(source.root, srcPath) && !source.noDecompress {
		return archiver.UntarPath(srcPath, destPath)
	}

	destExistsAsDir, err := isExistingDirectory(destEndpoint)
	if err != nil {
		return err
	}
	// dest.path must be used because destPath has already been cleaned of any
	// trailing slash
	if endsInSlash(dest.root, dest.path) || destExistsAsDir {
		// source.path must be used to get the correct filename when the source
		// is a symlink
		destPath = dest.root.Join(destPath, source.root.Base(source.path))
		destEndpoint = &copyEndpoint{driver: dest.root, path: destPath}
	}
	return copyFile(archiver, srcEndpoint, destEndpoint, options.identity)
}

func isArchivePath(driver containerfs.ContainerFS, path string) bool {
	file, err := driver.Open(path)
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

func copyDirectory(archiver Archiver, source, dest *copyEndpoint, identity *idtools.Identity) error {
	destExists, err := isExistingDirectory(dest)
	if err != nil {
		return errors.Wrapf(err, "failed to query destination path")
	}

	if err := archiver.CopyWithTar(source.path, dest.path); err != nil {
		return errors.Wrapf(err, "failed to copy directory")
	}
	if identity != nil {
		// TODO: @gupta-ak. Investigate how LCOW permission mappings will work.
		return fixPermissions(source.path, dest.path, *identity, !destExists)
	}
	return nil
}

func copyFile(archiver Archiver, source, dest *copyEndpoint, identity *idtools.Identity) error {
	if runtime.GOOS == "windows" && dest.driver.OS() == "linux" {
		// LCOW
		if err := dest.driver.MkdirAll(dest.driver.Dir(dest.path), 0755); err != nil {
			return errors.Wrapf(err, "failed to create new directory")
		}
	} else {
		if identity == nil {
			if err := os.MkdirAll(filepath.Dir(dest.path), 0755); err != nil {
				return err
			}
		} else {
			if err := idtools.MkdirAllAndChownNew(filepath.Dir(dest.path), 0755, *identity); err != nil {
				// Normal containers
				return errors.Wrapf(err, "failed to create new directory")
			}
		}
	}

	if err := archiver.CopyFileWithTar(source.path, dest.path); err != nil {
		return errors.Wrapf(err, "failed to copy file")
	}
	if identity != nil {
		// TODO: @gupta-ak. Investigate how LCOW permission mappings will work.
		return fixPermissions(source.path, dest.path, *identity, false)
	}
	return nil
}

func endsInSlash(driver containerfs.Driver, path string) bool {
	return strings.HasSuffix(path, string(driver.Separator()))
}

// isExistingDirectory returns true if the path exists and is a directory
func isExistingDirectory(point *copyEndpoint) (bool, error) {
	destStat, err := point.driver.Stat(point.path)
	switch {
	case os.IsNotExist(err):
		return false, nil
	case err != nil:
		return false, err
	}
	return destStat.IsDir(), nil
}
