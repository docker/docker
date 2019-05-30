package capabilities // import "github.com/docker/docker/pkg/capabilities"

import (
	"fmt"
	"testing"
)

func TestMatch(t *testing.T) {
	set := Set{
		"foo": struct{}{},
		"bar": struct{}{},
	}
	type testcase struct {
		caps     [][]string
		expected []string
	}
	var testcases = []testcase{
		// matches
		{
			caps:     [][]string{{}},
			expected: []string{},
		},
		{
			caps:     [][]string{{"foo"}},
			expected: []string{"foo"},
		},
		{
			caps:     [][]string{{"bar"}, {"foo"}},
			expected: []string{"bar"},
		},
		{
			caps:     [][]string{{"foo", "bar"}},
			expected: []string{"foo", "bar"},
		},
		{
			caps:     [][]string{{"qux"}, {"foo"}},
			expected: []string{"foo"},
		},
		{
			caps:     [][]string{{"foo", "bar"}, {"baz"}, {"bar"}},
			expected: []string{"foo", "bar"},
		},

		// non matches
		{caps: nil},
		{caps: [][]string{}},
		{caps: [][]string{{"qux"}}},
		{caps: [][]string{{"foo", "bar", "qux"}}},
		{caps: [][]string{{"qux"}, {"baz"}}},
		{caps: [][]string{{"foo", "baz"}}},
	}

	for _, m := range testcases {
		t.Run(fmt.Sprintf("%v", m.caps), func(t *testing.T) {
			selected := set.Match(m.caps)
			if m.expected == nil || selected == nil {
				if m.expected == nil && selected == nil {
					return
				}
				t.Fatalf("selected = %v, expected = %v", selected, m.expected)
			}
			if len(selected) != len(m.expected) {
				t.Fatalf("len(selected) = %d, len(expected) = %d", len(selected), len(m.expected))
			}
			for i, s := range selected {
				if m.expected[i] != s {
					t.Fatalf("selected[%d] = %s, expected[%d] = %s", i, s, i, m.expected[i])
				}
			}
		})
	}
}
