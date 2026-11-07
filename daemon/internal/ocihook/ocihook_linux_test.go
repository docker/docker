package ocihook

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/sys/reexec"
	"github.com/opencontainers/runtime-spec/specs-go"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/poll"
)

func TestMain(m *testing.M) {
	if reexec.Init() {
		return
	}
	os.Exit(m.Run())
}

// runOCIHook runs hook the way an OCI runtime does, passing state on stdin.
func runOCIHook(t *testing.T, hook specs.Hook, state specs.State) (stderr string, _ error) {
	t.Helper()
	stdin, err := json.Marshal(state)
	assert.NilError(t, err)
	return runOCIHookRaw(hook, stdin)
}

func runOCIHookRaw(hook specs.Hook, stdin []byte) (stderr string, _ error) {
	cmd, errBuf := hookCmd(hook, bytes.NewReader(stdin))
	err := cmd.Run()
	return errBuf.String(), err
}

func hookCmd(hook specs.Hook, stdin io.Reader) (*exec.Cmd, *bytes.Buffer) {
	var errBuf bytes.Buffer
	return &exec.Cmd{
		Path:   hook.Path,
		Args:   hook.Args,
		Env:    hook.Env,
		Stdin:  stdin,
		Stderr: &errBuf,
	}, &errBuf
}

func newServer(t *testing.T) *Server {
	t.Helper()
	s, err := Listen(filepath.Join(t.TempDir(), "oci-hook.sock"))
	assert.NilError(t, err)
	t.Cleanup(func() {
		// Shutdown cancels the contexts of callbacks still running when
		// the timeout expires.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	return s
}

func TestHook(t *testing.T) {
	s := newServer(t)

	var got []specs.State
	hook, unregister := s.Register(t.Context(), func(_ context.Context, st specs.State) error {
		got = append(got, st)
		return nil
	})
	defer unregister()

	want := specs.State{Version: specs.Version, ID: "ctr1", Status: specs.StateCreated, Pid: 1234, Bundle: "/bundle"}
	stderr, err := runOCIHook(t, hook, want)
	assert.NilError(t, err, stderr)
	assert.Check(t, is.DeepEqual(got, []specs.State{want}))
}

func TestHookCallbackError(t *testing.T) {
	s := newServer(t)

	hook, unregister := s.Register(t.Context(), func(context.Context, specs.State) error {
		return errors.New("callback failed")
	})
	defer unregister()

	stderr, err := runOCIHook(t, hook, specs.State{ID: "ctr1"})
	assert.Check(t, err != nil, "expected hook to fail")
	assert.Check(t, is.Contains(stderr, "callback failed"))
}

func TestHookLargeState(t *testing.T) {
	s := newServer(t)

	var got []specs.State
	hook, unregister := s.Register(t.Context(), func(_ context.Context, st specs.State) error {
		got = append(got, st)
		return nil
	})
	defer unregister()

	// A state too large for the server's read buffer makes the server
	// refill the buffer the token was read into.
	want := specs.State{ID: "ctr1", Annotations: map[string]string{"a": strings.Repeat("a", 64<<10)}}
	stderr, err := runOCIHook(t, hook, want)
	assert.NilError(t, err, stderr)
	assert.Check(t, is.DeepEqual(got, []specs.State{want}))
}

func TestHookTrailingData(t *testing.T) {
	s := newServer(t)

	var called bool
	hook, unregister := s.Register(t.Context(), func(context.Context, specs.State) error {
		called = true
		return nil
	})
	defer unregister()

	// Data after the state is ignored, and must not keep the hook from
	// reading the response.
	stdin, err := json.Marshal(specs.State{ID: "ctr1"})
	assert.NilError(t, err)
	stderr, err := runOCIHookRaw(hook, append(stdin, "\n"+strings.Repeat(" ", 64<<10)...))
	assert.Check(t, err, stderr)
	assert.Check(t, called)
}

func TestHookInvalidState(t *testing.T) {
	s := newServer(t)

	var called bool
	hook, unregister := s.Register(t.Context(), func(context.Context, specs.State) error {
		called = true
		return nil
	})
	defer unregister()

	for _, tc := range []struct {
		name  string
		stdin string
	}{
		{name: "empty", stdin: ""},
		{name: "truncated", stdin: `{"ociVersion":"1.0.2","id":"ctr1",`},
		{name: "not json", stdin: "not json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stderr, err := runOCIHookRaw(hook, []byte(tc.stdin))
			assert.Check(t, err != nil, "expected hook to fail")
			assert.Check(t, is.Contains(stderr, "decoding container state"))
		})
	}
	assert.Check(t, !called)
}

func TestRequestTooLarge(t *testing.T) {
	s := newServer(t)

	var called atomic.Bool
	hook, unregister := s.Register(t.Context(), func(context.Context, specs.State) error {
		called.Store(true)
		return nil
	})
	defer unregister()

	// Talk to the server directly: the hook process would fail writing
	// the excess with EPIPE before it could read the server's response.
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: hook.Args[1], Net: "unix"})
	assert.NilError(t, err)
	defer conn.Close()

	go func() {
		// Writes start failing once the server stops reading.
		_, _ = io.WriteString(conn, hook.Args[2]+"\x00")
		_, _ = io.WriteString(conn, `{"id":"`+strings.Repeat("a", maxRequestSize)+`"}`)
		_ = conn.CloseWrite()
	}()

	assert.Check(t, is.Error(readResponse(conn), fmt.Sprintf("request exceeds %d bytes", maxRequestSize)))
	assert.Check(t, !called.Load())
}

