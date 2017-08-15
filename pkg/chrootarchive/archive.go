package chrootarchive // import "github.com/docker/docker/pkg/chrootarchive"

import (
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/user"
	"path/filepath"

	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/idtools"
	"github.com/pkg/errors"
)

func init() {
	// initialize nss libraries in Glibc so that the dynamic libraries are loaded in the host
	// environment not in the chroot from untrusted files.
	_, _ = user.Lookup("docker")
	_, _ = net.LookupHost("localhost")
}

type invalidArchiveError struct {
	cause error
}

func (e invalidArchiveError) Error() string {
	return e.cause.Error()
}

func (e invalidArchiveError) InvalidParameter() {}

// NewArchiver returns a new Archiver which uses chrootarchive.Untar
func NewArchiver(idMapping *idtools.IdentityMapping) *archive.Archiver {
	if idMapping == nil {
		idMapping = &idtools.IdentityMapping{}
	}
	return &archive.Archiver{
		Untar:     Untar,
		IDMapping: idMapping,
	}
}

// Untar reads a stream of bytes from `archive`, parses it as a tar archive,
// and unpacks it into the directory at `dest`.
// The archive may be compressed with one of the following algorithms:
//  identity (uncompressed), gzip, bzip2, xz.
func Untar(tarArchive io.Reader, dest string, options *archive.TarOptions) error {
	return untarHandler(tarArchive, dest, options, true, dest)
}

// UntarWithRoot is the same as `Untar`, but allows you to pass in a root directory
// The root directory is the directory that will be chrooted to.
// `dest` must be a path within `root`, if it is not an error will be returned.
//
// `root` should set to a directory which is not controlled by any potentially
// malicious process.
//
// This should be used to prevent a potential attacker from manipulating `dest`
// such that it would provide access to files outside of `dest` through things
// like symlinks. Normally `ResolveSymlinksInScope` would handle this, however
// sanitizing symlinks in this manner is inherrently racey:
// ref: CVE-2018-15664
func UntarWithRoot(tarArchive io.Reader, dest string, options *archive.TarOptions, root string) error {
	return untarHandler(tarArchive, dest, options, true, root)
}

// UntarUncompressed reads a stream of bytes from `archive`, parses it as a tar archive,
// and unpacks it into the directory at `dest`.
// The archive must be an uncompressed stream.
func UntarUncompressed(tarArchive io.Reader, dest string, options *archive.TarOptions) error {
	return untarHandler(tarArchive, dest, options, false, dest)
}

// Handler for teasing out the automatic decompression
func untarHandler(tarArchive io.Reader, dest string, options *archive.TarOptions, decompress bool, root string) error {
	if tarArchive == nil {
		return invalidArchiveError{errors.New("empty archive")}
	}
	if options == nil {
		options = &archive.TarOptions{}
	}
	if options.ExcludePatterns == nil {
		options.ExcludePatterns = []string{}
	}

	idMapping := idtools.NewIDMappingsFromMaps(options.UIDMaps, options.GIDMaps)
	rootIDs := idMapping.RootPair()

	dest = filepath.Clean(dest)
	if _, err := os.Stat(dest); os.IsNotExist(err) {
		if err := idtools.MkdirAllAndChownNew(dest, 0755, rootIDs); err != nil {
			return err
		}
	}

	r := ioutil.NopCloser(tarArchive)
	if decompress {
		decompressedArchive, err := archive.DecompressStream(tarArchive)
		if err != nil {
			return err
		}
		defer decompressedArchive.Close()
		r = decompressedArchive
	}

	return invokeUnpack(r, dest, options, root)
}

// Tar tars the requested path while chrooted to the specified root.
func Tar(srcPath string, options *archive.TarOptions, root string) (io.ReadCloser, error) {
	if options == nil {
		options = &archive.TarOptions{}
	}
	return invokePack(srcPath, options, root)
}
