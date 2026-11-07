package filters

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp/cmpopts"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestMarshalJSON(t *testing.T) {
	a := NewArgs(
		Arg("created", "today"),
		Arg("image.name", "ubuntu*"),
		Arg("image.name", "*untu"),
	)

	s, err := a.MarshalJSON()
	assert.Check(t, err)
	const expected = `{"created":{"today":true},"image.name":{"*untu":true,"ubuntu*":true}}`
	assert.Check(t, is.Equal(string(s), expected))
}

func TestMarshalJSONWithEmpty(t *testing.T) {
	s, err := json.Marshal(NewArgs())
	assert.Check(t, err)
	const expected = `{}`
	assert.Check(t, is.Equal(string(s), expected))
}

func TestUnmarshalJSONZeroValue(t *testing.T) {
	var args Args

	assert.NilError(t, json.Unmarshal([]byte(`{"status":{"running":true}}`), &args))
	expected := []string{"running"}
	assert.DeepEqual(t, args.Get("status"), expected)
}

func TestToJSON(t *testing.T) {
	a := NewArgs(
		Arg("created", "today"),
		Arg("image.name", "ubuntu*"),
		Arg("image.name", "*untu"),
	)

	s, err := ToJSON(a)
	assert.Check(t, err)
	const expected = `{"created":{"today":true},"image.name":{"*untu":true,"ubuntu*":true}}`
	assert.Check(t, is.Equal(s, expected))
}

func TestFromJSON(t *testing.T) {
	invalids := []string{
		"anything",
		"['a','list']",
		"{'key': 'value'}",
		`{"key": "value"}`,
	}
	valid := map[*Args][]string{
		{fields: map[string]map[string]bool{"key": {"value": true}}}: {
			`{"key": ["value"]}`,
			`{"key": {"value": true}}`,
		},
		{fields: map[string]map[string]bool{"key": {"value1": true, "value2": true}}}: {
			`{"key": ["value1", "value2"]}`,
			`{"key": {"value1": true, "value2": true}}`,
		},
		{fields: map[string]map[string]bool{"key1": {"value1": true}, "key2": {"value2": true}}}: {
			`{"key1": ["value1"], "key2": ["value2"]}`,
			`{"key1": {"value1": true}, "key2": {"value2": true}}`,
		},
	}

	for _, invalid := range invalids {
		t.Run(invalid, func(t *testing.T) {
			_, err := FromJSON(invalid)
			if err == nil {
				t.Fatalf("Expected an error with %v, got nothing", invalid)
			}
			var invalidFilterError *invalidFilter
			assert.Check(t, is.ErrorType(err, invalidFilterError))
			wrappedErr := fmt.Errorf("something went wrong: %w", err)
			assert.Check(t, is.ErrorIs(wrappedErr, err))
		})
	}

	for expectedArgs, matchers := range valid {
		for _, jsonString := range matchers {
			args, err := FromJSON(jsonString)
			assert.Check(t, err)
			assert.Check(t, is.Equal(args.Len(), expectedArgs.Len()))
			for key, expectedValues := range expectedArgs.fields {
				values := args.Get(key)
				assert.Check(t, is.Len(values, len(expectedValues)), expectedArgs)

				for _, v := range values {
					if !expectedValues[v] {
						t.Errorf("Expected %v, go %v", expectedArgs, args)
					}
				}
			}
		}
	}
}

func TestEmpty(t *testing.T) {
	a := Args{}
	assert.Check(t, is.Equal(a.Len(), 0))
	v, err := ToJSON(a)
	assert.Check(t, err)
	v1, err := FromJSON(v)
	assert.Check(t, err)
	assert.Check(t, is.Equal(v1.Len(), 0))
}

func TestArgsMatchKVListEmptySources(t *testing.T) {
	args := NewArgs()
	if !args.MatchKVList("created", map[string]string{}) {
		t.Fatalf("Expected true for (%v,created), got true", args)
	}

	args = Args{map[string]map[string]bool{"created": {"today": true}}}
	if args.MatchKVList("created", map[string]string{}) {
		t.Fatalf("Expected false for (%v,created), got true", args)
	}
}

