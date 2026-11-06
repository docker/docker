package cleanups

import (
	"context"
	"slices"

	"github.com/moby/moby/v2/daemon/internal/multierror"
)

type Composite struct {
	cleanups []func(context.Context) error
}

// Add adds a cleanup to be called.
func (c *Composite) Add(f func(context.Context) error) {
	c.cleanups = append(c.cleanups, f)
}

// Call calls all cleanups in reverse order and returns an error combining all
// non-nil errors.
func (c *Composite) Call(ctx context.Context) error {
	err := call(ctx, c.cleanups)
	c.cleanups = nil
	return err
}

// Release removes all cleanups, turning Call into a no-op.
// Caller still can call the cleanups by calling the returned function
// which is equivalent to calling the Call before Release was called.
func (c *Composite) Release() func(context.Context) error {
	cleanups := c.cleanups
	c.cleanups = nil
	return func(ctx context.Context) error {
		return call(ctx, cleanups)
	}
}

func call(ctx context.Context, cleanups []func(context.Context) error) error {
	var errs []error
	for _, c := range slices.Backward(cleanups) {

		errs = append(errs, c(ctx))
	}
	return multierror.Join(errs...)
}
