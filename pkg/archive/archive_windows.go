package archive // import "github.com/docker/docker/pkg/archive"

import (
	"archive/tar"
	"os"
	"path/filepath"

	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/longpath"
)

// fixVolumePathPrefix does platform specific processing to ensure that if
// the path being passed in is not in a volume path format, convert it to one.
func fixVolumePathPrefix(srcPath string) string {
	return longpath.AddPrefix(srcPath)
}

// getWalkRoot calculates the root path when performing a TarWithOptions.
// We use a separate function as this is platform specific.
func getWalkRoot(srcPath string, include string) string {
	return filepath.Join(srcPath, include)
}

// CanonicalTarNameForPath returns platform-specific filepath
// to canonical posix-style path for tar archival. p is relative
// path.
func CanonicalTarNameForPath(p string) string {
	return filepath.ToSlash(p)
}

// chmodTarEntry is used to adjust the file permissions used in tar header based
// on the platform the archival is done.
func chmodTarEntry(perm os.FileMode) os.FileMode {
	return chmodWindowsTarEntry(perm)
}

func setHeaderForSpecialDevice(hdr *tar.Header, name string, stat interface{}) (err error) {
	// do nothing. no notion of Rdev, Nlink in stat on Windows
	return
}

func getInodeFromStat(stat interface{}) (inode uint64, err error) {
	// do nothing. no notion of Inode in stat on Windows
	return
}

// handleTarTypeBlockCharFifo is an OS-specific helper function used by
// createTarFile to handle the following types of header: Block; Char; Fifo
func handleTarTypeBlockCharFifo(hdr *tar.Header, path string) error {
	return nil
}

func handleLChmod(hdr *tar.Header, path string, hdrInfo os.FileInfo) error {
	return nil
}

func getFileUIDGID(stat interface{}) (idtools.Identity, error) {
	// no notion of file ownership mapping yet on Windows
	return idtools.Identity{UID: 0, GID: 0}, nil
}
