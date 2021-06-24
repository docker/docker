package compute // import "github.com/docker/docker/pkg/compute"

import (
	"context"
)

// TODO(rvolosatovs): Refactor into result[T] once support for generics lands.

// result is a result of a computation.
type result struct {
	Value interface{}
	Error error
}

type Func func(context.Context) (interface{}, error)

func NewSingleton(f Func) *Singleton {
	return &Singleton{
		callCh:   make(chan struct{}, 1),
		resultCh: make(chan result),
		f:        f,
	}
}

type Singleton struct {
	callCh   chan struct{}
	resultCh chan result
	f        Func
}

func (s Singleton) Do(ctx context.Context) (interface{}, error) {
	select {
	case s.callCh <- struct{}{}:
		// Lock acquired - perform computation.
		defer func() {
			<-s.callCh // Release lock.
		}()

	case res := <-s.resultCh:
		// Another goroutine computed the result - return.
		return res.Value, res.Error

	case <-ctx.Done():
		return nil, ctx.Err()
	}

	var res result
	v, err := s.f(ctx)
	if err != nil {
		res = result{
			Error: err,
		}
	} else {
		res = result{
			Value: v,
		}
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()

		case s.resultCh <- res:
			// Push computation result to other goroutines calling the function, if any.

		default:
			return res.Value, res.Error
		}
	}
}
