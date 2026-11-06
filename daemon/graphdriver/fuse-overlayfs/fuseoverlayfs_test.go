//go:build linux

package fuseoverlayfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/go-archive"
	"github.com/moby/moby/v2/daemon/graphdriver"
	"github.com/moby/moby/v2/daemon/graphdriver/graphtest"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func init() {
	// Do not sure chroot to speed run time and allow archive
	// errors or hangs to be debugged directly from the test process.
	untar = archive.UntarUncompressed
	graphdriver.ApplyUncompressedLayer = archive.ApplyUncompressedLayer
}

// This avoids creating a new driver for each test if all tests are run
// Make sure to put new tests between TestFUSEOverlayFSSetup and TestFUSEOverlayFSTeardown
func TestFUSEOverlayFSSetup(t *testing.T) {
	graphtest.GetDriver(t, driverName)
}

func TestFUSEOverlayFSCreateEmpty(t *testing.T) {
	graphtest.DriverTestCreateEmpty(t, driverName)
}

func TestFUSEOverlayFSCreateBase(t *testing.T) {
	graphtest.DriverTestCreateBase(t, driverName)
}

func TestFUSEOverlayFSCreateSnap(t *testing.T) {
	graphtest.DriverTestCreateSnap(t, driverName)
}

func TestFUSEOverlayFS128LayerRead(t *testing.T) {
	graphtest.DriverTestDeepLayerRead(t, 128, driverName)
}

func TestFUSEOverlayFSTeardown(t *testing.T) {
	graphtest.PutDriver(t)
}

// TestRemoveInvalidLink verifies that invalid link metadata cannot cause
// Remove to delete paths outside the driver's link directory.
func TestRemoveInvalidLink(t *testing.T) {
	// Use a dedicated driver because this test intentionally corrupts driver metadata.
	driver := graphtest.GetDriver(t, driverName)
	defer graphtest.PutDriver(t)

	d := driver.(*graphtest.Driver).Driver.(*Driver)

	for _, tc := range []struct {
		name   string
		linkID string
	}{
		{name: "empty", linkID: ""},
		{name: "dot", linkID: "."},
		{name: "dot-slash", linkID: "./"},
		{name: "root", linkID: "/"},
		{name: "absolute", linkID: "/foo"},
		{name: "parent", linkID: ".."},
		{name: "parent traversal", linkID: "../foo"},
		{name: "nested traversal", linkID: "foo/../../bar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := "invalid-link-" + tc.name
			assert.NilError(t, d.Create(id, "", nil))

			linkFile := filepath.Join(d.dir(id), "link")
			linkID, err := os.ReadFile(linkFile)
			assert.NilError(t, err)

			// Remove can no longer discover the original link after corrupting
			// the metadata below, so clean it up explicitly.
			t.Cleanup(func() {
				_ = os.Remove(filepath.Join(d.home, linkDir, string(linkID)))
			})

			target := filepath.Join(d.home, linkDir, tc.linkID)
			sentinel := filepath.Join(target, "sentinel")
			assert.NilError(t, os.MkdirAll(target, 0o755))
			assert.NilError(t, os.WriteFile(sentinel, nil, 0o644))
			assert.NilError(t, os.WriteFile(linkFile, []byte(tc.linkID), 0o644))

			assert.NilError(t, d.Remove(id))

			_, err = os.Stat(sentinel)
			assert.NilError(t, err)
		})
	}
}

func TestFUSEOverlayFSDiffSize(t *testing.T) {
	driver := graphtest.GetDriver(t, driverName)
	defer graphtest.PutDriver(t)

	d := driver.(*graphtest.Driver).Driver.(*Driver)

	baseID := "fuse-diffsize-base"
	upperID := "fuse-diffsize-upper"

	assert.NilError(t, d.Create(baseID, "", nil))
	defer func() { _ = d.Remove(baseID) }()

	assert.NilError(t, d.CreateReadWrite(upperID, baseID, nil))
	defer func() { _ = d.Remove(upperID) }()

	diffPath := d.getDiffPath(upperID)
	content := []byte("fuse-overlayfs diff size content")
	assert.NilError(t, os.WriteFile(filepath.Join(diffPath, "testfile.txt"), content, 0o644))

	size, err := d.DiffSize(t.Context(), upperID, baseID)
	assert.NilError(t, err)
	assert.Check(t, size == int64(len(content)))

	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = d.DiffSize(canceledCtx, upperID, baseID)
	assert.Check(t, is.ErrorIs(err, context.Canceled))
}

// Benchmarks should always setup new driver

func BenchmarkExists(b *testing.B) {
	graphtest.DriverBenchExists(b, driverName)
}

func BenchmarkGetEmpty(b *testing.B) {
	graphtest.DriverBenchGetEmpty(b, driverName)
}

func BenchmarkDiffBase(b *testing.B) {
	graphtest.DriverBenchDiffBase(b, driverName)
}

func BenchmarkDiffSmallUpper(b *testing.B) {
	graphtest.DriverBenchDiffN(b, 10, 10, driverName)
}

func BenchmarkDiff10KFileUpper(b *testing.B) {
	graphtest.DriverBenchDiffN(b, 10, 10000, driverName)
}

func BenchmarkDiff10KFilesBottom(b *testing.B) {
	graphtest.DriverBenchDiffN(b, 10000, 10, driverName)
}

func BenchmarkDiffApply100(b *testing.B) {
	graphtest.DriverBenchDiffApplyN(b, 100, driverName)
}

func BenchmarkDiff20Layers(b *testing.B) {
	graphtest.DriverBenchDeepLayerDiff(b, 20, driverName)
}

func BenchmarkRead20Layers(b *testing.B) {
	graphtest.DriverBenchDeepLayerRead(b, 20, driverName)
}
