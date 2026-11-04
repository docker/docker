package daemonapiv0

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/moby/moby/client"
)

// placeholderHost only selects the plain unix transport; the client dials the
// endpoint the provider reports.
// The client includes its host in connection errors, so the name points at
// where the real endpoint comes from rather than looking like a real path.
const placeholderHost = "unix:///resolved-by-daemonapi-v0.sock"

// NewClient returns an Engine API client for the endpoint api reports.
// It is a temporary shortcut; prefer a specific extension point when one
// covers what the extension needs.
// Resolve api with Point.Single during initialization, after declaring
// Point.Dependency().
// The client asks api for the endpoint when it first connects, so the daemon
// creates the socket only once an extension actually uses the API.
// opts are applied after the endpoint, so they may override it.
//
// The daemon serves the API only after it finishes initializing, so API calls
// made before then, including from Init, fail.
// The caller owns the returned client and must close it.
func NewClient(api DaemonAPI, opts ...client.Opt) (*client.Client, error) {
	d := &dialer{api: api, lock: make(chan struct{}, 1)}
	return client.New(append([]client.Opt{
		client.WithHost(placeholderHost),
		client.WithDialContext(d.dial),
	}, opts...)...)
}

type dialer struct {
	api DaemonAPI

	// lock is a one-slot semaphore rather than a mutex so that dials waiting
	// on a slow Get still honor their own context.
	lock chan struct{}
	path string
}

func (d *dialer) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	path, err := d.socketPath(ctx)
	if err != nil {
		return nil, err
	}
	var nd net.Dialer
	return nd.DialContext(ctx, "unix", path)
}

func (d *dialer) socketPath(ctx context.Context) (string, error) {
	select {
	case d.lock <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-d.lock }()
	if d.path != "" {
		return d.path, nil
	}
	resp, err := d.api.Get(ctx, &GetRequest{})
	if err != nil {
		return "", fmt.Errorf("get daemon API endpoint: %w", err)
	}
	path, ok := strings.CutPrefix(resp.Host, "unix://")
	if !ok {
		return "", fmt.Errorf("unsupported daemon API endpoint %q", resp.Host)
	}
	d.path = path
	return path, nil
}
