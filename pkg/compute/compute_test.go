package compute_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	. "github.com/docker/docker/pkg/compute"
)

func TestSingleton(t *testing.T) {
	assert := func(ctx context.Context, s *Singleton, expectedValue interface{}, expectedError error) {
		v, err := s.Do(ctx)
		if err != expectedError || v != expectedValue {
			t.Errorf(`actual: (value: %v; error: %v)
expected: (value: %v; error: %v)`,
				v, err,
				expectedValue, expectedError)
		}
	}

	const testValue = 42
	testError := errors.New("test error")

	makeIDFunc := func(v interface{}, err error) func(context.Context) (interface{}, error) {
		return func(context.Context) (interface{}, error) { return v, err }
	}

	assert(context.Background(), NewSingleton(makeIDFunc(nil, testError)), nil, testError)
	assert(context.Background(), NewSingleton(makeIDFunc(testValue, nil)), testValue, nil)

	startCh := make(chan struct{})
	doneCh := make(chan struct{})

	s := NewSingleton(func(context.Context) (interface{}, error) {
		close(startCh)

		<-doneCh
		return testValue, nil
	})

	baseWG := &sync.WaitGroup{}
	for i := 0; i < 10; i++ {
		baseWG.Add(1)
		go func() {
			assert(context.Background(), s, testValue, nil)
			baseWG.Done()
		}()
	}

	select {
	case <-startCh:

	case <-time.After(time.Second):
		t.Fatalf("test timeout")
	}

	cancelWG := &sync.WaitGroup{}
	cancelCtx, cancel := context.WithCancel(context.Background())
	for i := 0; i < 10; i++ {
		cancelWG.Add(1)
		go func() {
			assert(cancelCtx, s, nil, context.Canceled)
			cancelWG.Done()
		}()
	}
	time.Sleep(time.Nanosecond)
	cancel()
	cancelWG.Wait()

	select {
	case doneCh <- struct{}{}:

	case <-time.After(time.Second):
		t.Fatalf("test timeout")
	}
	baseWG.Wait()
}
