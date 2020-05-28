package sockets

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/Microsoft/go-winio"
)

func configureUnixTransport(tr *http.Transport, proto, addr string) error {
	return ErrProtocolNotAvailable
}

func configureNpipeTransport(tr *http.Transport, proto, addr string) error {
	// No need for compression in local communications.
	tr.DisableCompression = true
	tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return winio.DialPipeContext(ctx, addr)
	}
	return nil
}

// DialPipe connects to a Windows named pipe.
func DialPipe(addr string, timeout time.Duration) (net.Conn, error) {
	return winio.DialPipe(addr, &timeout)
}
