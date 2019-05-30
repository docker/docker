package grpc // import "github.com/docker/docker/api/server/router/grpc"

import (
	"context"
	"net/http"

	"github.com/pkg/errors"
	"golang.org/x/net/http2"
)

func (gr *grpcRouter) serveGRPC(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	h, ok := w.(http.Hijacker)
	if !ok {
		return errors.New("handler does not support hijack")
	}
	proto := r.Header.Get("Upgrade")
	if proto == "" {
		return errors.New("no upgrade proto in request")
	}
	if proto != "h2c" {
		return errors.Errorf("protocol %s not supported", proto)
	}

	conn, _, err := h.Hijack()
	if err != nil {
		return err
	}
	resp := &http.Response{
		StatusCode: http.StatusSwitchingProtocols,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     http.Header{},
	}
	resp.Header.Set("Connection", "Upgrade")
	resp.Header.Set("Upgrade", proto)

	// set raw mode
	conn.Write([]byte{})
	resp.Write(conn)

	// https://godoc.org/golang.org/x/net/http2#Server.ServeConn
	// TODO: is it a problem that conn has already been written to?
	gr.h2Server.ServeConn(conn, &http2.ServeConnOpts{Handler: gr.grpcServer})
	return nil
}