type ctxKey struct{}

func TestHookCallbackContext(t *testing.T) {
	s := newServer(t)

	ctx := context.WithValue(t.Context(), ctxKey{}, "registered")
	var got any
	hook, unregister := s.Register(ctx, func(ctx context.Context, _ specs.State) error {
		got = ctx.Value(ctxKey{})
		return nil
	})
	defer unregister()

	stderr, err := runOCIHook(t, hook, specs.State{ID: "ctr1"})
	assert.NilError(t, err, stderr)
	assert.Check(t, is.Equal(got, "registered"))
}

func TestHookRegistrationContextDone(t *testing.T) {
	s := newServer(t)

	ctx, cancel := context.WithCancel(t.Context())
	var called bool
	hook, unregister := s.Register(ctx, func(context.Context, specs.State) error {
		called = true
		return nil
	})
	defer unregister()
	cancel()

	stderr, err := runOCIHook(t, hook, specs.State{ID: "ctr1"})
	assert.Check(t, err != nil, "expected hook to fail")
	assert.Check(t, is.Contains(stderr, "hook for container ctr1: "+context.Canceled.Error()))
	assert.Check(t, !called)
}

func TestHookRegistrationContextCancelled(t *testing.T) {
	s := newServer(t)

	cause := errors.New("registration cancelled")
	ctx, cancel := context.WithCancelCause(t.Context())
	cmd, stderr, _ := startBlockingHook(t, s, ctx)
	cancel(cause)

	assert.Check(t, waitHook(t, cmd) != nil, "expected hook to fail")
	assert.Check(t, is.Contains(stderr.String(), cause.Error()))
}

func TestRequestWithoutToken(t *testing.T) {
	s := newServer(t)

	hook, unregister := s.Register(t.Context(), func(context.Context, specs.State) error {
		return nil
	})
	defer unregister()

	for _, tc := range []struct {
		name    string
		request string
		wantErr string
	}{
		{name: "empty", request: "", wantErr: "reading token: EOF"},
		{name: "no delimiter", request: hook.Args[2], wantErr: "reading token: EOF"},
		{name: "token too long", request: strings.Repeat("a", 8192) + "\x00{}", wantErr: "reading token: " + bufio.ErrBufferFull.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: hook.Args[1], Net: "unix"})
			assert.NilError(t, err)
			defer conn.Close()

			go func() {
				_, _ = io.WriteString(conn, tc.request)
				_ = conn.CloseWrite()
			}()

			assert.Check(t, is.Error(readResponse(conn), tc.wantErr))
		})
	}
}

func TestHookNoResponse(t *testing.T) {
	s := newServer(t)
	hook, unregister := s.Register(t.Context(), func(context.Context, specs.State) error {
		return nil
	})
	defer unregister()

	// Point the hook at a listener which reads the request and closes the
	// connection without responding, as a daemon which dies would.
	sock := filepath.Join(t.TempDir(), "dead.sock")
	l, err := net.Listen("unix", sock)
	assert.NilError(t, err)
	defer l.Close()
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, conn)
		_ = conn.Close()
	}()
	hook.Args[1] = sock

	stderr, err := runOCIHook(t, hook, specs.State{ID: "ctr1"})
	assert.Check(t, err != nil, "expected hook to fail")
	assert.Check(t, is.Contains(stderr, "connection closed without a response"))
}

