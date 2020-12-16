// +build linux

package containerd // import "github.com/docker/docker/daemon/graphdriver/containerd"

import (
	"io/ioutil"
	"os"
	"sync"
	"testing"

	content "github.com/containerd/containerd/content/local"
	"github.com/containerd/containerd/snapshots/overlay"
	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/daemon/graphdriver/graphtest"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/reexec"
)

func init() {
	graphdriver.ApplyUncompressedLayer = archive.ApplyUncompressedLayer

	reexec.Init()
}

var (
	cleanup   func()
	cleanupMu sync.Mutex
)

func addCleanup(f func()) {
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	oldCleanup := cleanup
	cleanup = func() {
		if oldCleanup != nil {
			defer oldCleanup()
		}
		f()
	}
}

// This avoids creating a new driver for each test if all tests are run
// Make sure to put new tests between TestSnapshotterSetup and TestSnapshotterTeardown
func TestSnapshotterSetup(t *testing.T) {
	// Prepare snapshotter
	snRoot, err := ioutil.TempDir("", "temp-snapshotter")
	if err != nil {
		t.Fatal(err)
	}
	addCleanup(func() { os.RemoveAll(snRoot) })
	builtinSnapshotterName = "overlayfs"
	builtinSnapshotter, err = overlay.NewSnapshotter(snRoot)
	if err != nil {
		t.Fatal(err)
	}

	// Prepare content store
	csRoot, err := ioutil.TempDir("", "temp-content")
	if err != nil {
		t.Fatal(err)
	}
	addCleanup(func() { os.RemoveAll(csRoot) })
	builtinStore, err = content.NewStore(csRoot)

	graphtest.GetDriver(t, driverName)
}

func TestSnapshotterCreateEmpty(t *testing.T) {
	graphtest.DriverTestCreateEmpty(t, driverName)
}

func TestSnapshotterCreateBase(t *testing.T) {
	graphtest.DriverTestCreateBase(t, driverName)
}

func TestSnapshotterCreateSnap(t *testing.T) {
	graphtest.DriverTestCreateSnap(t, driverName)
}

func TestSnapshotter128LayerRead(t *testing.T) {
	graphtest.DriverTestDeepLayerRead(t, 128, driverName)
}

func TestSnapshotterTeardown(t *testing.T) {
	graphtest.PutDriver(t)
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	if cleanup != nil {
		cleanup()
	}
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
