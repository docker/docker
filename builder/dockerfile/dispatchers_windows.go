package dockerfile // import "github.com/docker/docker/builder/dockerfile"

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/system"
	"github.com/moby/buildkit/frontend/dockerfile/instructions"
)

var pattern = regexp.MustCompile(`^[a-zA-Z]:\.$`)

// normalizeWorkdir normalizes a user requested working directory in a
// platform semantically consistent way.
func normalizeWorkdir(platform string, current string, requested string) (string, error) {
	if platform == "" {
		platform = "windows"
	}
	if platform == "windows" {
		return normalizeWorkdirWindows(current, requested)
	}
	return normalizeWorkdirUnix(current, requested)
}

// normalizeWorkdirUnix normalizes a user requested working directory in a
// platform semantically consistent way.
func normalizeWorkdirUnix(current string, requested string) (string, error) {
	if requested == "" {
		return "", errors.New("cannot normalize nothing")
	}
	current = strings.Replace(current, string(os.PathSeparator), "/", -1)
	requested = strings.Replace(requested, string(os.PathSeparator), "/", -1)
	if !path.IsAbs(requested) {
		return path.Join(`/`, current, requested), nil
	}
	return requested, nil
}

// normalizeWorkdirWindows normalizes a user requested working directory in a
// platform semantically consistent way.
func normalizeWorkdirWindows(current string, requested string) (string, error) {
	if requested == "" {
		return "", errors.New("cannot normalize nothing")
	}

	// `filepath.Clean` will replace "" with "." so skip in that case
	if current != "" {
		current = filepath.Clean(current)
	}
	if requested != "" {
		requested = filepath.Clean(requested)
	}

	// If either current or requested in Windows is:
	// C:
	// C:.
	// then an error will be thrown as the definition for the above
	// refers to `current directory on drive C:`
	// Since filepath.Clean() will automatically normalize the above
	// to `C:.`, we only need to check the last format
	if pattern.MatchString(current) {
		return "", fmt.Errorf("%s is not a directory. If you are specifying a drive letter, please add a trailing '\\'", current)
	}
	if pattern.MatchString(requested) {
		return "", fmt.Errorf("%s is not a directory. If you are specifying a drive letter, please add a trailing '\\'", requested)
	}

	// Target semantics is C:\somefolder, specifically in the format:
	// UPPERCASEDriveLetter-Colon-Backslash-FolderName. We are already
	// guaranteed that `current`, if set, is consistent. This allows us to
	// cope correctly with any of the following in a Dockerfile:
	//	WORKDIR a                       --> C:\a
	//	WORKDIR c:\\foo                 --> C:\foo
	//	WORKDIR \\foo                   --> C:\foo
	//	WORKDIR /foo                    --> C:\foo
	//	WORKDIR c:\\foo \ WORKDIR bar   --> C:\foo --> C:\foo\bar
	//	WORKDIR C:/foo \ WORKDIR bar    --> C:\foo --> C:\foo\bar
	//	WORKDIR C:/foo \ WORKDIR \\bar  --> C:\foo --> C:\bar
	//	WORKDIR /foo \ WORKDIR c:/bar   --> C:\foo --> C:\bar
	if len(current) == 0 || system.IsAbs(requested) {
		if (requested[0] == os.PathSeparator) ||
			(len(requested) > 1 && string(requested[1]) != ":") ||
			(len(requested) == 1) {
			requested = filepath.Join(`C:\`, requested)
		}
	} else {
		requested = filepath.Join(current, requested)
	}
	// Upper-case drive letter
	return (strings.ToUpper(string(requested[0])) + requested[1:]), nil
}

// resolveCmdLine takes a command line arg set and optionally prepends a platform-specific
// shell in front of it. It returns either an array of arguments and an indication that
// the arguments are not yet escaped; Or, an array containing a single command line element
// along with an indication that the arguments are escaped so the runtime shouldn't escape.
//
// A better solution could be made, but it would be exceptionally invasive throughout
// many parts of the daemon which are coded assuming Linux args array only only, not taking
// account of Windows-natural command line semantics and it's argv handling. Put another way,
// while what is here is good-enough, it could be improved, but would be highly invasive.
//
// The commands when this function is called are RUN, ENTRYPOINT and CMD.
func resolveCmdLine(cmd instructions.ShellDependantCmdLine, runConfig *container.Config, os, command, original string) ([]string, bool) {

	// Make sure we return an empty array if there is no cmd.CmdLine
	if len(cmd.CmdLine) == 0 {
		return []string{}, runConfig.ArgsEscaped
	}

	if os == "windows" { // ie WCOW
		if cmd.PrependShell {
			// WCOW shell-form. Return a single-element array containing the original command line prepended with the shell.
			// Also indicate that it has not been escaped (so will be passed through directly to HCS). Note that
			// we go back to the original un-parsed command line in the dockerfile line, strip off both the command part of
			// it (RUN/ENTRYPOINT/CMD), and also strip any leading white space. IOW, we deliberately ignore any prior parsing
			// so as to ensure it is treated exactly as a command line. For those interested, `RUN mkdir "c:/foo"` is a particularly
			// good example of why this is necessary if you fancy debugging how cmd.exe and its builtin mkdir works. (Windows
			// doesn't have a mkdir.exe, and I'm guessing cmd.exe has some very long unavoidable and unchangeable historical
			// design decisions over how both its built-in echo and mkdir are coded. Probably more too.)
			original = original[len(command):]               // Strip off the command
			original = strings.TrimLeft(original, " \t\v\n") // Strip of leading whitespace
			return []string{strings.Join(getShell(runConfig, os), " ") + " " + original}, true
		}

		// WCOW JSON/"exec" form.
		return cmd.CmdLine, false
	}

	// LCOW - use args as an array, same as LCOL.
	if cmd.PrependShell && cmd.CmdLine != nil {
		return append(getShell(runConfig, os), cmd.CmdLine...), false
	}
	return cmd.CmdLine, false
}
