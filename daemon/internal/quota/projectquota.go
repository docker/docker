//go:build linux && !exclude_disk_quota && cgo

//
// projectquota.go - implements XFS project quota controls
// for setting quota limits on a newly created directory.
// It currently supports the legacy XFS specific ioctls.
//
// TODO: use generic quota control ioctl FS_IOC_FS{GET,SET}XATTR
//       for both xfs/ext4 for kernel version >= v4.5
//

package quota

/*
#include <stdlib.h>
#include <dirent.h>
#include <linux/fs.h>
#include <linux/quota.h>
#include <linux/dqblk_xfs.h>

#ifndef FS_XFLAG_PROJINHERIT
struct fsxattr {
	__u32		fsx_xflags;
	__u32		fsx_extsize;
	__u32		fsx_nextents;
	__u32		fsx_projid;
	unsigned char	fsx_pad[12];
};
#define FS_XFLAG_PROJINHERIT	0x00000200
#endif
#ifndef FS_IOC_FSGETXATTR
#define FS_IOC_FSGETXATTR		_IOR ('X', 31, struct fsxattr)
#endif
#ifndef FS_IOC_FSSETXATTR
#define FS_IOC_FSSETXATTR		_IOW ('X', 32, struct fsxattr)
#endif

#ifndef PRJQUOTA
#define PRJQUOTA	2
#endif
#ifndef XFS_PROJ_QUOTA
#define XFS_PROJ_QUOTA	2
#endif
#ifndef Q_XSETPQLIM
#define Q_XSETPQLIM QCMD(Q_XSETQLIM, PRJQUOTA)
#endif
#ifndef Q_XGETPQUOTA
#define Q_XGETPQUOTA QCMD(Q_XGETQUOTA, PRJQUOTA)
#endif

const int Q_XGETQSTAT_PRJQUOTA = QCMD(Q_XGETQSTAT, PRJQUOTA);
*/
import "C"

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"sync"
	"unsafe"

	"github.com/containerd/log"
	"github.com/moby/sys/userns"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

type pquotaState struct {
	sync.Mutex
	nextProjectID   uint32
	freeProjectIDs  []uint32
	projectIDsInUse map[uint32]bool
}

var (
	pquotaStateInst *pquotaState
	pquotaStateOnce sync.Once
)

// getPquotaState - get global pquota state tracker instance
func getPquotaState() *pquotaState {
	pquotaStateOnce.Do(func() {
		pquotaStateInst = &pquotaState{
			nextProjectID:   1,
			freeProjectIDs:  []uint32{},
			projectIDsInUse: make(map[uint32]bool),
		}
	})
	return pquotaStateInst
}

// registerBasePath - register a new base path and update nextProjectID
func (state *pquotaState) updateMinProjID(minProjectID uint32) {
	state.Lock()
	defer state.Unlock()
	if state.nextProjectID <= minProjectID {
		state.nextProjectID = minProjectID + 1
	}
}

