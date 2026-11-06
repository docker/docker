package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/containerd/log"
	"github.com/moby/sys/mount"
	"github.com/moby/sys/symlink"
	"github.com/moby/sys/userns"
	"golang.org/x/sys/unix"

	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/mounttree"
	"github.com/moby/moby/v2/daemon/internal/rootless/mountopts"
	"github.com/moby/moby/v2/daemon/internal/unshare"
)

type future struct {
	fn  func() error
	res chan<- error
}

// containerFSView allows functions to be run in the context of a container's
// filesystem. Inside these functions, the root directory is the container root
// for all native OS filesystem APIs, including, but not limited to, the [os]
// and [golang.org/x/sys/unix] packages. The view of the container's filesystem
// is live and read-write. Each view has its own private set of tmpfs mounts.
// Any files written under a tmpfs mount are not visible to processes inside the
// container nor any other view of the container's filesystem, and vice versa.
//
// Each view has its own current working directory which is initialized to the
// root of the container filesystem and can be changed with [os.Chdir]. Changes
// to the current directory persist across successive [*containerFSView.RunInFS]
// and [*containerFSView.GoInFS] calls.
//
// Multiple views of the same container filesystem can coexist at the same time.
// Only one function can be running in a particular filesystem view at any given
// time. Calls to [*containerFSView.RunInFS] or [*containerFSView.GoInFS] will
// block while another function is running. If more than one call is blocked
// concurrently, the order they are unblocked is undefined.
type containerFSView struct {
	d    *Daemon
	ctr  *container.Container
	todo chan future
	done chan error
}

