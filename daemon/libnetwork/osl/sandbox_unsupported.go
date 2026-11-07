//go:build !linux && !windows && !freebsd

package osl

import "errors"

// ErrNotImplemented is for platforms which don't implement sandbox
var ErrNotImplemented = errors.New("not implemented")

// NewSandbox provides a new sandbox instance created in an os specific way
// provided a key which uniquely identifies the sandbox
func NewSandbox(key string, osCreate, isRestore bool) (*Namespace, error) {
	return nil, ErrNotImplemented
}
