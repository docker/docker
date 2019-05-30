// Package capabilities allows to generically handle capabilities.
package capabilities // import "github.com/docker/docker/pkg/capabilities"

// Set represents a set of capabilities.
type Set map[string]struct{}

// Match tries to match set with caps, which is an OR list of AND lists of capabilities.
// The matched AND list of capabilities is returned; or nil if none are matched.
func (set Set) Match(caps [][]string) []string {
	if set == nil {
		return nil
	}
anyof:
	for _, andList := range caps {
		for _, cap := range andList {
			if _, ok := set[cap]; !ok {
				continue anyof
			}
		}
		return andList
	}
	// match anything
	return nil
}
