package ioutils

import (
	"context"
	"io"
	"runtime/debug"
	"sync/atomic"

	"github.com/containerd/log"
)

// readCloserWrapper wraps an io.Reader, and implements an io.ReadCloser
// It calls the given callback function when closed. It should be constructed
// with NewReadCloserWrapper
type readCloserWrapper struct {
	io.Reader
	closer func() error
	closed atomic.Bool
}

// Close calls back the passed closer function
func (r *readCloserWrapper) Close() error {
	if !r.closed.CompareAndSwap(false, true) {
		subsequentCloseWarn("ReadCloserWrapper")
		return nil
	}
	return r.closer()
}

// NewReadCloserWrapper wraps an io.Reader, and implements an io.ReadCloser.
// It calls the given callback function when closed.
func NewReadCloserWrapper(r io.Reader, closer func() error) io.ReadCloser {
	return &readCloserWrapper{
		Reader: r,
		closer: closer,
	}
}

// cancelReadCloser wraps an io.ReadCloser with a context for cancelling read
// operations.
type cancelReadCloser struct {
	cancel func()
	pR     *io.PipeReader // Stream to read from
	pW     *io.PipeWriter
	closed atomic.Bool
}

// NewCancelReadCloser creates a wrapper that closes the ReadCloser when the
// context is cancelled. The returned io.ReadCloser must be closed when it is
// no longer needed.
func NewCancelReadCloser(ctx context.Context, in io.ReadCloser) io.ReadCloser {
	pR, pW := io.Pipe()

	// Create a context used to signal when the pipe is closed
	doneCtx, cancel := context.WithCancel(context.Background())

	p := &cancelReadCloser{
		cancel: cancel,
		pR:     pR,
		pW:     pW,
	}

	go func() {
		_, err := io.Copy(pW, in)
		select {
		case <-ctx.Done():
			// If the context was closed, p.closeWithError
			// was already called. Calling it again would
			// change the error that Read returns.
		default:
			p.closeWithError(err)
		}
		in.Close()
	}()
	go func() {
		select {
		case <-ctx.Done():
			p.closeWithError(ctx.Err())
		case <-doneCtx.Done():
		}
	}()

	return p
}

// Read wraps the Read method of the pipe that provides data from the wrapped
// ReadCloser.
func (p *cancelReadCloser) Read(buf []byte) (int, error) {
	return p.pR.Read(buf)
}

// closeWithError closes the wrapper and its underlying reader. It will
// cause future calls to Read to return err.
func (p *cancelReadCloser) closeWithError(err error) {
	_ = p.pW.CloseWithError(err)
	p.cancel()
}

// Close closes the wrapper its underlying reader. It will cause
// future calls to Read to return io.EOF.
func (p *cancelReadCloser) Close() error {
	if !p.closed.CompareAndSwap(false, true) {
		subsequentCloseWarn("cancelReadCloser")
		return nil
	}
	p.closeWithError(io.EOF)
	return nil
}

func subsequentCloseWarn(name string) {
	log.G(context.TODO()).Error("subsequent attempt to close " + name)
	if log.GetLevel() >= log.DebugLevel {
		log.G(context.TODO()).Errorf("stack trace: %s", string(debug.Stack()))
	}
}