func TestReadResponse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		resp    string
		wantErr string
	}{
		{name: "ok", resp: "\x00"},
		{name: "error", resp: "\x01callback failed", wantErr: "callback failed"},
		{name: "empty", resp: "", wantErr: "connection closed without a response"},
		{name: "ok with trailing data", resp: "\x00x", wantErr: `malformed response "\x00x"`},
		{name: "unknown status", resp: "\x02", wantErr: `malformed response "\x02"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := readResponse(strings.NewReader(tc.resp))
			if tc.wantErr == "" {
				assert.Check(t, err)
			} else {
				assert.Check(t, is.Error(err, tc.wantErr))
			}
		})
	}
}

func TestHookUnregistered(t *testing.T) {
	s := newServer(t)

	var called bool
	hook, unregister := s.Register(t.Context(), func(context.Context, specs.State) error {
		called = true
		return nil
	})
	unregister()

	stderr, err := runOCIHook(t, hook, specs.State{ID: "ctr1"})
	assert.Check(t, err != nil, "expected hook to fail")
	assert.Check(t, is.Contains(stderr, "no hook registered for container ctr1"))
	assert.Check(t, !called)
}

func TestHookDispatchesByRegistration(t *testing.T) {
	s := newServer(t)

	var calls []string
	hook1, unregister1 := s.Register(t.Context(), func(context.Context, specs.State) error {
		calls = append(calls, "first")
		return nil
	})
	defer unregister1()
	hook2, unregister2 := s.Register(t.Context(), func(context.Context, specs.State) error {
		calls = append(calls, "second")
		return nil
	})
	defer unregister2()

	// Both registrations see the same container ID; the hook must still
	// reach the callback it was registered with.
	stderr, err := runOCIHook(t, hook2, specs.State{ID: "ctr1"})
	assert.NilError(t, err, stderr)
	stderr, err = runOCIHook(t, hook1, specs.State{ID: "ctr1"})
	assert.NilError(t, err, stderr)
	assert.Check(t, is.DeepEqual(calls, []string{"second", "first"}))
}

func TestHookAfterShutdown(t *testing.T) {
	s := newServer(t)

	var called bool
	hook, unregister := s.Register(t.Context(), func(context.Context, specs.State) error {
		called = true
		return nil
	})
	defer unregister()
	assert.NilError(t, s.Shutdown(t.Context()))

	_, err := runOCIHook(t, hook, specs.State{ID: "ctr1"})
	assert.Check(t, err != nil, "expected hook to fail")
	assert.Check(t, !called)
}

// waitHook waits for the hook process to exit, and kills it if it takes
// longer than a few seconds.
func waitHook(t *testing.T, cmd *exec.Cmd) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("timed out waiting for the hook process to exit")
		return nil
	}
}

// startBlockingHook registers a callback with ctx which blocks until
// release is closed or its context is cancelled, and runs its hook. It
// returns once the callback has been called.
func startBlockingHook(t *testing.T, s *Server, ctx context.Context) (cmd *exec.Cmd, stderr *bytes.Buffer, release chan struct{}) {
	t.Helper()
	called := make(chan struct{})
	release = make(chan struct{})
	hook, unregister := s.Register(ctx, func(ctx context.Context, _ specs.State) error {
		close(called)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	})
	t.Cleanup(unregister)

	stdin, err := json.Marshal(specs.State{ID: "ctr1"})
	assert.NilError(t, err)
	cmd, stderr = hookCmd(hook, bytes.NewReader(stdin))
	assert.NilError(t, cmd.Start())
	select {
	case <-called:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for callback")
	}
	return cmd, stderr, release
}

func TestShutdownWaitsForCallback(t *testing.T) {
	s := newServer(t)
	cmd, stderr, release := startBlockingHook(t, s, t.Context())

	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- s.Shutdown(t.Context()) }()
	select {
	case err := <-shutdownErr:
		t.Fatalf("Shutdown returned while a callback was running: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	assert.Check(t, <-shutdownErr)
	assert.Check(t, waitHook(t, cmd), stderr.String())
}

func TestShutdownContextDone(t *testing.T) {
	s := newServer(t)
	cmd, stderr, _ := startBlockingHook(t, s, t.Context())

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	assert.Check(t, is.ErrorIs(s.Shutdown(ctx), context.DeadlineExceeded))

	// The callback's context is cancelled.
	assert.Check(t, waitHook(t, cmd) != nil, "expected hook to fail")
	assert.Check(t, is.Contains(stderr.String(), errShutdown.Error()))
}

func TestShutdownInterruptsPendingRequest(t *testing.T) {
	s := newServer(t)

	var called atomic.Bool
	hook, unregister := s.Register(t.Context(), func(context.Context, specs.State) error {
		called.Store(true)
		return nil
	})
	defer unregister()

	// Send part of the state and keep the pipe open, as a runtime which
	// never finishes writing the state would.
	r, w, err := os.Pipe()
	assert.NilError(t, err)
	defer w.Close()
	_, err = w.WriteString(`{"ociVersion":"1.0.2",`)
	assert.NilError(t, err)
	cmd, stderr := hookCmd(hook, r)
	assert.NilError(t, cmd.Start())
	assert.NilError(t, r.Close())

	poll.WaitOn(t, func(poll.LogT) poll.Result {
		s.mu.Lock()
		n := len(s.reading)
		s.mu.Unlock()
		if n != 1 {
			return poll.Continue("%d requests being read", n)
		}
		return poll.Success()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := time.Now()
	assert.Check(t, s.Shutdown(ctx))
	// The interrupted request is not drained.
	assert.Check(t, time.Since(start) < drainTimeout/2, "Shutdown took %v", time.Since(start))

	// Let the hook process finish sending, and read the response.
	assert.NilError(t, w.Close())
	assert.Check(t, cmd.Wait() != nil, "expected hook to fail")
	assert.Check(t, is.Contains(stderr.String(), errShutdown.Error()))
	assert.Check(t, !called.Load())
}

func TestListenReplacesStaleSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "oci-hook.sock")
	assert.NilError(t, os.WriteFile(sock, nil, 0o600))

	s, err := Listen(sock)
	assert.NilError(t, err)
	defer s.Shutdown(t.Context())

	fi, err := os.Stat(sock)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(fi.Mode().Type(), os.ModeSocket))
	assert.Check(t, is.Equal(fi.Mode().Perm(), os.FileMode(0o600)))
}
