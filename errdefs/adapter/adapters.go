// Package adapter defines additional adapters to convert errors from subsystems,
// such as containerd, grpc or the distribution API to a suitable HTTP status.

package adapter

import (
	"net/http"

	containerderrors "github.com/containerd/containerd/errdefs"
	"github.com/docker/distribution/registry/api/errcode"
	"github.com/docker/docker/errdefs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// All returns a slice with all adapters.
func All() []errdefs.Adapter {
	return []errdefs.Adapter{
		HTTPStatusCodeFromGRPCError,
		HTTPStatusCodeFromContainerdError,
		HTTPStatusCodeFromDistributionError,
	}
}

// HTTPStatusCodeFromGRPCError returns status code according to gRPC error
func HTTPStatusCodeFromGRPCError(err error) int {
	switch status.Code(err) {
	case codes.InvalidArgument: // code 3
		return http.StatusBadRequest
	case codes.NotFound: // code 5
		return http.StatusNotFound
	case codes.AlreadyExists: // code 6
		return http.StatusConflict
	case codes.PermissionDenied: // code 7
		return http.StatusForbidden
	case codes.FailedPrecondition: // code 9
		return http.StatusBadRequest
	case codes.Unauthenticated: // code 16
		return http.StatusUnauthorized
	case codes.OutOfRange: // code 11
		return http.StatusBadRequest
	case codes.Unimplemented: // code 12
		return http.StatusNotImplemented
	case codes.Unavailable: // code 14
		return http.StatusServiceUnavailable
	default:
		// codes.Canceled(1)
		// codes.Unknown(2)
		// codes.DeadlineExceeded(4)
		// codes.ResourceExhausted(8)
		// codes.Aborted(10)
		// codes.Internal(13)
		// codes.DataLoss(15)
		return errdefs.NoSuitableHTTPStatusCode
	}
}

// HTTPStatusCodeFromContainerdError returns status code for containerd errors
// when consumed directly (not through gRPC).
func HTTPStatusCodeFromContainerdError(err error) int {
	switch {
	case containerderrors.IsInvalidArgument(err):
		return http.StatusBadRequest
	case containerderrors.IsNotFound(err):
		return http.StatusNotFound
	case containerderrors.IsAlreadyExists(err):
		return http.StatusConflict
	case containerderrors.IsFailedPrecondition(err):
		return http.StatusPreconditionFailed
	case containerderrors.IsUnavailable(err):
		return http.StatusServiceUnavailable
	case containerderrors.IsNotImplemented(err):
		return http.StatusNotImplemented
	default:
		return errdefs.NoSuitableHTTPStatusCode
	}
}

// HTTPStatusCodeFromDistributionError returns status code according to registry
// errcode. Code is loosely based on errcode.ServeJSON() in docker/distribution.
func HTTPStatusCodeFromDistributionError(err error) int {
	switch errs := err.(type) {
	case errcode.Errors:
		if len(errs) < 1 {
			return http.StatusInternalServerError
		}
		if _, ok := errs[0].(errcode.ErrorCoder); ok {
			return HTTPStatusCodeFromDistributionError(errs[0])
		}
	case errcode.ErrorCoder:
		return errs.ErrorCode().Descriptor().HTTPStatusCode
	}
	return errdefs.NoSuitableHTTPStatusCode
}
