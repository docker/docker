// +build !windows

package stats // import "github.com/docker/docker/daemon/stats"

import (
	"bufio"
	"io"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
)

func TestCollector_cpuNanoSeconds(t *testing.T) {
	tests := []struct {
		name     string
		data     io.Reader
		expected uint64
	}{
		{
			name:     "zero",
			data:     strings.NewReader("cpu  0 0 0 0 0 0 0 0 0\n"),
			expected: 0,
		},
		{
			name:     "large",
			data:     strings.NewReader("cpu  403617957 265046558 675564912 22209554016 632456172 0 41772895 0 0\n"),
			expected: 242280125100000000,
		},
		{
			name:     "currentMax",
			data:     strings.NewReader("cpu  1844674407370 0 0 0 0 0 0 0 0\n"),
			expected: 18446744073700000000,
		},
	}

	collector := Collector{bufReader: bufio.NewReaderSize(nil, 128)}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual, err := collector.cpuNanoSeconds(tt.data)
			assert.NilError(t, err)
			assert.Equal(t, tt.expected, actual, "expected %v got %v", tt.expected, actual)
		})
	}
}
