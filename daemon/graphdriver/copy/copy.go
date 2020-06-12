// +build linux

package copy // import "github.com/docker/docker/daemon/graphdriver/copy"

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/pkg/pools"
	"github.com/docker/docker/pkg/system"
	rsystem "github.com/opencontainers/runc/libcontainer/system"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

const (
	// Docker sets this earlier on in initialization to 0022
	// Unfortunately, sometimes the special bits are not respected
	// and we have to force-set them.
	desiredUmask = 7022
	// This is based on some empirical testing on EXT4 filesystems
	fileCopyInfoChannelDepth = 100
	defaultWorkerCount       = 8
)

// Mode indicates whether to use hardlink or copy content
type Mode int

const (
	// Content creates a new file, and copies the content of the file
	Content Mode = iota
	// Hardlink creates a new hardlink to the existing file
	Hardlink
)

func copyRegular(srcPath, dstPath string, fileinfo os.FileInfo, copyWithFileRange, copyWithFileClone *bool) error {
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	// If the destination file already exists, we shouldn't blow it away
	dstFile, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileinfo.Mode())
	if err != nil {
		return err
	}
	defer dstFile.Close()

	if *copyWithFileClone {
		err = fiClone(srcFile, dstFile)
		if err == nil {
			return nil
		}

		*copyWithFileClone = false
		if err == unix.EXDEV {
			*copyWithFileRange = false
		}
	}
	if *copyWithFileRange {
		err = doCopyWithFileRange(srcFile, dstFile, fileinfo)
		// Trying the file_clone may not have caught the exdev case
		// as the ioctl may not have been available (therefore EINVAL)
		if err == unix.EXDEV || err == unix.ENOSYS {
			*copyWithFileRange = false
		} else {
			return err
		}
	}
	return legacyCopy(srcFile, dstFile)
}

func doCopyWithFileRange(srcFile, dstFile *os.File, fileinfo os.FileInfo) error {
	amountLeftToCopy := fileinfo.Size()

	for amountLeftToCopy > 0 {
		n, err := unix.CopyFileRange(int(srcFile.Fd()), nil, int(dstFile.Fd()), nil, int(amountLeftToCopy), 0)
		if err != nil {
			return err
		}

		amountLeftToCopy = amountLeftToCopy - int64(n)
	}

	return nil
}

func legacyCopy(srcFile io.Reader, dstFile io.Writer) error {
	_, err := pools.Copy(dstFile, srcFile)

	return err
}

func copyXattr(srcPath, dstPath, attr string) error {
	data, err := system.Lgetxattr(srcPath, attr)
	if err != nil {
		return err
	}
	if data != nil {
		if err := system.Lsetxattr(dstPath, attr, data, 0); err != nil {
			return err
		}
	}
	return nil
}

type fileCopyInfoMode int

const (
	fileMode fileCopyInfoMode = iota
	dirMode
	updateDirTimeMode
)

type fileCopyInfo struct {
	mode        fileCopyInfoMode
	dstPath     string
	srcPath     string
	stat        *syscall.Stat_t
	fileInfo    os.FileInfo
	sharedInode *sharedInode
}

type fileID struct {
	dev uint64
	ino uint64
}

// This represents an inode on the destination side with a unique identity
type sharedInode struct {
	lock                      sync.Mutex
	initialized               bool
	successfullyCopiedDstPath string
	err                       error
}

func doRegularFileCopy(srcPath, dstPath string, f os.FileInfo, sInode *sharedInode, copyWithFileRange, copyWithFileClone *bool) error {
	if sInode == nil {
		return copyRegular(srcPath, dstPath, f, copyWithFileRange, copyWithFileClone)
	}

	sInode.lock.Lock()
	defer sInode.lock.Unlock()
	if sInode.err != nil {
		return sInode.err
	}
	// the inode has been copied without error
	if sInode.initialized {
		return os.Link(sInode.successfullyCopiedDstPath, dstPath)
	}
	// We don't actually have to wait for all of copyRegular to finish, in fact
	// all we need is to wait for the inode creation, but there's all sorts of
	// terrible things that might happen between inode creation and writing the file,
	// and hardlinks are relatively rare in the primary use case (VFS)
	sInode.err = copyRegular(srcPath, dstPath, f, copyWithFileRange, copyWithFileClone)
	sInode.successfullyCopiedDstPath = dstPath
	sInode.initialized = true
	return sInode.err
}
func doFileCopy(srcPath, dstPath string, f os.FileInfo, stat *syscall.Stat_t, sInode *sharedInode, copyWithFileRange, copyWithFileClone *bool, copyXattrs bool, copyMode Mode) error {
	mode := f.Mode()
	switch {
	case mode.IsRegular(): // Regular file
		if copyMode == Hardlink {
			if err := os.Link(srcPath, dstPath); err != nil {
				return err
			}
			// Don't copy metadata for hard links
			return nil
		}
		if err := doRegularFileCopy(srcPath, dstPath, f, sInode, copyWithFileRange, copyWithFileClone); err != nil {
			return err
		}

	case mode&os.ModeSymlink != 0:
		link, err := os.Readlink(srcPath)
		if err != nil {
			return err
		}

		if err := os.Symlink(link, dstPath); err != nil {
			return err
		}

	case mode&os.ModeNamedPipe != 0:
		fallthrough
	case mode&os.ModeSocket != 0:
		if err := unix.Mkfifo(dstPath, stat.Mode); err != nil {
			return err
		}

	case mode&os.ModeDevice != 0:
		if rsystem.RunningInUserNS() {
			// cannot create a device if running in user namespace
			return nil
		}
		if err := unix.Mknod(dstPath, stat.Mode, int(stat.Rdev)); err != nil {
			return err
		}

	default:
		return fmt.Errorf("unknown file type for %s", srcPath)
	}

	return copyMetadata(dstPath, srcPath, f, stat, copyXattrs)
}

