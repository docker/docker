package ws

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gobwas/httphead"
	"github.com/gobwas/pool/pbufio"
)

// Constants used by Dialer.
const (
	DefaultClientReadBufferSize  = 4096
	DefaultClientWriteBufferSize = 4096
)

// Handshake represents handshake result.
type Handshake struct {
	// Protocol is the subprotocol selected during handshake.
	Protocol string

	// Extensions is the list of negotiated extensions.
	Extensions []httphead.Option
}

// Errors used by the websocket client.
var (
	ErrHandshakeBadStatus      = fmt.Errorf("unexpected http status")
	ErrHandshakeBadSubProtocol = fmt.Errorf("unexpected protocol in %q header", headerSecProtocol)
	ErrHandshakeBadExtensions  = fmt.Errorf("unexpected extensions in %q header", headerSecProtocol)
)

// DefaultDialer is dialer that holds no options and is used by Dial function.
var DefaultDialer Dialer

// Dial is like Dialer{}.Dial().
func Dial(ctx context.Context, urlstr string) (net.Conn, *bufio.Reader, Handshake, error) {
	return DefaultDialer.Dial(ctx, urlstr)
}

// Dialer contains options for establishing websocket connection to an url.
type Dialer struct {
	// ReadBufferSize and WriteBufferSize is an I/O buffer sizes.
	// They used to read and write http data while upgrading to WebSocket.
	// Allocated buffers are pooled with sync.Pool to avoid extra allocations.
	//
	// If a size is zero then default value is used.
	ReadBufferSize, WriteBufferSize int

	// Timeout is the maximum amount of time a Dial() will wait for a connect
	// and an handshake to complete.
	//
	// The default is no timeout.
	Timeout time.Duration

	// Protocols is the list of subprotocols that the client wants to speak,
	// ordered by preference.
	//
	// See https://tools.ietf.org/html/rfc6455#section-4.1
	Protocols []string

	// Extensions is the list of extensions that client wants to speak.
	//
	// Note that if server decides to use some of this extensions, Dial() will
	// return Handshake struct containing a slice of items, which are the
	// shallow copies of the items from this list. That is, internals of
	// Extensions items are shared during Dial().
	//
	// See https://tools.ietf.org/html/rfc6455#section-4.1
	// See https://tools.ietf.org/html/rfc6455#section-9.1
	Extensions []httphead.Option

	// Header is an optional HandshakeHeader instance that could be used to
	// write additional headers to the handshake request.
	//
	// It used instead of any key-value mappings to avoid allocations in user
	// land.
	Header HandshakeHeader

	// OnStatusError is the callback that will be called after receiving non
	// "101 Continue" HTTP response status. It receives an io.Reader object
	// representing server response bytes. That is, it gives ability to parse
	// HTTP response somehow (probably with http.ReadResponse call) and make a
	// decision of further logic.
	//
	// The arguments are only valid until the callback returns.
	OnStatusError func(status int, reason []byte, resp io.Reader)

	// OnHeader is the callback that will be called after successful parsing of
	// header, that is not used during WebSocket handshake procedure. That is,
	// it will be called with non-websocket headers, which could be relevant
	// for application-level logic.
	//
	// The arguments are only valid until the callback returns.
	//
	// Returned value could be used to prevent processing response.
	OnHeader func(key, value []byte) (err error)

	// NetDial is the function that is used to get plain tcp connection.
	// If it is not nil, then it is used instead of net.Dialer.
	NetDial func(ctx context.Context, network, addr string) (net.Conn, error)

	// TLSClient is the callback that will be called after successful dial with
	// received connection and its remote host name. If it is nil, then the
	// default tls.Client() will be used.
	// If it is not nil, then TLSConfig field is ignored.
	TLSClient func(conn net.Conn, hostname string) net.Conn

	// TLSConfig is passed to tls.Client() to start TLS over established
	// connection. If TLSClient is not nil, then it is ignored. If TLSConfig is
	// non-nil and its ServerName is empty, then for every Dial() it will be
	// cloned and appropriate ServerName will be set.
	TLSConfig *tls.Config

	// WrapConn is the optional callback that will be called when connection is
	// ready for an i/o. That is, it will be called after successful dial and
	// TLS initialization (for "wss" schemes). It may be helpful for different
	// user land purposes such as end to end encryption.
	//
	// Note that for debugging purposes of an http handshake (e.g. sent request
	// and received response), there is an wsutil.DebugDialer struct.
	WrapConn func(conn net.Conn) net.Conn
}

