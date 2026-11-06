package stream

import (
	"io"
	"slices"
	"sync"
)

// unbuffered accumulates multiple io.WriteCloser by stream.
type unbuffered struct {
	mu      sync.Mutex
	writers []io.WriteCloser
}

// Add adds new io.WriteCloser.
func (w *unbuffered) Add(writer io.WriteCloser) {
	w.mu.Lock()
	w.writers = append(w.writers, writer)
	w.mu.Unlock()
}

// Write writes bytes to all writers. Failed writers will be evicted during
// this call.
//
// The actual writes happen with mu released: a writer can legitimately block
// (e.g. a bytespipe applying backpressure to a client that stopped reading),
// and holding mu across that would make Clean unable to ever acquire it, since
// Clean is what closes the writer that would otherwise unblock the write.
func (w *unbuffered) Write(p []byte) (int, error) {
	w.mu.Lock()
	writers := slices.Clone(w.writers)
	w.mu.Unlock()

	var failed []io.WriteCloser
	for _, sw := range writers {
		if n, err := sw.Write(p); err != nil || n != len(p) {
			// On error, evict the writer
			failed = append(failed, sw)
		}
	}
	if len(failed) > 0 {
		w.mu.Lock()
		w.writers = slices.DeleteFunc(w.writers, func(sw io.WriteCloser) bool {
			return slices.Contains(failed, sw)
		})
		w.mu.Unlock()
	}
	return len(p), nil
}

// Clean closes and removes all writers. Last non-eol-terminated part of data
// will be saved.
func (w *unbuffered) Clean() error {
	w.mu.Lock()
	writers := w.writers
	w.writers = nil
	w.mu.Unlock()

	for _, sw := range writers {
		sw.Close()
	}
	return nil
}
