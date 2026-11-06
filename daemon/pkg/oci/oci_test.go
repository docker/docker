package oci

import (
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"gotest.tools/v3/assert"
)

func TestAppendDevicePermissionsFromCgroupRules(t *testing.T) {
	ptr := func(i int64) *int64 { return &i }

	tests := []struct {
		doc         string
		rule        string
		expected    specs.LinuxDeviceCgroup
		expectedErr string
	}{
		{
			doc:         "empty rule",
			rule:        "",
			expectedErr: `invalid device cgroup rule format: ''`,
		},
		{
			doc:         "multiple spaces after first column",
			rule:        "c 1:1  rwm",
			expectedErr: `invalid device cgroup rule format: 'c 1:1  rwm'`,
		},
		{
			doc:         "multiple spaces after second column",
			rule:        "c  1:1 rwm",
			expectedErr: `invalid device cgroup rule format: 'c  1:1 rwm'`,
		},
		{
			doc:         "leading spaces",
			rule:        " c 1:1 rwm",
			expectedErr: `invalid device cgroup rule format: ' c 1:1 rwm'`,
		},
		{
			doc:         "trailing spaces",
			rule:        "c 1:1 rwm ",
			expectedErr: `invalid device cgroup rule format: 'c 1:1 rwm '`,
		},
		{
			doc:         "unknown device type",
			rule:        "z 1:1 rwm",
			expectedErr: `invalid device cgroup rule format: 'z 1:1 rwm'`,
		},
		{
			doc:         "invalid device type",
			rule:        "zz  1:1 rwm",
			expectedErr: `invalid device cgroup rule format: 'zz  1:1 rwm'`,
		},
		{
			doc:         "missing colon",
			rule:        "c 11 rwm",
			expectedErr: `invalid device cgroup rule format: 'c 11 rwm'`,
		},
		{
			doc:         "invalid device major-minor",
			rule:        "c a:a rwm",
			expectedErr: `invalid device cgroup rule format: 'c a:a rwm'`,
		},
		{
			doc:         "negative major device",
			rule:        "c -1:1 rwm",
			expectedErr: `invalid device cgroup rule format: 'c -1:1 rwm'`,
		},
		{
			doc:         "negative minor device",
			rule:        "c 1:-1 rwm",
			expectedErr: `invalid device cgroup rule format: 'c 1:-1 rwm'`,
		},
		{
			doc:         "missing permissions",
			rule:        "c 1:1",
			expectedErr: `invalid device cgroup rule format: 'c 1:1'`,
		},
		{
			doc:         "invalid permissions",
			rule:        "c 1:1 x",
			expectedErr: `invalid device cgroup rule format: 'c 1:1 x'`,
		},
		{
			doc:         "too many permissions",
			rule:        "c 1:1 rwmrwm",
			expectedErr: `invalid device cgroup rule format: 'c 1:1 rwmrwm'`,
		},
		{
			doc:         "major out of range",
			rule:        "c 18446744073709551616:1 rwm",
			expectedErr: `invalid major value in device cgroup rule format: 'c 18446744073709551616:1 rwm'`,
		},
		{
			doc:         "minor out of range",
			rule:        "c 1:18446744073709551616 rwm",
			expectedErr: `invalid minor value in device cgroup rule format: 'c 1:18446744073709551616 rwm'`,
		},
		{
			doc:         "char (c) devices without major-minor and permissions",
			rule:        "c",
			expectedErr: `invalid device cgroup rule format: 'c'`,
		},
		{
			doc:         "block (b) devices without major-minor and permissions",
			rule:        "b",
			expectedErr: `invalid device cgroup rule format: 'b'`,
		},
		{
			doc:         "all (a) devices with trailing space",
			rule:        "a ",
			expectedErr: `invalid device cgroup rule format: 'a '`,
		},
		{
			doc:      "all (a) devices",
			rule:     "a 1:1 rwm",
			expected: specs.LinuxDeviceCgroup{Allow: true, Type: "a", Major: ptr(1), Minor: ptr(1), Access: "rwm"},
		},
		{
			// "a" (all) is a wildcard, and major-minor and permissions may
			// be omitted; it is equivalent to "a *:* rwm".
			doc:      "all (a) devices without major-minor and permissions",
			rule:     "a",
			expected: specs.LinuxDeviceCgroup{Allow: true, Type: "a", Major: ptr(-1), Minor: ptr(-1), Access: "rwm"},
		},
		{
			doc:      "char (c) devices",
			rule:     "c 1:1 rwm",
			expected: specs.LinuxDeviceCgroup{Allow: true, Type: "c", Major: ptr(1), Minor: ptr(1), Access: "rwm"},
		},
		{
			doc:      "block (b) devices",
			rule:     "b 1:1 rwm",
			expected: specs.LinuxDeviceCgroup{Allow: true, Type: "b", Major: ptr(1), Minor: ptr(1), Access: "rwm"},
		},
		{
			doc:      "char device with rwm permissions",
			rule:     "c 7:128 rwm",
			expected: specs.LinuxDeviceCgroup{Allow: true, Type: "c", Major: ptr(7), Minor: ptr(128), Access: "rwm"},
		},
		{
			doc:      "wildcard major",
			rule:     "c *:1 rwm",
			expected: specs.LinuxDeviceCgroup{Allow: true, Type: "c", Major: ptr(-1), Minor: ptr(1), Access: "rwm"},
		},
		{
			doc:      "wildcard minor",
			rule:     "c 1:* rwm",
			expected: specs.LinuxDeviceCgroup{Allow: true, Type: "c", Major: ptr(1), Minor: ptr(-1), Access: "rwm"},
		},
		{
			doc:      "wildcard major and minor",
			rule:     "c *:* rwm",
			expected: specs.LinuxDeviceCgroup{Allow: true, Type: "c", Major: ptr(-1), Minor: ptr(-1), Access: "rwm"},
		},
		{
			doc:      "read (r) permission",
			rule:     "c 1:1 r",
			expected: specs.LinuxDeviceCgroup{Allow: true, Type: "c", Major: ptr(1), Minor: ptr(1), Access: "r"},
		},
		{
			doc:      "write (w) permission",
			rule:     "c 1:1 w",
			expected: specs.LinuxDeviceCgroup{Allow: true, Type: "c", Major: ptr(1), Minor: ptr(1), Access: "w"},
		},
		{
			doc:      "mknod (m) permission",
			rule:     "c 1:1 m",
			expected: specs.LinuxDeviceCgroup{Allow: true, Type: "c", Major: ptr(1), Minor: ptr(1), Access: "m"},
		},
		{
			doc:      "mknod (m) and read (r) permission",
			rule:     "c 1:1 mr",
			expected: specs.LinuxDeviceCgroup{Allow: true, Type: "c", Major: ptr(1), Minor: ptr(1), Access: "mr"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.doc, func(t *testing.T) {
			out, err := AppendDevicePermissionsFromCgroupRules([]specs.LinuxDeviceCgroup{}, []string{tc.rule})
			if tc.expectedErr != "" {
				assert.Error(t, err, tc.expectedErr)
				return
			}
			assert.NilError(t, err)
			assert.DeepEqual(t, out, []specs.LinuxDeviceCgroup{tc.expected})
		})
	}
}
