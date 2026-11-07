/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package version

import (
	"fmt"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
)

const (
	// UnknownVersion is reported for failed version detection.
	UnknownVersion = "0.0.0-unknown"
	// DevelVersion is what we get from debug/build info when building
	// plugins within the NRI repository.
	DevelVersion = "(devel)"
	// nriModulePath is the module we look for to discover the NRI version.
	nriModulePath = "github.com/containerd/nri"
)

// version represents a struct type that holds relevant data
// that constitute a Semantic Version
type version struct {
	major string
	minor string
	patch string
	pre   string
}

func (v version) String() string {
	if v.pre != "" {
		return fmt.Sprintf("v%s.%s.%s-%s", v.major, v.minor, v.patch, v.pre)
	}
	return fmt.Sprintf("v%s.%s.%s", v.major, v.minor, v.patch)
}

// compareVersion parses two semver strings into the "version" struct type and compares them.
// NOTE: It ignores the build metadata when making the comparison
func compareVersion(a, b string) int {
	aVer, errA := parseVersion(a)
	bVer, errB := parseVersion(b)
	// Like semver.Compare, order invalid versions before valid ones
	// and consider any two invalid versions equal.
	switch {
	case errA != nil && errB != nil:
		return 0
	case errA != nil:
		return -1
	case errB != nil:
		return 1
	}

	if c := compareInt(aVer.major, bVer.major); c != 0 {
		return c
	}
	if c := compareInt(aVer.minor, bVer.minor); c != 0 {
		return c
	}
	if c := compareInt(aVer.patch, bVer.patch); c != 0 {
		return c
	}
	return comparePrerelease(aVer.pre, bVer.pre)
}

func compareInt(x, y string) int {
	if x == y {
		return 0
	}
	if len(x) < len(y) {
		return -1
	}
	if len(x) > len(y) {
		return 1
	}
	if x < y {
		return -1
	}
	return 1
}

func comparePrerelease(x, y string) int {
	if x == y {
		return 0
	}
	if x == "" {
		return 1
	}
	if y == "" {
		return -1
	}
	for x != "" && y != "" {
		var dx, dy string
		dx, x, _ = strings.Cut(x, ".")
		dy, y, _ = strings.Cut(y, ".")
		if dx != dy {
			ix := isNum(dx)
			iy := isNum(dy)
			if ix != iy {
				if ix {
					return -1
				}
				return 1
			}
			if ix {
				if len(dx) < len(dy) {
					return -1
				}
				if len(dx) > len(dy) {
					return 1
				}
			}
			if dx < dy {
				return -1
			}
			return 1
		}
	}
	if x == "" {
		return -1
	}
	return 1
}

func isNum(v string) bool {
	if len(v) == 0 {
		return false
	}
	i := 0
	for i < len(v) && '0' <= v[i] && v[i] <= '9' {
		i++
	}
	return i == len(v)
}

// parseVersion parses a semantic version string. Like the semver package it replaces, it requires a leading "v"
// and accepts the shorthand forms v<Major> and v<Major>.<Minor> (pre-release and build metadata are only allowed
// on a full v<Major>.<Minor>.<Patch> version). Build metadata is validated but discarded.
// parseVersion is inspired by the internal parse written in the upstream "golang.org/x/mod/semver" package
// https://cs.opensource.google/go/x/mod/+/master:semver/semver.go;l=178
func parseVersion(s string) (version, error) {
	rest, ok := strings.CutPrefix(s, "v")
	if !ok {
		return version{}, fmt.Errorf("invalid version %q: missing 'v' prefix", s)
	}

	rest, build, hasBuild := strings.Cut(rest, "+")
	if hasBuild {
		if err := validateIdentifiers(build, false); err != nil {
			return version{}, fmt.Errorf("invalid build metadata in %q: %w", s, err)
		}
	}

	core, pre, hasPre := strings.Cut(rest, "-")
	if hasPre {
		if err := validateIdentifiers(pre, true); err != nil {
			return version{}, fmt.Errorf("invalid pre-release in %q: %w", s, err)
		}
	}

	parts := strings.Split(core, ".")
	if len(parts) > 3 {
		return version{}, fmt.Errorf("invalid version %q: too many components", s)
	}
	if len(parts) < 3 && (hasPre || hasBuild) {
		return version{}, fmt.Errorf("invalid version %q: pre-release or build metadata requires major.minor.patch", s)
	}

	var nums [3]string
	for i, part := range parts {
		n, err := parseNumber(part)
		if err != nil {
			return version{}, fmt.Errorf("invalid version %q: %w", s, err)
		}
		nums[i] = n
	}
	for i := range nums {
		if nums[i] == "" {
			nums[i] = "0"
		}
	}

	return version{major: nums[0], minor: nums[1], patch: nums[2], pre: pre}, nil
}