// NewControl - initialize project quota support.
// Test to make sure that quota can be set on a test dir and find
// the first project id to be used for the next container create.
//
// Returns nil (and error) if project quota is not supported.
//
// First get the project id of the home directory.
// This test will fail if the backing fs is not xfs.
//
// xfs_quota tool can be used to assign a project id to the driver home directory, e.g.:
//
//	echo 999:/var/lib/docker/overlay2 >> /etc/projects
//	echo docker:999 >> /etc/projid
//	xfs_quota -x -c 'project -s docker' /<xfs mount point>
//
// In that case, the home directory project id will be used as a "start offset"
// and all containers will be assigned larger project ids (e.g. >= 1000).
// This is a way to prevent xfs_quota management from conflicting with docker.
//
// Then try to create a test directory with the next project id and set a quota
// on it. If that works, continue to scan existing containers to map allocated
// project ids.
func NewControl(basePath string) (*Control, error) {
	//
	// If we are running in a user namespace quota won't be supported for
	// now since makeBackingFsDev() will try to mknod().
	//
	if userns.RunningInUserNS() {
		return nil, ErrQuotaNotSupported
	}

	//
	// create backing filesystem device node
	//
	backingFsBlockDev, err := makeBackingFsDev(basePath)
	if err != nil {
		return nil, err
	}

	// check if we can call quotactl with project quotas
	// as a mechanism to determine (early) if we have support
	hasQuotaSupport, err := hasQuotaSupport(backingFsBlockDev)
	if err != nil {
		return nil, err
	}
	if !hasQuotaSupport {
		return nil, ErrQuotaNotSupported
	}

	//
	// Get project id of parent dir as minimal id to be used by driver
	//
	baseProjectID, err := getProjectID(basePath)
	if err != nil {
		return nil, err
	}
	minProjectID := baseProjectID + 1

	//
	// Test if filesystem supports project quotas by trying to set
	// a quota on the first available project id
	//
	quota := Quota{
		Size: 0,
	}
	if err := setProjectQuota(backingFsBlockDev, minProjectID, quota); err != nil {
		return nil, err
	}

	q := Control{
		backingFsBlockDev: backingFsBlockDev,
		quotas:            make(map[string]uint32),
	}

	//
	// update minimum project ID
	//
	state := getPquotaState()
	state.updateMinProjID(minProjectID)

	//
	// get first project id to be used for next container
	//
	err = q.findNextProjectID(basePath, baseProjectID)
	if err != nil {
		return nil, err
	}

	log.G(context.TODO()).Debugf("NewControl(%s): nextProjectID = %d", basePath, state.nextProjectID)
	return &q, nil
}

// SetQuota - assign a unique project id to directory and set the quota limits
// for that project id
func (q *Control) SetQuota(targetPath string, quota Quota) error {
	q.RLock()
	projectID, ok := q.quotas[targetPath]
	q.RUnlock()
	if !ok {
		state := getPquotaState()
		state.Lock()

		// Reuse a freed project ID if available, otherwise allocate a new one
		if n := len(state.freeProjectIDs); n > 0 {
			projectID = state.freeProjectIDs[n-1]
			state.freeProjectIDs = state.freeProjectIDs[:n-1]
		} else {
			projectID = state.nextProjectID
		}

		//
		// assign project id to new container directory
		//
		err := setProjectID(targetPath, projectID)
		if err != nil {
			// Return the project ID to the free list on failure
			state.freeProjectIDs = append(state.freeProjectIDs, projectID)
			state.Unlock()
			return err
		}

		state.projectIDsInUse[projectID] = true
		if projectID == state.nextProjectID {
			state.nextProjectID++
		}
		state.Unlock()

		q.Lock()
		q.quotas[targetPath] = projectID
		q.Unlock()
	}

	//
	// set the quota limit for the container's project id
	//
	log.G(context.TODO()).Debugf("SetQuota(%s, %d): projectID=%d", targetPath, quota.Size, projectID)
	return setProjectQuota(q.backingFsBlockDev, projectID, quota)
}

// RemoveQuota - clear the XFS quota limit, remove the path from in-memory
// tracking, and return the project ID to the free list for reuse by future
// SetQuota calls.
//
// The caller must have already deleted the target directory from the
// filesystem (e.g. via EnsureRemoveAll). Only the XFS block device quota
// needs clearing — the directory xattr is gone with the directory.
func (q *Control) RemoveQuota(targetPath string) error {
	q.Lock()
	defer q.Unlock()

	projectID, ok := q.quotas[targetPath]
	if !ok {
		return nil
	}

	log.G(context.TODO()).Debugf("RemoveQuota(%s): projectID=%d", targetPath, projectID)

	if err := setProjectQuota(q.backingFsBlockDev, projectID, Quota{Size: 0}); err != nil {
		return err
	}

	delete(q.quotas, targetPath)

	state := getPquotaState()
	state.Lock()
	delete(state.projectIDsInUse, projectID)
	state.freeProjectIDs = append(state.freeProjectIDs, projectID)
	state.Unlock()

	return nil
}

