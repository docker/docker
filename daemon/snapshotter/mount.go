package snapshotter

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/log"
	"github.com/moby/locker"
	"github.com/moby/moby/v2/daemon/internal/mountref"
	"github.com/moby/sys/mountinfo"
	"github.com/moby/sys/user"
)

// Mounter handles mounting/unmounting things coming in from a snapshotter
// with optional reference counting if needed by the filesystem
type Mounter interface {
	// Mount mounts the rootfs for a container and returns the mount point
	Mount(mounts []mount.Mount, containerID string) (string, error)
	// Unmount unmounts the container rootfs
	Unmount(target string) error
	// Mounted returns a target mountpoint if it's already mounted
	Mounted(containerID string) (string, error)
}

// NewMounter creates a new mounter for the provided snapshotter
func NewMounter(home string, snapshotter string, idMap user.IdentityMapping) *refCountMounter {
	return &refCountMounter{
		base: mounter{
			home:        home,
			snapshotter: snapshotter,
			idMap:       idMap,
		},
		rc:     mountref.NewCounter(isMounted),
		locker: locker.New(),
	}
}

type refCountMounter struct {
	rc     *mountref.Counter
	locker *locker.Locker
	base   mounter
}

func (m *refCountMounter) Mount(mounts []mount.Mount, containerID string) (target string, retErr error) {
	target = m.base.target(containerID)

	_, err := os.Stat(target)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}

	if count := m.rc.Increment(target); count > 1 {
		return target, nil
	}

	m.locker.Lock(target)
	defer m.locker.Unlock(target)

	defer func() {
		if retErr != nil {
			if c := m.rc.Decrement(target); c <= 0 {
				if mntErr := unmount(target); mntErr != nil {
					log.G(context.TODO()).Errorf("error unmounting %s: %v", target, mntErr)
				}
				if rmErr := os.Remove(target); rmErr != nil && !os.IsNotExist(rmErr) {
					log.G(context.TODO()).Debugf("Failed to remove %s: %v: %v", target, rmErr, err)
				}
			}
		}
	}()

	return m.base.Mount(mounts, containerID)
}

func (m *refCountMounter) Unmount(target string) error {
	if count := m.rc.Decrement(target); count > 0 {
		return nil
	}

	m.locker.Lock(target)
	defer m.locker.Unlock(target)

	if err := unmount(target); err != nil {
		log.G(context.TODO()).Debugf("Failed to unmount %s: %v", target, err)
	}

	if err := os.Remove(target); err != nil {
		log.G(context.TODO()).WithError(err).WithField("dir", target).Error("failed to remove mount temp dir")
	}

	return nil
}

func (m *refCountMounter) Mounted(containerID string) (string, error) {
	mounted, err := m.base.Mounted(containerID)
	if err != nil || mounted == "" {
		return mounted, err
	}

	target := m.base.target(containerID)

	// Check if the refcount is non-zero.
	m.rc.Increment(target)
	if m.rc.Decrement(target) > 0 {
		return mounted, nil
	}

	return "", nil
}

type mounter struct {
	home        string
	snapshotter string
	idMap       user.IdentityMapping
}

func (m mounter) Mount(mounts []mount.Mount, containerID string) (string, error) {
	target := m.target(containerID)

	uid, gid := m.idMap.RootPair()
	if err := user.MkdirAllAndChown(filepath.Dir(target), 0o710, os.Getuid(), gid); err != nil {
		return "", err
	}
	if err := user.MkdirAllAndChown(target, 0o710, uid, gid); err != nil {
		return "", err
	}

	for _, mount := range mounts {
		if slices.Contains(mount.Options, "volatile") {
			if workDirIndex := slices.IndexFunc(mount.Options, func(s string) bool {
				return strings.HasPrefix(s, "workdir=")
			}); workDirIndex >= 0 {
				workDir := strings.TrimPrefix(mount.Options[workDirIndex], "workdir=")
				// overlay has a check in place to prevent mounting "volatile" system twice
				volatileIncompatFile := path.Join(workDir, "work", "incompat", "volatile")
				_ = os.RemoveAll(volatileIncompatFile)
			}
		}
	}
	return target, mount.All(mounts, target)
}

func (m mounter) Unmount(target string) error {
	return unmount(target)
}

func (m mounter) Mounted(containerID string) (string, error) {
	target := m.target(containerID)

	mounted, err := mountinfo.Mounted(target)
	if err != nil || !mounted {
		return "", err
	}
	return target, nil
}

func (m mounter) target(containerID string) string {
	return filepath.Join(m.home, "rootfs", m.snapshotter, containerID)
}