// openContainerFS opens a new view of the container's filesystem.
//
// The container lock MUST be held when calling this function: the container's
// state is checked before the filesystem is opened, and [container.Container]
// embeds [container.State], whose mutex is the lock for both.
func (daemon *Daemon) openContainerFS(ctr *container.Container) (_ *containerFSView, retErr error) {
	ctx := context.TODO()

	if err := checkContainerFSAvailable(ctr); err != nil {
		return nil, err
	}

	if err := daemon.Mount(ctr); err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			if err := daemon.Unmount(ctr); err != nil {
				log.G(ctx).WithError(err).Debug("Failed to unmount container after failure")
			}
		}
	}()

	mounts, cleanup, err := daemon.setupMounts(ctx, ctr)
	if err != nil {
		return nil, err
	}
	defer func() {
		ctx := context.WithoutCancel(ctx)
		if err := cleanup(ctx); err != nil {
			log.G(ctx).WithError(err).Debug("Failed to cleanup container mounts")
		}
		if retErr != nil {
			if err := ctr.UnmountVolumes(ctx, daemon.LogVolumeEvent); err != nil {
				log.G(ctx).WithError(err).Debug("Failed to unmount container volumes after failure")
			}
		}
	}()

	// Setup in initial mount namespace complete. We're ready to unshare the
	// mount namespace and bind the volume mounts into that private view of
	// the container FS.
	todo := make(chan future)
	done := make(chan error)
	err = unshare.Go(unix.CLONE_NEWNS,
		func() error {
			if err := mount.MakeRSlave("/"); err != nil {
				return err
			}

			root, err := os.OpenRoot(ctr.BaseFS)
			if err != nil {
				return fmt.Errorf("open container root: %w", err)
			}
			defer root.Close()

			// TODO(vvoland): Refactor this after security release.
			for _, m := range mounts {
				// Walk m.Destination through the container's symlinks before
				// passing it to os.Root, which refuses absolute symlinks
				// (e.g. the common /var/run -> /run). The resolution itself
				// is lexical; subsequent os.Root operations still enforce
				// the GHSA-vp62-88p7-qqf5 / GHSA-rg2x-37c3-w2rh protections.
				resolved, err := ctr.GetResourcePath(m.Destination)
				if err != nil {
					return fmt.Errorf("resolve mount destination %q: %w", m.Destination, err)
				}
				relDest, err := filepath.Rel(ctr.BaseFS, resolved)
				if err != nil {
					return fmt.Errorf("make destination relative: %w", err)
				}

				var stat os.FileInfo
				stat, err = os.Stat(m.Source)
				if err != nil {
					return err
				}
				if err := createIfNotExists(root, relDest, stat.IsDir()); err != nil {
					return err
				}

				bindMode := "rbind"
				if m.NonRecursive {
					bindMode = "bind"
				}
				if m.Writable {
					if m.ReadOnlyNonRecursive {
						return errors.New("options conflict: Writable && ReadOnlyNonRecursive")
					}
					if m.ReadOnlyForceRecursive {
						return errors.New("options conflict: Writable && ReadOnlyForceRecursive")
					}
				}
				if m.ReadOnlyNonRecursive && m.ReadOnlyForceRecursive {
					return errors.New("options conflict: ReadOnlyNonRecursive && ReadOnlyForceRecursive")
				}

				// Open the mount target through os.Root so we have a
				// file descriptor pinning the resolved inode. Using
				// /proc/self/fd/<fd> as the mount target prevents any
				// subsequent symlink swap from redirecting the mount.
				// O_PATH is required: the target is not necessarily a
				// regular file or a directory, and opening a socket or a
				// FIFO for I/O would fail or block.
				targetFile, err := root.OpenFile(relDest, unix.O_PATH, 0)
				if err != nil {
					return fmt.Errorf("open mount target %q: %w", m.Destination, err)
				}
				targetPath := "/proc/self/fd/" + strconv.FormatUint(uint64(targetFile.Fd()), 10)

				// The kernel rejects remount and propagation-change syscalls
				// when the target is a /proc/self/fd path. Only the initial
				// bind mount works on such paths, so we perform that via the
				// fd path for TOCTOU safety and then resolve the real path for
				// the read-only remount and propagation change.
				if err := mount.Mount(m.Source, targetPath, "", bindMode); err != nil {
					targetFile.Close()
					return err
				}
				realPath, err := os.Readlink(targetPath)
				if err != nil {
					targetFile.Close()
					return fmt.Errorf("readlink %s: %w", targetPath, err)
				}
				if !m.Writable {
					opts := []string{"ro", "remount", "bind"}
					// A rootless daemon cannot clear the flags locked by
					// CL_UNPRIVILEGED, such as nosuid and nodev on a /tmp
					// tmpfs holding X11 sockets, so the remount must keep them.
					if userns.RunningInUserNS() {
						unprivOpts, err := mountopts.UnprivilegedMountFlags(realPath)
						if err != nil {
							targetFile.Close()
							return err
						}
						opts = append(opts, unprivOpts...)
					}
					if err := mount.Mount("", realPath, "", strings.Join(opts, ",")); err != nil {
						targetFile.Close()
						return err
					}
				}

				// openContainerFS() is called for temporary mounts
				// outside the container. Soon these will be unmounted
				// with lazy unmount option and given we have mounted
				// them rbind, all the submounts will propagate if these
				// are shared. If daemon is running in host namespace
				// and has / as shared then these unmounts will
				// propagate and unmount original mount as well. So make
				// all these mounts rprivate.  Do not use propagation
				// property of volume as that should apply only when
				// mounting happens inside the container.
				if err := mount.MakeRPrivate(realPath); err != nil {
					targetFile.Close()
					return err
				}

				if !m.Writable && !m.ReadOnlyNonRecursive {
					if err := makeMountRRO(realPath); err != nil {
						targetFile.Close()
						if m.ReadOnlyForceRecursive {
							return err
						}
						log.G(context.TODO()).WithError(err).Debugf("Failed to make %q recursively read-only", m.Destination)
					}
				}
				targetFile.Close()
			}

			return mounttree.SwitchRoot(ctr.BaseFS)
		},
		func() {
			defer close(done)

			for it := range todo {
				err := it.fn()
				if it.res != nil {
					it.res <- err
				}
			}

			// The thread will terminate when this goroutine returns, taking the
			// mount namespace and all the volume bind-mounts with it.
		},
	)
	if err != nil {
		return nil, err
	}
	vw := &containerFSView{
		d:    daemon,
		ctr:  ctr,
		todo: todo,
		done: done,
	}
	runtime.SetFinalizer(vw, (*containerFSView).Close)
	return vw, nil
}

