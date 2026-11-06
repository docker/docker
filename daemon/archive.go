package daemon

import (
	"fmt"
	"io"
	"os"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	daemoncontainer "github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/errdefs"
)

// checkContainerFSAvailable returns an error if the container's rootfs can no
// longer be used because the container is being removed or has been marked
// dead.
//
// This can occur when a filesystem operation races with the container being
// removed; for example a container created with "--rm" exiting while content
// is being copied into it. The container still resolves by name at that point,
// but its RW layer has already been released, so without this check the
// operation fails with an internal "RWLayer ... is unexpectedly nil" error.
//
// The container lock MUST be held when calling this function. Note that
// [container.Container] embeds [container.State], whose mutex is used as the
// lock for both, so the state must be read directly instead of through the
// locking accessors on State.
func checkContainerFSAvailable(ctr *daemoncontainer.Container) error {
	if ctr.State.RemovalInProgress {
		return errdefs.Conflict(fmt.Errorf("container %s is being removed", ctr.ID))
	}
	if ctr.State.Dead {
		return errdefs.Conflict(fmt.Errorf("container %s is dead", ctr.ID))
	}
	return nil
}

// ContainerStatPath stats the filesystem resource at the specified path in the
// container identified by the given name.
func (daemon *Daemon) ContainerStatPath(name string, path string) (*container.PathStat, error) {
	ctr, err := daemon.GetContainer(name)
	if err != nil {
		return nil, err
	}

	stat, err := daemon.containerStatPath(ctr, path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, containerFileNotFound{path, name}
		}
		// TODO(thaJeztah): check if daemon.containerStatPath returns any errors that are not typed; if not, then return as-is
		if cerrdefs.IsConflict(err) || cerrdefs.IsInvalidArgument(err) {
			return nil, err
		}
		return nil, errdefs.System(err)
	}
	return stat, nil
}

// ContainerArchivePath creates an archive of the filesystem resource at the
// specified path in the container identified by the given name. Returns a
// tar archive of the resource and whether it was a directory or a single file.
func (daemon *Daemon) ContainerArchivePath(name string, path string) (content io.ReadCloser, stat *container.PathStat, _ error) {
	ctr, err := daemon.GetContainer(name)
	if err != nil {
		return nil, nil, err
	}

	content, stat, err = daemon.containerArchivePath(ctr, path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, containerFileNotFound{path, name}
		}
		// TODO(thaJeztah): check if daemon.containerArchivePath returns any errors that are not typed; if not, then return as-is
		if cerrdefs.IsConflict(err) || cerrdefs.IsInvalidArgument(err) {
			return nil, nil, err
		}
		return nil, nil, errdefs.System(err)
	}
	return content, stat, nil
}

// ContainerExtractToDir extracts the given archive to the specified location
// in the filesystem of the container identified by the given name. The given
// path must be of a directory in the container. If it is not, the error will
// be an errdefs.InvalidParameter. It returns an error if unpacking the given
// content would cause an existing directory to be replaced with a non-directory
// or vice versa, unless allowOverwriteDirWithFile is set to true.
func (daemon *Daemon) ContainerExtractToDir(name, path string, copyUIDGID, allowOverwriteDirWithFile bool, content io.Reader) error {
	ctr, err := daemon.GetContainer(name)
	if err != nil {
		return err
	}

	err = daemon.containerExtractToDir(ctr, path, copyUIDGID, allowOverwriteDirWithFile, content)
	if err != nil {
		if os.IsNotExist(err) {
			return containerFileNotFound{path, name}
		}
		// TODO(thaJeztah): check if daemon.containerExtractToDir returns any errors that are not typed; if not, then return as-is
		if cerrdefs.IsConflict(err) || cerrdefs.IsInvalidArgument(err) {
			return err
		}
		return errdefs.System(err)
	}
	return nil
}
