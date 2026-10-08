// Package pruneprogress defines explicit backend options for prune operations.
package pruneprogress

import (
	"github.com/moby/moby/api/types/prune"
	"github.com/moby/moby/v2/daemon/internal/filters"
)

// Options contains filters and an optional synchronous object sink.
type Options struct {
	Filters  filters.Args
	Progress func(prune.Object)
}

// Notify reports a successful deletion, if the request has a reporter.
func Notify(report func(prune.Object), id string, action prune.Action) {
	if report != nil {
		report(prune.Object{ID: id, Action: action})
	}
}
