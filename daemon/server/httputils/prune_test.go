package httputils

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/prune"
	"github.com/moby/moby/v2/daemon/internal/pruneprogress"
	"github.com/moby/moby/v2/pkg/authorization"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestPruneResponseCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, version, query string
		stream               bool
	}{
		{name: "default", version: "1.57"},
		{name: "disabled", version: "1.57", query: "?stream=false"},
		{name: "old API", version: "1.55", query: "?stream=true"},
		{name: "previous API", version: "1.56", query: "?stream=true"},
		{name: "stream", version: "1.57", query: "?stream=true", stream: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(t.Context(), APIVersionKey{}, tc.version)
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/containers/prune"+tc.query, http.NoBody)
			assert.NilError(t, ParseForm(r))
			expected := container.PruneReport{ContainersDeleted: []string{"removed"}, SpaceReclaimed: 42}
			err := WritePruneResponse(ctx, w, r, func(ctx context.Context, progressSink func(prune.Object)) (any, error) {
				assert.Equal(t, progressSink == nil, !tc.stream)
				pruneprogress.Notify(progressSink, "removed", "deleted")
				return expected, nil
			})
			assert.NilError(t, err)
			dec := json.NewDecoder(w.Body)
			var report container.PruneReport
			if tc.stream {
				assert.Equal(t, w.Header().Get("Content-Type"), "application/jsonl")
				var progress, final prune.Event[json.RawMessage]
				assert.NilError(t, dec.Decode(&progress))
				assert.Equal(t, progress.Object.ID, "removed")
				assert.Equal(t, progress.Object.Action, prune.Deleted)
				assert.NilError(t, dec.Decode(&final))
				assert.Assert(t, final.Report != nil)
				assert.NilError(t, json.Unmarshal(*final.Report, &report))
			} else {
				assert.Equal(t, w.Header().Get("Content-Type"), "application/json")
				assert.NilError(t, dec.Decode(&report))
			}
			assert.DeepEqual(t, report, expected)
			assert.ErrorIs(t, dec.Decode(new(any)), io.EOF)
		})
	}
}

func TestPruneResponseFlushesBeforeCompletion(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), APIVersionKey{}, "1.57")
		assert.NilError(t, ParseForm(r))
		err := WritePruneResponse(ctx, w, r, func(ctx context.Context, progressSink func(prune.Object)) (any, error) {
			pruneprogress.Notify(progressSink, "removed", "deleted")
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return container.PruneReport{ContainersDeleted: []string{"removed"}}, nil
		})
		assert.Check(t, is.Nil(err))
	}))
	defer server.Close()
	defer close(release)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(server.URL+"?stream=true", "", nil)
	assert.NilError(t, err)
	defer resp.Body.Close()
	var message prune.Event[json.RawMessage]
	assert.NilError(t, json.NewDecoder(resp.Body).Decode(&message))
	assert.Equal(t, message.Object.ID, "removed")
	assert.Assert(t, message.Report == nil)
}

func TestPruneResponseEmpty(t *testing.T) {
	ctx := context.WithValue(t.Context(), APIVersionKey{}, "1.57")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/containers/prune?stream=true", http.NoBody)
	assert.NilError(t, ParseForm(r))
	assert.NilError(t, WritePruneResponse(ctx, w, r, func(context.Context, func(prune.Object)) (any, error) {
		return container.PruneReport{}, nil
	}))
	var message prune.Event[json.RawMessage]
	assert.NilError(t, json.NewDecoder(w.Body).Decode(&message))
	assert.Assert(t, message.Report != nil)
}

type pruneAuthorizationPlugin struct {
	onResponse func(*authorization.Request) (*authorization.Response, error)
}

func (p pruneAuthorizationPlugin) Name() string { return "prune-test" }
func (p pruneAuthorizationPlugin) AuthZRequest(*authorization.Request) (*authorization.Response, error) {
	return &authorization.Response{Allow: true}, nil
}
func (p pruneAuthorizationPlugin) AuthZResponse(r *authorization.Request) (*authorization.Response, error) {
	return p.onResponse(r)
}