// setProjectQuota - set the quota for project id on xfs block device
func setProjectQuota(backingFsBlockDev string, projectID uint32, quota Quota) error {
	var d C.fs_disk_quota_t
	d.d_version = C.FS_DQUOT_VERSION
	d.d_id = C.__u32(projectID)
	d.d_flags = C.XFS_PROJ_QUOTA

	d.d_fieldmask = C.FS_DQ_BHARD | C.FS_DQ_BSOFT
	d.d_blk_hardlimit = C.__u64(quota.Size / 512)
	d.d_blk_softlimit = d.d_blk_hardlimit

	cs := C.CString(backingFsBlockDev)
	defer C.free(unsafe.Pointer(cs))

	_, _, errno := unix.Syscall6(unix.SYS_QUOTACTL, C.Q_XSETPQLIM,
		uintptr(unsafe.Pointer(cs)), uintptr(d.d_id),
		uintptr(unsafe.Pointer(&d)), 0, 0)
	if errno != 0 {
		return errors.Wrapf(errno, "failed to set quota limit for projid %d on %s",
			projectID, backingFsBlockDev)
	}

	return nil
}

// GetQuota - get the quota limits of a directory that was configured with SetQuota
func (q *Control) GetQuota(targetPath string, quota *Quota) error {
	q.RLock()
	projectID, ok := q.quotas[targetPath]
	q.RUnlock()
	if !ok {
		return errors.Errorf("quota not found for path: %s", targetPath)
	}

	//
	// get the quota limit for the container's project id
	//
	var d C.fs_disk_quota_t

	cs := C.CString(q.backingFsBlockDev)
	defer C.free(unsafe.Pointer(cs))

	_, _, errno := unix.Syscall6(unix.SYS_QUOTACTL, C.Q_XGETPQUOTA,
		uintptr(unsafe.Pointer(cs)), uintptr(C.__u32(projectID)),
		uintptr(unsafe.Pointer(&d)), 0, 0)
	if errno != 0 {
		return errors.Wrapf(errno, "Failed to get quota limit for projid %d on %s",
			projectID, q.backingFsBlockDev)
	}
	quota.Size = uint64(d.d_blk_hardlimit) * 512

	return nil
}

// getProjectID - get the project id of path on xfs
func getProjectID(targetPath string) (uint32, error) {
	dir, err := openDir(targetPath)
	if err != nil {
		return 0, err
	}
	defer closeDir(dir)

	var fsx C.struct_fsxattr
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, getDirFd(dir), C.FS_IOC_FSGETXATTR,
		uintptr(unsafe.Pointer(&fsx)))
	if errno != 0 {
		return 0, errors.Wrapf(errno, "failed to get projid for %s", targetPath)
	}

	return uint32(fsx.fsx_projid), nil
}

// setProjectID - set the project id of path on xfs
func setProjectID(targetPath string, projectID uint32) error {
	dir, err := openDir(targetPath)
	if err != nil {
		return err
	}
	defer closeDir(dir)

	var fsx C.struct_fsxattr
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, getDirFd(dir), C.FS_IOC_FSGETXATTR,
		uintptr(unsafe.Pointer(&fsx)))
	if errno != 0 {
		return errors.Wrapf(errno, "failed to get projid for %s", targetPath)
	}
	fsx.fsx_projid = C.__u32(projectID)
	fsx.fsx_xflags |= C.FS_XFLAG_PROJINHERIT
	_, _, errno = unix.Syscall(unix.SYS_IOCTL, getDirFd(dir), C.FS_IOC_FSSETXATTR,
		uintptr(unsafe.Pointer(&fsx)))
	if errno != 0 {
		return errors.Wrapf(errno, "failed to set projid for %s", targetPath)
	}

	return nil
}

