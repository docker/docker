package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/testutil"
	"github.com/pkg/errors"
	"gotest.tools/v3/icmd"
)

func getPrefixAndSlashFromDaemonPlatform() (prefix, slash string) {
	if testEnv.OSType == "windows" {
		return "c:", `\`
	}
	return "", "/"
}

// dPath converts linux absolute paths to Windows absolute paths if the daemon
// is running on Windows
func dPath(path string) string {
	if testEnv.OSType == "windows" {
		return `c:` + strings.ReplaceAll(path, "/", `\`)
	}
	return path
}

// TODO: update code to call cmd.RunCmd directly, and remove this function
// Deprecated: use gotest.tools/icmd
func runCommandWithOutput(execCmd *exec.Cmd) (string, int, error) {
	result := icmd.RunCmd(transformCmd(execCmd))
	return result.Combined(), result.ExitCode, result.Error
}

// Temporary shim for migrating commands to the new function
func transformCmd(execCmd *exec.Cmd) icmd.Cmd {
	return icmd.Cmd{
		Command: execCmd.Args,
		Env:     execCmd.Env,
		Dir:     execCmd.Dir,
		Stdin:   execCmd.Stdin,
		Stdout:  execCmd.Stdout,
	}
}

// ParseCgroupPaths parses 'procCgroupData', which is output of '/proc/<pid>/cgroup', and returns
// a map which cgroup name as key and path as value.
func ParseCgroupPaths(procCgroupData string) map[string]string {
	cgroupPaths := map[string]string{}
	for _, line := range strings.Split(procCgroupData, "\n") {
		parts := strings.Split(line, ":")
		if len(parts) != 3 {
			continue
		}
		cgroupPaths[parts[1]] = parts[2]
	}
	return cgroupPaths
}

// RandomTmpDirPath provides a temporary path with rand string appended.
// does not create or checks if it exists.
func RandomTmpDirPath(s string, platform string) string {
	// TODO: why doesn't this use os.TempDir() ?
	tmp := "/tmp"
	if platform == "windows" {
		tmp = os.Getenv("TEMP")
	}
	path := filepath.Join(tmp, fmt.Sprintf("%s.%s", s, testutil.GenerateRandomAlphaOnlyString(10)))
	if platform == "windows" {
		return filepath.FromSlash(path) // Using \
	}
	return filepath.ToSlash(path) // Using /
}

// RunCommandPipelineWithOutput runs the array of commands with the output
// of each pipelined with the following (like cmd1 | cmd2 | cmd3 would do).
// It returns the final output, the exitCode different from 0 and the error
// if something bad happened.
// Deprecated: use icmd instead
func RunCommandPipelineWithOutput(cmds ...*exec.Cmd) (output string, err error) {
	if len(cmds) < 2 {
		return "", errors.New("pipeline does not have multiple cmds")
	}

	// connect stdin of each cmd to stdout pipe of previous cmd
	for i, cmd := range cmds {
		if i > 0 {
			prevCmd := cmds[i-1]
			cmd.Stdin, err = prevCmd.StdoutPipe()

			if err != nil {
				return "", fmt.Errorf("cannot set stdout pipe for %s: %v", cmd.Path, err)
			}
		}
	}

	// start all cmds except the last
	for _, cmd := range cmds[:len(cmds)-1] {
		if err = cmd.Start(); err != nil {
			return "", fmt.Errorf("starting %s failed with error: %v", cmd.Path, err)
		}
	}

	defer func() {
		var pipeErrMsgs []string
		// wait all cmds except the last to release their resources
		for _, cmd := range cmds[:len(cmds)-1] {
			if pipeErr := cmd.Wait(); pipeErr != nil {
				pipeErrMsgs = append(pipeErrMsgs, fmt.Sprintf("command %s failed with error: %v", cmd.Path, pipeErr))
			}
		}
		if len(pipeErrMsgs) > 0 && err == nil {
			err = fmt.Errorf("pipelineError from Wait: %v", strings.Join(pipeErrMsgs, ", "))
		}
	}()

	// wait on last cmd
	out, err := cmds[len(cmds)-1].CombinedOutput()
	return string(out), err
}

type elementListOptions struct {
	element, format string
}

func existingElements(c *testing.T, opts elementListOptions) []string {
	var args []string
	switch opts.element {
	case "container":
		args = append(args, "ps", "-a")
	case "image":
		args = append(args, "images", "-a")
	case "network":
		args = append(args, "network", "ls")
	case "plugin":
		args = append(args, "plugin", "ls")
	case "volume":
		args = append(args, "volume", "ls")
	}
	if opts.format != "" {
		args = append(args, "--format", opts.format)
	}
	out, _ := dockerCmd(c, args...)
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// ExistingContainerIDs returns a list of currently existing container IDs.
func ExistingContainerIDs(c *testing.T) []string {
	return existingElements(c, elementListOptions{element: "container", format: "{{.ID}}"})
}

// ExistingContainerNames returns a list of existing container names.
func ExistingContainerNames(c *testing.T) []string {
	return existingElements(c, elementListOptions{element: "container", format: "{{.Names}}"})
}

// RemoveLinesForExistingElements removes existing elements from the output of a
// docker command.
// This function takes an output []string and returns a []string.
func RemoveLinesForExistingElements(output, existing []string) []string {
	for _, e := range existing {
		index := -1
		for i, line := range output {
			if strings.Contains(line, e) {
				index = i
				break
			}
		}
		if index != -1 {
			output = append(output[:index], output[index+1:]...)
		}
	}
	return output
}

// RemoveOutputForExistingElements removes existing elements from the output of
// a docker command.
// This function takes an output string and returns a string.
func RemoveOutputForExistingElements(output string, existing []string) string {
	res := RemoveLinesForExistingElements(strings.Split(output, "\n"), existing)
	return strings.Join(res, "\n")
}
