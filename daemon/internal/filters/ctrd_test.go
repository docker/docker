package filters_test

import (
	"testing"

	"github.com/moby/moby/v2/daemon/internal/filters"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

const testDanglingPrefix = "moby-dangling@"

func TestBuildCtrdImageFilters(t *testing.T) {
	newArgs := func(kv ...string) filters.Args {
		args := filters.NewArgs()
		for i := 0; i+1 < len(kv); i += 2 {
			args.Add(kv[i], kv[i+1])
		}
		return args
	}

	t.Run("no filters returns empty", func(t *testing.T) {
		got := filters.BuildCtrdImageFilters(filters.NewArgs(), testDanglingPrefix)
		assert.Check(t, is.Len(got, 0))
	})

	t.Run("single reference", func(t *testing.T) {
		got := filters.BuildCtrdImageFilters(newArgs("reference", "alpine:latest"), testDanglingPrefix)
		assert.Assert(t, is.Len(got, 1))
		assert.Check(t, is.Equal(got[0], `name=="docker.io/library/alpine:latest"`))
	})

	t.Run("multiple references produce one entry each", func(t *testing.T) {
		args := filters.NewArgs()
		args.Add("reference", "alpine:latest")
		args.Add("reference", "nginx:latest")
		got := filters.BuildCtrdImageFilters(args, testDanglingPrefix)
		assert.Assert(t, is.Len(got, 2))
	})

	t.Run("dangling true", func(t *testing.T) {
		got := filters.BuildCtrdImageFilters(newArgs("dangling", "true"), testDanglingPrefix)
		assert.Assert(t, is.Len(got, 1))
		assert.Check(t, is.Equal(got[0], `name~="^moby-dangling@"`))
	})

	t.Run("dangling false produces no containerd filter", func(t *testing.T) {
		got := filters.BuildCtrdImageFilters(newArgs("dangling", "false"), testDanglingPrefix)
		assert.Check(t, is.Len(got, 0))
	})

	t.Run("non-pushable filter types produce no containerd filters", func(t *testing.T) {
		got := filters.BuildCtrdImageFilters(newArgs("label", "foo=bar", "before", "alpine:latest"), testDanglingPrefix)
		assert.Check(t, is.Len(got, 0))
	})
}

func TestReferenceToCtrdFilter(t *testing.T) {
	for _, tc := range []struct {
		name        string
		pattern     string
		expected    string
		expectErr   error
	}{
		{
			name:     "exact name and tag",
			pattern:  "alpine:latest",
			expected: `name=="docker.io/library/alpine:latest"`,
		},
		{
			name:     "name only, no tag",
			pattern:  "alpine",
			expected: `name~="^docker\\.io/library/alpine[:@]"`,
		},
		{
			name:     "fully qualified name and tag",
			pattern:  "docker.io/library/nginx:1.25",
			expected: `name=="docker.io/library/nginx:1.25"`,
		},
		{
			name:     "custom registry with name and tag",
			pattern:  "registry.example.com/myapp:v2",
			expected: `name=="registry.example.com/myapp:v2"`,
		},
		{
			name:     "tag glob",
			pattern:  "alpine:*",
			expected: `name~="^docker\\.io/library/alpine:.*$"`,
		},
		{
			name:     "tag glob with prefix",
			pattern:  "nginx:1.*",
			expected: `name~="^docker\\.io/library/nginx:1\\..*$"`,
		},
		{
			name:      "glob in name part is not pushed down",
			pattern:   "*:latest",
			expectErr: filters.ErrNotConvertible,
		},
		{
			name:      "glob in name part with slash is not pushed down",
			pattern:   "*/redis",
			expectErr: filters.ErrNotConvertible,
		},
		{
			name:     "digest reference",
			pattern:  "alpine@sha256:1234abcd",
			expected: `name=="docker.io/library/alpine@sha256:1234abcd"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := filters.ReferenceToCtrdFilter(tc.pattern)
			if tc.expectErr != nil {
				assert.Check(t, is.ErrorIs(err, tc.expectErr))
				return
			}
			assert.NilError(t, err)
			assert.Check(t, is.Equal(got, tc.expected))
		})
	}
}
