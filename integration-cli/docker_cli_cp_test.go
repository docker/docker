package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-check/check"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/icmd"
)

const (
	cpTestPathParent = "/some"
	cpTestPath       = "/some/path"
	cpTestName       = "test"
	cpFullPath       = "/some/path/test"

	cpContainerContents = "holla, i am the container"
	cpHostContents      = "hello, i am the host"
)

// Ensure that an all-local path case returns an error.
func (s *DockerSuite) TestCpLocalOnly(c *check.C) {
	err := runDockerCp(c, "foo", "bar", nil)
	assert.ErrorContains(c, err, "must specify at least one container source")
}

// Test for #5656
// Check that garbage paths don't escape the container's rootfs
func (s *DockerSuite) TestCpGarbagePath(c *check.C) {
	out, _ := dockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", "mkdir -p '"+cpTestPath+"' && echo -n '"+cpContainerContents+"' > "+cpFullPath)

	containerID := strings.TrimSpace(out)

	out, _ = dockerCmd(c, "wait", containerID)
	assert.Equal(c, strings.TrimSpace(out), "0", "failed to set up container")
	assert.NilError(c, os.MkdirAll(cpTestPath, os.ModeDir))

	hostFile, err := os.Create(cpFullPath)
	assert.NilError(c, err)
	defer hostFile.Close()
	defer os.RemoveAll(cpTestPathParent)

	fmt.Fprintf(hostFile, "%s", cpHostContents)

	tmpdir, err := ioutil.TempDir("", "docker-integration")
	assert.NilError(c, err)

	tmpname := filepath.Join(tmpdir, cpTestName)
	defer os.RemoveAll(tmpdir)

	path := path.Join("../../../../../../../../../../../../", cpFullPath)

	dockerCmd(c, "cp", containerID+":"+path, tmpdir)

	file, _ := os.Open(tmpname)
	defer file.Close()

	test, err := ioutil.ReadAll(file)
	assert.NilError(c, err)
	assert.Assert(c, string(test) != cpHostContents, "output matched host file -- garbage path can escape container rootfs")
	assert.Assert(c, string(test) == cpContainerContents, "output doesn't match the input for garbage path")
}

// Check that relative paths are relative to the container's rootfs
func (s *DockerSuite) TestCpRelativePath(c *check.C) {
	out, _ := dockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", "mkdir -p '"+cpTestPath+"' && echo -n '"+cpContainerContents+"' > "+cpFullPath)

	containerID := strings.TrimSpace(out)

	out, _ = dockerCmd(c, "wait", containerID)
	assert.Equal(c, strings.TrimSpace(out), "0", "failed to set up container")
	assert.NilError(c, os.MkdirAll(cpTestPath, os.ModeDir))

	hostFile, err := os.Create(cpFullPath)
	assert.NilError(c, err)
	defer hostFile.Close()
	defer os.RemoveAll(cpTestPathParent)

	fmt.Fprintf(hostFile, "%s", cpHostContents)

	tmpdir, err := ioutil.TempDir("", "docker-integration")
	assert.NilError(c, err)

	tmpname := filepath.Join(tmpdir, cpTestName)
	defer os.RemoveAll(tmpdir)

	var relPath string
	if path.IsAbs(cpFullPath) {
		// normally this is `filepath.Rel("/", cpFullPath)` but we cannot
		// get this unix-path manipulation on windows with filepath.
		relPath = cpFullPath[1:]
	}
	assert.Assert(c, path.IsAbs(cpFullPath), "path %s was assumed to be an absolute path", cpFullPath)

	dockerCmd(c, "cp", containerID+":"+relPath, tmpdir)

	file, _ := os.Open(tmpname)
	defer file.Close()

	test, err := ioutil.ReadAll(file)
	assert.NilError(c, err)
	assert.Assert(c, string(test) != cpHostContents, "output matched host file -- relative path can escape container rootfs")
	assert.Assert(c, string(test) == cpContainerContents, "output doesn't match the input for relative path")
}

