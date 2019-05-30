package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/docker/docker/pkg/archive"
	"github.com/go-check/check"
	"gotest.tools/assert"
)

type fileType uint32

const (
	ftRegular fileType = iota
	ftDir
	ftSymlink
)

type fileData struct {
	filetype fileType
	path     string
	contents string
	uid      int
	gid      int
	mode     int
}

func (fd fileData) creationCommand() string {
	var command string

	switch fd.filetype {
	case ftRegular:
		// Don't overwrite the file if it already exists!
		command = fmt.Sprintf("if [ ! -f %s ]; then echo %q > %s; fi", fd.path, fd.contents, fd.path)
	case ftDir:
		command = fmt.Sprintf("mkdir -p %s", fd.path)
	case ftSymlink:
		command = fmt.Sprintf("ln -fs %s %s", fd.contents, fd.path)
	}

	return command
}

func mkFilesCommand(fds []fileData) string {
	commands := make([]string, len(fds))

	for i, fd := range fds {
		commands[i] = fd.creationCommand()
	}

	return strings.Join(commands, " && ")
}

var defaultFileData = []fileData{
	{ftRegular, "file1", "file1", 0, 0, 0666},
	{ftRegular, "file2", "file2", 0, 0, 0666},
	{ftRegular, "file3", "file3", 0, 0, 0666},
	{ftRegular, "file4", "file4", 0, 0, 0666},
	{ftRegular, "file5", "file5", 0, 0, 0666},
	{ftRegular, "file6", "file6", 0, 0, 0666},
	{ftRegular, "file7", "file7", 0, 0, 0666},
	{ftDir, "dir1", "", 0, 0, 0777},
	{ftRegular, "dir1/file1-1", "file1-1", 0, 0, 0666},
	{ftRegular, "dir1/file1-2", "file1-2", 0, 0, 0666},
	{ftDir, "dir2", "", 0, 0, 0666},
	{ftRegular, "dir2/file2-1", "file2-1", 0, 0, 0666},
	{ftRegular, "dir2/file2-2", "file2-2", 0, 0, 0666},
	{ftDir, "dir3", "", 0, 0, 0666},
	{ftRegular, "dir3/file3-1", "file3-1", 0, 0, 0666},
	{ftRegular, "dir3/file3-2", "file3-2", 0, 0, 0666},
	{ftDir, "dir4", "", 0, 0, 0666},
	{ftRegular, "dir4/file3-1", "file4-1", 0, 0, 0666},
	{ftRegular, "dir4/file3-2", "file4-2", 0, 0, 0666},
	{ftDir, "dir5", "", 0, 0, 0666},
	{ftSymlink, "symlinkToFile1", "file1", 0, 0, 0666},
	{ftSymlink, "symlinkToDir1", "dir1", 0, 0, 0666},
	{ftSymlink, "brokenSymlinkToFileX", "fileX", 0, 0, 0666},
	{ftSymlink, "brokenSymlinkToDirX", "dirX", 0, 0, 0666},
	{ftSymlink, "symlinkToAbsDir", "/root", 0, 0, 0666},
	{ftDir, "permdirtest", "", 2, 2, 0700},
	{ftRegular, "permdirtest/permtest", "perm_test", 65534, 65534, 0400},
}

func defaultMkContentCommand() string {
	return mkFilesCommand(defaultFileData)
}

func makeTestContentInDir(c *check.C, dir string) {
	for _, fd := range defaultFileData {
		path := filepath.Join(dir, filepath.FromSlash(fd.path))
		switch fd.filetype {
		case ftRegular:
			assert.NilError(c, ioutil.WriteFile(path, []byte(fd.contents+"\n"), os.FileMode(fd.mode)))
		case ftDir:
			assert.NilError(c, os.Mkdir(path, os.FileMode(fd.mode)))
		case ftSymlink:
			assert.NilError(c, os.Symlink(fd.contents, path))
		}

		if fd.filetype != ftSymlink && runtime.GOOS != "windows" {
			assert.NilError(c, os.Chown(path, fd.uid, fd.gid))
		}
	}
}

type testContainerOptions struct {
	addContent bool
	readOnly   bool
	volumes    []string
	workDir    string
	command    string
}

func makeTestContainer(c *check.C, options testContainerOptions) (containerID string) {
	if options.addContent {
		mkContentCmd := defaultMkContentCommand()
		if options.command == "" {
			options.command = mkContentCmd
		} else {
			options.command = fmt.Sprintf("%s && %s", defaultMkContentCommand(), options.command)
		}
	}

	if options.command == "" {
		options.command = "#(nop)"
	}

	args := []string{"run", "-d"}

	for _, volume := range options.volumes {
		args = append(args, "-v", volume)
	}

	if options.workDir != "" {
		args = append(args, "-w", options.workDir)
	}

	if options.readOnly {
		args = append(args, "--read-only")
	}

	args = append(args, "busybox", "/bin/sh", "-c", options.command)

	out, _ := dockerCmd(c, args...)

	containerID = strings.TrimSpace(out)

	out, _ = dockerCmd(c, "wait", containerID)

	exitCode := strings.TrimSpace(out)
	if exitCode != "0" {
		out, _ = dockerCmd(c, "logs", containerID)
	}
	assert.Equal(c, exitCode, "0", "failed to make test container: %s", out)

	return
}

