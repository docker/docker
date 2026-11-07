//go:build !windows

package apisocket

import (
	"net"

	"github.com/docker/go-connections/sockets"
)

func listen(path string) (net.Listener, error) {
	return sockets.NewUnixSocketWithOpts(path, sockets.WithChmod(0o600))
}