// Check that absolute paths are relative to the container's rootfs
func (s *DockerSuite) TestCpAbsolutePath(c *check.C) {
	out, _ := dockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", "mkdir -p '"+cpTestPath+"' && echo -n '"+cpContainerContents+"' > "+cpFullPath)

	containerID := strings.TrimSpace(out)

	out, _ = dockerCmd(c, "wait", containerID)
	assert.Equal(c, strings.TrimSpace(out), "0", "failed to set up container")
	assert.NilError(c, os.MkdirAll(cpTestPath, os.ModeDir))

	hostFile, err := os.Create(cpFullPath)
	assert.NilError(c, err)
	defer hostFile.Close()
	defer os.RemoveAll(cpTestPathParent)

	fmt.Fprintf(hostFile, "%s", cpHostContents)

	tmpdir, err := ioutil.TempDir("", "docker-integration")
	assert.NilError(c, err)

	tmpname := filepath.Join(tmpdir, cpTestName)
	defer os.RemoveAll(tmpdir)

	path := cpFullPath

	dockerCmd(c, "cp", containerID+":"+path, tmpdir)

	file, _ := os.Open(tmpname)
	defer file.Close()

	test, err := ioutil.ReadAll(file)
	assert.NilError(c, err)
	assert.Assert(c, string(test) != cpHostContents, "output matched host file -- absolute path can escape container rootfs")
	assert.Assert(c, string(test) == cpContainerContents, "output doesn't match the input for absolute path")
}

// Test for #5619
// Check that absolute symlinks are still relative to the container's rootfs
func (s *DockerSuite) TestCpAbsoluteSymlink(c *check.C) {
	testRequires(c, DaemonIsLinux)
	out, _ := dockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", "mkdir -p '"+cpTestPath+"' && echo -n '"+cpContainerContents+"' > "+cpFullPath+" && ln -s "+cpFullPath+" container_path")

	containerID := strings.TrimSpace(out)

	out, _ = dockerCmd(c, "wait", containerID)
	assert.Equal(c, strings.TrimSpace(out), "0", "failed to set up container")

	assert.NilError(c, os.MkdirAll(cpTestPath, os.ModeDir))

	hostFile, err := os.Create(cpFullPath)
	assert.NilError(c, err)
	defer hostFile.Close()
	defer os.RemoveAll(cpTestPathParent)

	fmt.Fprintf(hostFile, "%s", cpHostContents)

	tmpdir, err := ioutil.TempDir("", "docker-integration")
	assert.NilError(c, err)

	tmpname := filepath.Join(tmpdir, "container_path")
	defer os.RemoveAll(tmpdir)

	path := path.Join("/", "container_path")

	dockerCmd(c, "cp", containerID+":"+path, tmpdir)

	// We should have copied a symlink *NOT* the file itself!
	linkTarget, err := os.Readlink(tmpname)
	assert.NilError(c, err)
	assert.Equal(c, linkTarget, filepath.FromSlash(cpFullPath))
}

// Check that symlinks to a directory behave as expected when copying one from
// a container.
func (s *DockerSuite) TestCpFromSymlinkToDirectory(c *check.C) {
	testRequires(c, DaemonIsLinux)
	out, _ := dockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", "mkdir -p '"+cpTestPath+"' && echo -n '"+cpContainerContents+"' > "+cpFullPath+" && ln -s "+cpTestPathParent+" /dir_link")

	containerID := strings.TrimSpace(out)

	out, _ = dockerCmd(c, "wait", containerID)
	assert.Equal(c, strings.TrimSpace(out), "0", "failed to set up container")

	testDir, err := ioutil.TempDir("", "test-cp-from-symlink-to-dir-")
	assert.NilError(c, err)
	defer os.RemoveAll(testDir)

	// This copy command should copy the symlink, not the target, into the
	// temporary directory.
	dockerCmd(c, "cp", containerID+":"+"/dir_link", testDir)

	expectedPath := filepath.Join(testDir, "dir_link")
	linkTarget, err := os.Readlink(expectedPath)
	assert.NilError(c, err)

	assert.Equal(c, linkTarget, filepath.FromSlash(cpTestPathParent))

	os.Remove(expectedPath)

	// This copy command should resolve the symlink (note the trailing
	// separator), copying the target into the temporary directory.
	dockerCmd(c, "cp", containerID+":"+"/dir_link/", testDir)

	// It *should not* have copied the directory using the target's name, but
	// used the given name instead.
	unexpectedPath := filepath.Join(testDir, cpTestPathParent)
	stat, err := os.Lstat(unexpectedPath)
	if err == nil {
		out = fmt.Sprintf("target name was copied: %q - %q", stat.Mode(), stat.Name())
	}
	assert.ErrorContains(c, err, "", out)

	// It *should* have copied the directory using the asked name "dir_link".
	stat, err = os.Lstat(expectedPath)
	assert.NilError(c, err, "unable to stat resource at %q", expectedPath)
	assert.Assert(c, stat.IsDir(), "should have copied a directory but got %q instead", stat.Mode())
}

