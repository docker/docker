package errdefs

import (
	"context"
	"runtime"
	"sync"

	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/util/bklog"
)

// ExecError will be returned when an error is encountered when evaluating an op.
// The error owns its original refs until Borrow or the first Release. Proxies
// and gateways retain the error; gateways also keep independent clones. They may
// clone cached originals after Release, which native result handles support
// while another ref pins the snapshot. When sharedOp.Exec completes with an
// active solver state, that state pins error refs until its last job leaves.
// Pinning follows a single Unwrap chain, not errors.Join branches; CacheMap and
// Acquire errors do not get that pin.
type ExecError struct {
	error
	Inputs        []solver.Result
	Mounts        []solver.Result
	OwnerBorrowed bool

	mu sync.Mutex // protects OwnerBorrowed
}

func (e *ExecError) Unwrap() error {
	return e.error
}

func (e *ExecError) EachRef(fn func(solver.Result) error) (err error) {
	m := map[solver.Result]struct{}{}
	for _, res := range e.Inputs {
		if res == nil {
			continue
		}
		if _, ok := m[res]; ok {
			continue
		}
		m[res] = struct{}{}
		if err1 := fn(res); err1 != nil && err == nil {
			err = err1
		}
	}
	for _, res := range e.Mounts {
		if res == nil {
			continue
		}
		if _, ok := m[res]; ok {
			continue
		}
		m[res] = struct{}{}
		if err1 := fn(res); err1 != nil && err == nil {
			err = err1
		}
	}
	return err
}

// Borrow marks the error's refs as owned by a caller, so the finalizer and
// Release do not release them. The same error can be shared by multiple
// callers; Borrow returns false if the refs were already borrowed.
// The successful caller must release the refs; later callers acquire no ownership.
func (e *ExecError) Borrow() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.OwnerBorrowed {
		return false
	}
	e.OwnerBorrowed = true
	return true
}

func (e *ExecError) Release() error {
	if !e.Borrow() {
		return nil
	}
	return e.EachRef(func(r solver.Result) error {
		return r.Release(context.TODO())
	})
}

func WithExecError(err error, inputs, mounts []solver.Result) error {
	return WithExecErrorWithContext(context.TODO(), err, inputs, mounts)
}

func WithExecErrorWithContext(ctx context.Context, err error, inputs, mounts []solver.Result) error {
	if err == nil {
		return nil
	}
	ee := &ExecError{
		error:  err,
		Inputs: inputs,
		Mounts: mounts,
	}
	runtime.SetFinalizer(ee, func(e *ExecError) {
		if e.Borrow() {
			e.EachRef(func(r solver.Result) error {
				bklog.G(ctx).Warn("leaked execError detected and released")
				r.Release(context.TODO())
				return nil
			})
		}
	})
	return ee
}
