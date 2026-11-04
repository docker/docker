package filters

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/distribution/reference"
)

// ErrNotConvertible is returned by ReferenceToCtrdFilter when the pattern
// cannot be expressed as a containerd filter expression (e.g. when the name
// part contains glob characters that prevent safe normalization).
var ErrNotConvertible = errors.New("pattern cannot be converted to a containerd filter")

// ReferenceToCtrdFilter converts a Docker "reference" filter pattern to a
// containerd name filter expression.
//
// Containerd stores names in their canonical form (e.g.
// "docker.io/library/alpine:latest"). This function normalizes the name part
// of the pattern before building the filter expression.
//
// The returned expression is conservative: it may match more images than the
// Go-side reference filter would accept, but will never exclude images that
// would pass.
//
// Returns [ErrNotConvertible] if the pattern cannot be safely expressed as a
// containerd filter.
func ReferenceToCtrdFilter(pattern string) (string, error) {
	// '@' takes precedence over ':': "name@digest" must not be split at the
	// ':' inside the digest algorithm prefix (e.g. "sha256:...").
	var namePart, tagPart, sep string
	if i := strings.IndexByte(pattern, '@'); i >= 0 {
		namePart, tagPart, sep = pattern[:i], pattern[i+1:], "@"
	} else if i := strings.IndexByte(pattern, ':'); i >= 0 {
		namePart, tagPart, sep = pattern[:i], pattern[i+1:], ":"
	} else {
		namePart = pattern
	}

	// Glob characters in the name part prevent safe normalization.
	if strings.ContainsAny(namePart, "*?[") {
		return "", ErrNotConvertible
	}

	normalizedRef, err := reference.ParseNormalizedNamed(namePart)
	if err != nil {
		return "", ErrNotConvertible
	}
	canonicalName := normalizedRef.Name()

	switch sep {
	case "":
		// No tag/digest: match any tag or digest of this image.
		nameRegex := "^" + regexp.QuoteMeta(canonicalName) + "[:@]"
		return "name~=" + strconv.Quote(nameRegex), nil
	case "@":
		if strings.ContainsAny(tagPart, "*?[") {
			return "", ErrNotConvertible
		}
		return fmt.Sprintf("name==%q", canonicalName+"@"+tagPart), nil
	default: // ":"
		if !strings.ContainsAny(tagPart, "*?[") {
			return fmt.Sprintf("name==%q", canonicalName+":"+tagPart), nil
		}
		// Tag contains glob characters: convert to regex.
		tagRegex := "^" + regexp.QuoteMeta(canonicalName) + ":" + tagGlobToRegex(tagPart) + "$"
		return "name~=" + strconv.Quote(tagRegex), nil
	}
}

// BuildCtrdImageFilters translates Docker image filter args into containerd
// image store filter strings that can pre-filter before Go-side filtering.
//
// danglingPrefix is the name prefix used to identify dangling images in the
// containerd store (e.g. "moby-dangling@").
//
// Each returned string is a separate argument to the containerd image store
// List call; containerd treats multiple arguments as OR semantics, which
// naturally models Docker's reference filter where multiple values are also
// OR-ed.
//
// The returned expressions are conservative: they may match more images than
// the full Docker filter accepts, but will never exclude images that would pass.
func BuildCtrdImageFilters(imageFilters Args, danglingPrefix string) []string {
	var ctrdFilters []string

	// reference: each value is OR-ed, which matches containerd's per-arg OR semantics.
	for _, ref := range imageFilters.Get("reference") {
		f, err := ReferenceToCtrdFilter(ref)
		if err != nil {
			// Pattern cannot be pushed down; Go-side filtering will handle it.
			continue
		}
		ctrdFilters = append(ctrdFilters, f)
	}

	// dangling=true: only dangling images have names prefixed with danglingPrefix.
	if imageFilters.Contains("dangling") {
		if v, err := imageFilters.GetBoolOrDefault("dangling", false); err == nil && v {
			ctrdFilters = append(ctrdFilters, "name~="+strconv.Quote("^"+danglingPrefix))
		}
	}

	return ctrdFilters
}

// tagGlobToRegex converts a path.Match glob (tag position, no "/" separators)
// to a Go regexp string.
func tagGlobToRegex(glob string) string {
	var sb strings.Builder
	for _, ch := range glob {
		switch ch {
		case '*':
			sb.WriteString(".*")
		case '?':
			sb.WriteByte('.')
		case '[', ']':
			sb.WriteRune(ch)
		default:
			sb.WriteString(regexp.QuoteMeta(string(ch)))
		}
	}
	return sb.String()
}
