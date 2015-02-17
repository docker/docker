package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestImportDisplay(t *testing.T) {
	runCmd := exec.Command(dockerBinary, "run", "-d", "busybox", "true")
	out, _, err := runCommandWithOutput(runCmd)
	if err != nil {
		t.Fatal("failed to create a container", out, err)
	}
	cleanedContainerID := stripTrailingCharacters(out)
	defer deleteContainer(cleanedContainerID)

	out, _, err = runCommandPipelineWithOutput(
		exec.Command(dockerBinary, "export", cleanedContainerID),
		exec.Command(dockerBinary, "import", "-"),
	)
	if err != nil {
		t.Errorf("import failed with errors: %v, output: %q", err, out)
	}

	// TODO: remove check after drop of go1.2 support
	var expected int
	if strings.HasPrefix(daemonGoVersion, "go1.2") {
		expected = 2
	} else {
		expected = 1
	}
	if n := strings.Count(out, "\n"); n != expected {
		t.Fatalf("display is messed up: %d '\\n' instead of %d, go version: %s", n, expected, daemonGoVersion)
	}
	image := strings.TrimSpace(out)
	defer deleteImages(image)

	runCmd = exec.Command(dockerBinary, "run", "--rm", image, "true")
	out, _, err = runCommandWithOutput(runCmd)
	if err != nil {
		t.Fatal("failed to create a container", out, err)
	}

	if out != "" {
		t.Fatalf("command output should've been nothing, was %q", out)
	}

	logDone("import - display is fine, imported image runs")
}
