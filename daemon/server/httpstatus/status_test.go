package httpstatus_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/distribution/registry/api/errcode"
	"github.com/moby/moby/v2/daemon/server/httpstatus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestFromError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{
			name: "nil",
			want: http.StatusInternalServerError,
		},
		{
			name: "not found",
			err:  cerrdefs.ErrNotFound,
			want: http.StatusNotFound,
		},
		{
			name: "invalid argument",
			err:  cerrdefs.ErrInvalidArgument,
			want: http.StatusBadRequest,
		},
		{
			name: "conflict",
			err:  cerrdefs.ErrConflict,
			want: http.StatusConflict,
		},
		{
			name: "unauthorized",
			err:  cerrdefs.ErrUnauthenticated,
			want: http.StatusUnauthorized,
		},
		{
			name: "unavailable",
			err:  cerrdefs.ErrUnavailable,
			want: http.StatusServiceUnavailable,
		},
		{
			name: "permission denied",
			err:  cerrdefs.ErrPermissionDenied,
			want: http.StatusForbidden,
		},
		{
			name: "not modified",
			err:  cerrdefs.ErrNotModified,
			want: http.StatusNotModified,
		},
		{
			name: "not implemented",
			err:  cerrdefs.ErrNotImplemented,
			want: http.StatusNotImplemented,
		},
		{
			name: "failed precondition",
			err:  cerrdefs.ErrFailedPrecondition,
			want: http.StatusPreconditionFailed,
		},
		{
			name: "resource exhausted",
			err:  cerrdefs.ErrResourceExhausted,
			want: http.StatusTooManyRequests,
		},
		{
			name: "internal",
			err:  cerrdefs.ErrInternal,
			want: http.StatusInternalServerError,
		},
		{
			name: "data loss",
			err:  cerrdefs.ErrDataLoss,
			want: http.StatusInternalServerError,
		},
		{
			name: "unknown",
			err:  cerrdefs.ErrUnknown,
			want: http.StatusInternalServerError,
		},
		{
			name: "unclassified",
			err:  errors.New("unclassified"),
			want: http.StatusInternalServerError,
		},
		{
			name: "canceled",
			err:  context.Canceled,
			want: http.StatusInternalServerError,
		},
		{
			name: "deadline exceeded",
			err:  context.DeadlineExceeded,
			want: http.StatusInternalServerError,
		},
		{
			name: "wrapped",
			err:  fmt.Errorf("wrapped: %w", cerrdefs.ErrNotFound),
			want: http.StatusNotFound,
		},
		{
			name: "outermost classification",
			err:  fmt.Errorf("wrapped: %w: %w", cerrdefs.ErrInvalidArgument, fmt.Errorf("wrapped: %w", cerrdefs.ErrNotFound)),
			want: http.StatusBadRequest,
		},
		{
			name: "wrapped unclassified error",
			err:  fmt.Errorf("wrapped: %w", errors.New("unclassified")),
			want: http.StatusInternalServerError,
		},
		{
			name: "joined errors",
			err:  errors.Join(errors.New("unclassified"), cerrdefs.ErrNotFound),
			want: http.StatusNotFound,
		},
		{
			name: "joined unclassified errors",
			err:  errors.Join(errors.New("first"), errors.New("second")),
			want: http.StatusInternalServerError,
		},
		// gRPC errors
		{
			name: "gRPC not found",
			err:  status.Error(codes.NotFound, "not found"),
			want: http.StatusNotFound,
		},
		{
			name: "gRPC failed precondition",
			err:  status.Error(codes.FailedPrecondition, "failed"),
			want: http.StatusBadRequest,
		},
		// distribution errors
		{
			name: "distribution unauthorized",
			err:  errcode.ErrorCodeUnauthorized,
			want: http.StatusUnauthorized,
		},
		{
			name: "distribution errors",
			err:  errcode.Errors{errcode.ErrorCodeDenied},
			want: http.StatusForbidden,
		},
		{
			name: "empty distribution errors",
			err:  errcode.Errors{},
			want: http.StatusInternalServerError,
		},
		{
			name: "wrapped distribution error",
			err:  fmt.Errorf("wrapped: %w", errcode.ErrorCodeDenied),
			want: http.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Check(t, is.Equal(tc.want, httpstatus.FromError(tc.err)))
		})
	}
}
