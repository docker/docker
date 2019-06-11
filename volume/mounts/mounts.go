package mounts // import "github.com/docker/docker/volume/mounts"

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	mounttypes "github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/volume"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
)

// MountPoint is the intersection point between a volume and a container. It
// specifies which volume is to be used and where inside a container it should
// be mounted.
//
// Note that this type is embedded in `container.Container` object and persisted to disk.
// Changes to this struct need to by synced with on disk state.
type MountPoint struct {
	// Source is the source path of the mount.
	// E.g. `mount --bind /foo /bar`, `/foo` is the `Source`.
	Source string
	// Destination is the path relative to the container root (`/`) to the mount point
	// It is where the `Source` is mounted to
	Destination string
	// RW is set to true when the mountpoint should be mounted as read-write
	RW bool
	// Name is the name reference to the underlying data defined by `Source`
	// e.g., the volume name
	Name string
	// Driver is the volume driver used to create the volume (if it is a volume)
	Driver string
	// Type of mount to use, see `Type<foo>` definitions in github.com/docker/docker/api/types/mount
	Type mounttypes.Type `json:",omitempty"`
	// Volume is the volume providing data to this mountpoint.
	// This is nil unless `Type` is set to `TypeVolume`
	Volume volume.Volume `json:"-"`

	// Mode is the comma separated list of options supplied by the user when creating
	// the bind/volume mount.
	// Note Mode is not used on Windows
	Mode string `json:"Relabel,omitempty"` // Originally field was `Relabel`"

	// Propagation describes how the mounts are propagated from the host into the
	// mount point, and vice-versa.
	// See https://www.kernel.org/doc/Documentation/filesystems/sharedsubtree.txt
	// Note Propagation is not used on Windows
	Propagation mounttypes.Propagation `json:",omitempty"` // Mount propagation string

	// Specifies if data should be copied from the container before the first mount
	// Use a pointer here so we can tell if the user set this value explicitly
	// This allows us to error out when the user explicitly enabled copy but we can't copy due to the volume being populated
	CopyData bool `json:"-"`
	// ID is the opaque ID used to pass to the volume driver.
	// This should be set by calls to `Mount` and unset by calls to `Unmount`
	ID string `json:",omitempty"`

	// Sepc is a copy of the API request that created this mount.
	Spec mounttypes.Mount

	// Some bind mounts should not be automatically created.
	// (Some are auto-created for backwards-compatibility)
	// This is checked on the API but setting this here prevents race conditions.
	// where a bind dir existed during validation was removed before reaching the setup code.
	SkipMountpointCreation bool

	// SubPath specifies the requested sub-directory of the volume to be mounted
	// for volumes that allows mounting a sub-directory.
	SubPath string

	// Track usage of this mountpoint
	// Specifically needed for containers which are running and calls to `docker cp`
	// because both these actions require mounting the volumes.
	active int

	// For volumes mounted with a subpath, track the full mounted path.
	mountedSubPath string
}

// Cleanup frees resources used by the mountpoint
func (m *MountPoint) Cleanup() error {
	if m.Volume == nil || m.ID == "" {
		return nil
	}

	if err := m.Volume.Unmount(m.ID); err != nil {
		return errors.Wrapf(err, "error unmounting volume %s", m.Volume.Name())
	}

	m.active--
	if m.active == 0 {
		m.ID = ""
		m.mountedSubPath = ""
	}
	return nil
}

// Setup sets up a mount point by either mounting the volume if it is
// configured, or creating the source directory if supplied.
// The, optional, checkFun parameter allows doing additional checking
// before creating the source directory on the host.
func (m *MountPoint) Setup(mountLabel string, rootIDs idtools.Identity, checkFun func(m *MountPoint) error) (path string, err error) {
	if m.SkipMountpointCreation {
		return m.Source, nil
	}

	defer func() {
		if err != nil || !label.RelabelNeeded(m.Mode) {
			return
		}

		var sourcePath string
		sourcePath, err = filepath.EvalSymlinks(m.Source)
		if err != nil {
			path = ""
			err = errors.Wrapf(err, "error evaluating symlinks from mount source %q", m.Source)
			return
		}
		err = label.Relabel(sourcePath, mountLabel, label.IsShared(m.Mode))
		if err == syscall.ENOTSUP {
			err = nil
		}
		if err != nil {
			path = ""
			err = errors.Wrapf(err, "error setting label on mount source '%s'", sourcePath)
		}
	}()

	if m.Volume != nil {
		return m.mountVolume()
	}

	if len(m.Source) == 0 {
		return "", fmt.Errorf("Unable to setup mount point, neither source nor volume defined")
	}

	if m.Type == mounttypes.TypeBind {
		// Before creating the source directory on the host, invoke checkFun if it's not nil. One of
		// the use case is to forbid creating the daemon socket as a directory if the daemon is in
		// the process of shutting down.
		if checkFun != nil {
			if err := checkFun(m); err != nil {
				return "", err
			}
		}

		// idtools.MkdirAllNewAs() produces an error if m.Source exists and is a file (not a directory)
		// also, makes sure that if the directory is created, the correct remapped rootUID/rootGID will own it
		if err := idtools.MkdirAllAndChownNew(m.Source, 0755, rootIDs); err != nil {
			if perr, ok := err.(*os.PathError); ok {
				if perr.Err != syscall.ENOTDIR {
					return "", errors.Wrapf(err, "error while creating mount source path '%s'", m.Source)
				}
			}
		}
	}
	return m.Source, nil
}

// mountVolume mounts a volume or a sub-path of it.
func (m *MountPoint) mountVolume() (string, error) {
	id := m.ID
	if id == "" {
		id = stringid.GenerateNonCryptoID()
	}

	path, err := m.Volume.Mount(id)
	if err != nil {
		return "", errors.Wrapf(err, "error while mounting volume '%s'", m.Source)
	}

	if m.SubPath != "" {
		path, err = m.mountVolumeSubPath(path)
		if err != nil {
			return "", err
		}
	}

	m.ID = id
	m.active++
	return path, nil
}

func (m *MountPoint) mountVolumeSubPath(volumePath string) (string, error) {
	realPath, err := filepath.EvalSymlinks(filepath.Join(volumePath, m.SubPath))
	if err != nil {
		if os.IsNotExist(err) {
			return "", errors.Wrapf(err, "directory %s does not exist under volume path: %s", m.SubPath, volumePath)
		}
		return "", errors.Wrapf(err, "error while parsing symlinks for volume path: %s, sub-path: %s", volumePath, m.SubPath)
	}

	fi, err := os.Stat(realPath)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", errors.Errorf("%s is not a directory", realPath)
	}

	if !strings.HasPrefix(realPath, volumePath) {
		return "", errors.Errorf("directory %s outside of the volume root %s", realPath, volumePath)
	}

	m.mountedSubPath = realPath
	return realPath, nil
}

// Path returns the path of a volume in a mount point.
func (m *MountPoint) Path() string {
	if m.Volume != nil {
		if m.mountedSubPath != "" {
			return m.mountedSubPath
		}
		return m.Volume.Path()
	}
	return m.Source
}

func errInvalidMode(mode string) error {
	return errors.Errorf("invalid mode: %v", mode)
}

func errInvalidSpec(spec string) error {
	return errors.Errorf("invalid volume specification: '%s'", spec)
}