// RunInFS synchronously runs fn in the context of the container filesystem and
// passes through its return value.
//
// The container filesystem is only visible to functions called in the same
// goroutine as fn. Goroutines started from fn will see the host's filesystem.
func (vw *containerFSView) RunInFS(ctx context.Context, fn func() error) error {
	res := make(chan error)
	select {
	case vw.todo <- future{fn: fn, res: res}:
	case <-ctx.Done():
		return ctx.Err()
	}
	return <-res
}

// GoInFS starts fn in the container FS. It blocks until fn is started but does
// not wait until fn returns. An error is returned if ctx is canceled before fn
// has been started.
//
// The container filesystem is only visible to functions called in the same
// goroutine as fn. Goroutines started from fn will see the host's filesystem.
func (vw *containerFSView) GoInFS(ctx context.Context, fn func()) error {
	select {
	case vw.todo <- future{fn: func() error { fn(); return nil }}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close waits until any in-flight operations complete and frees all
// resources associated with vw.
func (vw *containerFSView) Close() error {
	runtime.SetFinalizer(vw, nil)
	close(vw.todo)
	var errs []error
	errs = append(errs,
		<-vw.done,
		vw.ctr.UnmountVolumes(context.TODO(), vw.d.LogVolumeEvent),
		vw.d.Unmount(vw.ctr),
	)
	return errors.Join(errs...)
}

// Stat returns the metadata for path, relative to the current working directory
// of vw inside the container filesystem view.
func (vw *containerFSView) Stat(ctx context.Context, path string) (*containertypes.PathStat, error) {
	var stat *containertypes.PathStat
	err := vw.RunInFS(ctx, func() error {
		lstat, err := os.Lstat(path)
		if err != nil {
			return err
		}
		var target string
		if lstat.Mode()&os.ModeSymlink != 0 {
			// Fully evaluate symlinks along path to the ultimate
			// target, or as much as possible with broken links.
			target, err = symlink.FollowSymlinkInScope(path, "/")
			if err != nil {
				return err
			}
		}
		stat = &containertypes.PathStat{
			Name:       filepath.Base(path),
			Size:       lstat.Size(),
			Mode:       lstat.Mode(),
			Mtime:      lstat.ModTime(),
			LinkTarget: target,
		}
		return nil
	})
	return stat, err
}

// createIfNotExists creates a file or a directory only if it does not already exist.
// The path is scoped to root using [os.Root] to prevent symlink escape attacks.
func createIfNotExists(root *os.Root, unsafePath string, isDir bool) error {
	if isDir {
		return root.MkdirAll(unsafePath, 0o755)
	}

	parent := filepath.Dir(unsafePath)
	if parent != "." && parent != "/" {
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return err
		}
	}

	// O_EXCL leaves an existing destination alone whatever its type, and does
	// so atomically. Checking for the destination and then opening it would
	// let a container process substitute a FIFO in between, and opening a
	// FIFO for writing blocks until a reader shows up -- wedging the thread
	// while the container lock is held. Opening an existing socket would fail
	// with ENXIO, and an existing directory with EISDIR.
	f, err := root.OpenFile(unsafePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return f.Close()
}

// makeMountRRO makes the mount recursively read-only.
func makeMountRRO(dest string) error {
	attr := &unix.MountAttr{
		Attr_set: unix.MOUNT_ATTR_RDONLY,
	}
	var err error
	for {
		err = unix.MountSetattr(-1, dest, unix.AT_RECURSIVE, attr)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		err = fmt.Errorf("failed to apply MOUNT_ATTR_RDONLY with AT_RECURSIVE to %q: %w", dest, err)
	}
	return err
}