func fileCopyWorker(ctx context.Context, fileCopyInfoChan chan *fileCopyInfo, copyMode Mode, copyXattrs bool) error {
	copyWithFileRange := true
	copyWithFileClone := true

	// Just a dumb wrapper around the above function which does the heavy lifting
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case fci, ok := <-fileCopyInfoChan:
			if !ok {
				return nil
			}
			switch fci.mode {
			case dirMode:
				if err := copyMetadataShared(fci.dstPath, fci.srcPath, fci.fileInfo, fci.stat, copyXattrs); err != nil {
					return err
				}
			case fileMode:
				if err := doFileCopy(fci.srcPath, fci.dstPath, fci.fileInfo, fci.stat, fci.sharedInode, &copyWithFileRange, &copyWithFileClone, copyXattrs, copyMode); err != nil {
					return err
				}
			case updateDirTimeMode:
				ts := []syscall.Timespec{fci.stat.Atim, fci.stat.Mtim}
				if err := system.LUtimesNano(fci.dstPath, ts); err != nil {
					return err
				}
			}
		}
	}
}

// readDirNames reads the directory named by dirname and returns
// a sorted list of directory entries.
func readDirNames(dirname string) ([]string, error) {
	f, err := os.Open(dirname)
	if err != nil {
		return nil, err
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return nil, err
	}
	return names, nil
}

type walkFn struct {
	srcDir         string
	dstDir         string
	workers        []chan *fileCopyInfo
	rand           *rand.Rand
	sharedInodeMap map[fileID]*sharedInode
}

func (w *walkFn) rebasePath(srcPath string) (string, error) {
	// Rebase path
	relPath, err := filepath.Rel(w.srcDir, srcPath)
	if err != nil {
		return "", err
	}

	return filepath.Join(w.dstDir, relPath), nil
}

