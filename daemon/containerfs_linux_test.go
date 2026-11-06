package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	runcoptions "github.com/containerd/containerd/api/types/runc/options"
	"github.com/containerd/containerd/v2/plugins"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/container"
	"golang.org/x/sys/unix"
	"gotest.tools/v3/assert"
)

func TestContainerFSHostAccess(t *testing.T) {
	runcOpts := &runcoptions.Options{BinaryName: config.DefaultRuntimeBinary}
	tests := []struct {
		name        string
		snapshotter string
		runtime     string
		shim        string
		opts        any
		wantErr     bool
	}{
		{name: "Kata with blockfile", snapshotter: "blockfile", runtime: "kata", shim: "io.containerd.kata.v2", wantErr: true},
		{name: "Kata with devmapper", snapshotter: "devmapper", runtime: "kata", shim: "io.containerd.kata.v2", wantErr: true},
		{name: "runc with blockfile", snapshotter: "blockfile", runtime: config.StockRuntimeName, shim: plugins.RuntimeRuncV2, opts: runcOpts},
		{name: "runc with devmapper", snapshotter: "devmapper", runtime: config.StockRuntimeName, shim: plugins.RuntimeRuncV2, opts: runcOpts},
		{name: "custom runc", snapshotter: "blockfile", runtime: "custom-runc", shim: plugins.RuntimeRuncV2, wantErr: true},
		{name: "runc v2", snapshotter: "blockfile", runtime: linuxV2RuntimeName, shim: plugins.RuntimeRuncV2, opts: runcOpts},
		{name: "custom runc binary", snapshotter: "blockfile", runtime: linuxV2RuntimeName, shim: plugins.RuntimeRuncV2, opts: &runcoptions.Options{BinaryName: "kata-runtime"}, wantErr: true},
		{name: "missing runc options", snapshotter: "blockfile", runtime: config.StockRuntimeName, shim: plugins.RuntimeRuncV2, wantErr: true},
		{name: "Kata with overlayfs", snapshotter: "overlayfs", runtime: "kata", shim: "io.containerd.kata.v2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := containerFSHostAccessError(tt.snapshotter, tt.runtime, tt.shim, tt.opts)
			if tt.wantErr {
				assert.Assert(t, cerrdefs.IsInvalidArgument(err))
				return
			}
			assert.NilError(t, err)
		})
	}
}

func TestCreateIfNotExists(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		root, err := os.OpenRoot(dir)
		assert.NilError(t, err)
		defer root.Close()

		err = createIfNotExists(root, "tocreate", true)
		assert.NilError(t, err)

		fileinfo, err := os.Stat(filepath.Join(dir, "tocreate"))
		assert.NilError(t, err, "Did not create destination")
		assert.Assert(t, fileinfo.IsDir(), "Should have been a dir, seems it's not")

		err = createIfNotExists(root, "tocreate", true)
		assert.NilError(t, err, "Should not fail if already exists")
	})
	t.Run("file", func(t *testing.T) {
		dir := t.TempDir()
		root, err := os.OpenRoot(dir)
		assert.NilError(t, err)
		defer root.Close()

		err = createIfNotExists(root, "file/to/create", false)
		assert.NilError(t, err)

		fileinfo, err := os.Stat(filepath.Join(dir, "file/to/create"))
		assert.NilError(t, err, "Did not create destination")

		assert.Assert(t, !fileinfo.IsDir(), "Should have been a file, but created a directory")

		err = createIfNotExists(root, "file/to/create", false)
		assert.NilError(t, err, "Should not fail if already exists")
	})

	// A mount destination which already exists is left alone whatever its
	// type. Nested bind mounts make the destination of the inner mount
	// resolve to the outer mount's contents, so it can be any kind of file.
	t.Run("existing socket", func(t *testing.T) {
		dir := t.TempDir()
		l, err := net.Listen("unix", filepath.Join(dir, "sock"))
		assert.NilError(t, err)
		defer l.Close()

		root, err := os.OpenRoot(dir)
		assert.NilError(t, err)
		defer root.Close()

		err = createIfNotExists(root, "sock", false)
		assert.NilError(t, err, "Should not fail if already exists as a socket")
	})
	t.Run("existing fifo", func(t *testing.T) {
		dir := t.TempDir()
		assert.NilError(t, unix.Mkfifo(filepath.Join(dir, "fifo"), 0o600))

		root, err := os.OpenRoot(dir)
		assert.NilError(t, err)
		defer root.Close()

		// Opening a FIFO blocks until the other end is opened, so guard
		// against the test hanging for the whole -timeout if it regresses.
		done := make(chan error, 1)
		go func() { done <- createIfNotExists(root, "fifo", false) }()
		select {
		case err := <-done:
			assert.NilError(t, err, "Should not fail if already exists as a FIFO")
		case <-time.After(10 * time.Second):
			t.Fatal("createIfNotExists blocked on an existing FIFO")
		}
	})
	t.Run("existing socket when a directory is expected", func(t *testing.T) {
		dir := t.TempDir()
		l, err := net.Listen("unix", filepath.Join(dir, "sock"))
		assert.NilError(t, err)
		defer l.Close()

		root, err := os.OpenRoot(dir)
		assert.NilError(t, err)
		defer root.Close()

		// Nothing can be bind-mounted onto it -- the kernel rejects a
		// directory source with ENOTDIR -- so it is reported rather than
		// left for mount(2) to fail on.
		err = createIfNotExists(root, "sock", true)
		assert.ErrorIs(t, err, os.ErrExist)
	})
	t.Run("existing directory when a file is expected", func(t *testing.T) {
		dir := t.TempDir()
		assert.NilError(t, os.Mkdir(filepath.Join(dir, "dir"), 0o755))

		root, err := os.OpenRoot(dir)
		assert.NilError(t, err)
		defer root.Close()

		err = createIfNotExists(root, "dir", false)
		assert.NilError(t, err, "Should not fail if already exists as a directory")
	})
}

func TestContainerFSHostAccessStopped(t *testing.T) {
	daemon := &Daemon{usesSnapshotter: true}
	ctr := &container.Container{State: &container.State{}, Driver: "blockfile"}
	assert.NilError(t, daemon.checkContainerFSHostAccess(ctr))
}
