package httperror_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client/internal/httperror"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestFromStatusCode(t *testing.T) {
	original := errors.New("original error")
	wrapped := fmt.Errorf("request failed: %w", original)

	tests := []struct {
		name         string
		statusCode   int
		wantError    error
		wantOriginal bool
	}{
		{
			name:         "nil error",
			statusCode:   http.StatusNotFound,
			wantError:    nil,
			wantOriginal: true,
		},
		{
			name:       "bad request",
			statusCode: http.StatusBadRequest,
			wantError:  cerrdefs.ErrInvalidArgument,
		},
		{
			name:       "unauthorized",
			statusCode: http.StatusUnauthorized,
			wantError:  cerrdefs.ErrUnauthenticated,
		},
		{
			name:       "not found",
			statusCode: http.StatusNotFound,
			wantError:  cerrdefs.ErrNotFound,
		},
		{
			name:       "resource exhausted",
			statusCode: http.StatusTooManyRequests,
			wantError:  cerrdefs.ErrResourceExhausted,
		},
		{
			name:       "internal",
			statusCode: http.StatusInternalServerError,
			wantError:  cerrdefs.ErrInternal,
		},
		{
			name:       "unknown client status",
			statusCode: 499,
			wantError:  cerrdefs.ErrInvalidArgument,
		},
		{
			name:       "unknown server status",
			statusCode: 599,
			wantError:  cerrdefs.ErrInternal,
		},
		{
			name:         "missing status",
			statusCode:   0,
			wantError:    wrapped,
			wantOriginal: true,
		},
		{
			name:         "client error after successful response",
			statusCode:   http.StatusOK,
			wantError:    wrapped,
			wantOriginal: true,
		},
		{
			name:         "client error after redirect",
			statusCode:   http.StatusTemporaryRedirect,
			wantError:    wrapped,
			wantOriginal: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantError == nil {
				assert.NilError(t, httperror.FromStatusCode(nil, tc.statusCode))
				return
			}
			err := httperror.FromStatusCode(wrapped, tc.statusCode)
			assert.Check(t, is.Error(err, wrapped.Error()))
			assert.Check(t, errors.Is(err, original))
			if tc.wantOriginal {
				assert.Equal(t, err, tc.wantError)
			} else {
				assert.Check(t, errors.Is(err, tc.wantError))
				assert.Equal(t, errors.Unwrap(err), wrapped)
			}
		})
	}
}

func TestFromStatusCodeNil(t *testing.T) {
	assert.NilError(t, httperror.FromStatusCode(nil, http.StatusNotFound))
}

func TestFromStatusCodePreservesOriginalClassification(t *testing.T) {
	original := fmt.Errorf("original: %w", cerrdefs.ErrNotFound)
	err := httperror.FromStatusCode(original, http.StatusForbidden)

	assert.Check(t, errors.Is(err, cerrdefs.ErrPermissionDenied))
	assert.Check(t, errors.Is(err, cerrdefs.ErrNotFound))
}
