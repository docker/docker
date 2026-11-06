package daemon

import (
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/moby/go-archive"
	"github.com/moby/go-archive/chrootarchive"
	"github.com/moby/go-archive/compression"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/errdefs"
	"github.com/moby/moby/v2/pkg/ioutils"
)

// containerStatPath stats the filesystem resource at the specified path in this
// container. Returns stat info about the resource.
func (daemon *Daemon) containerStatPath(container *container.Container, path string) (*containertypes.PathStat, error) {
	container.Lock()
	defer container.Unlock()

	if err := checkContainerFSAvailable(container); err != nil {
		return nil, err
	}

	// Make sure an online file-system operation is permitted.
	if err := daemon.isOnlineFSOperationPermitted(container); err != nil {
		return nil, err
	}

	if err := daemon.Mount(container); err != nil {
		return nil, err
	}
	defer daemon.Unmount(container)

	err := daemon.mountVolumes(container)
	defer container.DetachAndUnmount(daemon.LogVolumeEvent)
	if err != nil {
		return nil, err
	}

	// Normalize path before sending to rootfs
	path = filepath.FromSlash(path)

	resolvedPath, absPath, err := container.ResolvePath(path)
	if err != nil {
		return nil, err
	}

	return container.StatPath(resolvedPath, absPath)
}

// containerArchivePath creates an archive of the filesystem resource at the specified
// path in this container. Returns a tar archive of the resource and stat info
// about the resource.
func (daemon *Daemon) containerArchivePath(container *container.Container, path string) (content io.ReadCloser, stat *containertypes.PathStat, retErr error) {
	container.Lock()

	defer func() {
		if retErr != nil {
			// Wait to unlock the container until the archive is fully read
			// (see the ReadCloseWrapper func below) or if there is an error
			// before that occurs.
			container.Unlock()
		}
	}()

	if err := checkContainerFSAvailable(container); err != nil {
		return nil, nil, err
	}

	// Make sure an online file-system operation is permitted.
	if err := daemon.isOnlineFSOperationPermitted(container); err != nil {
		return nil, nil, err
	}

	if err := daemon.Mount(container); err != nil {
		return nil, nil, err
	}

	defer func() {
		if retErr != nil {
			// unmount any volumes
			container.DetachAndUnmount(daemon.LogVolumeEvent)
			// unmount the container's rootfs
			daemon.Unmount(container)
		}
	}()

	if err := daemon.mountVolumes(container); err != nil {
		return nil, nil, err
	}

	// Normalize path before sending to rootfs
	path = filepath.FromSlash(path)

	resolvedPath, absPath, err := container.ResolvePath(path)
	if err != nil {
		return nil, nil, err
	}

	stat, err = container.StatPath(resolvedPath, absPath)
	if err != nil {
		return nil, nil, err
	}

	// We need to rebase the archive entries if the last element of the
	// resolved path was a symlink that was evaluated and is now different
	// than the requested path. For example, if the given path was "/foo/bar/",
	// but it resolved to "/var/lib/docker/containers/{id}/foo/baz/", we want
	// to ensure that the archive entries start with "bar" and not "baz". This
	// also catches the case when the root directory of the container is
	// requested: we want the archive entries to start with "/" and not the
	// container ID.

	// Get the source and the base paths of the container resolved path in order
	// to get the proper tar options for the rebase tar.
	resolvedPath = filepath.Clean(resolvedPath)
	if filepath.Base(resolvedPath) == "." {
		resolvedPath += string(filepath.Separator) + "."
	}

	sourceDir := resolvedPath
	sourceBase := "."

	if stat.Mode&os.ModeDir == 0 { // not dir
		sourceDir, sourceBase = filepath.Split(resolvedPath)
	}
	opts := archive.TarResourceRebaseOpts(sourceBase, filepath.Base(absPath))

	data, err := chrootarchive.Tar(sourceDir, opts, container.BaseFS)
	if err != nil {
		return nil, nil, err
	}

	content = ioutils.NewReadCloserWrapper(data, func() error {
		err := data.Close()
		container.DetachAndUnmount(daemon.LogVolumeEvent)
		daemon.Unmount(container)
		container.Unlock()
		return err
	})

	daemon.LogContainerEvent(container, events.ActionArchivePath)

	return content, stat, nil
}

