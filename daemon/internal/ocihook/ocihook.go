// Package ocihook lets the daemon run code synchronously at an OCI runtime
// hook point of a container it does not otherwise drive, such as one
// started by BuildKit's runc executor.
//
// The daemon creates a [Server] and calls [Server.Register] to obtain a
// [specs.Hook] to put in the container's spec. When the runtime runs the
// hook, it re-executes the daemon binary, which forwards the container's
// [specs.State] to the Server and blocks until the registered callback
// returns. An error returned by the callback makes the hook fail, which
// makes the runtime abort the container's start.
package ocihook

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/internal/stringid"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Callback is called with the state of the container the hook was run for.
type Callback func(context.Context, specs.State) error

type registration struct {
	ctx context.Context
	fn  Callback
}

// A hook invocation is sent as the registration's token, terminated by a
// NUL byte, followed by the container's state exactly as the runtime
// provided it. The client then half-closes the connection, and the server
// replies with a single status byte: statusOK, or statusError followed by
// the error message. The token comes from the hook's arguments, which
// cannot contain a NUL byte.

const (
	statusOK    byte = 0
	statusError byte = 1
)

// maxRequestSize limits how much the server reads from a client, so that
// a client cannot make it buffer an unbounded amount of data. A container's
// state is far smaller.
const maxRequestSize = 1 << 20

// drainTimeout limits how long the server waits, after responding, for the
// client to finish sending the rest of its request.
const drainTimeout = time.Second

// errShutdown is returned to hooks which are run while the server is
// shutting down.
var errShutdown = errors.New("OCI hook server is shutting down")

// Server receives hook invocations over a unix socket and dispatches them
// to registered callbacks.
type Server struct {
	sock      string
	l         net.Listener
	serveDone chan struct{}
	handlers  sync.WaitGroup

	// stopCallbacks cancels the contexts of running callbacks.
	callbacksCtx  context.Context
	stopCallbacks context.CancelCauseFunc

	mu        sync.Mutex
	callbacks map[string]registration
	// reading holds the connections whose request is still being read.
	reading  map[net.Conn]struct{}
	shutdown bool
}

// Listen creates a Server listening on a unix socket at sockPath, replacing
// any file already there.
func Listen(sockPath string) (*Server, error) {
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return nil, err
	}
	l, err := listen(sockPath)
	if err != nil {
		return nil, err
	}
	callbacksCtx, stopCallbacks := context.WithCancelCause(context.Background())
	s := &Server{
		sock:          sockPath,
		l:             l,
		serveDone:     make(chan struct{}),
		callbacksCtx:  callbacksCtx,
		stopCallbacks: stopCallbacks,
		callbacks:     map[string]registration{},
		reading:       map[net.Conn]struct{}{},
	}
	go s.serve()
	return s, nil
}

// Shutdown stops the server from accepting hook invocations, fails those
// whose callbacks have not been called yet, and waits for the callbacks
// which have been called to return. If ctx is done first, Shutdown cancels
// the contexts of the remaining callbacks and returns the context's error
// without waiting for them.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.l.Close()
	// Wait for serve to return so that it starts no more handlers.
	<-s.serveDone

	s.mu.Lock()
	s.shutdown = true
	// Interrupt reading requests which may never finish arriving.
	for conn := range s.reading {
		_ = conn.SetReadDeadline(time.Now())
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.handlers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return err
	case <-ctx.Done():
		s.stopCallbacks(errShutdown)
		return ctx.Err()
	}
}

// Register registers fn and returns a hook which calls it when run. The
// context passed to fn is derived from ctx, so it carries ctx's values and
// is cancelled when ctx is. The hook fails without calling fn if it is run
// after ctx is done or unregister is called.
func (s *Server) Register(ctx context.Context, fn Callback) (hook specs.Hook, unregister func()) {
	token := stringid.GenerateRandomID()
	s.mu.Lock()
	s.callbacks[token] = registration{ctx: ctx, fn: fn}
	s.mu.Unlock()

	hook = specs.Hook{
		// Use the path of the running binary, not os.Args[0], so the hook
		// speaks the same protocol as the server even if the binary on disk
		// has been replaced.
		Path: filepath.Join("/proc", strconv.Itoa(os.Getpid()), "exe"),
		Args: []string{reexecName, s.sock, token},
	}
	unregister = func() {
		s.mu.Lock()
		delete(s.callbacks, token)
		s.mu.Unlock()
	}
	return hook, unregister
}

func (s *Server) serve() {
	defer close(s.serveDone)
	var delay time.Duration
	for {
		conn, err := s.l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Back off like net/http.Server.Serve, so that a persistent
			// error such as EMFILE does not make this a busy loop.
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			log.G(context.TODO()).WithError(err).WithField("retry", delay).Error("ocihook: error accepting connection")
			time.Sleep(delay)
			continue
		}
		delay = 0
		s.mu.Lock()
		s.reading[conn] = struct{}{}
		s.mu.Unlock()
		s.handlers.Go(func() { s.handleConn(conn) })
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	err := s.handle(conn)
	resp := []byte{statusOK}
	if err != nil {
		resp = append([]byte{statusError}, err.Error()...)
	}
	if _, err := conn.Write(resp); err != nil {
		log.G(context.TODO()).WithError(err).Error("ocihook: error responding to hook")
	}

	// Shutdown may have interrupted reading the request, and the client
	// fails anyway, so do not keep Shutdown waiting on it.
	if errors.Is(err, errShutdown) {
		return
	}

	// Read whatever the client sent which the server has not read, such as
	// data after the state or the remainder of a rejected request: closing
	// a unix socket with unread data makes the client's read of the
	// response fail with ECONNRESET.
	_ = conn.SetReadDeadline(time.Now().Add(drainTimeout))
	_, _ = io.Copy(io.Discard, io.LimitReader(conn, maxRequestSize))
}

func (s *Server) handle(conn net.Conn) error {
	token, state, err := readRequest(conn)

	s.mu.Lock()
	delete(s.reading, conn)
	shutdown := s.shutdown
	reg, ok := s.callbacks[token]
	s.mu.Unlock()

	if shutdown {
		return errShutdown
	}
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no hook registered for container %s", state.ID)
	}
	if err := context.Cause(reg.ctx); err != nil {
		return fmt.Errorf("hook for container %s: %w", state.ID, err)
	}

	ctx, cancel := context.WithCancelCause(reg.ctx)
	defer cancel(nil)
	defer context.AfterFunc(s.callbacksCtx, func() { cancel(context.Cause(s.callbacksCtx)) })()
	return reg.fn(ctx, state)
}

func readRequest(conn net.Conn) (token string, _ specs.State, _ error) {
	r := &io.LimitedReader{R: conn, N: maxRequestSize}
	br := bufio.NewReader(r)

	// ReadSlice fails if the token does not fit in br's buffer. The slice
	// it returns is overwritten by the next read from br.
	tok, err := br.ReadSlice(0)
	if err != nil {
		return "", specs.State{}, fmt.Errorf("reading token: %w", err)
	}
	token = string(tok[:len(tok)-1])

	var state specs.State
	if err := json.NewDecoder(br).Decode(&state); err != nil {
		if r.N == 0 {
			return "", specs.State{}, fmt.Errorf("request exceeds %d bytes", maxRequestSize)
		}
		return "", specs.State{}, fmt.Errorf("decoding container state: %w", err)
	}
	return token, state, nil
}