// Check that symlinks to a directory behave as expected when copying one to a
// container.
func (s *DockerSuite) TestCpToSymlinkToDirectory(c *check.C) {
	testRequires(c, DaemonIsLinux)
	testRequires(c, testEnv.IsLocalDaemon) // Requires local volume mount bind.

	testVol, err := ioutil.TempDir("", "test-cp-to-symlink-to-dir-")
	assert.NilError(c, err)
	defer os.RemoveAll(testVol)

	// Create a test container with a local volume. We will test by copying
	// to the volume path in the container which we can then verify locally.
	out, _ := dockerCmd(c, "create", "-v", testVol+":/testVol", "busybox")

	containerID := strings.TrimSpace(out)

	// Create a temp directory to hold a test file nested in a directory.
	testDir, err := ioutil.TempDir("", "test-cp-to-symlink-to-dir-")
	assert.NilError(c, err)
	defer os.RemoveAll(testDir)

	// This file will be at "/testDir/some/path/test" and will be copied into
	// the test volume later.
	hostTestFilename := filepath.Join(testDir, cpFullPath)
	assert.NilError(c, os.MkdirAll(filepath.Dir(hostTestFilename), os.FileMode(0700)))
	assert.NilError(c, ioutil.WriteFile(hostTestFilename, []byte(cpHostContents), os.FileMode(0600)))

	// Now create another temp directory to hold a symlink to the
	// "/testDir/some" directory.
	linkDir, err := ioutil.TempDir("", "test-cp-to-symlink-to-dir-")
	assert.NilError(c, err)
	defer os.RemoveAll(linkDir)

	// Then symlink "/linkDir/dir_link" to "/testdir/some".
	linkTarget := filepath.Join(testDir, cpTestPathParent)
	localLink := filepath.Join(linkDir, "dir_link")
	assert.NilError(c, os.Symlink(linkTarget, localLink))

	// Now copy that symlink into the test volume in the container.
	dockerCmd(c, "cp", localLink, containerID+":/testVol")

	// This copy command should have copied the symlink *not* the target.
	expectedPath := filepath.Join(testVol, "dir_link")
	actualLinkTarget, err := os.Readlink(expectedPath)
	assert.NilError(c, err, "unable to read symlink at %q", expectedPath)
	assert.Equal(c, actualLinkTarget, linkTarget)

	// Good, now remove that copied link for the next test.
	os.Remove(expectedPath)

	// This copy command should resolve the symlink (note the trailing
	// separator), copying the target into the test volume directory in the
	// container.
	dockerCmd(c, "cp", localLink+"/", containerID+":/testVol")

	// It *should not* have copied the directory using the target's name, but
	// used the given name instead.
	unexpectedPath := filepath.Join(testVol, cpTestPathParent)
	stat, err := os.Lstat(unexpectedPath)
	if err == nil {
		out = fmt.Sprintf("target name was copied: %q - %q", stat.Mode(), stat.Name())
	}
	assert.ErrorContains(c, err, "", out)

	// It *should* have copied the directory using the asked name "dir_link".
	stat, err = os.Lstat(expectedPath)
	assert.NilError(c, err, "unable to stat resource at %q", expectedPath)
	assert.Assert(c, stat.IsDir(), "should have copied a directory but got %q instead", stat.Mode())

	// And this directory should contain the file copied from the host at the
	// expected location: "/testVol/dir_link/path/test"
	expectedFilepath := filepath.Join(testVol, "dir_link/path/test")
	fileContents, err := ioutil.ReadFile(expectedFilepath)
	assert.NilError(c, err)
	assert.Equal(c, string(fileContents), cpHostContents)
}

