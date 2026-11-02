// SPDX-FileCopyrightText: Copyright The Moby Authors
// SPDX-License-Identifier: Apache-2.0

package apparmor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

// profileDirectory is the file store for AppArmor profiles and macros.
const profileDirectory = "/etc/apparmor.d"

// generate creates an AppArmor profile from ProfileData.
func generate(p *profileData, out io.Writer, macroExistsFn func(string) bool) error {
	compiled, err := template.New("apparmor_profile").Parse(baseTemplate)
	if err != nil {
		return err
	}

	if p.daemonProfile == "" {
		p.daemonProfile = "unconfined"
	}

	const abi = "abi/3.0"
	if macroExistsFn(abi) {
		p.abi = abi
	}

	if macroExistsFn("tunables/global") {
		p.imports = append(p.imports, "#include <tunables/global>")
	} else {
		p.imports = append(p.imports, "@{PROC}=/proc/")
	}

	if macroExistsFn("abstractions/base") {
		p.innerImports = append(p.innerImports, "#include <abstractions/base>")
	}

	return compiled.Execute(out, p)
}

// macroExists checks if the passed macro exists.
func macroExists(m string) bool {
	_, err := os.Stat(filepath.Join(profileDirectory, m))
	return err == nil
}

// InstallDefault generates a default profile, then loads the profile into the
// kernel using 'apparmor_parser'.
func InstallDefault(name string) error {
	return installDefault(context.Background(), name)
}

func installDefault(ctx context.Context, name string) error {
	// Figure out the daemon profile.
	var daemonProfile string
	if currentProfile, err := os.ReadFile("/proc/self/attr/current"); err == nil {
		// Profile may be either "<profile> (<mode>)", e.g. "foo (enforce)"
		// or a bare profile (e.g. "unconfined"). Use cleanProfileName to
		// only get the profile name.
		daemonProfile = cleanProfileName(string(currentProfile))
	}

	p := profileData{
		name:          name,
		daemonProfile: daemonProfile,
	}

	var buf bytes.Buffer
	if err := generate(&p, &buf, macroExists); err != nil {
		return err
	}

	return loadProfile(ctx, &buf)
}

// IsLoaded checks if a profile with the given name has been loaded into the
// kernel.
func IsLoaded(name string) (bool, error) {
	return isLoaded(name, "/sys/kernel/security/apparmor/profiles")
}

func isLoaded(name string, fileName string) (bool, error) {
	f, err := os.Open(fileName)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// Entries are of the form "<profile> (<mode>)", e.g. "foo (enforce)".
		// Profile names may contain spaces (quoted names are supported in AppArmor);
		// use splitCon to correctly handle profile names containing spaces and/or parentheses.
		label, _ := splitCon(scanner.Text())
		if label == name {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return false, nil
}

// loadProfile runs "apparmor_parser -Kr", providing the AppArmor profile on
// stdin to replace the profile. The "-K" is necessary to make sure that
// apparmor_parser doesn't try to write to a read-only filesystem.
func loadProfile(ctx context.Context, profile io.Reader) error {
	c := exec.CommandContext(ctx, "apparmor_parser", "-Kr")
	c.Stdin = profile
	if out, err := c.CombinedOutput(); err != nil {
		return fmt.Errorf("running '%s' failed with output: %s\nerror: %w", c, out, err)
	}

	return nil
}

// cleanProfileName returns the AppArmor profile name from a confinement
// context as reported by /proc/self/attr/current.
//
// The value may be either a bare profile name, "unconfined", or a profile name
// with a trailing mode suffix of the form " (<mode>)". If profile is empty,
// cleanProfileName returns "unconfined".
func cleanProfileName(profile string) string {
	label, _ := splitCon(profile)
	if label == "" {
		return "unconfined"
	}
	return label
}

// splitCon splits an AppArmor confinement context into a label and mode,
// similar to libapparmor [splitcon]. splitCon follows libapparmor's parsing
// semantics and does not validate the returned mode.
//
// /proc/self/attr/current returns the current confinement context for the
// process. Unlike /sys/kernel/security/apparmor/profiles, this value may not
// include a " (<mode>)" suffix.
//
// Supported forms:
//
//	<profile>
//	<profile> (<mode>)
//	unconfined
//
// splitCon strips one trailing newline before parsing.
//
// [splitcon]: https://gitlab.com/apparmor/apparmor/-/blob/v5.0.1/libraries/libapparmor/src/kernel.c#L562-615
func splitCon(con string) (label, mode string) {
	// Value includes a trailing newline.
	con = strings.TrimSuffix(con, "\n")
	if con == "" || con == "unconfined" {
		return con, ""
	}

	if strings.HasSuffix(con, ")") {
		// Profile names may contain spaces, so split on the last " (" before
		// the trailing ")" rather than the first space.
		if i := strings.LastIndex(con[:len(con)-1], " ("); i >= 0 {
			return con[:i], con[i+2 : len(con)-1]
		}
	}

	return con, ""
}
