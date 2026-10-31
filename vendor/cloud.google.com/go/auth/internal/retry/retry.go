// Copyright 2025 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package retry

import (
	"context"
	"io"
	"math/rand"
	"net/http"
	"time"
)

var (
	syscallRetryable = func(error) bool { return false }
)

// defaultBackoff is basically equivalent to gax.Backoff without the need for
// the dependency.
type defaultBackoff struct {
	max time.Duration
	mul float64
	cur time.Duration
}

func (b *defaultBackoff) Pause() time.Duration {
	d := time.Duration(1 + rand.Int63n(int64(b.cur)))
	b.cur = time.Duration(float64(b.cur) * b.mul)
	if b.cur > b.max {
		b.cur = b.max
	}
	return d
}

// Sleep is the equivalent of gax.Sleep without the need for the dependency.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	select {
	case <-ctx.Done():
		t.Stop()
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// New returns a new Retryer with the default backoff strategy.
func New() *Retryer {
	return NewWithOptions(&Options{
		Initial:     100 * time.Millisecond,
		Max:         30 * time.Second,
		Multiplier:  2,
		MaxAttempts: 5,
	})
}

// Options defines the configuration for the Retryer.
type Options struct {
	// Initial is the initial backoff duration.
	Initial time.Duration
	// Max is the maximum backoff duration for a single retry attempt.
	// It does not limit the total time of all retries.
	Max time.Duration
	// Multiplier is the factor by which the backoff duration is multiplied after each attempt.
	Multiplier float64
	// MaxAttempts is the maximum number of attempts before giving up.
	MaxAttempts int
}

// NewWithOptions returns a new Retryer with the specified backoff strategy.
// If any option is not set (zero value), it defaults to the values used in New().
func NewWithOptions(opts *Options) *Retryer {
	initial := opts.Initial
	if initial <= 0 {
		initial = 100 * time.Millisecond
	}

	max := opts.Max
	if max <= 0 {
		max = 30 * time.Second
	}

	multiplier := opts.Multiplier
	if multiplier < 1.0 {
		multiplier = 2.0
	}

	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}

	return &Retryer{
		bo: &defaultBackoff{
			cur: initial,
			max: max,
			mul: multiplier,
		},
		maxAttempts: maxAttempts,
	}
}

type backoff interface {
	Pause() time.Duration
}

// Retryer handles retry logic for HTTP requests using a configurable backoff strategy.
type Retryer struct {
	bo          backoff
	attempts    int
	maxAttempts int
}

// Retry determines if a request should be retried.
func (r *Retryer) Retry(status int, err error) (time.Duration, bool) {
	if status == http.StatusOK {
		return 0, false
	}
	retryOk := shouldRetry(status, err)
	if !retryOk {
		return 0, false
	}
	if r.attempts == r.maxAttempts {
		return 0, false
	}
	r.attempts++
	return r.bo.Pause(), true
}

func shouldRetry(status int, err error) bool {
	if 500 <= status && status <= 599 {
		return true
	}
	if err == io.ErrUnexpectedEOF {
		return true
	}
	// Transient network errors should be retried.
	if syscallRetryable(err) {
		return true
	}
	if err, ok := err.(interface{ Temporary() bool }); ok {
		if err.Temporary() {
			return true
		}
	}
	if err, ok := err.(interface{ Unwrap() error }); ok {
		return shouldRetry(status, err.Unwrap())
	}
	return false
}
