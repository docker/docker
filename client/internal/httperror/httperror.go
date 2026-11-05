// Package httperror classifies errors using HTTP status codes.
package httperror

import (
	"errors"
	"net/http"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errhttp"
)

type httpError struct {
	err    error
	errDef error
}

func (e *httpError) Error() string {
	return e.err.Error()
}

func (e *httpError) Unwrap() error {
	return e.err
}

func (e *httpError) Is(target error) bool {
	return errors.Is(e.errDef, target)
}

// FromStatusCode classifies err using the HTTP status code while preserving
// its message and error chain.
func FromStatusCode(err error, statusCode int) error {
	if err == nil || statusCode == 0 {
		return err
	}

	// TODO(thaJeztah): Remove the explicit status-code list once errhttp.ToNative
	// provides a way to distinguish recognized codes from unexpected statuses,
	// allowing us to apply our fallback classifications only when no match is found.
	var classification error
	switch statusCode {
	case http.StatusNotModified,
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusConflict,
		http.StatusPreconditionFailed,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusNotImplemented,
		http.StatusServiceUnavailable:
		classification = errhttp.ToNative(statusCode)
	default:
		switch {
		case statusCode >= http.StatusOK && statusCode < http.StatusBadRequest:
			// The HTTP status indicates success or redirection, so err may originate
			// from client-side processing. Preserve it without adding a classification.
			return err
		case statusCode >= http.StatusBadRequest && statusCode < http.StatusInternalServerError:
			classification = cerrdefs.ErrInvalidArgument
		case statusCode >= http.StatusInternalServerError && statusCode < 600:
			classification = cerrdefs.ErrInternal
		default:
			classification = cerrdefs.ErrUnknown
		}
	}

	return &httpError{err: err, errDef: classification}
}
