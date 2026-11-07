// Package apisocket serves the Engine API on unix sockets that the daemon
// creates for its own consumers, separately from the configured hosts.
package apisocket

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/log"
)

// Manager owns the daemon-created API sockets and serves them all with one
// server.
type Manager struct {
	handler atomic.Pointer[http.Handler]
	srv     *http.Server

	mu      sync.Mutex
	sockets map[string]*listener
	closed  bool
}

// NewManager returns a Manager with no sockets.
func NewManager() *Manager {
	m := &Manager{sockets: make(map[string]*listener)}

	var p http.Protocols
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	m.srv = &http.Server{
		Handler:           http.HandlerFunc(m.serveHTTP),
		Protocols:         &p,
		ReadHeaderTimeout: 5 * time.Minute, // Same as the configured hosts.
	}
	return m
}

// Open creates a unix socket at path and serves the API on it.
// On Unix, the socket is accessible only to its owner.
// Until [Manager.Start], requests fail with 503 Service Unavailable, so a
// socket can be handed out before the API is ready.
// Opening a path that is already open does nothing.
func (m *Manager) Open(ctx context.Context, path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("API socket manager is closed")
	}
	if _, ok := m.sockets[path]; ok {
		return nil
	}

	ls, err := listen(path)
	if err != nil {
		return fmt.Errorf("listen on API socket %q: %w", path, err)
	}
	ul, ok := ls.(*net.UnixListener)
	if !ok {
		_ = ls.Close()
		return fmt.Errorf("listen on API socket %q: unexpected listener type %T", path, ls)
	}

	l := &listener{UnixListener: ul, conns: make(map[*conn]struct{})}
	m.sockets[path] = l

	log.G(ctx).Infof("API listen on %s", path)
	go func() {
		if err := m.srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.G(ctx).WithFields(log.Fields{
				"error":    err,
				"listener": path,
			}).Error("ServeAPI error")
		}
	}()
	return nil
}

// Start serves h on all sockets, including ones opened later.
func (m *Manager) Start(h http.Handler) {
	m.handler.Store(&h)
}

func (m *Manager) serveHTTP(w http.ResponseWriter, r *http.Request) {
	h := m.handler.Load()
	if h == nil {
		http.Error(w, "the daemon is not ready to serve API requests", http.StatusServiceUnavailable)
		return
	}
	(*h).ServeHTTP(w, r)
}

// Close stops serving all sockets, closes all their connections, including
// hijacked ones, and removes the socket files.
// Later calls to [Manager.Open] fail.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	err := m.srv.Close()
	for _, l := range m.sockets {
		// Serve may not track the listener yet; closing it here removes the
		// socket file either way.
		_ = l.Close()
		l.closeConns()
	}
	clear(m.sockets)
	return err
}

// listener tracks its connections, because the server does not track
// hijacked connections such as attach and exec streams.
type listener struct {
	*net.UnixListener

	mu    sync.Mutex
	conns map[*conn]struct{}
}

func (l *listener) Accept() (net.Conn, error) {
	uc, err := l.AcceptUnix()
	if err != nil {
		return nil, err
	}
	c := &conn{UnixConn: uc, l: l}
	l.mu.Lock()
	l.conns[c] = struct{}{}
	l.mu.Unlock()
	return c, nil
}

func (l *listener) closeConns() {
	l.mu.Lock()
	conns := make([]*conn, 0, len(l.conns))
	for c := range l.conns {
		conns = append(conns, c)
	}
	l.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// conn embeds *net.UnixConn so that CloseWrite stays available for
// half-closing hijacked streams.
type conn struct {
	*net.UnixConn
	l *listener
}

func (c *conn) Close() error {
	c.l.mu.Lock()
	delete(c.l.conns, c)
	c.l.mu.Unlock()
	return c.UnixConn.Close()
}
