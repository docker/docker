package client

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// postHijacked sends a POST request and hijacks the connection.
func (cli *Client) postHijacked(ctx context.Context, path string, query url.Values, headers map[string][]string, body any) (HijackedResponse, error) {
	jsonBody, err := jsonEncode(body)
	if err != nil {
		return HijackedResponse{}, err
	}
	req, err := cli.buildRequest(ctx, http.MethodPost, cli.getAPIPath(ctx, path, query), headers, jsonBody)
	if err != nil {
		return HijackedResponse{}, err
	}
	conn, mediaType, err := cli.setupHijackConn(req, "tcp")
	if err != nil {
		return HijackedResponse{}, err
	}

	return NewHijackedResponse(conn, mediaType), nil
}

// DialHijack returns a hijacked connection with negotiated protocol proto.
func (cli *Client) DialHijack(ctx context.Context, url, proto string, meta map[string][]string) (net.Conn, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	req = cli.addHeaders(req, meta)

	conn, _, err := cli.setupHijackConn(req, proto)
	return conn, err
}

func (cli *Client) setupHijackConn(req *http.Request, proto string) (retConn net.Conn, mediaType string, retErr error) {
	ctx := req.Context()
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", proto)

	dial := cli.dialer()
	conn, err := dial(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("cannot connect to the Docker daemon. Is 'docker daemon' running on this host?: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = conn.Close()
		}
	}()

	// When we set up a TCP connection for hijack, there could be long periods
	// of inactivity (a long running command with no output) that in certain
	// network setups may cause ECONNTIMEOUT, leaving the client in an unknown
	// state. Setting TCP KeepAlive on the socket connection prohibits
	// ECONNTIMEOUT unless the socket connection truly is broken
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
	}

	hc := &hijackedConn{conn, bufio.NewReader(conn)}

	// Cancel the handshake without tying the returned connection to ctx.
	cancelled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = hc.Conn.Close()
		close(cancelled)
	})
	defer func() {
		if !stop() {
			// Cancellation won the race; don't return a connection being closed.
			<-cancelled
			retConn, mediaType, retErr = nil, "", ctx.Err()
		}
	}()

	cfg := &cli.clientConfig

	var rt http.RoundTripper = otelhttp.NewTransport(hc, cli.traceOpts...)
	if len(cfg.requestHooks) > 0 || len(cfg.responseHooks) > 0 {
		rt = &hookTransport{
			base:      rt,
			reqHooks:  cfg.requestHooks,
			respHooks: cfg.responseHooks,
		}
	}

	// Server hijacks the connection, error 'connection closed' expected
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, "", err
	}
	// Closing the body ends the otelhttp span. A 101 response has no body,
	// so this does not close the connection.
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, "", fmt.Errorf("unable to upgrade to %s, received %d", proto, resp.StatusCode)
	}

	if hc.r.Buffered() > 0 {
		// If there is buffered content, wrap the connection.  We return an
		// object that implements CloseWrite if the underlying connection
		// implements it.
		if _, ok := hc.Conn.(CloseWriter); ok {
			conn = &hijackedConnCloseWriter{hc}
		} else {
			conn = hc
		}
	} else {
		hc.r.Reset(nil)
	}

	return conn, resp.Header.Get("Content-Type"), nil
}

// hijackedConn wraps a net.Conn and is returned by setupHijackConn in the case
// that a) there was already buffered data in the http layer when Hijack() was
// called, and b) the underlying net.Conn does *not* implement CloseWrite().
// hijackedConn does not implement CloseWrite() either.
type hijackedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *hijackedConn) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Write(c.Conn); err != nil {
		return nil, err
	}
	return http.ReadResponse(c.r, req)
}

func (c *hijackedConn) Read(b []byte) (int, error) {
	return c.r.Read(b)
}

// hijackedConnCloseWriter is a hijackedConn which additionally implements
// CloseWrite().  It is returned by setupHijackConn in the case that a) there
// was already buffered data in the http layer when Hijack() was called, and b)
// the underlying net.Conn *does* implement CloseWrite().
type hijackedConnCloseWriter struct {
	*hijackedConn
}

var _ CloseWriter = &hijackedConnCloseWriter{}

func (c *hijackedConnCloseWriter) CloseWrite() error {
	conn := c.Conn.(CloseWriter)
	return conn.CloseWrite()
}

// NewHijackedResponse initializes a [HijackedResponse] type.
func NewHijackedResponse(conn net.Conn, mediaType string) HijackedResponse {
	return HijackedResponse{Conn: conn, Reader: bufio.NewReader(conn), mediaType: mediaType}
}

// HijackedResponse holds connection information for a hijacked request.
type HijackedResponse struct {
	mediaType string
	Conn      net.Conn
	Reader    *bufio.Reader
}

// Close closes the hijacked connection and reader.
func (h *HijackedResponse) Close() {
	h.Conn.Close()
}

// MediaType let client know if HijackedResponse hold a raw or multiplexed stream.
// returns false if HTTP Content-Type is not relevant, and the container must be
// inspected.
func (h *HijackedResponse) MediaType() (string, bool) {
	if h.mediaType == "" {
		return "", false
	}
	return h.mediaType, true
}

// CloseWriter is an interface that implements structs
// that close input streams to prevent from writing.
type CloseWriter interface {
	CloseWrite() error
}

// CloseWrite closes a readWriter for writing.
func (h *HijackedResponse) CloseWrite() error {
	if conn, ok := h.Conn.(CloseWriter); ok {
		return conn.CloseWrite()
	}
	return nil
}