// parseNumber parses a non-negative decimal number without leading zeros.
func parseNumber(s string) (string, error) {
	if s == "" || !isNum(s) {
		return "", fmt.Errorf("invalid number %q", s)
	}
	if len(s) > 1 && s[0] == '0' {
		return "", fmt.Errorf("number %q has a leading zero", s)
	}
	return s, nil
}

// validateIdentifiers checks a dot-separated list of pre-release or build identifiers:
// each must be non-empty and consist of [0-9A-Za-z-]. Numeric pre-release identifiers must not have leading zeros.
func validateIdentifiers(s string, pre bool) error {
	for id := range strings.SplitSeq(s, ".") {
		if id == "" {
			return fmt.Errorf("empty identifier in %q", s)
		}
		for i := 0; i < len(id); i++ {
			c := id[i]
			if !isValidIdentifierChar(c) {
				return fmt.Errorf("invalid character %q in identifier %q", c, id)
			}
		}
		if pre && isNum(id) && len(id) > 1 && id[0] == '0' {
			return fmt.Errorf("numeric identifier %q has a leading zero", id)
		}
	}
	return nil
}

func isValidIdentifierChar(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '-'
}

// GetFromBuildInfo returns the locally used NRI version. This
// is taken either from the debug/build info provided by the
// golang runtime, or for plugins hosted in the NRI repository
// from a git-described version generated at build time.
func GetFromBuildInfo() string {
	version := UnknownVersion

	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, mod := range bi.Deps {
			if mod.Path != nriModulePath {
				continue
			}

			if mod.Replace != nil && mod.Replace.Version != DevelVersion {
				version = mod.Replace.Version
			} else {
				version = mod.Version
			}
		}
	}

	if version == DevelVersion {
		return fallbackVersion()
	}

	return version
}

// majorMinorPatch returns the major.minor.patch prefix of the semantic version v.
func majorMinorPatch(v version) string {
	return fmt.Sprintf("v%s.%s.%s", v.major, v.minor, v.patch)
}

// FindClosestMatch returns the largest version smaller or equal to a given one.
// "" is returned if no such version if found.
func FindClosestMatch(v string, versions []string) string {
	// Note: A git-described version suffix (-N-gSHA1[.*])) is not semantically
	// semver-correct as semver considers it a prerelease identifier. Therefore
	// semver for instance considers v2.2.0-225-ge9dc15b7a.m < v2.2.0, which is
	// obviously not the case. In lack of a better choice, we strip any such
	// suffix from v before comparison.
	v = stripGitSuffix(v)

	slices.SortFunc(versions, compareVersion)

	latest := ""
	for _, ver := range versions {
		if compareVersion(ver, v) > 0 {
			break
		}
		latest = ver
	}
	return latest
}

// stripGitSuffix strips any git described suffix from a version string.
// We expect a valid git suffix to be of the form "-N-gSHA1[.m], where
// N is an decimal integer and SHA1 is a hexadecimal integer.
func stripGitSuffix(version string) string {
	pv, _ := parseVersion(version)
	mmp := majorMinorPatch(pv)
	if pv.String() != version {
		return version
	}

	pre := pv.pre
	if len(pre) == 0 {
		return version
	}

	commits, gsha1, ok := strings.Cut(pre, "-")
	if !ok || len(gsha1) == 0 || gsha1[0] != 'g' {
		return version
	}
	if _, err := strconv.ParseUint(commits, 10, 64); err != nil {
		return version
	}

	sha1, _, _ := strings.Cut(gsha1[1:], ".")
	if _, err := strconv.ParseUint(sha1, 16, 64); err != nil {
		return version
	}

	return mmp
}
