package httpstatus

import (
	"context"
	"fmt"
	"net/http"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/docker/distribution/registry/api/errcode"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// FromError retrieves status code from error message.
func FromError(err error) int {
	if err == nil {
		log.G(context.TODO()).WithError(err).Error("unexpected HTTP error handling")
		return http.StatusInternalServerError
	}

	// Resolve the error to ensure status is chosen from the first outermost error
	rerr := cerrdefs.Resolve(err)

	// Note that the below functions are already checking the error causal chain for matches.
	// Only check errors from the errdefs package, no new error type checking may be added
	//
	// TODO(thaJeztah): rewrite using github.com/containerd/errdefs/pkg/errhttp.ToHTTP after providing a utility to detect if a match was found.
	switch {
	case cerrdefs.IsNotFound(rerr):
		return http.StatusNotFound
	case cerrdefs.IsInvalidArgument(rerr):
		return http.StatusBadRequest
	case cerrdefs.IsConflict(rerr):
		return http.StatusConflict
	case cerrdefs.IsUnauthorized(rerr):
		return http.StatusUnauthorized
	case cerrdefs.IsFailedPrecondition(rerr):
		return http.StatusPreconditionFailed
	case cerrdefs.IsResourceExhausted(rerr):
		return http.StatusTooManyRequests
	case cerrdefs.IsUnavailable(rerr):
		return http.StatusServiceUnavailable
	case cerrdefs.IsPermissionDenied(rerr):
		return http.StatusForbidden
	case cerrdefs.IsNotModified(rerr):
		return http.StatusNotModified
	case cerrdefs.IsNotImplemented(rerr):
		return http.StatusNotImplemented
	case cerrdefs.IsInternal(rerr) || cerrdefs.IsDataLoss(rerr) || cerrdefs.IsDeadlineExceeded(rerr) || cerrdefs.IsCanceled(rerr):
		return http.StatusInternalServerError
	default:
		if statusCode := statusCodeFromGRPCError(err); statusCode != http.StatusInternalServerError {
			return statusCode
		}
		if statusCode := statusCodeFromDistributionError(err); statusCode != http.StatusInternalServerError {
			return statusCode
		}
		switch e := err.(type) {
		case interface{ Unwrap() error }:
			return FromError(e.Unwrap())
		case interface{ Unwrap() []error }:
			for _, ue := range e.Unwrap() {
				if statusCode := FromError(ue); statusCode != http.StatusInternalServerError {
					return statusCode
				}
			}
		}

		if !cerrdefs.IsUnknown(err) {
			log.G(context.TODO()).WithFields(log.Fields{
				"module":     "api",
				"error":      err,
				"error_type": fmt.Sprintf("%T", err),
			}).Debug("FIXME: Got an API for which error does not match any expected type!!!")
		}

		return http.StatusInternalServerError
	}
}

// statusCodeFromGRPCError returns status code according to gRPC error
func statusCodeFromGRPCError(err error) int {
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
		return http.StatusInternalServerError
	}
}

// statusCodeFromDistributionError returns status code according to registry errcode
// code is loosely based on errcode.ServeJSON() in docker/distribution
func statusCodeFromDistributionError(err error) int {
	switch errs := err.(type) {
	case errcode.Errors:
		if len(errs) > 0 {
			return statusCodeFromDistributionError(errs[0])
		}
		return http.StatusInternalServerError
	case errcode.ErrorCoder:
		return errs.ErrorCode().Descriptor().HTTPStatusCode
	default:
		return http.StatusInternalServerError
	}
}
