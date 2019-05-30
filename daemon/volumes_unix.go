// +build !windows

package daemon // import "github.com/docker/docker/daemon"

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	mounttypes "github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/container"
	"github.com/docker/docker/pkg/fileutils"
	"github.com/docker/docker/pkg/mount"
	volumemounts "github.com/docker/docker/volume/mounts"
)

// setupMounts iterates through each of the mount points for a container and
// calls Setup() on each. It also looks to see if is a network mount such as
// /etc/resolv.conf, and if it is not, appends it to the array of mounts.
func (daemon *Daemon) setupMounts(c *container.Container) ([]container.Mount, error) {
	var mounts []container.Mount
	// TODO: tmpfs mounts should be part of Mountpoints
	tmpfsMounts := make(map[string]bool)
	tmpfsMountInfo, err := c.TmpfsMounts()
	if err != nil {
		return nil, err
	}
	for _, m := range tmpfsMountInfo {
		tmpfsMounts[m.Destination] = true
	}
	for _, m := range c.MountPoints {
		if tmpfsMounts[m.Destination] {
			continue
		}
		if err := daemon.lazyInitializeVolume(c.ID, m); err != nil {
			return nil, err
		}
		// If the daemon is being shutdown, we should not let a container start if it is trying to
		// mount the socket the daemon is listening on. During daemon shutdown, the socket
		// (/var/run/docker.sock by default) doesn't exist anymore causing the call to m.Setup to
		// create at directory instead. This in turn will prevent the daemon to restart.
		checkfunc := func(m *volumemounts.MountPoint) error {
			if _, exist := daemon.hosts[m.Source]; exist && daemon.IsShuttingDown() {
				return fmt.Errorf("Could not mount %q to container while the daemon is shutting down", m.Source)
			}
			return nil
		}

		path, err := m.Setup(c.MountLabel, daemon.idMapping.RootPair(), checkfunc)
		if err != nil {
			return nil, err
		}
		if !c.TrySetNetworkMount(m.Destination, path) {
			mnt := container.Mount{
				Source:      path,
				Destination: m.Destination,
				Writable:    m.RW,
				Propagation: string(m.Propagation),
			}
			if m.Spec.Type == mounttypes.TypeBind && m.Spec.BindOptions != nil {
				mnt.NonRecursive = m.Spec.BindOptions.NonRecursive
			}
			if m.Volume != nil {
				attributes := map[string]string{
					"driver":      m.Volume.DriverName(),
					"container":   c.ID,
					"destination": m.Destination,
					"read/write":  strconv.FormatBool(m.RW),
					"propagation": string(m.Propagation),
				}
				daemon.LogVolumeEvent(m.Volume.Name(), "mount", attributes)
			}
			mounts = append(mounts, mnt)
		}
	}

	mounts = sortMounts(mounts)
	netMounts := c.NetworkMounts()
	// if we are going to mount any of the network files from container
	// metadata, the ownership must be set properly for potential container
	// remapped root (user namespaces)
	rootIDs := daemon.idMapping.RootPair()
	for _, mount := range netMounts {
		// we should only modify ownership of network files within our own container
		// metadata repository. If the user specifies a mount path external, it is
		// up to the user to make sure the file has proper ownership for userns
		if strings.Index(mount.Source, daemon.repository) == 0 {
			if err := os.Chown(mount.Source, rootIDs.UID, rootIDs.GID); err != nil {
				return nil, err
			}
		}
	}
	return append(mounts, netMounts...), nil
}

// sortMounts sorts an array of mounts in lexicographic order. This ensure that
// when mounting, the mounts don't shadow other mounts. For example, if mounting
// /etc and /etc/resolv.conf, /etc/resolv.conf must not be mounted first.
func sortMounts(m []container.Mount) []container.Mount {
	sort.Sort(mounts(m))
	return m
}

// setBindModeIfNull is platform specific processing to ensure the
// shared mode is set to 'z' if it is null. This is called in the case
// of processing a named volume and not a typical bind.
func setBindModeIfNull(bind *volumemounts.MountPoint) {
	if bind.Mode == "" {
		bind.Mode = "z"
	}
}

func (daemon *Daemon) mountVolumes(container *container.Container) error {
	mounts, err := daemon.setupMounts(container)
	if err != nil {
		return err
	}

	for _, m := range mounts {
		dest, err := container.GetResourcePath(m.Destination)
		if err != nil {
			return err
		}

		var stat os.FileInfo
		stat, err = os.Stat(m.Source)
		if err != nil {
			return err
		}
		if err = fileutils.CreateIfNotExists(dest, stat.IsDir()); err != nil {
			return err
		}

		bindMode := "rbind"
		if m.NonRecursive {
			bindMode = "bind"
		}
		writeMode := "ro"
		if m.Writable {
			writeMode = "rw"
		}

		// mountVolumes() seems to be called for temporary mounts
		// outside the container. Soon these will be unmounted with
		// lazy unmount option and given we have mounted the rbind,
		// all the submounts will propagate if these are shared. If
		// daemon is running in host namespace and has / as shared
		// then these unmounts will propagate and unmount original
		// mount as well. So make all these mounts rprivate.
		// Do not use propagation property of volume as that should
		// apply only when mounting happens inside the container.
		opts := strings.Join([]string{bindMode, writeMode, "rprivate"}, ",")
		if err := mount.Mount(m.Source, dest, "", opts); err != nil {
			return err
		}
	}

	return nil
}