func TestArgsMatchKVList(t *testing.T) {
	// Not empty sources
	sources := map[string]string{
		"key1": "value1",
		"key2": "value2",
		"key3": "value3",
	}

	matches := map[*Args]string{
		{}: "field",
		{
			map[string]map[string]bool{
				"created": {"today": true},
				"labels":  {"key1": true},
			},
		}: "labels",
		{
			map[string]map[string]bool{
				"created": {"today": true},
				"labels":  {"key1=value1": true},
			},
		}: "labels",
	}

	for args, field := range matches {
		if !args.MatchKVList(field, sources) {
			t.Errorf("Expected true for %v on %v, got false", sources, args)
		}
	}

	differs := map[*Args]string{
		{
			map[string]map[string]bool{
				"created": {"today": true},
			},
		}: "created",
		{
			map[string]map[string]bool{
				"created": {"today": true},
				"labels":  {"key4": true},
			},
		}: "labels",
		{
			map[string]map[string]bool{
				"created": {"today": true},
				"labels":  {"key1=value3": true},
			},
		}: "labels",
	}

	for args, field := range differs {
		if args.MatchKVList(field, sources) {
			t.Errorf("Expected false for %v on %v, got true", sources, args)
		}
	}
}

func TestArgsMatch(t *testing.T) {
	source := "today"

	matches := map[*Args]string{
		{}: "field",
		{
			map[string]map[string]bool{
				"created": {"today": true},
			},
		}: "today",
		{
			map[string]map[string]bool{
				"created": {"to*": true},
			},
		}: "created",
		{
			map[string]map[string]bool{
				"created": {"to(.*)": true},
			},
		}: "created",
		{
			map[string]map[string]bool{
				"created": {"tod": true},
			},
		}: "created",
		{
			map[string]map[string]bool{
				"created": {"anything": true, "to*": true},
			},
		}: "created",
	}

	for args, field := range matches {
		assert.Check(t, args.Match(field, source), "Expected field %s to match %s", field, source)
	}

	differs := map[*Args]string{
		{
			map[string]map[string]bool{
				"created": {"tomorrow": true},
			},
		}: "created",
		{
			map[string]map[string]bool{
				"created": {"to(day": true},
			},
		}: "created",
		{
			map[string]map[string]bool{
				"created": {"tom(.*)": true},
			},
		}: "created",
		{
			map[string]map[string]bool{
				"created": {"tom": true},
			},
		}: "created",
		{
			map[string]map[string]bool{
				"created": {"today1": true},
				"labels":  {"today": true},
			},
		}: "created",
	}

	for args, field := range differs {
		assert.Check(t, !args.Match(field, source), "Expected field %s to not match %s", field, source)
	}
}

func TestAddZeroValue(t *testing.T) {
	var args Args
	args.Add("status", "running")
	assert.DeepEqual(t, args.Get("status"), []string{"running"})
}

func TestAdd(t *testing.T) {
	f := NewArgs()
	f.Add("status", "running")
	v := f.Get("status")
	assert.Check(t, is.DeepEqual(v, []string{"running"}))

	f.Add("status", "paused")
	v = f.Get("status")
	assert.Check(t, is.Len(v, 2))
	assert.Check(t, is.Contains(v, "running"))
	assert.Check(t, is.Contains(v, "paused"))
}

func TestDel(t *testing.T) {
	f := NewArgs()
	f.Add("status", "running")
	f.Del("status", "running")
	assert.Check(t, is.Equal(f.Len(), 0))
	assert.Check(t, is.DeepEqual(f.Get("status"), []string{}))
}

func TestLen(t *testing.T) {
	f := NewArgs()
	assert.Check(t, is.Equal(f.Len(), 0))
	f.Add("status", "running")
	assert.Check(t, is.Equal(f.Len(), 1))
}

