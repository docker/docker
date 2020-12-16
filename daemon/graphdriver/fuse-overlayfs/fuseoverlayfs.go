// +build linux

package fuseoverlayfs // import "github.com/docker/docker/daemon/graphdriver/fuse-overlayfs"

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/sys"
	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/daemon/graphdriver/overlayutils"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/chrootarchive"
	"github.com/docker/docker/pkg/containerfs"
	"github.com/docker/docker/pkg/directory"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/parsers/kernel"
	"github.com/docker/docker/pkg/system"
	"github.com/moby/locker"
	"github.com/moby/sys/mount"
	"github.com/opencontainers/selinux/go-selinux/label"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

var (
	// untar defines the untar method
	untar = chrootarchive.UntarUncompressed
)

const (
	driverName    = "fuse-overlayfs"
	binary        = "fuse-overlayfs"
	linkDir       = "l"
	diffDirName   = "diff"
	workDirName   = "work"
	mergedDirName = "merged"
	lowerFile     = "lower"
	maxDepth      = 128

	// idLength represents the number of random characters
	// which can be used to create the unique link identifier
	// for every layer. If this value is too long then the
	// page size limit for the mount command may be exceeded.
	// The idLength should be selected such that following equation
	// is true (512 is a buffer for label metadata).
	// ((idLength + len(linkDir) + 1) * maxDepth) <= (pageSize - 512)
	idLength = 26
)

// Driver contains information about the home directory and the list of active
// mounts that are created using this driver.
type Driver struct {
	home      string
	uidMaps   []idtools.IDMap
	gidMaps   []idtools.IDMap
	ctr       *graphdriver.RefCounter
	naiveDiff graphdriver.DiffDriver
	locker    *locker.Locker
}

var (
	logger = logrus.WithField("storage-driver", driverName)
)

func init() {
	graphdriver.Register(driverName, Init)
}

// Init returns the native diff driver for overlay filesystem.
// If overlay filesystem is not supported on the host, the error
// graphdriver.ErrNotSupported is returned.
// If an overlay filesystem is not supported over an existing filesystem then
// the error graphdriver.ErrIncompatibleFS is returned.
func Init(home string, options []string, uidMaps, gidMaps []idtools.IDMap, _ *graphdriver.InitOptions) (graphdriver.Driver, error) {
	if _, err := exec.LookPath(binary); err != nil {
		logger.Error(err)
		return nil, graphdriver.ErrNotSupported
	}
	if !kernel.CheckKernelVersion(4, 18, 0) {
		return nil, graphdriver.ErrNotSupported
	}

	rootUID, rootGID, err := idtools.GetRootUIDGID(uidMaps, gidMaps)
	if err != nil {
		return nil, err
	}
	// Create the driver home dir
	if err := idtools.MkdirAllAndChown(path.Join(home, linkDir), 0700, idtools.Identity{UID: rootUID, GID: rootGID}); err != nil {
		return nil, err
	}

	d := &Driver{
		home:    home,
		uidMaps: uidMaps,
		gidMaps: gidMaps,
		ctr:     graphdriver.NewRefCounter(graphdriver.NewFsChecker(graphdriver.FsMagicFUSE)),
		locker:  locker.New(),
	}

	d.naiveDiff = graphdriver.NewNaiveDiffDriver(d, uidMaps, gidMaps)

	return d, nil
}

func (d *Driver) String() string {
	return driverName
}

// Status returns current driver information in a two dimensional string array.
// Output contains "Backing Filesystem" used in this implementation.
func (d *Driver) Status() [][2]string {
	return [][2]string{}
}

// GetMetadata returns metadata about the overlay driver such as the LowerDir,
// UpperDir, WorkDir, and MergeDir used to store data.
func (d *Driver) GetMetadata(id string) (map[string]string, error) {
	dir := d.dir(id)
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}

	metadata := map[string]string{
		"WorkDir":   path.Join(dir, workDirName),
		"MergedDir": path.Join(dir, mergedDirName),
		"UpperDir":  path.Join(dir, diffDirName),
	}

	lowerDirs, err := d.getLowerDirs(id)
	if err != nil {
		return nil, err
	}
	if len(lowerDirs) > 0 {
		metadata["LowerDir"] = strings.Join(lowerDirs, ":")
	}

	return metadata, nil
}

