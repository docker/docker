package retryhandler

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/core/images"
	remoteserrors "github.com/containerd/containerd/v2/core/remotes/errors"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// MaxRetryBackoff is the maximum backoff time before giving up. This is a
// variable so that code which embeds BuildKit can override the default value.
var MaxRetryBackoff = 8 * time.Second

// MaxRetryAfter is the longest server-requested delay accepted for a retry.
// It is independent of the exponential backoff budget. Code which embeds
// BuildKit can override the default value.
var MaxRetryAfter = 30 * time.Second

// RetryAfterError marks a failure as retryable after at least Delay.
// A zero Delay uses the normal backoff. Delays above MaxRetryAfter
// stop retries rather than retrying before the requested delay has elapsed.
type RetryAfterError struct {
	Err   error
	Delay time.Duration
}

func (e *RetryAfterError) Error() string {
	return e.Err.Error()
}

func (e *RetryAfterError) Unwrap() error {
	return e.Err
}

func New(f images.HandlerFunc, logger func([]byte)) images.HandlerFunc {
	return func(ctx context.Context, desc ocispecs.Descriptor) ([]ocispecs.Descriptor, error) {
		descs, err := WithRetry(ctx, logger, func(ctx context.Context) ([]ocispecs.Descriptor, error) {
			return f(ctx, desc)
		})
		if err != nil {
			return nil, err
		}
		return descs, nil
	}
}

type retryContextKey struct{}

// WithRetry retries transient failures with jittered exponential backoff. Nested calls
// using the supplied context leave retries to the outer call, including New.
// f must be safe to retry and preserve retryable errors when wrapping them.
func WithRetry[T any](ctx context.Context, logger func([]byte), f func(context.Context) (T, error)) (T, error) {
	return WithRetryIf(ctx, logger, f, nil)
}

// WithRetryIf applies the normal retry policy only when shouldRetry also
// accepts the error. A nil predicate accepts all errors in the normal policy.
// This lets callers avoid retrying errors handled by an enclosing transport.
func WithRetryIf[T any](ctx context.Context, logger func([]byte), f func(context.Context) (T, error), shouldRetry func(error) bool) (T, error) {
	if ctx.Value(retryContextKey{}) != nil {
		return f(ctx)
	}
	ctx = context.WithValue(ctx, retryContextKey{}, true)
	backoff := time.Second
	for {
		v, err := f(ctx)
		if err == nil {
			return v, nil
		}
		if context.Cause(ctx) != nil {
			return v, contextFailure(ctx)
		}
		if !retryError(err) || (shouldRetry != nil && !shouldRetry(err)) {
			return v, err
		}
		if logger != nil {
			logger(fmt.Appendf(nil, "error: %v\n", err.Error()))
		}
		if backoff >= MaxRetryBackoff {
			return v, err
		}
		// Keep the retry budget independent of jitter, including the first retry.
		delay := backoff/2 + rand.N(backoff/2) //nolint:gosec // Jitter spreads load; it does not need cryptographic randomness.
		if retryAfter, ok := stderrors.AsType[*RetryAfterError](err); ok {
			if retryAfter.Delay > MaxRetryAfter {
				return v, err
			}
			delay = max(delay, retryAfter.Delay)
		}
		if logger != nil {
			logger(fmt.Appendf(nil, "retrying in %v\n", delay))
		}
		select {
		case <-ctx.Done():
			return v, contextFailure(ctx)
		case <-time.After(delay):
		}
		backoff *= 2
	}
}

func contextFailure(ctx context.Context) error {
	err := ctx.Err() //nolint:forbidigo // Cause can be custom; Err preserves canceled vs deadline exceeded.
	cause := context.Cause(ctx)
	if errors.Is(cause, err) {
		return errors.WithStack(cause)
	}
	return errors.WithStack(stderrors.Join(err, cause))
}

func retryError(err error) bool {
	if _, ok := stderrors.AsType[*RetryAfterError](err); ok {
		return true
	}

	// Retry on 5xx errors
	var errUnexpectedStatus remoteserrors.ErrUnexpectedStatus
	if errors.As(err, &errUnexpectedStatus) &&
		errUnexpectedStatus.StatusCode >= 500 &&
		errUnexpectedStatus.StatusCode <= 599 {
		return true
	}

	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, errConnectionReset) || errors.Is(err, syscall.EPIPE) || errors.Is(err, net.ErrClosed) {
		return true
	}
	// catches TLS timeout or other network-related temporary errors
	if ne := net.Error(nil); errors.As(err, &ne) && ne.Temporary() { //nolint:staticcheck // ignoring "SA1019: Temporary is deprecated", continue to propagate net.Error through the "temporary" status
		return true
	}

	return false
}
