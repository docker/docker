package client

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestHijackCancellation(t *testing.T) {
	for _, phase := range []string{"write request", "read response"} {
		for _, post := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/post=%t", phase, post), func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				conn, peer := net.Pipe()
				t.Cleanup(func() { _ = conn.Close(); _ = peer.Close() })
				c, err := New(WithAPIVersion(MaxAPIVersion), WithDialContext(func(context.Context, string, string) (net.Conn, error) {
					return conn, nil
				}))
				assert.NilError(t, err)
				t.Cleanup(func() { _ = c.Close() })

				peerDone := make(chan error, 1)
				go func() {
					var err error
					if phase == "write request" {
						// Consume only one byte, leaving the request write blocked.
						_, err = peer.Read(make([]byte, 1))
					} else {
						_, err = http.ReadRequest(bufio.NewReader(peer))
					}
					cancel()
					peerDone <- err
				}()

				if post {
					resp, err := c.postHijacked(ctx, "/test", nil, nil, nil)
					assert.ErrorIs(t, err, context.Canceled)
					assert.Check(t, resp.Conn == nil)
				} else {
					got, err := c.DialHijack(ctx, "http://docker/test", "tcp", nil)
					assert.ErrorIs(t, err, context.Canceled)
					assert.Check(t, got == nil)
				}
				assert.NilError(t, <-peerDone)
				_, err = peer.Read(make([]byte, 1))
				assert.ErrorIs(t, err, io.EOF)
			})
		}
	}
}

func TestHijackDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	conn, peer := net.Pipe()
	t.Cleanup(func() { _ = conn.Close(); _ = peer.Close() })
	c, err := New(WithDialContext(func(context.Context, string, string) (net.Conn, error) {
		return conn, nil
	}))
	assert.NilError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	peerDone := make(chan error, 1)
	go func() {
		_, err := http.ReadRequest(bufio.NewReader(peer))
		peerDone <- err
	}()
	got, err := c.DialHijack(ctx, "http://docker/test", "tcp", nil)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Check(t, got == nil)
	assert.NilError(t, <-peerDone)
}

func TestHijackCancellationOwnership(t *testing.T) {
	for _, duringHandshake := range []bool{false, true} {
		t.Run(fmt.Sprintf("during handshake=%t", duringHandshake), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			conn, peer := net.Pipe()
			t.Cleanup(func() { _ = conn.Close(); _ = peer.Close() })
			c, err := New(
				WithDialContext(func(context.Context, string, string) (net.Conn, error) { return conn, nil }),
				WithHTTPResponseHook(func(*http.Response) {
					if duringHandshake {
						cancel()
					}
				}),
			)
			assert.NilError(t, err)
			t.Cleanup(func() { _ = c.Close() })
			peerDone := make(chan error, 1)
			go func() {
				if _, err := http.ReadRequest(bufio.NewReader(peer)); err != nil {
					peerDone <- err
					return
				}
				// Include stream data in the same write to exercise buffered data.
				_, err := io.WriteString(peer, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\nhello")
				peerDone <- err
			}()
			got, err := c.DialHijack(ctx, "http://docker/test", "tcp", nil)
			if duringHandshake {
				assert.ErrorIs(t, err, context.Canceled)
				assert.Check(t, got == nil)
			} else {
				assert.NilError(t, err)
				defer func() { _ = got.Close() }()
				cancel()
				buf := make([]byte, 5)
				_, err = io.ReadFull(got, buf)
				assert.NilError(t, err)
				assert.Equal(t, string(buf), "hello")
				// Read from the underlying connection after cancellation as well.
				go func() { _, _ = io.WriteString(peer, "world") }()
				_, err = io.ReadFull(got, buf)
				assert.NilError(t, err)
				assert.Equal(t, string(buf), "world")
			}
			assert.NilError(t, <-peerDone)
		})
	}
}