// containerExtractToDir extracts the given tar archive to the specified location in the
// filesystem of this container. The given path must be of a directory in the
// container. If it is not, the error will be an errdefs.InvalidParameter. If
// noOverwriteDirNonDir is true then it will be an error if unpacking the
// given content would cause an existing directory to be replaced with a non-
// directory and vice versa.
//
// FIXME(thaJeztah): copyUIDGID is not supported on Windows, but currently ignored silently
func (daemon *Daemon) containerExtractToDir(container *container.Container, path string, copyUIDGID, allowOverwriteDirWithFile bool, content io.Reader) error {
	container.Lock()
	defer container.Unlock()

	if err := checkContainerFSAvailable(container); err != nil {
		return err
	}

	// Make sure an online file-system operation is permitted.
	if err := daemon.isOnlineFSOperationPermitted(container); err != nil {
		return err
	}

	if err := daemon.Mount(container); err != nil {
		return err
	}
	defer daemon.Unmount(container)

	err := daemon.mountVolumes(container)
	defer container.DetachAndUnmount(daemon.LogVolumeEvent)
	if err != nil {
		return err
	}

	// Normalize path before sending to rootfs'
	path = filepath.FromSlash(path)

	// Check if a drive letter supplied, it must be the system drive. No-op except on Windows
	path, err = archive.CheckSystemDriveAndRemoveDriveLetter(path)
	if err != nil {
		return err
	}

	// The destination path needs to be resolved to a host path, with all
	// symbolic links followed in the scope of the container's rootfs. Note
	// that we do not use `container.ResolvePath(path)` here because we need
	// to also evaluate the last path element if it is a symlink. This is so
	// that you can extract an archive to a symlink that points to a directory.

	// Consider the given path as an absolute path in the container.
	absPath := archive.PreserveTrailingDotOrSeparator(filepath.Join(string(filepath.Separator), path), path)

	// This will evaluate the last path element if it is a symlink.
	resolvedPath, err := container.GetResourcePath(absPath)
	if err != nil {
		return err
	}

	stat, err := os.Lstat(resolvedPath)
	if err != nil {
		return err
	}

	if !stat.IsDir() {
		return errdefs.InvalidParameter(errors.New("extraction point is not a directory"))
	}

	// TODO(thaJeztah): add check for writable path once windows supports read-only rootFS and (read-only) volumes during copy
	//
	// - e5261d6e4a1e96d4c0fa4b4480042046b695eda1 added "FIXME Post-TP4 / TP5"; check whether this would be possible to implement on Windows.
	// - e5261d6e4a1e96d4c0fa4b4480042046b695eda1 added "or extracting to a mount point inside a volume"; check whether this is still true, and adjust this check accordingly
	//
	// This comment is left in-place as a reminder :)
	//
	// absPath = filepath.Join(string(filepath.Separator), baseRel)
	// if err := checkWritablePath(container, absPath); err != nil {
	// 	return err
	// }

	options := daemon.defaultTarCopyOptions(allowOverwriteDirWithFile)
	if err := chrootarchive.UntarWithRoot(content, resolvedPath, options, container.BaseFS); err != nil {
		return err
	}

	daemon.LogContainerEvent(container, events.ActionExtractToDir)

	return nil
}

func (daemon *Daemon) containerCopy(container *container.Container, resource string) (_ io.ReadCloser, retErr error) {
	if resource[0] == '/' || resource[0] == '\\' {
		resource = resource[1:]
	}
	container.Lock()

	defer func() {
		if retErr != nil {
			// Wait to unlock the container until the archive is fully read
			// (see the ReadCloseWrapper func below) or if there is an error
			// before that occurs.
			container.Unlock()
		}
	}()

	// Make sure an online file-system operation is permitted.
	if err := daemon.isOnlineFSOperationPermitted(container); err != nil {
		return nil, err
	}

	if err := daemon.Mount(container); err != nil {
		return nil, err
	}

	defer func() {
		if retErr != nil {
			// unmount any volumes
			container.DetachAndUnmount(daemon.LogVolumeEvent)
			// unmount the container's rootfs
			daemon.Unmount(container)
		}
	}()

	if err := daemon.mountVolumes(container); err != nil {
		return nil, err
	}

	// Normalize path before sending to rootfs
	resource = filepath.FromSlash(resource)

	basePath, err := container.GetResourcePath(resource)
	if err != nil {
		return nil, err
	}
	stat, err := os.Stat(basePath)
	if err != nil {
		return nil, err
	}
	var filter []string
	if !stat.IsDir() {
		d, f := filepath.Split(basePath)
		basePath = d
		filter = []string{f}
	}
	archv, err := chrootarchive.Tar(basePath, &archive.TarOptions{
		Compression:  compression.None,
		IncludeFiles: filter,
	}, container.BaseFS)
	if err != nil {
		return nil, err
	}

	reader := ioutils.NewReadCloserWrapper(archv, func() error {
		err := archv.Close()
		container.DetachAndUnmount(daemon.LogVolumeEvent)
		daemon.Unmount(container)
		container.Unlock()
		return err
	})
	daemon.LogContainerEvent(container, events.ActionCopy)
	return reader, nil
}

// isOnlineFSOperationPermitted returns an error if an online filesystem operation
// is not permitted (such as stat or for copying). Running Hyper-V containers
// cannot have their file-system interrogated from the host as the filter is
// loaded inside the utility VM, not the host.
// IMPORTANT: The container lock MUST be held when calling this function.
func (daemon *Daemon) isOnlineFSOperationPermitted(ctr *container.Container) error {
	if !ctr.State.Running {
		return nil
	}

	// Determine isolation. If not specified in the hostconfig, use daemon default.
	if ctr.HostConfig.Isolation.IsHyperV() || ctr.HostConfig.Isolation.IsDefault() && daemon.defaultIsolation.IsHyperV() {
		return errors.New("filesystem operations against a running Hyper-V container are not supported")
	}
	return nil
}
