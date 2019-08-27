// +build !windows

package archive // import "github.com/docker/docker/pkg/archive"

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/docker/docker/pkg/system"
	rsystem "github.com/opencontainers/runc/libcontainer/system"
	"golang.org/x/sys/unix"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/skip"
)

func TestCanonicalTarNameForPath(t *testing.T) {
	cases := []struct{ in, expected string }{
		{"foo", "foo"},
		{"foo/bar", "foo/bar"},
		{"foo/dir/", "foo/dir/"},
	}
	for _, v := range cases {
		if CanonicalTarNameForPath(v.in) != v.expected {
			t.Fatalf("wrong canonical tar name. expected:%s got:%s", v.expected, CanonicalTarNameForPath(v.in))
		}
	}
}

func TestCanonicalTarName(t *testing.T) {
	cases := []struct {
		in       string
		isDir    bool
		expected string
	}{
		{"foo", false, "foo"},
		{"foo", true, "foo/"},
		{"foo/bar", false, "foo/bar"},
		{"foo/bar", true, "foo/bar/"},
	}
	for _, v := range cases {
		if canonicalTarName(v.in, v.isDir) != v.expected {
			t.Fatalf("wrong canonical tar name. expected:%s got:%s", v.expected, canonicalTarName(v.in, v.isDir))
		}
	}
}

func TestTarWithHardLink(t *testing.T) {
	origin, err := ioutil.TempDir("", "docker-test-tar-hardlink")
	assert.NilError(t, err)
	defer os.RemoveAll(origin)

	err = ioutil.WriteFile(filepath.Join(origin, "1"), []byte("hello world"), 0700)
	assert.NilError(t, err)

	err = os.Link(filepath.Join(origin, "1"), filepath.Join(origin, "2"))
	assert.NilError(t, err)

	var i1, i2 uint64
	i1, err = getNlink(filepath.Join(origin, "1"))
	assert.NilError(t, err)

	// sanity check that we can hardlink
	if i1 != 2 {
		t.Skipf("skipping since hardlinks don't work here; expected 2 links, got %d", i1)
	}

	dest, err := ioutil.TempDir("", "docker-test-tar-hardlink-dest")
	assert.NilError(t, err)
	defer os.RemoveAll(dest)

	// we'll do this in two steps to separate failure
	fh, err := Tar(origin, Uncompressed)
	assert.NilError(t, err)

	// ensure we can read the whole thing with no error, before writing back out
	buf, err := ioutil.ReadAll(fh)
	assert.NilError(t, err)

	bRdr := bytes.NewReader(buf)
	err = Untar(bRdr, dest, &TarOptions{Compression: Uncompressed})
	assert.NilError(t, err)

	i1, err = getInode(filepath.Join(dest, "1"))
	assert.NilError(t, err)

	i2, err = getInode(filepath.Join(dest, "2"))
	assert.NilError(t, err)

	assert.Check(t, is.Equal(i1, i2))
}

func TestTarWithHardLinkAndRebase(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "docker-test-tar-hardlink-rebase")
	assert.NilError(t, err)
	defer os.RemoveAll(tmpDir)

	origin := filepath.Join(tmpDir, "origin")
	err = os.Mkdir(origin, 0700)
	assert.NilError(t, err)

	err = ioutil.WriteFile(filepath.Join(origin, "1"), []byte("hello world"), 0700)
	assert.NilError(t, err)

	err = os.Link(filepath.Join(origin, "1"), filepath.Join(origin, "2"))
	assert.NilError(t, err)

	var i1, i2 uint64
	i1, err = getNlink(filepath.Join(origin, "1"))
	assert.NilError(t, err)

	// sanity check that we can hardlink
	if i1 != 2 {
		t.Skipf("skipping since hardlinks don't work here; expected 2 links, got %d", i1)
	}

	dest := filepath.Join(tmpDir, "dest")
	bRdr, err := TarResourceRebase(origin, "origin")
	assert.NilError(t, err)

	dstDir, srcBase := SplitPathDirEntry(origin)
	_, dstBase := SplitPathDirEntry(dest)
	content := RebaseArchiveEntries(bRdr, srcBase, dstBase)
	err = Untar(content, dstDir, &TarOptions{Compression: Uncompressed, NoLchown: true, NoOverwriteDirNonDir: true})
	assert.NilError(t, err)

	i1, err = getInode(filepath.Join(dest, "1"))
	assert.NilError(t, err)
	i2, err = getInode(filepath.Join(dest, "2"))
	assert.NilError(t, err)

	assert.Check(t, is.Equal(i1, i2))
}