// Dial connects to the url host and upgrades connection to WebSocket.
//
// If server has sent frames right after successful handshake then returned
// buffer will be non-nil. In other cases buffer is always nil. For better
// memory efficiency received non-nil bufio.Reader should be returned to the
// inner pool with PutReader() function after use.
//
// Note that Dialer does not implement IDNA (RFC5895) logic as net/http does.
// If you want to dial non-ascii host name, take care of its name serialization
// avoiding bad request issues. For more info see net/http Request.Write()
// implementation, especially cleanHost() function.
func (d Dialer) Dial(ctx context.Context, urlstr string) (conn net.Conn, br *bufio.Reader, hs Handshake, err error) {
	u, err := url.ParseRequestURI(urlstr)
	if err != nil {
		return
	}

	// Prepare context to dial with. Initially it is the same as original, but
	// if d.Timeout is non-zero and points to time that is before ctx.Deadline,
	// we use more shorter context for dial.
	dialctx := ctx

	var deadline time.Time
	if t := d.Timeout; t != 0 {
		deadline = time.Now().Add(t)
		if d, ok := ctx.Deadline(); !ok || deadline.Before(d) {
			var cancel context.CancelFunc
			dialctx, cancel = context.WithDeadline(ctx, deadline)
			defer cancel()
		}
	}
	if conn, err = d.dial(dialctx, u); err != nil {
		return
	}
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()
	if ctx == context.Background() {
		// No need to start I/O interrupter goroutine which is not zero-cost.
		conn.SetDeadline(deadline)
		defer conn.SetDeadline(noDeadline)
	} else {
		// Context could be canceled or its deadline could be exceeded.
		// Start the interrupter goroutine to handle context cancelation.
		done := setupContextDeadliner(ctx, conn)
		defer func() {
			// Map Upgrade() error to a possible context expiration error. That
			// is, even if Upgrade() err is nil, context could be already
			// expired and connection be "poisoned" by SetDeadline() call.
			// In that case we must not return ctx.Err() error.
			done(&err)
		}()
	}

	br, hs, err = d.Upgrade(conn, u)

	return
}

var (
	// netEmptyDialer is a net.Dialer without options, used in Dialer.dial() if
	// Dialer.NetDial is not provided.
	netEmptyDialer net.Dialer
	// tlsEmptyConfig is an empty tls.Config used as default one.
	tlsEmptyConfig tls.Config
)

func tlsDefaultConfig() *tls.Config {
	return &tlsEmptyConfig
}

func hostport(host string, defaultPort string) (hostname, addr string) {
	var (
		colon   = strings.LastIndexByte(host, ':')
		bracket = strings.IndexByte(host, ']')
	)
	if colon > bracket {
		return host[:colon], host
	}
	return host, host + defaultPort
}

func (d Dialer) dial(ctx context.Context, u *url.URL) (conn net.Conn, err error) {
	dial := d.NetDial
	if dial == nil {
		dial = netEmptyDialer.DialContext
	}
	switch u.Scheme {
	case "ws":
		_, addr := hostport(u.Host, ":80")
		conn, err = dial(ctx, "tcp", addr)
	case "wss":
		hostname, addr := hostport(u.Host, ":443")
		conn, err = dial(ctx, "tcp", addr)
		if err != nil {
			return
		}
		tlsClient := d.TLSClient
		if tlsClient == nil {
			tlsClient = d.tlsClient
		}
		conn = tlsClient(conn, hostname)
	default:
		return nil, fmt.Errorf("unexpected websocket scheme: %q", u.Scheme)
	}
	if wrap := d.WrapConn; wrap != nil {
		conn = wrap(conn)
	}
	return
}

func (d Dialer) tlsClient(conn net.Conn, hostname string) net.Conn {
	config := d.TLSConfig
	if config == nil {
		config = tlsDefaultConfig()
	}
	if config.ServerName == "" {
		config = tlsCloneConfig(config)
		config.ServerName = hostname
	}
	// Do not make conn.Handshake() here because downstairs we will prepare
	// i/o on this conn with proper context's timeout handling.
	return tls.Client(conn, config)
}

var (
	// This variables are set like in net/net.go.
	// noDeadline is just zero value for readability.
	noDeadline = time.Time{}
	// aLongTimeAgo is a non-zero time, far in the past, used for immediate
	// cancelation of dials.
	aLongTimeAgo = time.Unix(42, 0)
)