// Cleanup any state created by overlay which should be cleaned when daemon
// is being shutdown. For now, we just have to unmount the bind mounted
// we had created.
func (d *Driver) Cleanup() error {
	return mount.RecursiveUnmount(d.home)
}

// CreateReadWrite creates a layer that is writable for use as a container
// file system.
func (d *Driver) CreateReadWrite(id, parent string, opts *graphdriver.CreateOpts) error {
	if opts != nil && len(opts.StorageOpt) != 0 {
		return fmt.Errorf("--storage-opt is not supported")
	}
	return d.create(id, parent, opts)
}

// Create is used to create the upper, lower, and merge directories required for overlay fs for a given id.
// The parent filesystem is used to configure these directories for the overlay.
func (d *Driver) Create(id, parent string, opts *graphdriver.CreateOpts) (retErr error) {
	if opts != nil && len(opts.StorageOpt) != 0 {
		return fmt.Errorf("--storage-opt is not supported")
	}
	return d.create(id, parent, opts)
}

func (d *Driver) create(id, parent string, opts *graphdriver.CreateOpts) (retErr error) {
	dir := d.dir(id)

	rootUID, rootGID, err := idtools.GetRootUIDGID(d.uidMaps, d.gidMaps)
	if err != nil {
		return err
	}
	root := idtools.Identity{UID: rootUID, GID: rootGID}

	if err := idtools.MkdirAllAndChown(path.Dir(dir), 0700, root); err != nil {
		return err
	}
	if err := idtools.MkdirAndChown(dir, 0700, root); err != nil {
		return err
	}

	defer func() {
		// Clean up on failure
		if retErr != nil {
			os.RemoveAll(dir)
		}
	}()

	if opts != nil && len(opts.StorageOpt) > 0 {
		return fmt.Errorf("--storage-opt is not supported")
	}

	if err := idtools.MkdirAndChown(path.Join(dir, diffDirName), 0755, root); err != nil {
		return err
	}

	lid := overlayutils.GenerateID(idLength, logger)
	if err := os.Symlink(path.Join("..", id, diffDirName), path.Join(d.home, linkDir, lid)); err != nil {
		return err
	}

	// Write link id to link file
	if err := ioutil.WriteFile(path.Join(dir, "link"), []byte(lid), 0644); err != nil {
		return err
	}

	// if no parent directory, done
	if parent == "" {
		return nil
	}

	if err := idtools.MkdirAndChown(path.Join(dir, workDirName), 0700, root); err != nil {
		return err
	}

	if err := ioutil.WriteFile(path.Join(d.dir(parent), "committed"), []byte{}, 0600); err != nil {
		return err
	}

	lower, err := d.getLower(parent)
	if err != nil {
		return err
	}
	if lower != "" {
		if err := ioutil.WriteFile(path.Join(dir, lowerFile), []byte(lower), 0666); err != nil {
			return err
		}
	}

	return nil
}

func (d *Driver) getLower(parent string) (string, error) {
	parentDir := d.dir(parent)

	// Ensure parent exists
	if _, err := os.Lstat(parentDir); err != nil {
		return "", err
	}

	// Read Parent link fileA
	parentLink, err := ioutil.ReadFile(path.Join(parentDir, "link"))
	if err != nil {
		return "", err
	}
	lowers := []string{path.Join(linkDir, string(parentLink))}

	parentLower, err := ioutil.ReadFile(path.Join(parentDir, lowerFile))
	if err == nil {
		parentLowers := strings.Split(string(parentLower), ":")
		lowers = append(lowers, parentLowers...)
	}
	if len(lowers) > maxDepth {
		return "", errors.New("max depth exceeded")
	}
	return strings.Join(lowers, ":"), nil
}

func (d *Driver) dir(id string) string {
	return path.Join(d.home, id)
}

