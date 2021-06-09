package fileutils // import "github.com/docker/docker/pkg/fileutils"

import "github.com/docker/docker/filematch"

// PatternMatcher allows checking paths against a list of patterns
// Deprecated: use github.com/docker/docker/filematch.PatternMatcher
type PatternMatcher = filematch.PatternMatcher

// Pattern defines a single regexp used to filter file paths.
// Deprecated: use github.com/docker/docker/filematch.Pattern
type Pattern = filematch.Pattern

var (
	// NewPatternMatcher creates a new matcher object for specific patterns that can
	// be used later to match against patterns against paths
	// Deprecated: use github.com/docker/docker/filematch.NewPatternMatcher
	NewPatternMatcher = filematch.NewPatternMatcher

	// Matches returns true if file matches any of the patterns
	// and isn't excluded by any of the subsequent patterns.
	// Deprecated: use github.com/docker/docker/filematch.Matches
	Matches = filematch.Matches
)
