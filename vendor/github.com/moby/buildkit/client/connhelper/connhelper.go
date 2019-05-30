// Package connhelper provides helpers for connecting to a remote daemon host with custom logic.
package connhelper

import (
	"context"
	"net"
	"net/url"
)

var helpers = map[string]func(*url.URL) (*ConnectionHelper, error){}

// ConnectionHelper allows to connect to a remote host with custom stream provider binary.
type ConnectionHelper struct {
	// ContextDialer can be passed to grpc.WithContextDialer
	ContextDialer func(ctx context.Context, addr string) (net.Conn, error)
}

// GetConnectionHelper returns BuildKit-specific connection helper for the given URL.
// GetConnectionHelper returns nil without error when no helper is registered for the scheme.
func GetConnectionHelper(daemonURL string) (*ConnectionHelper, error) {
	u, err := url.Parse(daemonURL)
	if err != nil {
		return nil, err
	}

	fn, ok := helpers[u.Scheme]
	if !ok {
		return nil, nil
	}

	return fn(u)
}

// Register registers new connectionhelper for scheme
func Register(scheme string, fn func(*url.URL) (*ConnectionHelper, error)) {
	helpers[scheme] = fn
}