func getNlink(path string) (uint64, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	statT, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("expected type *syscall.Stat_t, got %t", stat.Sys())
	}
	// We need this conversion on ARM64
	return uint64(statT.Nlink), nil
}

func getInode(path string) (uint64, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	statT, ok := stat.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("expected type *syscall.Stat_t, got %t", stat.Sys())
	}
	return statT.Ino, nil
}

func TestTarWithBlockCharFifo(t *testing.T) {
	skip.If(t, os.Getuid() != 0, "skipping test that requires root")
	skip.If(t, rsystem.RunningInUserNS(), "skipping test that requires initial userns")
	origin, err := ioutil.TempDir("", "docker-test-tar-hardlink")
	assert.NilError(t, err)

	defer os.RemoveAll(origin)
	err = ioutil.WriteFile(filepath.Join(origin, "1"), []byte("hello world"), 0700)
	assert.NilError(t, err)

	err = system.Mknod(filepath.Join(origin, "2"), unix.S_IFBLK, int(system.Mkdev(int64(12), int64(5))))
	assert.NilError(t, err)
	err = system.Mknod(filepath.Join(origin, "3"), unix.S_IFCHR, int(system.Mkdev(int64(12), int64(5))))
	assert.NilError(t, err)
	err = system.Mknod(filepath.Join(origin, "4"), unix.S_IFIFO, int(system.Mkdev(int64(12), int64(5))))
	assert.NilError(t, err)

	dest, err := ioutil.TempDir("", "docker-test-tar-hardlink-dest")
	assert.NilError(t, err)
	defer os.RemoveAll(dest)

	// we'll do this in two steps to separate failure
	fh, err := Tar(origin, Uncompressed)
	assert.NilError(t, err)

	// ensure we can read the whole thing with no error, before writing back out
	buf, err := ioutil.ReadAll(fh)
	assert.NilError(t, err)

	bRdr := bytes.NewReader(buf)
	err = Untar(bRdr, dest, &TarOptions{Compression: Uncompressed})
	assert.NilError(t, err)

	changes, err := ChangesDirs(origin, dest)
	assert.NilError(t, err)

	if len(changes) > 0 {
		t.Fatalf("Tar with special device (block, char, fifo) should keep them (recreate them when untar) : %v", changes)
	}
}

// TestTarUntarWithXattr is Unix as Lsetxattr is not supported on Windows
func TestTarUntarWithXattr(t *testing.T) {
	skip.If(t, os.Getuid() != 0, "skipping test that requires root")
	if _, err := exec.LookPath("setcap"); err != nil {
		t.Skip("setcap not installed")
	}
	if _, err := exec.LookPath("getcap"); err != nil {
		t.Skip("getcap not installed")
	}

	origin, err := ioutil.TempDir("", "docker-test-untar-origin")
	assert.NilError(t, err)
	defer os.RemoveAll(origin)
	err = ioutil.WriteFile(filepath.Join(origin, "1"), []byte("hello world"), 0700)
	assert.NilError(t, err)

	err = ioutil.WriteFile(filepath.Join(origin, "2"), []byte("welcome!"), 0700)
	assert.NilError(t, err)
	err = ioutil.WriteFile(filepath.Join(origin, "3"), []byte("will be ignored"), 0700)
	assert.NilError(t, err)
	// there is no known Go implementation of setcap/getcap with support for v3 file capability
	out, err := exec.Command("setcap", "cap_block_suspend+ep", filepath.Join(origin, "2")).CombinedOutput()
	assert.NilError(t, err, string(out))

	for _, c := range []Compression{
		Uncompressed,
		Gzip,
	} {
		changes, err := tarUntar(t, origin, &TarOptions{
			Compression:     c,
			ExcludePatterns: []string{"3"},
		})

		if err != nil {
			t.Fatalf("Error tar/untar for compression %s: %s", c.Extension(), err)
		}

		if len(changes) != 1 || changes[0].Path != "/3" {
			t.Fatalf("Unexpected differences after tarUntar: %v", changes)
		}
		out, err := exec.Command("getcap", filepath.Join(origin, "2")).CombinedOutput()
		assert.NilError(t, err, string(out))
		assert.Check(t, is.Contains(string(out), "= cap_block_suspend+ep"), "untar should have kept the 'security.capability' xattr")
	}
}