// Test for #5619
// Check that symlinks which are part of the resource path are still relative to the container's rootfs
func (s *DockerSuite) TestCpSymlinkComponent(c *check.C) {
	testRequires(c, DaemonIsLinux)
	out, _ := dockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", "mkdir -p '"+cpTestPath+"' && echo -n '"+cpContainerContents+"' > "+cpFullPath+" && ln -s "+cpTestPath+" container_path")

	containerID := strings.TrimSpace(out)

	out, _ = dockerCmd(c, "wait", containerID)
	assert.Equal(c, strings.TrimSpace(out), "0", "failed to set up container")

	assert.NilError(c, os.MkdirAll(cpTestPath, os.ModeDir))

	hostFile, err := os.Create(cpFullPath)
	assert.NilError(c, err)
	defer hostFile.Close()
	defer os.RemoveAll(cpTestPathParent)

	fmt.Fprintf(hostFile, "%s", cpHostContents)

	tmpdir, err := ioutil.TempDir("", "docker-integration")

	assert.NilError(c, err)

	tmpname := filepath.Join(tmpdir, cpTestName)
	defer os.RemoveAll(tmpdir)

	path := path.Join("/", "container_path", cpTestName)

	dockerCmd(c, "cp", containerID+":"+path, tmpdir)

	file, _ := os.Open(tmpname)
	defer file.Close()

	test, err := ioutil.ReadAll(file)
	assert.NilError(c, err)
	assert.Assert(c, string(test) != cpHostContents, "output matched host file -- symlink path component can escape container rootfs")
	assert.Equal(c, string(test), cpContainerContents, "output doesn't match the input for symlink path component")
}

// Check that cp with unprivileged user doesn't return any error
func (s *DockerSuite) TestCpUnprivilegedUser(c *check.C) {
	testRequires(c, DaemonIsLinux, testEnv.IsLocalDaemon)
	testRequires(c, UnixCli) // uses chmod/su: not available on windows

	out, _ := dockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", "touch "+cpTestName)

	containerID := strings.TrimSpace(out)

	out, _ = dockerCmd(c, "wait", containerID)
	assert.Equal(c, strings.TrimSpace(out), "0", "failed to set up container")

	tmpdir, err := ioutil.TempDir("", "docker-integration")
	assert.NilError(c, err)

	defer os.RemoveAll(tmpdir)

	err = os.Chmod(tmpdir, 0777)
	assert.NilError(c, err)

	result := icmd.RunCommand("su", "unprivilegeduser", "-c",
		fmt.Sprintf("%s cp %s:%s %s", dockerBinary, containerID, cpTestName, tmpdir))
	result.Assert(c, icmd.Expected{})
}

func (s *DockerSuite) TestCpSpecialFiles(c *check.C) {
	testRequires(c, DaemonIsLinux)
	testRequires(c, testEnv.IsLocalDaemon)

	outDir, err := ioutil.TempDir("", "cp-test-special-files")
	assert.NilError(c, err)
	defer os.RemoveAll(outDir)

	out, _ := dockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", "touch /foo")

	containerID := strings.TrimSpace(out)

	out, _ = dockerCmd(c, "wait", containerID)
	assert.Equal(c, strings.TrimSpace(out), "0", "failed to set up container")

	// Copy actual /etc/resolv.conf
	dockerCmd(c, "cp", containerID+":/etc/resolv.conf", outDir)

	expected := readContainerFile(c, containerID, "resolv.conf")
	actual, err := ioutil.ReadFile(outDir + "/resolv.conf")
	assert.NilError(c, err)
	assert.Assert(c, bytes.Equal(actual, expected), "Expected copied file to be duplicate of the container resolvconf")

	// Copy actual /etc/hosts
	dockerCmd(c, "cp", containerID+":/etc/hosts", outDir)

	expected = readContainerFile(c, containerID, "hosts")
	actual, err = ioutil.ReadFile(outDir + "/hosts")
	assert.NilError(c, err)
	assert.Assert(c, bytes.Equal(actual, expected), "Expected copied file to be duplicate of the container hosts")

	// Copy actual /etc/resolv.conf
	dockerCmd(c, "cp", containerID+":/etc/hostname", outDir)

	expected = readContainerFile(c, containerID, "hostname")
	actual, err = ioutil.ReadFile(outDir + "/hostname")
	assert.NilError(c, err)
	assert.Assert(c, bytes.Equal(actual, expected), "Expected copied file to be duplicate of the container hostname")
}