// findNextProjectID - find the next project id to be used for containers
// by scanning driver home directory to find used project ids. After scanning,
// project IDs in (baseID, nextProjectID) that are not referenced by any
// existing directory are added to the free list for reuse.
func (q *Control) findNextProjectID(home string, baseID uint32) error {
	state := getPquotaState()
	state.Lock()
	defer state.Unlock()

	checkProjID := func(path string) (uint32, error) {
		projid, err := getProjectID(path)
		if err != nil {
			return projid, err
		}
		if projid > 0 {
			q.quotas[path] = projid
			state.projectIDsInUse[projid] = true
		}
		if state.nextProjectID <= projid {
			state.nextProjectID = projid + 1
		}
		return projid, nil
	}

	files, err := os.ReadDir(home)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Errorf("read directory failed: %s", home)
	}
	for _, file := range files {
		if !file.IsDir() {
			continue
		}
		path := filepath.Join(home, file.Name())
		projid, err := checkProjID(path)
		if err != nil {
			return err
		}
		if projid > 0 && projid != baseID {
			continue
		}
		subfiles, err := os.ReadDir(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return errors.Errorf("read directory failed: %s", path)
		}
		for _, subfile := range subfiles {
			if !subfile.IsDir() {
				continue
			}
			subpath := filepath.Join(path, subfile.Name())
			_, err := checkProjID(subpath)
			if err != nil {
				return err
			}
		}
	}

	// Collect project IDs that were allocated but whose directories no
	// longer exist (e.g. removed while the daemon was down). These are
	// gaps in (baseID, nextProjectID) not present in the global in-use map.
	for projid := baseID + 1; projid < state.nextProjectID; projid++ {
		if !state.projectIDsInUse[projid] {
			state.freeProjectIDs = append(state.freeProjectIDs, projid)
		}
	}

	return nil
}

func free(p *C.char) {
	C.free(unsafe.Pointer(p))
}

func openDir(path string) (*C.DIR, error) {
	Cpath := C.CString(path)
	defer free(Cpath)

	dir := C.opendir(Cpath)
	if dir == nil {
		return nil, errors.Errorf("failed to open dir: %s", path)
	}
	return dir, nil
}

func closeDir(dir *C.DIR) {
	if dir != nil {
		C.closedir(dir)
	}
}

func getDirFd(dir *C.DIR) uintptr {
	return uintptr(C.dirfd(dir))
}

// makeBackingFsDev gets the backing block device of the driver home directory
// and creates a block device node under the home directory to be used by
// quotactl commands.
func makeBackingFsDev(home string) (string, error) {
	var stat unix.Stat_t
	if err := unix.Stat(home, &stat); err != nil {
		return "", err
	}

	backingFsBlockDev := path.Join(home, "backingFsBlockDev")
	// Re-create just in case someone copied the home directory over to a new device
	unix.Unlink(backingFsBlockDev)
	err := unix.Mknod(backingFsBlockDev, unix.S_IFBLK|0o600, int(stat.Dev))
	switch {
	case err == nil:
		return backingFsBlockDev, nil

	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EPERM):
		return "", ErrQuotaNotSupported

	default:
		return "", errors.Wrapf(err, "failed to mknod %s", backingFsBlockDev)
	}
}

func hasQuotaSupport(backingFsBlockDev string) (bool, error) {
	cs := C.CString(backingFsBlockDev)
	defer free(cs)
	var qstat C.fs_quota_stat_t

	_, _, errno := unix.Syscall6(unix.SYS_QUOTACTL, uintptr(C.Q_XGETQSTAT_PRJQUOTA), uintptr(unsafe.Pointer(cs)), 0, uintptr(unsafe.Pointer(&qstat)), 0, 0)
	if errno == 0 && qstat.qs_flags&C.FS_QUOTA_PDQ_ENFD > 0 && qstat.qs_flags&C.FS_QUOTA_PDQ_ACCT > 0 {
		return true, nil
	}

	switch errno {
	// These are the known fatal errors, consider all other errors (ENOTTY, etc.. not supporting quota)
	case unix.EFAULT, unix.ENOENT, unix.ENOTBLK, unix.EPERM:
	default:
		return false, nil
	}

	return false, errno
}
