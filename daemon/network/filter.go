package network

import (
	"slices"
	"time"

	"github.com/moby/moby/v2/daemon/internal/filters"
	"github.com/moby/moby/v2/daemon/internal/timestamp"
	"github.com/moby/moby/v2/errdefs"
	"github.com/pkg/errors"
)

var (
	acceptedFilters = map[string]bool{
		"dangling": true,
		"driver":   true,
		"id":       true,
		"label":    true,
		"name":     true,
		"scope":    true,
		"type":     true,
	}

	pruneFilters = map[string]bool{
		"label":  true,
		"label!": true,
		"until":  true,
	}
)

type Filter struct {
	args filters.Args

	filterByUse, danglingOnly bool
	until                     time.Time

	// IDAlsoMatchesName makes the "id" filter term also match against
	// network names.
	IDAlsoMatchesName bool
}

type FilterNetwork interface {
	Driver() string
	Name() string
	ID() string
	Labels() map[string]string
	Scope() string
	Created() time.Time
	HasContainerAttachments() bool
	HasServiceAttachments() bool
}

// NewFilter returns a network list filter that filters by the provided args.
//
// An [errdefs.InvalidParameter] error is returned if the filter args are not
// well-formed.
func NewFilter(args filters.Args) (Filter, error) {
	if err := args.Validate(acceptedFilters); err != nil {
		return Filter{}, err
	}
	return newFilter(args)
}

// NewPruneFilter returns a network prune filter that filters by the provided args.
//
// The filter matches dangling networks which also match args.
func NewPruneFilter(args filters.Args) (Filter, error) {
	if err := args.Validate(pruneFilters); err != nil {
		return Filter{}, err
	}
	f, err := newFilter(args)
	if err != nil {
		return Filter{}, err
	}
	f.filterByUse = true
	f.danglingOnly = true
	return f, nil
}

func newFilter(args filters.Args) (Filter, error) {
	var filterByUse, danglingOnly bool
	if values := args.Get("dangling"); len(values) > 0 {
		if len(values) > 1 {
			return Filter{}, errdefs.InvalidParameter(errors.New(`got more than one value for filter key "dangling"`))
		}

		filterByUse = true
		switch values[0] {
		case "0", "false":
			// dangling is false already
		case "1", "true":
			danglingOnly = true
		default:
			return Filter{}, errdefs.InvalidParameter(errors.New(`invalid value for filter 'dangling': must be "true" (or "1"), or "false" (or "0")`))
		}
	}
	if err := args.WalkValues("type", func(netType string) error {
		switch netType {
		case "builtin", "custom":
			return nil
		default:
			return errdefs.InvalidParameter(errors.New("invalid value for filter 'type': must be 'builtin' or 'custom'"))
		}
	}); err != nil {
		return Filter{}, err
	}
	var until time.Time
	if untilFilters := args.Get("until"); len(untilFilters) > 0 {
		if len(untilFilters) > 1 {
			return Filter{}, errdefs.InvalidParameter(errors.New("invalid filter: 'until' cannot be specified more than once"))
		}
		var err error
		until, err = timestamp.Parse(untilFilters[0], time.Now())
		if err != nil {
			return Filter{}, errdefs.InvalidParameter(err)
		}
	}
	return Filter{
		args:         args,
		filterByUse:  filterByUse,
		danglingOnly: danglingOnly,
		until:        until,
	}, nil
}

func (f Filter) Get(key string) []string {
	return f.args.Get(key)
}

// Matches returns true if nw satisfies the filter criteria.
func (f Filter) Matches(nw FilterNetwork) bool {
	if f.args.Contains("driver") &&
		!f.args.ExactMatch("driver", nw.Driver()) {
		return false
	}
	if f.args.Contains("name") &&
		!f.args.Match("name", nw.Name()) {
		return false
	}
	if f.args.Contains("id") &&
		!f.args.Match("id", nw.ID()) &&
		(!f.IDAlsoMatchesName || !f.args.Match("id", nw.Name())) {
		return false
	}
	if f.args.Contains("label") &&
		!f.args.MatchKVList("label", nw.Labels()) {
		return false
	}
	if f.args.Contains("label!") &&
		f.args.MatchKVList("label!", nw.Labels()) {
		return false
	}
	if f.args.Contains("scope") &&
		!f.args.ExactMatch("scope", nw.Scope()) {
		return false
	}
	if f.filterByUse &&
		!matchesUse(f.danglingOnly, nw) {
		return false
	}
	if netTypes := f.args.Get("type"); len(netTypes) > 0 &&
		!matchesType(netTypes, nw) {
		return false
	}
	if !f.until.IsZero() &&
		nw.Created().After(f.until) {
		return false
	}
	return true
}

func matchesUse(danglingOnly bool, nw FilterNetwork) bool {
	if danglingOnly {
		return !IsPredefined(nw.Name()) && !nw.HasContainerAttachments() && !nw.HasServiceAttachments()
	}
	return IsPredefined(nw.Name()) || nw.HasContainerAttachments() || nw.HasServiceAttachments()
}

func matchesType(netTypes []string, nw FilterNetwork) bool {
	if IsPredefined(nw.Name()) {
		return slices.Contains(netTypes, "builtin")
	}
	return slices.Contains(netTypes, "custom")
}