func (s *DockerSuite) TestCpVolumePath(c *check.C) {
	//  stat /tmp/cp-test-volumepath851508420/test gets permission denied for the user
	testRequires(c, NotUserNamespace)
	testRequires(c, DaemonIsLinux)
	testRequires(c, testEnv.IsLocalDaemon)

	tmpDir, err := ioutil.TempDir("", "cp-test-volumepath")
	assert.NilError(c, err)
	defer os.RemoveAll(tmpDir)
	outDir, err := ioutil.TempDir("", "cp-test-volumepath-out")
	assert.NilError(c, err)
	defer os.RemoveAll(outDir)
	_, err = os.Create(tmpDir + "/test")
	assert.NilError(c, err)

	out, _ := dockerCmd(c, "run", "-d", "-v", "/foo", "-v", tmpDir+"/test:/test", "-v", tmpDir+":/baz", "busybox", "/bin/sh", "-c", "touch /foo/bar")

	containerID := strings.TrimSpace(out)

	out, _ = dockerCmd(c, "wait", containerID)
	assert.Equal(c, strings.TrimSpace(out), "0", "failed to set up container")

	// Copy actual volume path
	dockerCmd(c, "cp", containerID+":/foo", outDir)

	stat, err := os.Stat(outDir + "/foo")
	assert.NilError(c, err)
	assert.Assert(c, stat.IsDir(), "Expected copied content to be dir")

	stat, err = os.Stat(outDir + "/foo/bar")
	assert.NilError(c, err)
	assert.Assert(c, !stat.IsDir(), "Expected file `bar` to be a file")

	// Copy file nested in volume
	dockerCmd(c, "cp", containerID+":/foo/bar", outDir)

	stat, err = os.Stat(outDir + "/bar")
	assert.NilError(c, err)
	assert.Assert(c, !stat.IsDir(), "Expected file `bar` to be a file")

	// Copy Bind-mounted dir
	dockerCmd(c, "cp", containerID+":/baz", outDir)
	stat, err = os.Stat(outDir + "/baz")
	assert.NilError(c, err)
	assert.Assert(c, stat.IsDir(), "Expected `baz` to be a dir")

	// Copy file nested in bind-mounted dir
	dockerCmd(c, "cp", containerID+":/baz/test", outDir)
	fb, err := ioutil.ReadFile(outDir + "/baz/test")
	assert.NilError(c, err)
	fb2, err := ioutil.ReadFile(tmpDir + "/test")
	assert.NilError(c, err)
	assert.Assert(c, bytes.Equal(fb, fb2), "Expected copied file to be duplicate of bind-mounted file")

	// Copy bind-mounted file
	dockerCmd(c, "cp", containerID+":/test", outDir)
	fb, err = ioutil.ReadFile(outDir + "/test")
	assert.NilError(c, err)
	fb2, err = ioutil.ReadFile(tmpDir + "/test")
	assert.NilError(c, err)
	assert.Assert(c, bytes.Equal(fb, fb2), "Expected copied file to be duplicate of bind-mounted file")
}

func (s *DockerSuite) TestCpToDot(c *check.C) {
	out, _ := dockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", "echo lololol > /test")

	containerID := strings.TrimSpace(out)

	out, _ = dockerCmd(c, "wait", containerID)
	assert.Equal(c, strings.TrimSpace(out), "0", "failed to set up container")

	tmpdir, err := ioutil.TempDir("", "docker-integration")
	assert.NilError(c, err)
	defer os.RemoveAll(tmpdir)
	cwd, err := os.Getwd()
	assert.NilError(c, err)
	defer os.Chdir(cwd)
	err = os.Chdir(tmpdir)
	assert.NilError(c, err)

	dockerCmd(c, "cp", containerID+":/test", ".")
	content, err := ioutil.ReadFile("./test")
	assert.NilError(c, err)
	assert.Equal(c, string(content), "lololol\n")
}

func (s *DockerSuite) TestCpToStdout(c *check.C) {
	out, _ := dockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", "echo lololol > /test")

	containerID := strings.TrimSpace(out)

	out, _ = dockerCmd(c, "wait", containerID)
	assert.Equal(c, strings.TrimSpace(out), "0", "failed to set up container")

	out, err := RunCommandPipelineWithOutput(
		exec.Command(dockerBinary, "cp", containerID+":/test", "-"),
		exec.Command("tar", "-vtf", "-"))

	assert.NilError(c, err)
	assert.Check(c, is.Contains(out, "test"))
	assert.Check(c, is.Contains(out, "-rw"))
}

