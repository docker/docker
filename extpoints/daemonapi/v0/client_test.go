package daemonapiv0_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/moby/moby/client"
	daemonapiv0 "github.com/moby/moby/v2/extpoints/daemonapi/v0"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

type fakeAPI struct {
	host  string
	calls atomic.Int32
}

func (f *fakeAPI) Get(context.Context, *daemonapiv0.GetRequest) (*daemonapiv0.GetResponse, error) {
	f.calls.Add(1)
	return &daemonapiv0.GetResponse{Host: f.host}, nil
}

func TestNewClientGetsEndpointOnFirstConnect(t *testing.T) {
	t.Parallel()

	// t.TempDir paths can exceed the unix socket path limit on macOS.
	dir, err := os.MkdirTemp("", "daemonapi")
	assert.NilError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock := filepath.Join(dir, "api.sock")
	l, err := net.Listen("unix", sock)
	assert.NilError(t, err)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Force a new connection per request so the second Ping dials again.
		w.Header().Set("Connection", "close")
	}))
	_ = srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)

	api := &fakeAPI{host: "unix://" + sock}
	c, err := daemonapiv0.NewClient(api)
	assert.NilError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	assert.Check(t, is.Equal(api.calls.Load(), int32(0)))

	for range 2 {
		_, err = c.Ping(t.Context(), client.PingOptions{})
		assert.NilError(t, err)
	}
	assert.Check(t, is.Equal(api.calls.Load(), int32(1)))
}
