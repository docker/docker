package wsutil

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"io/ioutil"
	"net"
	"net/http"

	"github.com/gobwas/ws"
)

// DebugDialer is a wrapper around ws.Dialer. It tracks i/o of WebSocket
// handshake. That is, it gives ability to receive copied HTTP request and
// response bytes that made inside Dialer.Dial().
//
// Note that it must not be used in production applications that requires
// Dial() to be efficient.
type DebugDialer struct {
	// Dialer contains WebSocket connection establishment options.
	Dialer ws.Dialer

	// OnRequest and OnResponse are the callbacks that will be called with the
	// HTTP request and response respectively.
	OnRequest, OnResponse func([]byte)
}

// Dial connects to the url host and upgrades connection to WebSocket. It makes
// it by calling d.Dialer.Dial().
func (d *DebugDialer) Dial(ctx context.Context, urlstr string) (conn net.Conn, br *bufio.Reader, hs ws.Handshake, err error) {
	// Need to copy Dialer to prevent original object mutation.
	dialer := d.Dialer
	var (
		reqBuf bytes.Buffer
		resBuf bytes.Buffer

		resContentLength int64
	)
	userWrap := dialer.WrapConn
	dialer.WrapConn = func(c net.Conn) net.Conn {
		if userWrap != nil {
			c = userWrap(c)
		}

		// Save the pointer to the raw connection.
		conn = c

		var (
			r io.Reader = conn
			w io.Writer = conn
		)
		if d.OnResponse != nil {
			r = &prefetchResponseReader{
				source:        conn,
				buffer:        &resBuf,
				contentLength: &resContentLength,
			}
		}
		if d.OnRequest != nil {
			w = io.MultiWriter(conn, &reqBuf)
		}
		return rwConn{conn, r, w}
	}

	_, br, hs, err = dialer.Dial(ctx, urlstr)

	if onRequest := d.OnRequest; onRequest != nil {
		onRequest(reqBuf.Bytes())
	}
	if onResponse := d.OnResponse; onResponse != nil {
		// We must split response inside buffered bytes from other received
		// bytes from server.
		p := resBuf.Bytes()
		n := bytes.Index(p, headEnd)
		h := n + len(headEnd)         // Head end index.
		n = h + int(resContentLength) // Body end index.

		onResponse(p[:n])

		if br != nil {
			// If br is non-nil, then it mean two things. First is that
			// handshake is OK and server has sent additional bytes – probably
			// immediate sent frames (or weird but possible response body).
			// Second, the bad one, is that br buffer's source is now rwConn
			// instance from above WrapConn call. It is incorrect, so we must
			// fix it.
			var r io.Reader = conn
			if len(p) > h {
				// Buffer contains more than just HTTP headers bytes.
				r = io.MultiReader(
					bytes.NewReader(p[h:]),
					conn,
				)
			}
			br.Reset(r)
			// Must make br.Buffered() to be non-zero.
			br.Peek(len(p[h:]))
		}
	}

	return conn, br, hs, err
}

type rwConn struct {
	net.Conn

	r io.Reader
	w io.Writer
}

func (rwc rwConn) Read(p []byte) (int, error) {
	return rwc.r.Read(p)
}
func (rwc rwConn) Write(p []byte) (int, error) {
	return rwc.w.Write(p)
}

var headEnd = []byte("\r\n\r\n")

type prefetchResponseReader struct {
	source io.Reader // Original connection source.
	reader io.Reader // Wrapped reader used to read from by clients.
	buffer *bytes.Buffer

	contentLength *int64
}

func (r *prefetchResponseReader) Read(p []byte) (int, error) {
	if r.reader == nil {
		resp, err := http.ReadResponse(bufio.NewReader(
			io.TeeReader(r.source, r.buffer),
		), nil)
		if err == nil {
			*r.contentLength, _ = io.Copy(ioutil.Discard, resp.Body)
			resp.Body.Close()
		}
		bts := r.buffer.Bytes()
		r.reader = io.MultiReader(
			bytes.NewReader(bts),
			r.source,
		)
	}
	return r.reader.Read(p)
}
