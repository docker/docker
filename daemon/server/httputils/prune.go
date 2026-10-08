package httputils

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/moby/moby/api/types/prune"
	"github.com/moby/moby/v2/daemon/internal/versions"
	"github.com/moby/moby/v2/pkg/authorization"
)

// WritePruneResponse runs a prune operation and writes its report. API v1.57
// clients may opt into a JSON Lines stream with stream=true. Progress is scoped
// to this request; the last message contains the complete report or an error.
// Before the first progress message, errors retain their normal HTTP status.
func WritePruneResponse(ctx context.Context, w http.ResponseWriter, r *http.Request, run func(context.Context, func(prune.Object)) (any, error)) error {
	// Response authorization runs after the handler returns. Keep its single
	// JSON response buffered so plugins can inspect and deny it before any
	// content reaches the client. Flushing progress would bypass that decision.
	_, authorizedResponse := w.(authorization.ResponseModifier)
	if authorizedResponse || versions.LessThan(VersionFromContext(ctx), "1.57") || !BoolValue(r, "stream") {
		report, err := run(ctx, nil)
		if err != nil {
			return err
		}
		return WriteJSON(w, http.StatusOK, report)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	controller := http.NewResponseController(w)
	// Bound each network write, rather than the entire operation: removing an
	// object may legitimately take a long time, but a stalled reader must not
	// hold prune locks indefinitely. Clear the deadline between notifications.
	const writeTimeout = 30 * time.Second
	var mu sync.Mutex
	var started bool
	var writeErr error
	write := func(message prune.Event[any]) {
		if writeErr != nil {
			return
		}
		if !started {
			w.Header().Set("Content-Type", "application/jsonl")
			started = true
		}
		writeErr = controller.SetWriteDeadline(time.Now().Add(writeTimeout))
		if errors.Is(writeErr, http.ErrNotSupported) {
			writeErr = nil
		}
		if writeErr == nil {
			writeErr = enc.Encode(message)
		}
		if writeErr == nil {
			writeErr = controller.Flush()
		}
		_ = controller.SetWriteDeadline(time.Time{})
		if writeErr != nil {
			cancel()
		}
	}
	reporter := func(progress prune.Object) {
		mu.Lock()
		defer mu.Unlock()
		write(prune.Event[any]{Object: &progress})
	}
	report, err := run(ctx, reporter)
	mu.Lock()
	defer mu.Unlock()
	if writeErr != nil {
		// The response has already started; returning an error would append a
		// second, incompatible HTTP error body to the stream.
		return nil
	}
	if err != nil {
		if !started {
			return err
		}
		write(prune.Event[any]{Error: &prune.Error{Message: err.Error()}})
		return nil
	}
	_, err = json.Marshal(report)
	if err != nil {
		if !started {
			return err
		}
		write(prune.Event[any]{Error: &prune.Error{Message: err.Error()}})
		return nil
	}
	write(prune.Event[any]{Report: &report})
	return nil
}
