//go:build !windows

package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	containertypes "github.com/moby/moby/api/types/container"
	mounttypes "github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/idtools"
	"github.com/moby/moby/v2/daemon/pkg/oci"
	volumemounts "github.com/moby/moby/v2/daemon/volume/mounts"
	volumeopts "github.com/moby/moby/v2/daemon/volume/service/opts"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
)

// createContainerOSSpecificSettings performs host-OS specific container create functionality
func (daemon *Daemon) createContainerOSSpecificSettings(ctx context.Context, ctr *container.Container) error {
	// Set the default masked and readonly paths with regard to the host config options if they are not set.
	if ctr.HostConfig.MaskedPaths == nil && !ctr.HostConfig.Privileged {
		ctr.HostConfig.MaskedPaths = oci.DefaultSpec().Linux.MaskedPaths // Set it to the default if nil
	}
	if ctr.HostConfig.ReadonlyPaths == nil && !ctr.HostConfig.Privileged {
		ctr.HostConfig.ReadonlyPaths = oci.DefaultSpec().Linux.ReadonlyPaths // Set it to the default if nil
	}
	return nil
}

// createContainerVolumesOS performs host-OS specific volume creation
func (daemon *Daemon) createContainerVolumesOS(ctx context.Context, ctr *container.Container, config *containertypes.Config) error {
	if err := daemon.Mount(ctr); err != nil {
		return err
	}
	defer daemon.Unmount(ctr)

	// The working directory, when created, is owned by the container's
	// user, so that it is writable when running with a non-root USER.
	usr, err := getUser(ctr, config.User)
	if err != nil {
		return err
	}
	uid, gid, err := daemon.idMapping.ToHost(int(usr.UID), int(usr.GID))
	if err != nil {
		return err
	}
	if err := ctr.SetupWorkingDirectory(uid, gid); err != nil {
		return err
	}

	for spec := range config.Volumes {
		destination := filepath.Clean(spec)

		// Skip volumes for which we already have something mounted on that
		// destination because of a --volume-from.
		if ctr.HasMountFor(destination) {
			log.G(ctx).WithField("container", ctr.ID).WithField("destination", spec).Debug("mountpoint already exists, skipping anonymous volume")
			// Not an error, this could easily have come from the image config.
			continue
		}
		path, err := ctr.GetResourcePath(destination)
		if err != nil {
			return err
		}

		stat, err := os.Stat(path)
		if err == nil && !stat.IsDir() {
			return fmt.Errorf("cannot mount volume over existing file, file exists %s", path)
		}

		v, err := daemon.volumes.Create(context.TODO(), "", ctr.HostConfig.VolumeDriver, volumeopts.WithCreateReference(ctr.ID))
		if err != nil {
			return err
		}

		if err := label.Relabel(v.Mountpoint, ctr.MountLabel, true); err != nil {
			return err
		}

		ctr.AddMountPointWithVolume(destination, &volumeWrapper{v: v, s: daemon.volumes}, true)
	}
	return daemon.populateVolumes(ctx, ctr)
}

// populateVolumes copies data from the container's rootfs into the volume for non-binds.
// this is only called when the container is created.
func (daemon *Daemon) populateVolumes(ctx context.Context, c *container.Container) error {
	for _, mnt := range c.MountPoints {
		if mnt.Volume == nil {
			continue
		}

		if mnt.Type != mounttypes.TypeVolume || !mnt.CopyData {
			continue
		}

		if err := daemon.populateVolume(ctx, c, mnt); err != nil {
			return err
		}
	}
	return nil
}

func (daemon *Daemon) populateVolume(ctx context.Context, c *container.Container, mnt *volumemounts.MountPoint) error {
	ctrDestPath, err := c.GetResourcePath(mnt.Destination)
	if err != nil {
		return err
	}

	stat, err := os.Stat(ctrDestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	// Volume population copies directory contents from the container image
	// into the volume. This only applies when the source is a directory.
	if !stat.IsDir() {
		return nil
	}

	uid, gid := daemon.idMapping.RootPair()
	volumePath, cleanup, err := mnt.Setup(ctx, c.MountLabel, idtools.Identity{UID: uid, GID: gid}, nil)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil
		}
		log.G(ctx).WithError(err).Debugf("can't copy data from %s:%s, to %s", c.ID, mnt.Destination, volumePath)
		return errors.Wrapf(err, "failed to populate volume")
	}
	defer func() {
		ctx := context.WithoutCancel(ctx)
		_ = cleanup(ctx)
		_ = mnt.Cleanup(ctx)
	}()

	log.G(ctx).Debugf("copying image data from %s:%s, to %s", c.ID, mnt.Destination, volumePath)
	if err := c.CopyImagePathContent(volumePath, ctrDestPath); err != nil {
		return err
	}

	return nil
}