// Upgrade writes an upgrade request to the given io.ReadWriter conn at given
// url u and reads a response from it.
//
// It is a caller responsibility to manage I/O deadlines on conn.
//
// It returns handshake info and some bytes which could be written by the peer
// right after response and be caught by us during buffered read.
func (d Dialer) Upgrade(conn io.ReadWriter, u *url.URL) (br *bufio.Reader, hs Handshake, err error) {
	// headerSeen constants helps to report whether or not some header was seen
	// during reading request bytes.
	const (
		headerSeenUpgrade = 1 << iota
		headerSeenConnection
		headerSeenSecAccept

		// headerSeenAll is the value that we expect to receive at the end of
		// headers read/parse loop.
		headerSeenAll = 0 |
			headerSeenUpgrade |
			headerSeenConnection |
			headerSeenSecAccept
	)

	br = pbufio.GetReader(conn,
		nonZero(d.ReadBufferSize, DefaultClientReadBufferSize),
	)
	bw := pbufio.GetWriter(conn,
		nonZero(d.WriteBufferSize, DefaultClientWriteBufferSize),
	)
	defer func() {
		pbufio.PutWriter(bw)
		if br.Buffered() == 0 || err != nil {
			// Server does not wrote additional bytes to the connection or
			// error occurred. That is, no reason to return buffer.
			pbufio.PutReader(br)
			br = nil
		}
	}()

	nonce := make([]byte, nonceSize)
	initNonce(nonce)

	httpWriteUpgradeRequest(bw, u, nonce, d.Protocols, d.Extensions, d.Header)
	if err = bw.Flush(); err != nil {
		return
	}

	// Read HTTP status line like "HTTP/1.1 101 Switching Protocols".
	sl, err := readLine(br)
	if err != nil {
		return
	}
	// Begin validation of the response.
	// See https://tools.ietf.org/html/rfc6455#section-4.2.2
	// Parse request line data like HTTP version, uri and method.
	resp, err := httpParseResponseLine(sl)
	if err != nil {
		return
	}
	// Even if RFC says "1.1 or higher" without mentioning the part of the
	// version, we apply it only to minor part.
	if resp.major != 1 || resp.minor < 1 {
		err = ErrHandshakeBadProtocol
		return
	}
	if resp.status != 101 {
		err = StatusError(resp.status)
		if onStatusError := d.OnStatusError; onStatusError != nil {
			// Invoke callback with multireader of status-line bytes br.
			onStatusError(resp.status, resp.reason,
				io.MultiReader(
					bytes.NewReader(sl),
					strings.NewReader(crlf),
					br,
				),
			)
		}
		return
	}
	// If response status is 101 then we expect all technical headers to be
	// valid. If not, then we stop processing response without giving user
	// ability to read non-technical headers. That is, we do not distinguish
	// technical errors (such as parsing error) and protocol errors.
	var headerSeen byte
	for {
		line, e := readLine(br)
		if e != nil {
			err = e
			return
		}
		if len(line) == 0 {
			// Blank line, no more lines to read.
			break
		}

		k, v, ok := httpParseHeaderLine(line)
		if !ok {
			err = ErrMalformedResponse
			return
		}

		switch btsToString(k) {
		case headerUpgradeCanonical:
			headerSeen |= headerSeenUpgrade
			if !bytes.Equal(v, specHeaderValueUpgrade) && !bytes.EqualFold(v, specHeaderValueUpgrade) {
				err = ErrHandshakeBadUpgrade
				return
			}

		case headerConnectionCanonical:
			headerSeen |= headerSeenConnection
			// Note that as RFC6455 says:
			//   > A |Connection| header field with value "Upgrade".
			// That is, in server side, "Connection" header could contain
			// multiple token. But in response it must contains exactly one.
			if !bytes.Equal(v, specHeaderValueConnection) && !bytes.EqualFold(v, specHeaderValueConnection) {
				err = ErrHandshakeBadConnection
				return
			}

		case headerSecAcceptCanonical:
			headerSeen |= headerSeenSecAccept
			if !checkAcceptFromNonce(v, nonce) {
				err = ErrHandshakeBadSecAccept
				return
			}

		case headerSecProtocolCanonical:
			// RFC6455 1.3:
			//   "The server selects one or none of the acceptable protocols
			//   and echoes that value in its handshake to indicate that it has
			//   selected that protocol."
			for _, want := range d.Protocols {
				if string(v) == want {
					hs.Protocol = want
					break
				}
			}
			if hs.Protocol == "" {
				// Server echoed subprotocol that is not present in client
				// requested protocols.
				err = ErrHandshakeBadSubProtocol
				return
			}

		case headerSecExtensionsCanonical:
			hs.Extensions, err = matchSelectedExtensions(v, d.Extensions, hs.Extensions)
			if err != nil {
				return
			}

		default:
			if onHeader := d.OnHeader; onHeader != nil {
				if e := onHeader(k, v); e != nil {
					err = e
					return
				}
			}
		}
	}
	if err == nil && headerSeen != headerSeenAll {
		switch {
		case headerSeen&headerSeenUpgrade == 0:
			err = ErrHandshakeBadUpgrade
		case headerSeen&headerSeenConnection == 0:
			err = ErrHandshakeBadConnection
		case headerSeen&headerSeenSecAccept == 0:
			err = ErrHandshakeBadSecAccept
		default:
			panic("unknown headers state")
		}
	}
	return
}

