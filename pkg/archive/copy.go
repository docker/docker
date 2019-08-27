package archive // import "github.com/docker/docker/pkg/archive"

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Errors used or returned by this file.
var (
	ErrNotDirectory      = errors.New("not a directory")
	ErrDirNotExists      = errors.New("no such directory")
	ErrCannotCopyDir     = errors.New("cannot copy directory")
	ErrInvalidCopySource = errors.New("invalid copy source content")
)

// PreserveTrailingDotOrSeparator returns the given cleaned path (after
// processing using any utility functions from the path or filepath stdlib
// packages) and appends a trailing `/.` or `/` if its corresponding  original
// path (from before being processed by utility functions from the path or
// filepath stdlib packages) ends with a trailing `/.` or `/`. If the cleaned
// path already ends in a `.` path segment, then another is not added. If the
// clean path already ends in the separator, then another is not added.
func PreserveTrailingDotOrSeparator(cleanedPath string, originalPath string, sep byte) string {
	// Ensure paths are in platform semantics
	cleanedPath = strings.Replace(cleanedPath, "/", string(sep), -1)
	originalPath = strings.Replace(originalPath, "/", string(sep), -1)

	if !specifiesCurrentDir(cleanedPath) && specifiesCurrentDir(originalPath) {
		if !hasTrailingPathSeparator(cleanedPath, sep) {
			// Add a separator if it doesn't already end with one (a cleaned
			// path would only end in a separator if it is the root).
			cleanedPath += string(sep)
		}
		cleanedPath += "."
	}

	if !hasTrailingPathSeparator(cleanedPath, sep) && hasTrailingPathSeparator(originalPath, sep) {
		cleanedPath += string(sep)
	}

	return cleanedPath
}

// assertsDirectory returns whether the given path is
// asserted to be a directory, i.e., the path ends with
// a trailing '/' or `/.`, assuming a path separator of `/`.
func assertsDirectory(path string, sep byte) bool {
	return hasTrailingPathSeparator(path, sep) || specifiesCurrentDir(path)
}

// hasTrailingPathSeparator returns whether the given
// path ends with the system's path separator character.
func hasTrailingPathSeparator(path string, sep byte) bool {
	return len(path) > 0 && path[len(path)-1] == sep
}

// specifiesCurrentDir returns whether the given path specifies
// a "current directory", i.e., the last path segment is `.`.
func specifiesCurrentDir(path string) bool {
	return filepath.Base(path) == "."
}

// SplitPathDirEntry splits the given path between its directory name and its
// basename by first cleaning the path but preserves a trailing "." if the
// original path specified the current directory.
func SplitPathDirEntry(path string) (dir, base string) {
	cleanedPath := filepath.Clean(filepath.FromSlash(path))

	if specifiesCurrentDir(path) {
		cleanedPath += string(os.PathSeparator) + "."
	}

	return filepath.Dir(cleanedPath), filepath.Base(cleanedPath)
}

// TarResourceRebaseOpts does not preform the Tar, but instead just creates the rebase
// parameters to be sent to TarWithOptions (the TarOptions struct)
func TarResourceRebaseOpts(sourceBase string, rebaseName string) *TarOptions {
	filter := []string{sourceBase}
	return &TarOptions{
		Compression:      Uncompressed,
		IncludeFiles:     filter,
		IncludeSourceDir: true,
		RebaseNames: map[string]string{
			sourceBase: rebaseName,
		},
	}
}
