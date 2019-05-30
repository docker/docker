// +build !windows

package dockerfile // import "github.com/docker/docker/builder/dockerfile"

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/docker/docker/api/types/container"
	"github.com/moby/buildkit/frontend/dockerfile/instructions"
)

// normalizeWorkdir normalizes a user requested working directory in a
// platform semantically consistent way.
func normalizeWorkdir(_ string, current string, requested string) (string, error) {
	if requested == "" {
		return "", errors.New("cannot normalize nothing")
	}
	current = filepath.FromSlash(current)
	requested = filepath.FromSlash(requested)
	if !filepath.IsAbs(requested) {
		return filepath.Join(string(os.PathSeparator), current, requested), nil
	}
	return requested, nil
}

// resolveCmdLine takes a command line arg set and optionally prepends a platform-specific
// shell in front of it.
func resolveCmdLine(cmd instructions.ShellDependantCmdLine, runConfig *container.Config, os, _, _ string) ([]string, bool) {
	result := cmd.CmdLine
	if cmd.PrependShell && result != nil {
		result = append(getShell(runConfig, os), result...)
	}
	return result, false
}
