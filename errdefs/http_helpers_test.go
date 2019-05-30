package errdefs

import (
	"fmt"
	"net/http"
	"testing"

	"gotest.tools/assert"
)

func TestFromStatusCode(t *testing.T) {
	testErr := fmt.Errorf("some error occurred")

	testCases := []struct {
		err    error
		status int
		check  func(error) bool
	}{
		{
			err:    testErr,
			status: http.StatusNotFound,
			check:  IsNotFound,
		},
		{
			err:    testErr,
			status: http.StatusBadRequest,
			check:  IsInvalidParameter,
		},
		{
			err:    testErr,
			status: http.StatusConflict,
			check:  IsConflict,
		},
		{
			err:    testErr,
			status: http.StatusUnauthorized,
			check:  IsUnauthorized,
		},
		{
			err:    testErr,
			status: http.StatusServiceUnavailable,
			check:  IsUnavailable,
		},
		{
			err:    testErr,
			status: http.StatusForbidden,
			check:  IsForbidden,
		},
		{
			err:    testErr,
			status: http.StatusNotModified,
			check:  IsNotModified,
		},
		{
			err:    testErr,
			status: http.StatusNotImplemented,
			check:  IsNotImplemented,
		},
		{
			err:    testErr,
			status: http.StatusInternalServerError,
			check:  IsSystem,
		},
		{
			err:    Unknown(testErr),
			status: http.StatusInternalServerError,
			check:  IsUnknown,
		},
		{
			err:    DataLoss(testErr),
			status: http.StatusInternalServerError,
			check:  IsDataLoss,
		},
		{
			err:    Deadline(testErr),
			status: http.StatusInternalServerError,
			check:  IsDeadline,
		},
		{
			err:    Cancelled(testErr),
			status: http.StatusInternalServerError,
			check:  IsCancelled,
		},
	}

	for _, tc := range testCases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			err := FromStatusCode(tc.err, tc.status)
			assert.Check(t, tc.check(err), "unexpected error-type %T", err)
		})
	}
}
