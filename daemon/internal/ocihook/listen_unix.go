//go:build !windows

package ocihook

import (
	"net"

	"github.com/docker/go-connections/sockets"
)

func listen(sockPath string) (net.Listener, error) {
	return sockets.NewUnixSocketWithOpts(sockPath, sockets.WithChmod(0o600))
}