// PutReader returns bufio.Reader instance to the inner reuse pool.
// It is useful in rare cases, when Dialer.Dial() returns non-nil buffer which
// contains unprocessed buffered data, that was sent by the server quickly
// right after handshake.
func PutReader(br *bufio.Reader) {
	pbufio.PutReader(br)
}

// StatusError contains an unexpected status-line code from the server.
type StatusError int

func (s StatusError) Error() string {
	return "unexpected HTTP response status: " + strconv.Itoa(int(s))
}

func isTimeoutError(err error) bool {
	t, ok := err.(net.Error)
	return ok && t.Timeout()
}

func matchSelectedExtensions(selected []byte, wanted, received []httphead.Option) ([]httphead.Option, error) {
	if len(selected) == 0 {
		return received, nil
	}
	var (
		index  int
		option httphead.Option
		err    error
	)
	index = -1
	match := func() (ok bool) {
		for _, want := range wanted {
			if option.Equal(want) {
				// Check parsed extension to be present in client
				// requested extensions. We move matched extension
				// from client list to avoid allocation.
				received = append(received, want)
				return true
			}
		}
		return false
	}
	ok := httphead.ScanOptions(selected, func(i int, name, attr, val []byte) httphead.Control {
		if i != index {
			// Met next option.
			index = i
			if i != 0 && !match() {
				// Server returned non-requested extension.
				err = ErrHandshakeBadExtensions
				return httphead.ControlBreak
			}
			option = httphead.Option{Name: name}
		}
		if attr != nil {
			option.Parameters.Set(attr, val)
		}
		return httphead.ControlContinue
	})
	if !ok {
		err = ErrMalformedResponse
		return received, err
	}
	if !match() {
		return received, ErrHandshakeBadExtensions
	}
	return received, err
}

// setupContextDeadliner is a helper function that starts connection I/O
// interrupter goroutine.
//
// Started goroutine calls SetDeadline() with long time ago value when context
// become expired to make any I/O operations failed. It returns done function
// that stops started goroutine and maps error received from conn I/O methods
// to possible context expiration error.
//
// In concern with possible SetDeadline() call inside interrupter goroutine,
// caller passes pointer to its I/O error (even if it is nil) to done(&err).
// That is, even if I/O error is nil, context could be already expired and
// connection "poisoned" by SetDeadline() call. In that case done(&err) will
// store at *err ctx.Err() result. If err is caused not by timeout, it will
// leaved untouched.
func setupContextDeadliner(ctx context.Context, conn net.Conn) (done func(*error)) {
	var (
		quit      = make(chan struct{})
		interrupt = make(chan error, 1)
	)
	go func() {
		select {
		case <-quit:
			interrupt <- nil
		case <-ctx.Done():
			// Cancel i/o immediately.
			conn.SetDeadline(aLongTimeAgo)
			interrupt <- ctx.Err()
		}
	}()
	return func(err *error) {
		close(quit)
		// If ctx.Err() is non-nil and the original err is net.Error with
		// Timeout() == true, then it means that I/O was canceled by us by
		// SetDeadline(aLongTimeAgo) call, or by somebody else previously
		// by conn.SetDeadline(x).
		//
		// Even on race condition when both deadlines are expired
		// (SetDeadline() made not by us and context's), we prefer ctx.Err() to
		// be returned.
		if ctxErr := <-interrupt; ctxErr != nil && (*err == nil || isTimeoutError(*err)) {
			*err = ctxErr
		}
	}
}
