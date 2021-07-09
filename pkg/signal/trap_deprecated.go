package signal // import "github.com/docker/docker/pkg/signal"

import msignal "github.com/moby/sys/signal"

var (
	// Trap sets up a simplified signal "trap", appropriate for common
	// behavior expected from a vanilla unix command-line tool in general
	// (and the Docker engine in particular).
	// Deprecated: use github.com/moby/sys/signal.Trap instead
	Trap = msignal.Trap

	// DumpStacks appends the runtime stack into file in dir and returns full path
	// to that file.
	// Deprecated: use github.com/moby/sys/signal.DumpStacks instead
	DumpStacks = msignal.DumpStacks
)
