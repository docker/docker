//go:generate go tool mobyextgen

// Package daemonapiv0 gives Moby extensions access to the daemon's Engine API
// through a regular Moby API client.
//
// This is a temporary shortcut until Moby defines specific extension points
// for the daemon functionality extensions need.
// Once a specific point covers what an extension uses the API for, the
// extension should depend on that point instead.
package daemonapiv0

import (
	"context"

	"github.com/moby/extensions"
)

// DaemonAPI locates the Engine API endpoint the daemon serves for extensions.
// Use [NewClient] instead of calling it directly.
type DaemonAPI interface {
	// Get returns the endpoint.
	Get(ctx context.Context, req *GetRequest) (*GetResponse, error)
}

// Point is single-cardinality for the daemon API endpoint.
// It is a temporary shortcut; prefer a specific point when one exists.
// The daemon registers its builtin provider for extension dependencies,
// including out-of-process extensions, but does not publish it on the public
// daemon socket.
//
// The endpoint is a unix socket that only the daemon user can access; on
// Windows, only Administrators and LocalSystem.
// The daemon creates it on the first Get call, which a [NewClient] client
// makes when it first connects.
// Extensions run as the daemon user, so every extension can reach the socket
// and gets unrestricted Engine API access, whether or not it declares a
// dependency on this point.
var Point = extensions.DefineSinglePoint[DaemonAPI]("org.mobyproject.extension.daemon.api.v0")

// GetRequest requests the daemon API endpoint.
type GetRequest struct{}

// GetResponse contains the daemon API endpoint.
type GetResponse struct {
	// Host is the endpoint in DOCKER_HOST form, such as unix:///path/api.sock.
	Host string `pb:"1"`
}