func TestCopyInfoDestinationPathSymlink(t *testing.T) {
	tmpDir, _ := getTestTempDirs(t)
	defer removeAllPaths(tmpDir)

	root := strings.TrimRight(tmpDir, "/") + "/"

	type FileTestData struct {
		resource FileData
		file     string
		expected CopyInfo
	}

	testData := []FileTestData{
		//Create a directory: /tmp/archive-copy-test*/dir1
		//Test will "copy" file1 to dir1
		{resource: FileData{filetype: Dir, path: "dir1", permissions: 0740}, file: "file1", expected: CopyInfo{Path: root + "dir1/file1", Exists: false, IsDir: false}},

		//Create a symlink directory to dir1: /tmp/archive-copy-test*/dirSymlink -> dir1
		//Test will "copy" file2 to dirSymlink
		{resource: FileData{filetype: Symlink, path: "dirSymlink", contents: root + "dir1", permissions: 0600}, file: "file2", expected: CopyInfo{Path: root + "dirSymlink/file2", Exists: false, IsDir: false}},

		//Create a file in tmp directory: /tmp/archive-copy-test*/file1
		//Test to cover when the full file path already exists.
		{resource: FileData{filetype: Regular, path: "file1", permissions: 0600}, file: "", expected: CopyInfo{Path: root + "file1", Exists: true}},

		//Create a directory: /tmp/archive-copy*/dir2
		//Test to cover when the full directory path already exists
		{resource: FileData{filetype: Dir, path: "dir2", permissions: 0740}, file: "", expected: CopyInfo{Path: root + "dir2", Exists: true, IsDir: true}},

		//Create a symlink to a non-existent target: /tmp/archive-copy*/symlink1 -> noSuchTarget
		//Negative test to cover symlinking to a target that does not exit
		{resource: FileData{filetype: Symlink, path: "symlink1", contents: "noSuchTarget", permissions: 0600}, file: "", expected: CopyInfo{Path: root + "noSuchTarget", Exists: false}},

		//Create a file in tmp directory for next test: /tmp/existingfile
		{resource: FileData{filetype: Regular, path: "existingfile", permissions: 0600}, file: "", expected: CopyInfo{Path: root + "existingfile", Exists: true}},

		//Create a symlink to an existing file: /tmp/archive-copy*/symlink2 -> /tmp/existingfile
		//Test to cover when the parent directory of a new file is a symlink
		{resource: FileData{filetype: Symlink, path: "symlink2", contents: "existingfile", permissions: 0600}, file: "", expected: CopyInfo{Path: root + "existingfile", Exists: true}},
	}

	var dirs []FileData
	for _, data := range testData {
		dirs = append(dirs, data.resource)
	}
	provisionSampleDir(t, tmpDir, dirs)

	for _, info := range testData {
		p := filepath.Join(tmpDir, info.resource.path, info.file)
		ci, err := CopyInfoDestinationPath(p)
		assert.Check(t, err)
		assert.Check(t, is.DeepEqual(info.expected, ci))
	}
}
