// Copyright 2019+ Klaus Post. All rights reserved.
// License information can be found in the LICENSE file.
// Based on work by Yann Collet, released under BSD License.

package zstd

import (
	"errors"
	"fmt"
	"runtime"
)

// DOption is an option for creating a decoder.
type DOption func(*decoderOptions) error

// options retains accumulated state of multiple options.
type decoderOptions struct {
	lowMem         bool
	concurrent     int
	maxDecodedSize uint64
	dicts          []dict
}

func (o *decoderOptions) setDefault() {
	*o = decoderOptions{
		// use less ram: true for now, but may change.
		lowMem:     true,
		concurrent: runtime.GOMAXPROCS(0),
	}
	o.maxDecodedSize = 1 << 63
}

// WithDecoderLowmem will set whether to use a lower amount of memory,
// but possibly have to allocate more while running.
func WithDecoderLowmem(b bool) DOption {
	return func(o *decoderOptions) error { o.lowMem = b; return nil }
}

// WithDecoderConcurrency will set the concurrency,
// meaning the maximum number of decoders to run concurrently.
// The value supplied must be at least 1.
// By default this will be set to GOMAXPROCS.
func WithDecoderConcurrency(n int) DOption {
	return func(o *decoderOptions) error {
		if n <= 0 {
			return fmt.Errorf("Concurrency must be at least 1")
		}
		o.concurrent = n
		return nil
	}
}

// WithDecoderMaxMemory allows to set a maximum decoded size for in-memory
// non-streaming operations or maximum window size for streaming operations.
// This can be used to control memory usage of potentially hostile content.
// For streaming operations, the maximum window size is capped at 1<<30 bytes.
// Maximum and default is 1 << 63 bytes.
func WithDecoderMaxMemory(n uint64) DOption {
	return func(o *decoderOptions) error {
		if n == 0 {
			return errors.New("WithDecoderMaxMemory must be at least 1")
		}
		if n > 1<<63 {
			return fmt.Errorf("WithDecoderMaxmemory must be less than 1 << 63")
		}
		o.maxDecodedSize = n
		return nil
	}
}

// WithDecoderDicts allows to register one or more dictionaries for the decoder.
// If several dictionaries with the same ID is provided the last one will be used.
func WithDecoderDicts(dicts ...[]byte) DOption {
	return func(o *decoderOptions) error {
		for _, b := range dicts {
			d, err := loadDict(b)
			if err != nil {
				return err
			}
			o.dicts = append(o.dicts, *d)
		}
		return nil
	}
}
