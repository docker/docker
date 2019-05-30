package vfs // import "github.com/docker/docker/daemon/graphdriver/vfs"

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/daemon/graphdriver/quota"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/containerfs"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/parsers"
	"github.com/docker/docker/pkg/system"
	"github.com/docker/go-units"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
)

var (
	// CopyDir defines the copy method to use.
	CopyDir = dirCopy
)

func init() {
	graphdriver.Register("vfs", Init)
}

// Init returns a new VFS driver.
// This sets the home directory for the driver and returns NaiveDiffDriver.
func Init(home string, options []string, uidMaps, gidMaps []idtools.IDMap) (graphdriver.Driver, error) {
	d := &Driver{
		home:      home,
		idMapping: idtools.NewIDMappingsFromMaps(uidMaps, gidMaps),
	}

	if err := d.parseOptions(options); err != nil {
		return nil, err
	}

	rootIDs := d.idMapping.RootPair()
	if err := idtools.MkdirAllAndChown(home, 0700, rootIDs); err != nil {
		return nil, err
	}

	setupDriverQuota(d)

	if size := d.getQuotaOpt(); !d.quotaSupported() && size > 0 {
		return nil, quota.ErrQuotaNotSupported
	}

	return graphdriver.NewNaiveDiffDriver(d, uidMaps, gidMaps), nil
}

// Driver holds information about the driver, home directory of the driver.
// Driver implements graphdriver.ProtoDriver. It uses only basic vfs operations.
// In order to support layering, files are copied from the parent layer into the new layer. There is no copy-on-write support.
// Driver must be wrapped in NaiveDiffDriver to be used as a graphdriver.Driver
type Driver struct {
	driverQuota
	home      string
	idMapping *idtools.IdentityMapping
}

func (d *Driver) String() string {
	return "vfs"
}

// Status is used for implementing the graphdriver.ProtoDriver interface. VFS does not currently have any status information.
func (d *Driver) Status() [][2]string {
	return nil
}

// GetMetadata is used for implementing the graphdriver.ProtoDriver interface. VFS does not currently have any meta data.
func (d *Driver) GetMetadata(id string) (map[string]string, error) {
	return nil, nil
}

// Cleanup is used to implement graphdriver.ProtoDriver. There is no cleanup required for this driver.
func (d *Driver) Cleanup() error {
	return nil
}

func (d *Driver) parseOptions(options []string) error {
	for _, option := range options {
		key, val, err := parsers.ParseKeyValueOpt(option)
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
		return fmt.Errorf("--storage-opt is not supported for vfs on read-only layers")
	}

	return d.create(id, parent, 0)
}

func (d *Driver) create(id, parent string, size uint64) error {
	dir := d.dir(id)
	rootIDs := d.idMapping.RootPair()
	if err := idtools.MkdirAllAndChown(filepath.Dir(dir), 0700, rootIDs); err != nil {
		return err
	}
	if err := idtools.MkdirAndChown(dir, 0755, rootIDs); err != nil {
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
	return CopyDir(parentDir.Path(), dir)
}

func (d *Driver) dir(id string) string {
	return filepath.Join(d.home, "dir", filepath.Base(id))
}

// Remove deletes the content from the directory for a given id.
func (d *Driver) Remove(id string) error {
	return system.EnsureRemoveAll(d.dir(id))
}

// Get returns the directory for the given id.
func (d *Driver) Get(id, mountLabel string) (containerfs.ContainerFS, error) {
	dir := d.dir(id)
	if st, err := os.Stat(dir); err != nil {
		return nil, err
	} else if !st.IsDir() {
		return nil, fmt.Errorf("%s: not a directory", dir)
	}
	return containerfs.NewLocalContainerFS(dir), nil
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
