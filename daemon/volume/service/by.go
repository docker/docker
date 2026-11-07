package service

import (
	"strings"

	"github.com/moby/moby/v2/daemon/internal/filters"
	"github.com/moby/moby/v2/daemon/volume"
)

// By is an interface which is used to implement filtering on volumes.
type By interface {
	isBy()
}

// ByDriver is `By` that filters based on the driver names that are passed in
func ByDriver(drivers ...string) By {
	return byDriver(drivers)
}

type byDriver []string

func (byDriver) isBy() {}

// ByReferenced is a `By` that filters based on if the volume has references
type ByReferenced bool

func (ByReferenced) isBy() {}

// And creates a `By` combining all the passed in bys using AND logic.
func And(bys ...By) By {
	and := make(andCombinator, 0, len(bys))
	for _, by := range bys {
		and = append(and, by)
	}
	return and
}

type andCombinator []By

func (andCombinator) isBy() {}

// Or creates a `By` combining all the passed in bys using OR logic.
func Or(bys ...By) By {
	or := make(orCombinator, 0, len(bys))
	for _, by := range bys {
		or = append(or, by)
	}
	return or
}

type orCombinator []By

func (orCombinator) isBy() {}

// CustomFilter is a `By` that is used by callers to provide custom filtering
// logic.
type CustomFilter filterFunc

func (CustomFilter) isBy() {}

// FromList returns a By which sets the initial list of volumes to use
func FromList(ls *[]volume.Volume, by By) By {
	return &fromList{by: by, ls: ls}
}

type fromList struct {
	by By
	ls *[]volume.Volume
}

func (fromList) isBy() {}

func byLabelFilter(filter filters.Args) By {
	return CustomFilter(func(v volume.Volume) bool {
		dv, ok := v.(volume.DetailedVolume)
		if !ok {
			return false
		}

		labels := dv.Labels()
		if !filter.MatchKVList("label", labels) {
			return false
		}
		if filter.Contains("label!") && matchesAnyLabel(filter.Get("label!"), labels) {
			return false
		}
		return true
	})
}
