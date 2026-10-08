package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/prune"
	"gotest.tools/v3/assert"
)

func TestPruneResponse(t *testing.T) {
	for _, tc := range []struct {
		name, body, contentType, errorText string
		objects                            int
		fallback                           bool
	}{
		{name: "stream", body: "{\"object\":{\"id\":\"removed\",\"action\":\"deleted\"}}\n{\"report\":{\"ContainersDeleted\":[\"removed\"],\"SpaceReclaimed\":42}}", objects: 1},
		{name: "fallback", body: `{"ContainersDeleted":["removed"],"SpaceReclaimed":42}`, fallback: true},
		{name: "empty", body: `{"report":{}}`},
		{name: "missing final", body: `{"object":{"id":"removed","action":"deleted"}}`, objects: 1, errorText: "unexpected EOF"},
		{name: "invalid report", body: `{"report":{"SpaceReclaimed":"bad"}}`, errorText: "cannot unmarshal"},
		{name: "daemon error", body: `{"error":{"message":"operation failed"}}`, errorText: "operation failed"},
		{name: "ambiguous", body: `{"object":{"id":"removed"},"report":{}}`, errorText: "expected one"},
		{name: "invalid JSON", body: `{`, errorText: "unexpected EOF"},
		{name: "missing object id", body: `{"object":{"action":"deleted"}}`, errorText: "invalid prune object"},
		{name: "invalid action", body: `{"object":{"id":"removed","action":"started"}}`, errorText: "invalid prune object"},
		{name: "unknown event", body: `{}`, errorText: "expected one"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			closed := false
			cli, err := New(WithAPIVersion("1.57"), WithMockClient(func(req *http.Request) (*http.Response, error) {
				requests++
				assert.Equal(t, req.URL.Query().Get("stream"), "true")
				contentType := "application/jsonl"
				if tc.fallback {
					contentType = "application/json"
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: &pruneTestBody{Reader: strings.NewReader(tc.body), closed: &closed}}, nil
			}))
			assert.NilError(t, err)
			response, err := cli.ContainerPrune(t.Context(), ContainerPruneOptions{Stream: true})
			assert.NilError(t, err)
			assert.Assert(t, !closed)
			var objects []prune.Object
			for object, err := range response.Events(t.Context()) {
				if err != nil {
					assert.ErrorContains(t, err, tc.errorText)
					break
				}
				objects = append(objects, object)
			}
			assert.Equal(t, len(objects), tc.objects)
			report, err := response.Wait(t.Context())
			if tc.errorText != "" {
				assert.ErrorContains(t, err, tc.errorText)
			} else {
				assert.NilError(t, err)
				if tc.name != "empty" {
					assert.Equal(t, report.SpaceReclaimed, uint64(42))
				}
			}
			assert.Assert(t, closed)
			assert.Equal(t, requests, 1)
		})
	}
}

type pruneTestBody struct {
	io.Reader
	closed *bool
}

func (b *pruneTestBody) Close() error { *b.closed = true; return nil }

func TestPruneProgressRequiresVersion(t *testing.T) {
	for _, version := range []string{"1.55", "1.56"} {
		for _, endpoint := range []string{"containers", "images", "networks", "volumes", "build"} {
			t.Run(version+"/"+endpoint, func(t *testing.T) {
				cli, err := New(WithAPIVersion(version), WithMockClient(func(*http.Request) (*http.Response, error) {
					t.Fatal("destructive request sent")
					return nil, nil
				}))
				assert.NilError(t, err)
				switch endpoint {
				case "containers":
					_, err = cli.ContainerPrune(t.Context(), ContainerPruneOptions{Stream: true})
				case "images":
					_, err = cli.ImagePrune(t.Context(), ImagePruneOptions{Stream: true})
				case "networks":
					_, err = cli.NetworkPrune(t.Context(), NetworkPruneOptions{Stream: true})
				case "volumes":
					_, err = cli.VolumePrune(t.Context(), VolumePruneOptions{Stream: true})
				case "build":
					_, err = cli.BuildCachePrune(t.Context(), BuildCachePruneOptions{Stream: true})
				}
				assert.ErrorContains(t, err, "requires API version 1.57")
			})
		}
	}
}

func TestPruneResponseEarlyBreakCancels(t *testing.T) {
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/jsonl")
		_, _ = io.WriteString(w, "{\"object\":{\"id\":\"removed\",\"action\":\"deleted\"}}\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()
	cli, err := New(WithHost(server.URL), WithAPIVersion("1.57"), WithHTTPClient(server.Client()))
	assert.NilError(t, err)
	response, err := cli.ContainerPrune(t.Context(), ContainerPruneOptions{Stream: true})
	assert.NilError(t, err)
	for object, err := range response.Events(t.Context()) {
		assert.NilError(t, err)
		assert.Equal(t, object.ID, "removed")
		break
	}
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("early break did not cancel request")
	}
	_, err = response.Wait(t.Context())
	assert.ErrorIs(t, err, context.Canceled)
	assert.NilError(t, response.Close())
}

func TestPruneResponseIterationContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/jsonl")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	cli, err := New(WithHost(server.URL), WithAPIVersion("1.57"), WithHTTPClient(server.Client()))
	assert.NilError(t, err)
	response, err := cli.ContainerPrune(t.Context(), ContainerPruneOptions{Stream: true})
	assert.NilError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = response.Wait(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestPruneResponseLegacyMethods(t *testing.T) {
	result := ContainerPruneResult{Report: container.PruneReport{SpaceReclaimed: 42}}
	for range result.Events(t.Context()) {
		t.Fatal("completed result emitted progress")
	}
	report, err := result.Wait(t.Context())
	assert.NilError(t, err)
	assert.Equal(t, report.SpaceReclaimed, uint64(42))
	assert.NilError(t, result.Close())
}