func makeCatFileCommand(path string) string {
	return fmt.Sprintf("if [ -f %s ]; then cat %s; fi", path, path)
}

func cpPath(pathElements ...string) string {
	localizedPathElements := make([]string, len(pathElements))
	for i, path := range pathElements {
		localizedPathElements[i] = filepath.FromSlash(path)
	}
	return strings.Join(localizedPathElements, string(filepath.Separator))
}

func cpPathTrailingSep(pathElements ...string) string {
	return fmt.Sprintf("%s%c", cpPath(pathElements...), filepath.Separator)
}

func containerCpPath(containerID string, pathElements ...string) string {
	joined := strings.Join(pathElements, "/")
	return fmt.Sprintf("%s:%s", containerID, joined)
}

func containerCpPathTrailingSep(containerID string, pathElements ...string) string {
	return fmt.Sprintf("%s/", containerCpPath(containerID, pathElements...))
}

func runDockerCp(c *check.C, src, dst string, params []string) (err error) {
	c.Logf("running `docker cp %s %s %s`", strings.Join(params, " "), src, dst)

	args := []string{"cp"}

	args = append(args, params...)

	args = append(args, src, dst)

	out, _, err := runCommandWithOutput(exec.Command(dockerBinary, args...))
	if err != nil {
		err = fmt.Errorf("error executing `docker cp` command: %s: %s", err, out)
	}

	return
}

func startContainerGetOutput(c *check.C, containerID string) (out string, err error) {
	c.Logf("running `docker start -a %s`", containerID)

	args := []string{"start", "-a", containerID}

	out, _, err = runCommandWithOutput(exec.Command(dockerBinary, args...))
	if err != nil {
		err = fmt.Errorf("error executing `docker start` command: %s: %s", err, out)
	}

	return
}

func getTestDir(c *check.C, label string) (tmpDir string) {
	var err error

	tmpDir, err = ioutil.TempDir("", label)
	// unable to make temporary directory
	assert.NilError(c, err)

	return
}

func isCpDirNotExist(err error) bool {
	return strings.Contains(err.Error(), archive.ErrDirNotExists.Error())
}

func isCpCannotCopyDir(err error) bool {
	return strings.Contains(err.Error(), archive.ErrCannotCopyDir.Error())
}

func isCpCannotCopyReadOnly(err error) bool {
	return strings.Contains(err.Error(), "marked read-only")
}

func fileContentEquals(c *check.C, filename, contents string) (err error) {
	c.Logf("checking that file %q contains %q\n", filename, contents)

	fileBytes, err := ioutil.ReadFile(filename)
	if err != nil {
		return
	}

	expectedBytes, err := ioutil.ReadAll(strings.NewReader(contents))
	if err != nil {
		return
	}

	if !bytes.Equal(fileBytes, expectedBytes) {
		err = fmt.Errorf("file content not equal - expected %q, got %q", string(expectedBytes), string(fileBytes))
	}

	return
}

func symlinkTargetEquals(c *check.C, symlink, expectedTarget string) (err error) {
	c.Logf("checking that the symlink %q points to %q\n", symlink, expectedTarget)

	actualTarget, err := os.Readlink(symlink)
	if err != nil {
		return
	}

	if actualTarget != expectedTarget {
		err = fmt.Errorf("symlink target points to %q not %q", actualTarget, expectedTarget)
	}

	return
}

func containerStartOutputEquals(c *check.C, containerID, contents string) (err error) {
	c.Logf("checking that container %q start output contains %q\n", containerID, contents)

	out, err := startContainerGetOutput(c, containerID)
	if err != nil {
		return
	}

	if out != contents {
		err = fmt.Errorf("output contents not equal - expected %q, got %q", contents, out)
	}

	return
}

func defaultVolumes(tmpDir string) []string {
	if testEnv.IsLocalDaemon() {
		return []string{
			"/vol1",
			fmt.Sprintf("%s:/vol2", tmpDir),
			fmt.Sprintf("%s:/vol3", filepath.Join(tmpDir, "vol3")),
			fmt.Sprintf("%s:/vol_ro:ro", filepath.Join(tmpDir, "vol_ro")),
		}
	}

	// Can't bind-mount volumes with separate host daemon.
	return []string{"/vol1", "/vol2", "/vol3", "/vol_ro:/vol_ro:ro"}
}
