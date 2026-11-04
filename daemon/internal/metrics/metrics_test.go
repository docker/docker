package metrics

import (
	"errors"
	"fmt"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/v2/errdefs"
	"gotest.tools/v3/assert"
)

func TestCategorizeErrorReason(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected string
	}{
		// Containerd errdefs
		{
			name:     "containerd not found error",
			err:      cerrdefs.ErrNotFound,
			expected: "not_found",
		},
		{
			name:     "containerd conflict error",
			err:      cerrdefs.ErrConflict,
			expected: "conflict",
		},
		{
			name:     "containerd unauthorized error",
			err:      cerrdefs.ErrUnauthenticated,
			expected: "permission_denied",
		},
		{
			name:     "containerd permission denied error",
			err:      cerrdefs.ErrPermissionDenied,
			expected: "permission_denied",
		},
		{
			name:     "containerd invalid argument error",
			err:      cerrdefs.ErrInvalidArgument,
			expected: "invalid_argument",
		},
		{
			name:     "wrapped containerd not found error",
			err:      fmt.Errorf("image not available: %w", cerrdefs.ErrNotFound),
			expected: "not_found",
		},
		{
			name:     "wrapped containerd conflict error",
			err:      fmt.Errorf("container is using image: %w", cerrdefs.ErrConflict),
			expected: "conflict",
		},
		// Moby errdefs
		{
			name:     "moby not found error",
			err:      errdefs.NotFound(errors.New("not found")),
			expected: "not_found",
		},
		{
			name:     "moby conflict error",
			err:      errdefs.Conflict(errors.New("conflict")),
			expected: "conflict",
		},
		{
			name:     "moby unauthorized error",
			err:      errdefs.Unauthorized(errors.New("unauthorized")),
			expected: "permission_denied",
		},
		{
			name:     "moby forbidden error",
			err:      errdefs.Forbidden(errors.New("forbidden")),
			expected: "permission_denied",
		},
		{
			name:     "moby invalid parameter error",
			err:      errdefs.InvalidParameter(errors.New("invalid")),
			expected: "invalid_argument",
		},
		{
			name:     "wrapped moby not found error",
			err:      errdefs.NotFound(errors.New("image does not exist")),
			expected: "not_found",
		},
		{
			name:     "wrapped moby conflict error",
			err:      errdefs.Conflict(errors.New("container is using image")),
			expected: "conflict",
		},
		// Unknown errors
		{
			name:     "unknown error",
			err:      errors.New("some random error"),
			expected: "unknown",
		},
		{
			name:     "nil error",
			err:      nil,
			expected: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := CategorizeErrorReason(tt.err)
			assert.Equal(t, tt.expected, result)
		})
	}
}
