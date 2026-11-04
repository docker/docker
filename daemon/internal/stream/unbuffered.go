package stream

import (
	"io"
	"slices"
	"sync"
)

// writerEntry wraps a writer so eviction can use pointer identity instead of
// interface equality, which would panic on non-comparable dynamic types and
// would collapse eviction of distinct-but-equal writer values.
type writerEntry struct{ w io.WriteCloser }

// unbuffered accumulates multiple io.WriteCloser by stream.
//
// mu guards only the writers slice: no I/O call (Write or Close) is ever
// made while mu is held, so Clean is always bounded even if a writer's
// Write blocks (e.g. a bytespipe under backpressure). writeMu serializes
// Write calls so writes still reach writers in order; Clean never takes it.
type unbuffered struct {
	writeMu sync.Mutex

	mu      sync.Mutex
	writers []*writerEntry
}

// Add adds new io.WriteCloser.
func (w *unbuffered) Add(writer io.WriteCloser) {
	w.mu.Lock()
	w.writers = append(w.writers, &writerEntry{writer})
	w.mu.Unlock()
}

// Write writes bytes to all writers. Failed writers will be evicted during
// this call. The write to each writer happens without w.mu held, so a
// writer that blocks (e.g. backpressure on an attached client) does not
// prevent a concurrent Clean from closing writers and returning.
func (w *unbuffered) Write(p []byte) (int, error) {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	w.mu.Lock()
	writers := slices.Clone(w.writers)
	w.mu.Unlock()

	var failed []*writerEntry
	for _, e := range writers {
		if n, err := e.w.Write(p); err != nil || n != len(p) {
			// On error, evict the writer
			failed = append(failed, e)
		}
	}
	if len(failed) > 0 {
		w.mu.Lock()
		w.writers = slices.DeleteFunc(w.writers, func(e *writerEntry) bool {
			return slices.Contains(failed, e)
		})
		w.mu.Unlock()
	}
	return len(p), nil
}

// Clean closes and removes all writers. Last non-eol-terminated part of data
// will be saved. Writers are closed without w.mu held, so a writer whose
// Write is currently blocked in another goroutine does not make Clean
// block: closing it is expected to unblock that Write instead.
func (w *unbuffered) Clean() error {
	w.mu.Lock()
	writers := w.writers
	w.writers = nil
	w.mu.Unlock()

	for _, e := range writers {
		e.w.Close()
	}
	return nil
}
