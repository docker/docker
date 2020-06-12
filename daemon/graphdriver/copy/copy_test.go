// +build linux

package copy // import "github.com/docker/docker/daemon/graphdriver/copy"

import (
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/pkg/system"
	"golang.org/x/sys/unix"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestCopy(t *testing.T) {
	copyWithFileRange := true
	copyWithFileClone := true
	doCopyTest(t, &copyWithFileRange, &copyWithFileClone)
}

func TestCopyWithoutRange(t *testing.T) {
	copyWithFileRange := false
	copyWithFileClone := false
	doCopyTest(t, &copyWithFileRange, &copyWithFileClone)
}

func benchmarkCopyDir(b *testing.B, maxDirCount, maxFileCount int) {
	srcDir, err := ioutil.TempDir("", "srcDir")
	assert.NilError(b, err)
	populateSrcDir(b, srcDir, 10000, maxDirCount, maxFileCount, 6)
	defer os.RemoveAll(srcDir)

	b.ResetTimer()
	b.StopTimer()
	for i := 0; i < b.N; i++ {
		dstDir, err := ioutil.TempDir("", "testdst")
		assert.NilError(b, err)
		dstDirFile, err := os.Open(dstDir)
		assert.NilError(b, err)
		assert.NilError(b, unix.Syncfs(int(dstDirFile.Fd())))
		assert.NilError(b, dstDirFile.Close())

		b.StartTimer()
		assert.Check(b, DirCopy(srcDir, dstDir, Content, true))
		b.StopTimer()
		assert.NilError(b, os.RemoveAll(dstDir))
	}
}

func BenchmarkCopyDirFewerDirsMoreFiles(b *testing.B) {
	benchmarkCopyDir(b, 5, 25)
}

func BenchmarkCopyDir(b *testing.B) {
	benchmarkCopyDir(b, 25, 25)
}

func TestCopyDir(t *testing.T) {
	srcDir, err := ioutil.TempDir("", "srcDir")
	assert.NilError(t, err)
	populateSrcDir(t, srcDir, 100, 25, 25, 3)
	defer os.RemoveAll(srcDir)

	dstDir, err := ioutil.TempDir("", "testdst")
	assert.NilError(t, err)
	defer os.RemoveAll(dstDir)

	assert.Check(t, DirCopy(srcDir, dstDir, Content, false))
	assert.NilError(t, filepath.Walk(srcDir, func(srcPath string, f os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Rebase path
		relPath, err := filepath.Rel(srcDir, srcPath)
		assert.NilError(t, err)
		if relPath == "." {
			return nil
		}

		dstPath := filepath.Join(dstDir, relPath)
		assert.NilError(t, err)

		// If we add non-regular dirs and files to the test
		// then we need to add more checks here.
		dstFileInfo, err := os.Lstat(dstPath)
		assert.NilError(t, err)

		srcFileSys := f.Sys().(*syscall.Stat_t)
		dstFileSys := dstFileInfo.Sys().(*syscall.Stat_t)

		msg := fmt.Sprintf("%s has inconsistency", relPath)
		if srcFileSys.Dev == dstFileSys.Dev {
			assert.Check(t, srcFileSys.Ino != dstFileSys.Ino, msg)
		}
		// Todo: check size, and ctim is not equal on filesystems that have granular ctimes
		assert.Check(t, is.DeepEqual(srcFileSys.Mode, dstFileSys.Mode))
		assert.Check(t, is.DeepEqual(srcFileSys.Uid, dstFileSys.Uid))
		assert.Check(t, is.DeepEqual(srcFileSys.Gid, dstFileSys.Gid))
		assert.Check(t, is.DeepEqual(srcFileSys.Mtim, dstFileSys.Mtim))
		assert.Check(t, is.DeepEqual(f.Size(), dstFileInfo.Size()), msg)

		return nil
	}))
}

func randomMode(baseMode int) os.FileMode {
	for i := 0; i < 7; i++ {
		baseMode = baseMode | (1&rand.Intn(2))<<uint(i)
	}
	return os.FileMode(baseMode)
}

func populateSrcDir(t testing.TB, srcDir string, maxDatalength, maxDirCount, maxFileCount, remainingDepth int) {
	if remainingDepth == 0 {
		return
	}
	aTime := time.Unix(rand.Int63(), 0)
	mTime := time.Unix(rand.Int63(), 0)

	for i := 0; i < rand.Intn(maxDirCount); i++ {
		dirName := filepath.Join(srcDir, fmt.Sprintf("srcdir-%d", i))
		// Owner all bits set
		assert.NilError(t, os.Mkdir(dirName, randomMode(0700)))
		populateSrcDir(t, dirName, maxDatalength, maxDirCount, maxFileCount, remainingDepth-1)
		assert.NilError(t, system.Chtimes(dirName, aTime, mTime))
	}

	for i := 0; i < rand.Intn(maxFileCount); i++ {
		fileName := filepath.Join(srcDir, fmt.Sprintf("srcfile-%d", i))
		datalen := 0
		if maxDatalength > 0 {
			datalen = rand.Intn(maxDatalength)
		}
		// Owner read bit set
		assert.NilError(t, ioutil.WriteFile(fileName, make([]byte, datalen), randomMode(0400)))
		assert.NilError(t, system.Chtimes(fileName, aTime, mTime))
	}
}

func doCopyTest(t *testing.T, copyWithFileRange, copyWithFileClone *bool) {
	dir, err := ioutil.TempDir("", "docker-copy-check")
	assert.NilError(t, err)
	defer os.RemoveAll(dir)
	srcFilename := filepath.Join(dir, "srcFilename")
	dstFilename := filepath.Join(dir, "dstilename")

	r := rand.New(rand.NewSource(0))
	buf := make([]byte, 1024)
	_, err = r.Read(buf)
	assert.NilError(t, err)
	assert.NilError(t, ioutil.WriteFile(srcFilename, buf, 0777))
	fileinfo, err := os.Stat(srcFilename)
	assert.NilError(t, err)

	assert.NilError(t, copyRegular(srcFilename, dstFilename, fileinfo, copyWithFileRange, copyWithFileClone))
	readBuf, err := ioutil.ReadFile(dstFilename)
	assert.NilError(t, err)
	assert.Check(t, is.DeepEqual(buf, readBuf))
}

func TestCopyHardlink(t *testing.T) {
	var srcFile1FileInfo, srcFile2FileInfo, dstFile1FileInfo, dstFile2FileInfo unix.Stat_t

	srcDir, err := ioutil.TempDir("", "srcDir")
	assert.NilError(t, err)
	defer os.RemoveAll(srcDir)

	dstDir, err := ioutil.TempDir("", "dstDir")
	assert.NilError(t, err)
	defer os.RemoveAll(dstDir)

	srcFile1 := filepath.Join(srcDir, "file1")
	srcFile2 := filepath.Join(srcDir, "file2")
	dstFile1 := filepath.Join(dstDir, "file1")
	dstFile2 := filepath.Join(dstDir, "file2")
	assert.NilError(t, ioutil.WriteFile(srcFile1, []byte{}, 0777))
	assert.NilError(t, os.Link(srcFile1, srcFile2))

	assert.Check(t, DirCopy(srcDir, dstDir, Content, false))

	assert.NilError(t, unix.Stat(srcFile1, &srcFile1FileInfo))
	assert.NilError(t, unix.Stat(srcFile2, &srcFile2FileInfo))
	assert.Equal(t, srcFile1FileInfo.Ino, srcFile2FileInfo.Ino)

	assert.NilError(t, unix.Stat(dstFile1, &dstFile1FileInfo))
	assert.NilError(t, unix.Stat(dstFile2, &dstFile2FileInfo))
	assert.Check(t, is.Equal(dstFile1FileInfo.Ino, dstFile2FileInfo.Ino))
}

type dirOrFile int

const (
	dir dirOrFile = iota
	file
)

func testCopyWithSpecialBit(t *testing.T, mode uint32, testKind dirOrFile) {
	var dst1FileInfo, src1FileInfo unix.Stat_t
	srcDir, err := ioutil.TempDir("", "srcDir")
	assert.NilError(t, err)
	defer os.RemoveAll(srcDir)

	dstDir, err := ioutil.TempDir("", "dstDir")
	assert.NilError(t, err)
	defer os.RemoveAll(dstDir)

	nestedSrcDir := filepath.Join(srcDir, "dir")
	nestedDstDir := filepath.Join(dstDir, "dir")
	assert.NilError(t, os.Mkdir(nestedSrcDir, 0755))
	src1 := filepath.Join(nestedSrcDir, "foo")
	dst1 := filepath.Join(nestedDstDir, "foo")

	// Setup source directory, and verify we've laid it down correctly
	switch testKind {
	case dir:
		assert.NilError(t, unix.Mkdir(src1, mode))
	case file:
		assert.NilError(t, ioutil.WriteFile(src1, []byte("temp"), os.FileMode(mode)))

	}
	assert.NilError(t, unix.Chmod(src1, mode)) // To deal with the umask

	assert.NilError(t, unix.Stat(src1, &src1FileInfo))
	assert.Equal(t, mode, 07777&src1FileInfo.Mode)
	// Do copy
	assert.Check(t, DirCopy(srcDir, dstDir, Content, false))
	// Verify results
	assert.NilError(t, unix.Stat(dst1, &dst1FileInfo))
	assert.Equal(t, mode, 07777&dst1FileInfo.Mode)
}

func TestCopyDirWithSpecialBit(t *testing.T) {
	for i := 0; i <= 7; i++ {
		// Shift the i number into the special bit place
		mode := uint32(0777 | (i << 9))
		testName := fmt.Sprintf("mode=%#o", mode)
		t.Run(testName, func(localT *testing.T) {
			testCopyWithSpecialBit(localT, mode, dir)
		})
	}
}

func TestCopyFileWithSpecialBit(t *testing.T) {
	for i := 0; i <= 7; i++ {
		// Shift the i number into the special bit place
		mode := uint32(0777 | (i << 9))
		testName := fmt.Sprintf("mode=%#o", mode)
		t.Run(testName, func(localT *testing.T) {
			testCopyWithSpecialBit(localT, mode, file)
		})
	}
}
