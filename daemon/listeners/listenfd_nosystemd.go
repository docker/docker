// +build linux,no_systemd

package listeners // import "github.com/docker/docker/daemon/listeners"

import (
	"crypto/tls"
	"errors"
	"net"
)

func listenFD(addr string, tlsConfig *tls.Config) ([]net.Listener, error) {
	return nil, errors.New("listenFD not implemented")
}
