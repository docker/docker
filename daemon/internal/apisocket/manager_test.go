package apisocket

import (
	"bufio"
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestManagerLifecycle(t *testing.T) {
	// t.TempDir paths can exceed the unix socket path limit on macOS.
	dir, err := os.MkdirTemp("", "apisocket")
	assert.NilError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "api.sock")
	ctx := t.Context()

	m := NewManager()
	assert.NilError(t, m.Open(ctx, path))
	assert.NilError(t, m.Open(ctx, path), "opening an open socket again must be a no-op")

	c := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
	t.Cleanup(c.CloseIdleConnections)
	status := func() int {
		t.Helper()
		resp, err := c.Get("http://api/_ping")
		assert.NilError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	assert.Check(t, is.Equal(status(), http.StatusServiceUnavailable), "requests before Start must fail")

	m.Start(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hijack" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Keep the hijacked connection open, like an attach stream.
		hc, _, err := http.NewResponseController(w).Hijack()
		if err == nil {
			_, _ = hc.Write([]byte("hijacked\n"))
		}
	}))
	assert.Check(t, is.Equal(status(), http.StatusNoContent))

	hijacked, err := net.Dial("unix", path)
	assert.NilError(t, err)
	defer hijacked.Close()
	_, err = hijacked.Write([]byte("GET /hijack HTTP/1.1\r\nHost: api\r\n\r\n"))
	assert.NilError(t, err)
	br := bufio.NewReader(hijacked)
	line, err := br.ReadString('\n')
	assert.NilError(t, err)
	assert.Check(t, is.Equal(line, "hijacked\n"))

	assert.NilError(t, m.Close())
	_, err = os.Stat(path)
	assert.Check(t, errors.Is(err, fs.ErrNotExist), "Close must remove the socket file")
	assert.Check(t, m.Open(ctx, path) != nil, "Open after Close must fail")

	assert.NilError(t, hijacked.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, err = br.ReadByte()
	assert.Check(t, is.ErrorIs(err, io.EOF), "Close must close hijacked connections")
}
