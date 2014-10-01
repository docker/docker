package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestCommitAfterContainerIsDone(t *testing.T) {
	runCmd := exec.Command(dockerBinary, "run", "-i", "-a", "stdin", "busybox", "echo", "foo")
	out, _, _, err := runCommandWithStdoutStderr(runCmd)
	if err != nil {
		t.Fatalf("failed to run container: %s, %v", out, err)
	}

	cleanedContainerID := stripTrailingCharacters(out)

	waitCmd := exec.Command(dockerBinary, "wait", cleanedContainerID)
	if _, _, err = runCommandWithOutput(waitCmd); err != nil {
		t.Fatalf("error thrown while waiting for container: %s, %v", out, err)
	}

	commitCmd := exec.Command(dockerBinary, "commit", cleanedContainerID)
	out, _, err = runCommandWithOutput(commitCmd)
	if err != nil {
		t.Fatalf("failed to commit container to image: %s, %v", out, err)
	}

	cleanedImageID := stripTrailingCharacters(out)

	inspectCmd := exec.Command(dockerBinary, "inspect", cleanedImageID)
	if out, _, err = runCommandWithOutput(inspectCmd); err != nil {
		t.Fatalf("failed to inspect image: %s, %v", out, err)
	}

	deleteContainer(cleanedContainerID)
	deleteImages(cleanedImageID)

	logDone("commit - echo foo and commit the image")
}

func TestCommitWithoutPause(t *testing.T) {
	runCmd := exec.Command(dockerBinary, "run", "-i", "-a", "stdin", "busybox", "echo", "foo")
	out, _, _, err := runCommandWithStdoutStderr(runCmd)
	if err != nil {
		t.Fatalf("failed to run container: %s, %v", out, err)
	}

	cleanedContainerID := stripTrailingCharacters(out)

	waitCmd := exec.Command(dockerBinary, "wait", cleanedContainerID)
	if _, _, err = runCommandWithOutput(waitCmd); err != nil {
		t.Fatalf("error thrown while waiting for container: %s, %v", out, err)
	}

	commitCmd := exec.Command(dockerBinary, "commit", "-p=false", cleanedContainerID)
	out, _, err = runCommandWithOutput(commitCmd)
	if err != nil {
		t.Fatalf("failed to commit container to image: %s, %v", out, err)
	}

	cleanedImageID := stripTrailingCharacters(out)

	inspectCmd := exec.Command(dockerBinary, "inspect", cleanedImageID)
	if out, _, err = runCommandWithOutput(inspectCmd); err != nil {
		t.Fatalf("failed to inspect image: %s, %v", out, err)
	}

	deleteContainer(cleanedContainerID)
	deleteImages(cleanedImageID)

	logDone("commit - echo foo and commit the image with --pause=false")
}

func TestCommitNewFile(t *testing.T) {
	cmd := exec.Command(dockerBinary, "run", "--name", "commiter", "busybox", "/bin/sh", "-c", "echo koye > /foo")
	if _, err := runCommand(cmd); err != nil {
		t.Fatal(err)
	}

	cmd = exec.Command(dockerBinary, "commit", "commiter")
	imageID, _, err := runCommandWithOutput(cmd)
	if err != nil {
		t.Fatal(err)
	}
	imageID = strings.Trim(imageID, "\r\n")

	cmd = exec.Command(dockerBinary, "run", imageID, "cat", "/foo")

	out, _, err := runCommandWithOutput(cmd)
	if err != nil {
		t.Fatal(err, out)
	}
	if actual := strings.Trim(out, "\r\n"); actual != "koye" {
		t.Fatalf("expected output koye received %q", actual)
	}

	deleteAllContainers()
	deleteImages(imageID)

	logDone("commit - commit file and read")
}

func TestCommitTTY(t *testing.T) {
	cmd := exec.Command(dockerBinary, "run", "-t", "--name", "tty", "busybox", "/bin/ls")
	if _, err := runCommand(cmd); err != nil {
		t.Fatal(err)
	}

	cmd = exec.Command(dockerBinary, "commit", "tty", "ttytest")
	imageID, _, err := runCommandWithOutput(cmd)
	if err != nil {
		t.Fatal(err)
	}
	imageID = strings.Trim(imageID, "\r\n")

	cmd = exec.Command(dockerBinary, "run", "ttytest", "/bin/ls")
	if _, err := runCommand(cmd); err != nil {
		t.Fatal(err)
	}

	logDone("commit - commit tty")
}

func TestCommitWithHostBindMount(t *testing.T) {
	cmd := exec.Command(dockerBinary, "run", "--name", "bind-commit", "-v", "/dev/null:/winning", "busybox", "true")
	if _, err := runCommand(cmd); err != nil {
		t.Fatal(err)
	}

	cmd = exec.Command(dockerBinary, "commit", "bind-commit", "bindtest")
	imageID, _, err := runCommandWithOutput(cmd)
	if err != nil {
		t.Fatal(imageID, err)
	}

	imageID = strings.Trim(imageID, "\r\n")

	cmd = exec.Command(dockerBinary, "run", "bindtest", "true")

	if _, err := runCommand(cmd); err != nil {
		t.Fatal(err)
	}

	deleteAllContainers()
	deleteImages(imageID)

	logDone("commit - commit bind mounted file")
}

func TestCommitChange(t *testing.T) {
	defer deleteAllContainers()
	cmd(t, "run", "--name", "test", "busybox", "true")

	cmd := exec.Command(dockerBinary, "commit",
		"--change", "EXPOSE 8080",
		"--change", "ENV DEBUG true",
		"test", "test-commit")
	imageId, _, err := runCommandWithOutput(cmd)
	if err != nil {
		t.Fatal(imageId, err)
	}
	imageId = strings.Trim(imageId, "\r\n")
	defer deleteImages(imageId)

	expected := map[string]string{
		"Config.ExposedPorts": "map[8080/tcp:map[]]",
		"Config.Env":          "[DEBUG=true PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin]",
	}

	for conf, value := range expected {
		res, err := inspectField(imageId, conf)
		if err != nil {
			t.Errorf("failed to get value %s, error: %s", conf, err)
		}
		if res != value {
			t.Errorf("%s('%s'), expected %s", conf, res, value)
		}
	}

	logDone("commit - commit --change")
}