func TestExactMatch(t *testing.T) {
	f := NewArgs()

	assert.Check(t, f.ExactMatch("status", "running"), "Expected to match `running` when there are no filters")

	f.Add("status", "running")
	f.Add("status", "pause*")

	assert.Check(t, f.ExactMatch("status", "running"), "Expected to match `running` with one of the filters")
	assert.Check(t, !f.ExactMatch("status", "paused"), "Expected to not match `paused` with one of the filters")
}

func TestOnlyOneExactMatch(t *testing.T) {
	f := NewArgs()

	assert.Check(t, f.ExactMatch("status", "running"), "Expected to match `running` when there are no filters")

	f.Add("status", "running")
	assert.Check(t, f.ExactMatch("status", "running"), "Expected to match `running` with one of the filters")
	assert.Check(t, !f.UniqueExactMatch("status", "paused"), "Expected to not match `paused` with one of the filters")

	f.Add("status", "pause")
	assert.Check(t, !f.UniqueExactMatch("status", "running"), "Expected to not match only `running` with two filters")
}

func TestContains(t *testing.T) {
	f := NewArgs()
	assert.Check(t, !f.Contains("status"))

	f.Add("status", "running")
	assert.Check(t, f.Contains("status"))
}

func TestValidate(t *testing.T) {
	f := NewArgs()
	f.Add("status", "running")

	valid := map[string]bool{
		"status":   true,
		"dangling": true,
	}

	assert.Check(t, f.Validate(valid))

	f.Add("bogus", "running")
	err := f.Validate(valid)
	var invalidFilterError *invalidFilter
	assert.Check(t, is.ErrorType(err, invalidFilterError))
	wrappedErr := fmt.Errorf("something went wrong: %w", err)
	assert.Check(t, is.ErrorIs(wrappedErr, err))
}

func TestWalkValues(t *testing.T) {
	f := NewArgs()
	f.Add("status", "running")
	f.Add("status", "paused")

	err := f.WalkValues("status", func(value string) error {
		if value != "running" && value != "paused" {
			t.Fatalf("Unexpected value %s", value)
		}
		return nil
	})
	assert.Check(t, err)

	loops1 := 0
	err = f.WalkValues("status", func(value string) error {
		loops1++
		return nil
	})
	assert.Check(t, err)
	assert.Check(t, is.Equal(loops1, 2), "Expected to not iterate when the field doesn't exist")

	loops2 := 0
	err = f.WalkValues("unknown-key", func(value string) error {
		loops2++
		return nil
	})
	assert.Check(t, err)
	assert.Check(t, is.Equal(loops2, 0), "Expected to not iterate when the field doesn't exist")
}

func TestFuzzyMatch(t *testing.T) {
	f := NewArgs()
	f.Add("container", "foo")

	cases := map[string]bool{
		"foo":    true,
		"foobar": true,
		"barfoo": false,
		"bar":    false,
	}
	for source, match := range cases {
		got := f.FuzzyMatch("container", source)
		if got != match {
			t.Errorf("Expected %v, got %v: %s", match, got, source)
		}
	}
}

func TestClone(t *testing.T) {
	f := NewArgs()
	f.Add("foo", "bar")
	f2 := f.Clone()
	f2.Add("baz", "qux")
	assert.Check(t, is.Len(f.Get("baz"), 0))
}

