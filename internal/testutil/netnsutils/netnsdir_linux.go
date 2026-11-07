package netnsutils

import (
	"errors"
	"io/fs"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TempNetnsDir returns a directory for the network namespaces of a
// libnetwork controller's sandboxes, which is removed when the test ends.
// The network namespaces left in it are unmounted first.
func TempNetnsDir(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	// Cleanup functions run in reverse order, so this one runs before
	// the one which removes dir.
	t.Cleanup(func() {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if err := unix.Unmount(path, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) {
				t.Logf("unmounting %s: %v", path, err)
			}
			return nil
		})
	})
	return dir
}
