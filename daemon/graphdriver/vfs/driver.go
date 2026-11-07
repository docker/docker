package vfs

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/docker/go-units"
	"github.com/moby/moby/v2/daemon/graphdriver"
	"github.com/moby/moby/v2/daemon/internal/containerfs"
	"github.com/moby/moby/v2/daemon/internal/quota"
	"github.com/moby/moby/v2/errdefs"
	"github.com/moby/sys/user"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
)

const (
	xattrsStorageOpt         = "vfs.xattrs"
	bestEffortXattrsOptValue = "i_want_broken_containers"
)

// CopyDir defines the copy method to use.
var CopyDir = dirCopy

func init() {
	graphdriver.Register("vfs", Init)
}

// Init returns a new VFS driver.
// This sets the home directory for the driver and returns NaiveDiffDriver.
func Init(home string, options []string, idMap user.IdentityMapping) (graphdriver.Driver, error) {
	d := &Driver{
		home:      home,
		idMapping: idMap,
	}

	if err := d.parseOptions(options); err != nil {
		return nil, err
	}

	_, gid := d.idMapping.RootPair()
	if err := user.MkdirAndChown(home, 0o710, os.Getuid(), gid); err != nil {
		return nil, err
	}
	if err := user.MkdirAndChown(filepath.Join(home, "dir"), 0o710, os.Getuid(), gid); err != nil {
		return nil, err
	}

	setupDriverQuota(d)

	if size := d.getQuotaOpt(); !d.quotaSupported() && size > 0 {
		return nil, quota.ErrQuotaNotSupported
	}

	return &graphdriver.NaiveDiffDriver{
		ProtoDriver:      d,
		IDMap:            d.idMapping,
		BestEffortXattrs: d.bestEffortXattrs,
	}, nil
}

// Driver holds information about the driver, home directory of the driver.
// Driver implements graphdriver.ProtoDriver. It uses only basic vfs operations.
// In order to support layering, files are copied from the parent layer into the new layer. There is no copy-on-write support.
// Driver must be wrapped in NaiveDiffDriver to be used as a graphdriver.Driver
type Driver struct {
	driverQuota
	home             string
	idMapping        user.IdentityMapping
	bestEffortXattrs bool
}

func (d *Driver) String() string {
	return "vfs"
}

// Status is used for implementing the graphdriver.ProtoDriver interface.
func (d *Driver) Status() [][2]string {
	if d.bestEffortXattrs {
		return [][2]string{
			// These strings are looked for in daemon/info_unix.go:fillDriverWarnings()
			// because plumbing is hard and temporary is forever. Forgive me.
			{"Extended file attributes", "best-effort"},
		}
	}
	return nil
}

// GetMetadata is used for implementing the graphdriver.ProtoDriver interface. VFS does not currently have any meta data.
func (d *Driver) GetMetadata(id string) (map[string]string, error) {
	dir := d.dir(id)
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}

	metadata := map[string]string{
		"SourceDir": dir,
	}

	return metadata, nil
}

// Cleanup is used to implement graphdriver.ProtoDriver. There is no cleanup required for this driver.
func (d *Driver) Cleanup() error {
	return nil
}

func (d *Driver) parseOptions(options []string) error {
	for _, option := range options {
		key, val, err := graphdriver.ParseStorageOptKeyValue(option)
		if err != nil {
			return errdefs.InvalidParameter(err)
		}
		switch key {
		case "size":
			size, err := units.RAMInBytes(val)
			if err != nil {
				return errdefs.InvalidParameter(err)
			}
			if err = d.setQuotaOpt(uint64(size)); err != nil {
				return errdefs.InvalidParameter(errors.Wrap(err, "failed to set option size for vfs"))
			}
		case xattrsStorageOpt:
			if val != bestEffortXattrsOptValue {
				return errdefs.InvalidParameter(errors.Errorf("do not set the " + xattrsStorageOpt + " option unless you are willing to accept the consequences"))
			}
			d.bestEffortXattrs = true
		default:
			return errdefs.InvalidParameter(errors.Errorf("unknown option %s for vfs", key))
		}
	}
	return nil
}

// CreateReadWrite creates a layer that is writable for use as a container
// file system.
func (d *Driver) CreateReadWrite(id, parent string, opts *graphdriver.CreateOpts) error {
	quotaSize := d.getQuotaOpt()

	if opts != nil {
		for key, val := range opts.StorageOpt {
			switch key {
			case "size":
				if !d.quotaSupported() {
					return quota.ErrQuotaNotSupported
				}
				size, err := units.RAMInBytes(val)
				if err != nil {
					return errdefs.InvalidParameter(err)
				}
				quotaSize = uint64(size)
			default:
				return errdefs.InvalidParameter(errors.Errorf("Storage opt %s not supported", key))
			}
		}
	}

	return d.create(id, parent, quotaSize)
}

// Create prepares the filesystem for the VFS driver and copies the directory for the given id under the parent.
func (d *Driver) Create(id, parent string, opts *graphdriver.CreateOpts) error {
	if opts != nil && len(opts.StorageOpt) != 0 {
		return errors.New("--storage-opt is not supported for vfs on read-only layers")
	}

	return d.create(id, parent, 0)
}

func (d *Driver) create(id, parent string, size uint64) error {
	dir := d.dir(id)
	uid, gid := d.idMapping.RootPair()

	if err := user.MkdirAndChown(dir, 0o755, uid, gid); err != nil {
		return err
	}

	if size != 0 {
		if err := d.setupQuota(dir, size); err != nil {
			return err
		}
	}

	labelOpts := []string{"level:s0"}
	if _, mountLabel, err := label.InitLabels(labelOpts); err == nil {
		label.SetFileLabel(dir, mountLabel)
	}
	if parent == "" {
		return nil
	}
	parentDir, err := d.Get(parent, "")
	if err != nil {
		return fmt.Errorf("%s: %s", parent, err)
	}
	return CopyDir(parentDir, dir)
}

func (d *Driver) dir(id string) string {
	return filepath.Join(d.home, "dir", filepath.Base(id))
}

// Remove deletes the content from the directory for a given id.
func (d *Driver) Remove(id string) error {
	dir := d.dir(id)
	if err := containerfs.EnsureRemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	d.removeQuota(dir)
	return nil
}

// Get returns the directory for the given id.
func (d *Driver) Get(id, mountLabel string) (string, error) {
	dir := d.dir(id)
	if st, err := os.Stat(dir); err != nil {
		return "", err
	} else if !st.IsDir() {
		return "", fmt.Errorf("%s: not a directory", dir)
	}
	return dir, nil
}

// Put is a noop for vfs that return nil for the error, since this driver has no runtime resources to clean up.
func (d *Driver) Put(id string) error {
	// The vfs driver has no runtime resources (e.g. mounts)
	// to clean up, so we don't need anything here
	return nil
}

// Exists checks to see if the directory exists for the given id.
func (d *Driver) Exists(id string) bool {
	_, err := os.Stat(d.dir(id))
	return err == nil
}