func (d *Driver) getLowerDirs(id string) ([]string, error) {
	var lowersArray []string
	lowers, err := ioutil.ReadFile(path.Join(d.dir(id), lowerFile))
	if err == nil {
		for _, s := range strings.Split(string(lowers), ":") {
			lp, err := os.Readlink(path.Join(d.home, s))
			if err != nil {
				return nil, err
			}
			lowersArray = append(lowersArray, path.Clean(path.Join(d.home, linkDir, lp)))
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return lowersArray, nil
}

// Remove cleans the directories that are created for this id.
func (d *Driver) Remove(id string) error {
	if id == "" {
		return fmt.Errorf("refusing to remove the directories: id is empty")
	}
	d.locker.Lock(id)
	defer d.locker.Unlock(id)
	dir := d.dir(id)
	lid, err := ioutil.ReadFile(path.Join(dir, "link"))
	if err == nil {
		if len(lid) == 0 {
			logger.Errorf("refusing to remove empty link for layer %v", id)
		} else if err := os.RemoveAll(path.Join(d.home, linkDir, string(lid))); err != nil {
			logger.Debugf("Failed to remove link: %v", err)
		}
	}

	if err := system.EnsureRemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Get creates and mounts the required file system for the given id and returns the mount path.
func (d *Driver) Get(id, mountLabel string) (_ containerfs.ContainerFS, retErr error) {
	d.locker.Lock(id)
	defer d.locker.Unlock(id)
	dir := d.dir(id)
	if _, err := os.Stat(dir); err != nil {
		return nil, err
	}

	diffDir := path.Join(dir, diffDirName)
	lowers, err := ioutil.ReadFile(path.Join(dir, lowerFile))
	if err != nil {
		// If no lower, just return diff directory
		if os.IsNotExist(err) {
			return containerfs.NewLocalContainerFS(diffDir), nil
		}
		return nil, err
	}

	mergedDir := path.Join(dir, mergedDirName)
	if count := d.ctr.Increment(mergedDir); count > 1 {
		return containerfs.NewLocalContainerFS(mergedDir), nil
	}
	defer func() {
		if retErr != nil {
			if c := d.ctr.Decrement(mergedDir); c <= 0 {
				if unmounted := fusermountU(mergedDir); !unmounted {
					if mntErr := unix.Unmount(mergedDir, 0); mntErr != nil {
						logger.Errorf("error unmounting %v: %v", mergedDir, mntErr)
					}
				}
				// Cleanup the created merged directory; see the comment in Put's rmdir
				if rmErr := unix.Rmdir(mergedDir); rmErr != nil && !os.IsNotExist(rmErr) {
					logger.Debugf("Failed to remove %s: %v: %v", id, rmErr, err)
				}
			}
		}
	}()

	workDir := path.Join(dir, workDirName)
	splitLowers := strings.Split(string(lowers), ":")
	absLowers := make([]string, len(splitLowers))
	for i, s := range splitLowers {
		absLowers[i] = path.Join(d.home, s)
	}
	var readonly bool
	if _, err := os.Stat(path.Join(dir, "committed")); err == nil {
		readonly = true
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	var opts string
	if readonly {
		opts = "lowerdir=" + diffDir + ":" + strings.Join(absLowers, ":")
	} else {
		opts = "lowerdir=" + strings.Join(absLowers, ":") + ",upperdir=" + diffDir + ",workdir=" + workDir
	}

	mountData := label.FormatMountLabel(opts, mountLabel)
	mountTarget := mergedDir

	rootUID, rootGID, err := idtools.GetRootUIDGID(d.uidMaps, d.gidMaps)
	if err != nil {
		return nil, err
	}
	if err := idtools.MkdirAndChown(mergedDir, 0700, idtools.Identity{UID: rootUID, GID: rootGID}); err != nil {
		return nil, err
	}

	mountProgram := exec.Command(binary, "-o", mountData, mountTarget)
	mountProgram.Dir = d.home
	var b bytes.Buffer
	mountProgram.Stderr = &b
	if err = mountProgram.Run(); err != nil {
		output := b.String()
		if output == "" {
			output = "<stderr empty>"
		}
		return nil, errors.Wrapf(err, "using mount program %s: %s", binary, output)
	}

	return containerfs.NewLocalContainerFS(mergedDir), nil
}

// Put unmounts the mount path created for the give id.
// It also removes the 'merged' directory to force the kernel to unmount the
// overlay mount in other namespaces.
func (d *Driver) Put(id string) error {
	d.locker.Lock(id)
	defer d.locker.Unlock(id)
	dir := d.dir(id)
	_, err := ioutil.ReadFile(path.Join(dir, lowerFile))
	if err != nil {
		// If no lower, no mount happened and just return directly
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	mountpoint := path.Join(dir, mergedDirName)
	if count := d.ctr.Decrement(mountpoint); count > 0 {
		return nil
	}
	if unmounted := fusermountU(mountpoint); !unmounted {
		if err := unix.Unmount(mountpoint, unix.MNT_DETACH); err != nil {
			logger.Debugf("Failed to unmount %s overlay: %s - %v", id, mountpoint, err)
		}
	}
	// Remove the mountpoint here. Removing the mountpoint (in newer kernels)
	// will cause all other instances of this mount in other mount namespaces
	// to be unmounted. This is necessary to avoid cases where an overlay mount
	// that is present in another namespace will cause subsequent mounts
	// operations to fail with ebusy.  We ignore any errors here because this may
	// fail on older kernels which don't have
	// torvalds/linux@8ed936b5671bfb33d89bc60bdcc7cf0470ba52fe applied.
	if err := unix.Rmdir(mountpoint); err != nil && !os.IsNotExist(err) {
		logger.Debugf("Failed to remove %s overlay: %v", id, err)
	}
	return nil
}

// Exists checks to see if the id is already mounted.
func (d *Driver) Exists(id string) bool {
	_, err := os.Stat(d.dir(id))
	return err == nil
}

// isParent determines whether the given parent is the direct parent of the
// given layer id
func (d *Driver) isParent(id, parent string) bool {
	lowers, err := d.getLowerDirs(id)
	if err != nil {
		return false
	}
	if parent == "" && len(lowers) > 0 {
		return false
	}

	parentDir := d.dir(parent)
	var ld string
	if len(lowers) > 0 {
		ld = filepath.Dir(lowers[0])
	}
	if ld == "" && parent == "" {
		return true
	}
	return ld == parentDir
}

// ApplyDiff applies the new layer into a root
func (d *Driver) ApplyDiff(id string, parent string, diff io.Reader) (size int64, err error) {
	if !d.isParent(id, parent) {
		return d.naiveDiff.ApplyDiff(id, parent, diff)
	}

	applyDir := d.getDiffPath(id)

	logger.Debugf("Applying tar in %s", applyDir)
	// Overlay doesn't need the parent id to apply the diff
	if err := untar(diff, applyDir, &archive.TarOptions{
		UIDMaps: d.uidMaps,
		GIDMaps: d.gidMaps,
		// Use AUFS whiteout format: https://github.com/containers/storage/blob/39a8d5ed9843844eafb5d2ba6e6a7510e0126f40/drivers/overlay/overlay.go#L1084-L1089
		WhiteoutFormat: archive.AUFSWhiteoutFormat,
		InUserNS:       sys.RunningInUserNS(),
	}); err != nil {
		return 0, err
	}

	return directory.Size(context.TODO(), applyDir)
}

func (d *Driver) getDiffPath(id string) string {
	dir := d.dir(id)

	return path.Join(dir, diffDirName)
}

// DiffSize calculates the changes between the specified id
// and its parent and returns the size in bytes of the changes
// relative to its base filesystem directory.
func (d *Driver) DiffSize(id, parent string) (size int64, err error) {
	return d.naiveDiff.DiffSize(id, parent)
}

// Diff produces an archive of the changes between the specified
// layer and its parent layer which may be "".
func (d *Driver) Diff(id, parent string) (io.ReadCloser, error) {
	return d.naiveDiff.Diff(id, parent)
}

// Changes produces a list of changes between the specified layer and its
// parent layer. If parent is "", then all changes will be ADD changes.
func (d *Driver) Changes(id, parent string) ([]archive.Change, error) {
	return d.naiveDiff.Changes(id, parent)
}

// fusermountU is from https://github.com/containers/storage/blob/39a8d5ed9843844eafb5d2ba6e6a7510e0126f40/drivers/overlay/overlay.go#L1016-L1040
func fusermountU(mountpoint string) (unmounted bool) {
	// Attempt to unmount the FUSE mount using either fusermount or fusermount3.
	// If they fail, fallback to unix.Unmount
	for _, v := range []string{"fusermount3", "fusermount"} {
		err := exec.Command(v, "-u", mountpoint).Run()
		if err != nil && !os.IsNotExist(err) {
			logrus.Debugf("Error unmounting %s with %s - %v", mountpoint, v, err)
		}
		if err == nil {
			unmounted = true
			break
		}
	}
	// If fusermount|fusermount3 failed to unmount the FUSE file system, make sure all
	// pending changes are propagated to the file system
	if !unmounted {
		fd, err := unix.Open(mountpoint, unix.O_DIRECTORY, 0)
		if err == nil {
			if err := unix.Syncfs(fd); err != nil {
				logrus.Debugf("Error Syncfs(%s) - %v", mountpoint, err)
			}
			unix.Close(fd)
		}
	}
	return
}