func TestPruneResponseAuthorization(t *testing.T) {
	for _, allow := range []bool{true, false} {
		t.Run(fmt.Sprintf("allow=%t", allow), func(t *testing.T) {
			ctx := context.WithValue(t.Context(), APIVersionKey{}, "1.57")
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/containers/prune?stream=true", http.NoBody)
			assert.NilError(t, ParseForm(r))
			expected := container.PruneReport{ContainersDeleted: []string{"removed"}, SpaceReclaimed: 42}
			plugin := pruneAuthorizationPlugin{onResponse: func(r *authorization.Request) (*authorization.Response, error) {
				assert.Equal(t, w.Body.Len(), 0, "response must not reach the client before authorization")
				assert.Assert(t, !w.Flushed)
				assert.Equal(t, r.ResponseStatusCode, http.StatusOK)
				assert.Equal(t, r.ResponseHeaders["Content-Type"], "application/json")
				var report container.PruneReport
				assert.NilError(t, json.Unmarshal(r.ResponseBody, &report))
				assert.DeepEqual(t, report, expected)
				return &authorization.Response{Allow: allow, Msg: "response denied"}, nil
			}}
			auth := authorization.NewCtx([]authorization.Plugin{plugin}, "", "", r.Method, r.RequestURI)
			assert.NilError(t, auth.AuthZRequest(w, r))
			wrapped := authorization.NewResponseModifier(w)
			assert.NilError(t, WritePruneResponse(ctx, wrapped, r, func(ctx context.Context, progressSink func(prune.Object)) (any, error) {
				pruneprogress.Notify(progressSink, "removed", "deleted")
				assert.Equal(t, w.Body.Len(), 0)
				return expected, nil
			}))
			err := auth.AuthZResponse(wrapped, r)
			if !allow {
				assert.ErrorContains(t, err, "response denied")
				assert.Equal(t, w.Body.Len(), 0)
				return
			}
			assert.NilError(t, err)
			var report container.PruneReport
			assert.NilError(t, json.NewDecoder(w.Body).Decode(&report))
			assert.DeepEqual(t, report, expected)
		})
	}
}

func TestPruneResponseErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		progress bool
	}{
		{name: "before progress"},
		{name: "after progress", progress: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(t.Context(), APIVersionKey{}, "1.57")
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/containers/prune?stream=true", http.NoBody)
			assert.NilError(t, ParseForm(r))
			expected := errors.New("prune failed")
			err := WritePruneResponse(ctx, w, r, func(ctx context.Context, progressSink func(prune.Object)) (any, error) {
				if tc.progress {
					pruneprogress.Notify(progressSink, "removed", "deleted")
				}
				return nil, expected
			})
			if !tc.progress {
				assert.ErrorIs(t, err, expected)
				assert.Equal(t, w.Body.Len(), 0)
				return
			}
			assert.NilError(t, err)
			dec := json.NewDecoder(w.Body)
			var first, last prune.Event[json.RawMessage]
			assert.NilError(t, dec.Decode(&first))
			assert.NilError(t, dec.Decode(&last))
			assert.Assert(t, last.Error != nil)
			assert.Equal(t, last.Error.Message, expected.Error())
			assert.Assert(t, last.Report == nil)
		})
	}
}

type failingPruneWriter struct {
	header http.Header
	writes int
}

func (w *failingPruneWriter) Header() http.Header { return w.header }
func (w *failingPruneWriter) WriteHeader(int)     {}
func (w *failingPruneWriter) Flush()              {}
func (w *failingPruneWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, io.ErrClosedPipe
}

func TestPruneResponseWriteFailureCancels(t *testing.T) {
	ctx := context.WithValue(t.Context(), APIVersionKey{}, "1.57")
	w := &failingPruneWriter{header: make(http.Header)}
	r := httptest.NewRequest(http.MethodPost, "/containers/prune?stream=true", http.NoBody)
	assert.NilError(t, ParseForm(r))
	assert.NilError(t, WritePruneResponse(ctx, w, r, func(ctx context.Context, progressSink func(prune.Object)) (any, error) {
		pruneprogress.Notify(progressSink, "removed", "deleted")
		assert.ErrorIs(t, ctx.Err(), context.Canceled)
		pruneprogress.Notify(progressSink, "another", "deleted")
		return container.PruneReport{}, nil
	}))
	assert.Equal(t, w.writes, 1)
}

func TestPruneResponseStalledReader(t *testing.T) {
	// Use a real HTTP connection and leave its body unread until the server's
	// send buffer fills. A blocked write must cancel pruning without requiring
	// the client to disconnect. The request outlives the per-write timeout.
	done := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), APIVersionKey{}, "1.57")
		assert.Check(t, is.Nil(ParseForm(r)))
		err := WritePruneResponse(ctx, w, r, func(ctx context.Context, progressSink func(prune.Object)) (any, error) {
			for range 1_000_000 {
				pruneprogress.Notify(progressSink, strings.Repeat("a", 64), "deleted")
				if ctx.Err() != nil {
					done <- ctx.Err()
					return nil, ctx.Err()
				}
			}
			done <- errors.New("reader did not stall")
			return container.PruneReport{}, nil
		})
		assert.Check(t, is.Nil(err))
	}))
	defer server.Close()
	resp, err := http.Post(server.URL+"?stream=true", "", nil)
	assert.NilError(t, err)
	defer resp.Body.Close()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(45 * time.Second):
		resp.Body.Close()
		t.Fatal("stalled reader held prune beyond its write deadline")
	}
}