func TestHijackHooksAndTracing(t *testing.T) {
	const (
		reqHeaderKey    = "X-Test-Request"
		reqHeaderValue  = "request"
		respHeaderKey   = "X-Test-Header"
		respHeaderValue = "hello-world"
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assert.Equal(t, req.Header.Get(reqHeaderKey), reqHeaderValue)

		conn, _, err := w.(http.Hijacker).Hijack()
		assert.NilError(t, err)
		defer func() { _ = conn.Close() }()

		headers := []string{
			"HTTP/1.1 101 UPGRADED",
			"Connection: Upgrade",
			"Upgrade: tcp",
			respHeaderKey + ": " + respHeaderValue,
		}

		_, err = io.WriteString(conn, strings.Join(headers, "\r\n")+"\r\n\r\n")
		assert.NilError(t, err)
	}))
	defer ts.Close()

	serverURL, err := url.Parse(ts.URL)
	assert.NilError(t, err)

	tp := &spanCounter{}
	var gotResponseHeader string
	c, err := New(
		WithHost("tcp://"+serverURL.Host),
		WithTraceProvider(tp),
		WithHTTPRequestHook(func(req *http.Request) error {
			req.Header.Set(reqHeaderKey, reqHeaderValue)
			return nil
		}),
		WithHTTPResponseHook(func(resp *http.Response) {
			gotResponseHeader = resp.Header.Get(respHeaderKey)
		}),
	)
	assert.NilError(t, err)

	conn, err := c.DialHijack(t.Context(), ts.URL+"/test", "tcp", nil)
	assert.NilError(t, err)
	defer func() { _ = conn.Close() }()

	assert.Equal(t, gotResponseHeader, respHeaderValue)
	assert.Check(t, is.Equal(tp.started.Load(), int32(1)))
	assert.Check(t, is.Equal(tp.ended.Load(), int32(1)))
}

// spanCounter counts the spans started and ended through it.
type spanCounter struct {
	noop.TracerProvider
	started, ended atomic.Int32
}

func (p *spanCounter) Tracer(string, ...trace.TracerOption) trace.Tracer {
	return countingTracer{p: p}
}

type countingTracer struct {
	noop.Tracer
	p *spanCounter
}

func (t countingTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	t.p.started.Add(1)
	ctx, span := t.Tracer.Start(ctx, name, opts...)
	return ctx, countingSpan{Span: span, p: t.p}
}

type countingSpan struct {
	trace.Span
	p *spanCounter
}

func (s countingSpan) End(...trace.SpanEndOption) {
	s.p.ended.Add(1)
}

func TestTLSCloseWriter(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var chErr chan error
	ts := &httptest.Server{Config: &http.Server{
		ReadHeaderTimeout: 5 * time.Minute, // "G112: Potential Slowloris Attack (gosec)"; not a real concern for our use, so setting a long timeout.
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path == "/_ping" {
				resp, err := mockPingResponse(http.StatusOK, PingResult{APIVersion: MaxAPIVersion})(req)
				if err != nil {
					chErr <- fmt.Errorf("sending ping response: %w", err)
					return
				}
				_ = resp.Header.Write(w)
				w.WriteHeader(resp.StatusCode)
				return
			}

			chErr = make(chan error, 1)
			defer close(chErr)

			if err := req.ParseForm(); err != nil && !strings.HasPrefix(err.Error(), "mime:") {
				chErr <- fmt.Errorf("error parsing form: %w", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				chErr <- fmt.Errorf("error hijacking connection: %w", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer func() { _ = conn.Close() }()

			// Flush the options to make sure the client sets the raw mode
			_, _ = conn.Write([]byte{})

			_, err = fmt.Fprint(conn, "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\n")
			if err != nil {
				chErr <- fmt.Errorf("writing update response: %w", err)
				return
			}

			buf := make([]byte, 5)
			_, err = conn.Read(buf)
			if err != nil {
				chErr <- fmt.Errorf("error reading from client: %w", err)
				return
			}
			_, err = conn.Write(buf)
			if err != nil {
				chErr <- fmt.Errorf("error writing to client: %w", err)
				return
			}
		}),
	}}

	var (
		l   net.Listener
		err error
	)
	for i := 1024; i < 10000; i++ {
		l, err = net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", i))
		if err == nil {
			break
		}
	}
	assert.NilError(t, err)

	ts.Listener = l
	defer func() { _ = l.Close() }()

	defer func() {
		if chErr != nil {
			assert.NilError(t, <-chErr)
		}
	}()

	ts.StartTLS()
	defer ts.Close()

	serverURL, err := url.Parse(ts.URL)
	assert.NilError(t, err)

	httpClient := ts.Client()
	defer httpClient.CloseIdleConnections()
	client, err := New(WithHost("tcp://"+serverURL.Host), WithHTTPClient(httpClient))
	assert.NilError(t, err)

	headers := http.Header{"Content-Type": {"text/plain"}}
	resp, err := client.postHijacked(ctx, "/asdf", url.Values{}, headers, nil)
	assert.NilError(t, err)
	defer resp.Close()

	_, ok := resp.Conn.(CloseWriter)
	assert.Check(t, ok, "tls conn did not implement the CloseWrite interface")

	_, err = resp.Conn.Write([]byte("hello"))
	assert.NilError(t, err)

	b, err := io.ReadAll(resp.Reader)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(string(b), "hello"))
	assert.NilError(t, resp.CloseWrite())

	// This should error since writes are closed
	_, err = resp.Conn.Write([]byte("no"))
	assert.Check(t, err != nil)
}