func (w *walkFn) walkFunc(ctx context.Context, srcPath string, f os.FileInfo, c chan *fileCopyInfo, err error) error {
	var sInode *sharedInode
	mode := fileMode
	if err != nil {
		return err
	}
	// Check if the errgroup has gotten canceled / shutdown
	err = ctx.Err()
	if err != nil {
		return ctx.Err()
	}

	dstPath, err := w.rebasePath(srcPath)
	if err != nil {
		return err
	}

	stat, ok := f.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("Unable to get raw syscall.Stat_t data for %s", srcPath)
	}
	if f.IsDir() {
		mode = dirMode
		if err := os.Mkdir(dstPath, f.Mode()); err != nil && !os.IsExist(err) {
			return err
		}
		if f.Mode()&desiredUmask > 0 {
			if err := os.Chmod(dstPath, f.Mode()); err != nil {
				return err
			}
		}
	}
	if stat.Nlink > 1 {
		id := fileID{dev: stat.Dev, ino: stat.Ino}
		if val, ok := w.sharedInodeMap[id]; ok {
			sInode = val
		} else {
			sInode = &sharedInode{}
			w.sharedInodeMap[id] = sInode
		}
	}
	select {
	case c <- &fileCopyInfo{
		mode:        mode,
		dstPath:     dstPath,
		srcPath:     srcPath,
		stat:        stat,
		fileInfo:    f,
		sharedInode: sInode,
	}:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (w *walkFn) updateDirTime(ctx context.Context, srcPath string, f os.FileInfo, c chan *fileCopyInfo) error {
	dstPath, err := w.rebasePath(srcPath)
	if err != nil {
		return err
	}
	stat, ok := f.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("Unable to get raw syscall.Stat_t data for %s", srcPath)
	}
	select {
	case c <- &fileCopyInfo{
		mode:     updateDirTimeMode,
		dstPath:  dstPath,
		srcPath:  srcPath,
		stat:     stat,
		fileInfo: f,
	}:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// walk and walk2 are borrowed from filepath.WalkDir, but adapted for our purposes
// we have a special field for the channel that's used for this given directory
// the reason why is to serialize writes for that dirent, and then we can simply
// rely on delivery ordering to make sure our message to update mtimes gets there
// last
func (w *walkFn) walk2(ctx context.Context, path string, info os.FileInfo) error {
	c := w.workers[w.rand.Intn(len(w.workers))]

	names, err := readDirNames(path)
	err1 := w.walkFunc(ctx, path, info, c, err)
	// If err != nil, walk can't walk into this directory.
	// err1 != nil means walkFn want walk to skip this directory or stop walking.
	// Therefore, if one of err and err1 isn't nil, walk will return.
	if err != nil || err1 != nil {
		// The caller's behavior is controlled by the return value, which is decided
		// by walkFn. walkFn may ignore err and return nil.
		// If walkFn returns SkipDir, it will be handled by the caller.
		// So walk should return whatever walkFn returns.
		return err1
	}

	for _, name := range names {
		filename := filepath.Join(path, name)
		fileInfo, err := os.Lstat(filename)
		if err != nil {
			if err := w.walkFunc(ctx, filename, fileInfo, c, err); err != nil {
				return err
			}
		} else if fileInfo.IsDir() {
			err = w.walk2(ctx, filename, fileInfo)
			if err != nil {
				return err
			}
		} else {
			if err := w.walkFunc(ctx, filename, fileInfo, c, err); err != nil {
				return err
			}
		}
	}
	return w.updateDirTime(ctx, path, info, c)
}

func (w *walkFn) walk(ctx context.Context, root string) error {
	c := w.workers[w.rand.Intn(len(w.workers))]
	info, err := os.Lstat(root)
	if err != nil {
		err = w.walkFunc(ctx, root, nil, c, err)
	} else {
		err = w.walk2(ctx, root, info)
	}
	return err
}

// DirCopy copies or hardlinks the contents of one directory to another,
// properly handling xattrs, and soft links
//
// Copying xattrs can be opted out of by passing false for copyXattrs.
func DirCopy(srcDir, dstDir string, copyMode Mode, copyXattrs bool) error {
	return DirCopyWithConcurrency(srcDir, dstDir, copyMode, copyXattrs, 0)
}

// DirCopyWithConcurrency performs the same work as DirCopy, but allows you to specify the
// concurrency level. Specifying workerCount as 0 will make it use the default worker
// count
func DirCopyWithConcurrency(srcDir, dstDir string, copyMode Mode, copyXattrs bool, workerCount int) error {
	if workerCount < 0 {
		return fmt.Errorf("Copy concurrency '%d' is less than 0", workerCount)
	}
	if workerCount == 0 {
		workerCount = defaultWorkerCount
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errGroup, errGroupCtx := errgroup.WithContext(ctx)
	workers := make([]chan *fileCopyInfo, workerCount)
	for i := 0; i < len(workers); i++ {
		c := make(chan *fileCopyInfo, fileCopyInfoChannelDepth)
		workers[i] = c
		errGroup.Go(func() error {
			return fileCopyWorker(errGroupCtx, c, copyMode, copyXattrs)
		})
	}

	w := &walkFn{
		srcDir:         srcDir,
		dstDir:         dstDir,
		workers:        workers,
		rand:           rand.New(rand.NewSource(3)),
		sharedInodeMap: make(map[fileID]*sharedInode),
	}

	err := w.walk(errGroupCtx, srcDir)
	for i := 0; i < len(workers); i++ {
		close(workers[i])
	}
	if err != nil {
		cancel()
		return err
	}

	return errGroup.Wait()
}

// copyMetadataShared copies the metadata for directories and files, but doesn't
// set atimes / mtimes because those have to happen after all file modifications
// are complete
func copyMetadataShared(dstPath, srcPath string, f os.FileInfo, stat *syscall.Stat_t, copyXattrs bool) error {
	if err := os.Lchown(dstPath, int(stat.Uid), int(stat.Gid)); err != nil {
		return err
	}

	if copyXattrs {
		if err := doCopyXattrs(srcPath, dstPath); err != nil {
			return err
		}
	}

	return nil
}

func copyMetadata(dstPath, srcPath string, f os.FileInfo, stat *syscall.Stat_t, copyXattrs bool) error {
	if err := copyMetadataShared(dstPath, srcPath, f, stat, copyXattrs); err != nil {
		return err
	}
	isSymlink := f.Mode()&os.ModeSymlink != 0
	if isSymlink {
		// There is no LChmod, so ignore mode for symlink. Also, this
		// must happen after chown, as that can modify the file mode
		ts := []syscall.Timespec{stat.Atim, stat.Mtim}
		if err := system.LUtimesNano(dstPath, ts); err != nil {
			return err
		}
	} else {
		if f.Mode()&desiredUmask > 0 {
			if err := os.Chmod(dstPath, f.Mode()); err != nil {
				return err
			}
		}
		aTime := time.Unix(stat.Atim.Sec, stat.Atim.Nsec)
		mTime := time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec)
		if err := system.Chtimes(dstPath, aTime, mTime); err != nil {
			return err
		}
	}

	return nil
}

func doCopyXattrs(srcPath, dstPath string) error {
	if err := copyXattr(srcPath, dstPath, "security.capability"); err != nil {
		return err
	}

	// We need to copy this attribute if it appears in an overlay upper layer, as
	// this function is used to copy those. It is set by overlay if a directory
	// is removed and then re-created and should not inherit anything from the
	// same dir in the lower dir.
	return copyXattr(srcPath, dstPath, "trusted.overlay.opaque")
}