func TestGetBoolOrDefault(t *testing.T) {
	for _, tc := range []struct {
		name          string
		args          map[string][]string
		defValue      bool
		expectedErr   error
		expectedValue bool
	}{
		{
			name: "single true",
			args: map[string][]string{
				"dangling": {"true"},
			},
			defValue:      false,
			expectedErr:   nil,
			expectedValue: true,
		},
		{
			name: "single false",
			args: map[string][]string{
				"dangling": {"false"},
			},
			defValue:      true,
			expectedErr:   nil,
			expectedValue: false,
		},
		{
			name: "single bad value",
			args: map[string][]string{
				"dangling": {"potato"},
			},
			defValue:      true,
			expectedErr:   &invalidFilter{Filter: "dangling", Value: []string{"potato"}},
			expectedValue: true,
		},
		{
			name: "two bad values",
			args: map[string][]string{
				"dangling": {"banana", "potato"},
			},
			defValue:      true,
			expectedErr:   &invalidFilter{Filter: "dangling", Value: []string{"banana", "potato"}},
			expectedValue: true,
		},
		{
			name: "two conflicting values",
			args: map[string][]string{
				"dangling": {"false", "true"},
			},
			defValue:      false,
			expectedErr:   &invalidFilter{Filter: "dangling", Value: []string{"false", "true"}},
			expectedValue: false,
		},
		{
			name: "multiple conflicting values",
			args: map[string][]string{
				"dangling": {"false", "true", "1"},
			},
			defValue:      true,
			expectedErr:   &invalidFilter{Filter: "dangling", Value: []string{"false", "true", "1"}},
			expectedValue: true,
		},
		{
			name: "1 means true",
			args: map[string][]string{
				"dangling": {"1"},
			},
			defValue:      false,
			expectedErr:   nil,
			expectedValue: true,
		},
		{
			name: "0 means false",
			args: map[string][]string{
				"dangling": {"0"},
			},
			defValue:      true,
			expectedErr:   nil,
			expectedValue: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := NewArgs()

			for key, values := range tc.args {
				for _, value := range values {
					a.Add(key, value)
				}
			}

			value, err := a.GetBoolOrDefault("dangling", tc.defValue)

			if tc.expectedErr == nil {
				assert.Check(t, is.Nil(err))
			} else {
				assert.Check(t, is.ErrorType(err, tc.expectedErr))

				// Check if error is the same.
				expected, ok := errors.AsType[*invalidFilter](tc.expectedErr)
				assert.Check(t, ok)

				actual, ok := errors.AsType[*invalidFilter](err)
				assert.Check(t, ok)

				assert.Check(t, is.Equal(expected.Filter, actual.Filter))
				assert.Check(t, is.DeepEqual(expected.Value, actual.Value, cmpopts.SortSlices(func(a, b string) bool {
					return a < b
				})))

				wrappedErr := fmt.Errorf("something went wrong: %w", err)
				assert.Check(t, is.ErrorIs(wrappedErr, err), "Expected a wrapped error to be detected as invalidFilter")
			}

			assert.Check(t, is.Equal(tc.expectedValue, value))
		})
	}
}

func TestExcludeMatch(t *testing.T) {
	f := NewArgs()
	assert.Check(t, !f.ExcludeMatch("status", "running"), "nothing is excluded when there are no filters")

	f.Add("status", "running")
	assert.Check(t, !f.ExcludeMatch("status", "running"), "positive filters do not exclude")

	f.Add("status!", "exited")
	f.Add("status!", "^pa")
	f.Add("status!", "[")

	assert.Check(t, f.ExcludeMatch("status", "exited"), "exact match is excluded")
	assert.Check(t, f.ExcludeMatch("status", "paused"), "regular expression match is excluded")
	assert.Check(t, !f.ExcludeMatch("status", "running"), "non-matching value is not excluded")
	assert.Check(t, !f.ExcludeMatch("status", "created"), "invalid regular expression does not match")
	assert.Check(t, f.ExcludeMatch("status", "["), "invalid regular expression still matches literally")
	assert.Check(t, !f.ExcludeMatch("other", "exited"), "other fields are not affected")
}

func TestExcludeExactMatch(t *testing.T) {
	f := NewArgs()
	assert.Check(t, !f.ExcludeExactMatch("health", "healthy"), "nothing is excluded when there are no filters")

	f.Add("health", "healthy")
	assert.Check(t, !f.ExcludeExactMatch("health", "healthy"), "positive filters do not exclude")

	f.Add("health!", "healthy")
	f.Add("health!", "start.*")

	assert.Check(t, f.ExcludeExactMatch("health", "healthy"), "exact match is excluded")
	assert.Check(t, !f.ExcludeExactMatch("health", "starting"), "regular expressions are not evaluated")
	assert.Check(t, !f.ExcludeExactMatch("health", "unhealthy"), "non-matching value is not excluded")
}