func (s *DockerSuite) TestCpNameHasColon(c *check.C) {
	testRequires(c, testEnv.IsLocalDaemon, DaemonIsLinux)

	out, _ := dockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", "echo lololol > /te:s:t")

	containerID := strings.TrimSpace(out)

	out, _ = dockerCmd(c, "wait", containerID)
	assert.Equal(c, strings.TrimSpace(out), "0", "failed to set up container")

	tmpdir, err := ioutil.TempDir("", "docker-integration")
	assert.NilError(c, err)
	defer os.RemoveAll(tmpdir)
	dockerCmd(c, "cp", containerID+":/te:s:t", tmpdir)
	content, err := ioutil.ReadFile(tmpdir + "/te:s:t")
	assert.NilError(c, err)
	assert.Equal(c, string(content), "lololol\n")
}

func (s *DockerSuite) TestCopyAndRestart(c *check.C) {
	testRequires(c, DaemonIsLinux)
	expectedMsg := "hello"
	out, _ := dockerCmd(c, "run", "-d", "busybox", "echo", expectedMsg)
	containerID := strings.TrimSpace(out)

	out, _ = dockerCmd(c, "wait", containerID)
	assert.Equal(c, strings.TrimSpace(out), "0", "failed to set up container")

	tmpDir, err := ioutil.TempDir("", "test-docker-restart-after-copy-")
	assert.NilError(c, err)
	defer os.RemoveAll(tmpDir)

	dockerCmd(c, "cp", fmt.Sprintf("%s:/etc/group", containerID), tmpDir)

	out, _ = dockerCmd(c, "start", "-a", containerID)
	assert.Equal(c, strings.TrimSpace(out), expectedMsg)
}

func (s *DockerSuite) TestCopyCreatedContainer(c *check.C) {
	testRequires(c, DaemonIsLinux)
	dockerCmd(c, "create", "--name", "test_cp", "-v", "/test", "busybox")

	tmpDir, err := ioutil.TempDir("", "test")
	assert.NilError(c, err)
	defer os.RemoveAll(tmpDir)
	dockerCmd(c, "cp", "test_cp:/bin/sh", tmpDir)
}

// test copy with option `-L`: following symbol link
// Check that symlinks to a file behave as expected when copying one from
// a container to host following symbol link
func (s *DockerSuite) TestCpSymlinkFromConToHostFollowSymlink(c *check.C) {
	testRequires(c, DaemonIsLinux)
	out, exitCode := dockerCmd(c, "run", "-d", "busybox", "/bin/sh", "-c", "mkdir -p '"+cpTestPath+"' && echo -n '"+cpContainerContents+"' > "+cpFullPath+" && ln -s "+cpFullPath+" /dir_link")
	assert.Equal(c, exitCode, 0, "failed to set up container: %s", out)

	cleanedContainerID := strings.TrimSpace(out)

	out, _ = dockerCmd(c, "wait", cleanedContainerID)
	assert.Equal(c, strings.TrimSpace(out), "0", "failed to set up container")

	testDir, err := ioutil.TempDir("", "test-cp-symlink-container-to-host-follow-symlink")
	assert.NilError(c, err)
	defer os.RemoveAll(testDir)

	// This copy command should copy the symlink, not the target, into the
	// temporary directory.
	dockerCmd(c, "cp", "-L", cleanedContainerID+":"+"/dir_link", testDir)

	expectedPath := filepath.Join(testDir, "dir_link")

	expected := []byte(cpContainerContents)
	actual, err := ioutil.ReadFile(expectedPath)
	assert.NilError(c, err)
	os.Remove(expectedPath)
	assert.Assert(c, bytes.Equal(actual, expected), "Expected copied file to be duplicate of the container symbol link target")

	// now test copy symbol link to a non-existing file in host
	expectedPath = filepath.Join(testDir, "somefile_host")
	// expectedPath shouldn't exist, if exists, remove it
	if _, err := os.Lstat(expectedPath); err == nil {
		os.Remove(expectedPath)
	}

	dockerCmd(c, "cp", "-L", cleanedContainerID+":"+"/dir_link", expectedPath)

	actual, err = ioutil.ReadFile(expectedPath)
	assert.NilError(c, err)
	defer os.Remove(expectedPath)
	assert.Assert(c, bytes.Equal(actual, expected), "Expected copied file to be duplicate of the container symbol link target")
}
