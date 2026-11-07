/*
Package filters provides tools for encoding a mapping of keys to a set of
multiple values.
*/
package filters

import (
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Args stores a mapping of keys to a set of multiple values.
type Args struct {
	fields map[string]map[string]bool
}

// KeyValuePair are used to initialize a new Args
type KeyValuePair struct {
	Key   string
	Value string
}

// Arg creates a new KeyValuePair for initializing Args
func Arg(key, value string) KeyValuePair {
	return KeyValuePair{Key: key, Value: value}
}

// NewArgs returns a new Args populated with the initial args
func NewArgs(initialArgs ...KeyValuePair) Args {
	args := Args{fields: map[string]map[string]bool{}}
	for _, arg := range initialArgs {
		args.Add(arg.Key, arg.Value)
	}
	return args
}

// Keys returns all the keys in Args.
func (args Args) Keys() []string {
	return slices.Collect(maps.Keys(args.fields))
}

// MarshalJSON returns a JSON byte representation of the Args
func (args Args) MarshalJSON() ([]byte, error) {
	if len(args.fields) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(args.fields)
}

// ToJSON returns the Args as a JSON encoded string
func ToJSON(a Args) (string, error) {
	if a.Len() == 0 {
		return "", nil
	}
	buf, err := json.Marshal(a)
	return string(buf), err
}

// FromJSON decodes a JSON encoded string into Args
func FromJSON(p string) (Args, error) {
	args := NewArgs()

	if p == "" {
		return args, nil
	}

	raw := []byte(p)
	err := json.Unmarshal(raw, &args)
	if err == nil {
		return args, nil
	}

	// Fallback to parsing arguments in the legacy slice format
	deprecated := map[string][]string{}
	if legacyErr := json.Unmarshal(raw, &deprecated); legacyErr != nil {
		return args, &invalidFilter{}
	}

	args.fields = deprecatedArgs(deprecated)
	return args, nil
}

// UnmarshalJSON populates the Args from JSON encode bytes
func (args *Args) UnmarshalJSON(raw []byte) error {
	return json.Unmarshal(raw, &args.fields)
}

// Get returns the list of values associated with the key
func (args Args) Get(key string) []string {
	values := args.fields[key]
	if values == nil {
		return []string{}
	}
	return slices.Collect(maps.Keys(values))
}

// Add a new value to the set of values
func (args *Args) Add(key, value string) {
	if args.fields == nil {
		args.fields = make(map[string]map[string]bool)
	}
	values := args.fields[key]
	if values == nil {
		values = make(map[string]bool)
		args.fields[key] = values
	}
	values[value] = true
}

// Del removes a value from the set
func (args Args) Del(key, value string) {
	delete(args.fields[key], value)
	if len(args.fields[key]) == 0 {
		delete(args.fields, key)
	}
}

// Len returns the number of keys in the mapping
func (args Args) Len() int {
	return len(args.fields)
}

// MatchKVList returns true if all the pairs in sources exist as key=value
// pairs in the mapping at key, or if there are no values at key.
func (args Args) MatchKVList(key string, sources map[string]string) bool {
	fieldValues := args.fields[key]

	// do not filter if there is no filter set or cannot determine filter
	if len(fieldValues) == 0 {
		return true
	}

	if len(sources) == 0 {
		return false
	}

	for value := range fieldValues {
		testK, testV, hasValue := strings.Cut(value, "=")

		v, ok := sources[testK]
		if !ok {
			return false
		}
		if hasValue && testV != v {
			return false
		}
	}

	return true
}

// Match returns true if any of the values at key match the source string
func (args Args) Match(field, source string) bool {
	if args.ExactMatch(field, source) {
		return true
	}
	return matchAny(args.fields[field], source)
}

// matchAny returns true if source equals one of the values, or matches one
// of them as a regular expression. Values that are not a valid regular
// expression can only match literally.
func matchAny(values map[string]bool, source string) bool {
	if values[source] {
		return true
	}
	for name2match := range values {
		match, err := regexp.MatchString(name2match, source)
		if err != nil {
			continue
		}
		if match {
			return true
		}
	}
	return false
}

// ExcludeMatch returns true if the source matches any of the values of the
// exclusion filter for field ("<field>!"), using the same matching rules as
// [Args.Match]. Unlike [Args.Match], it returns false if the filter is not set.
func (args Args) ExcludeMatch(field, source string) bool {
	return matchAny(args.fields[field+"!"], source)
}

// ExcludeExactMatch returns true if the source is exactly one of the values
// of the exclusion filter for field ("<field>!"). Unlike [Args.ExactMatch],
// it returns false if the filter is not set.
func (args Args) ExcludeExactMatch(field, source string) bool {
	return args.fields[field+"!"][source]
}

// GetBoolOrDefault returns a boolean value of the key if the key is present
// and is interpretable as a boolean value. Otherwise the default value is returned.
// Error is not nil only if the filter values are not valid boolean or are conflicting.
func (args Args) GetBoolOrDefault(key string, defaultValue bool) (bool, error) {
	fieldValues, ok := args.fields[key]
	if !ok {
		return defaultValue, nil
	}

	if len(fieldValues) == 0 {
		return defaultValue, &invalidFilter{key, nil}
	}

	isFalse := fieldValues["0"] || fieldValues["false"]
	isTrue := fieldValues["1"] || fieldValues["true"]
	if isFalse == isTrue {
		// Either no or conflicting truthy/falsy value were provided
		return defaultValue, &invalidFilter{key, args.Get(key)}
	}
	return isTrue, nil
}

// ExactMatch returns true if the source matches exactly one of the values.
func (args Args) ExactMatch(key, source string) bool {
	fieldValues := args.fields[key]
	// do not filter if there is no filter set or cannot determine filter
	if len(fieldValues) == 0 {
		return true
	}

	// try to match full name value to avoid O(N) regular expression matching
	return fieldValues[source]
}

// UniqueExactMatch returns true if there is only one value and the source
// matches exactly the value.
func (args Args) UniqueExactMatch(key, source string) bool {
	fieldValues := args.fields[key]
	// do not filter if there is no filter set or cannot determine filter
	if len(fieldValues) == 0 {
		return true
	}
	if len(fieldValues) != 1 {
		return false
	}

	// try to match full name value to avoid O(N) regular expression matching
	return fieldValues[source]
}

// FuzzyMatch returns true if the source matches exactly one value,  or the
// source has one of the values as a prefix.
func (args Args) FuzzyMatch(key, source string) bool {
	if args.ExactMatch(key, source) {
		return true
	}

	fieldValues := args.fields[key]
	for prefix := range fieldValues {
		if strings.HasPrefix(source, prefix) {
			return true
		}
	}
	return false
}

// Contains returns true if the key exists in the mapping
func (args Args) Contains(field string) bool {
	_, ok := args.fields[field]
	return ok
}

// Validate compared the set of accepted keys against the keys in the mapping.
// An error is returned if any mapping keys are not in the accepted set.
func (args Args) Validate(accepted map[string]bool) error {
	for name := range args.fields {
		if !accepted[name] {
			return &invalidFilter{name, nil}
		}
	}
	return nil
}

// WalkValues iterates over the list of values for a key in the mapping and calls
// op() for each value. If op returns an error the iteration stops and the
// error is returned.
func (args Args) WalkValues(field string, op func(value string) error) error {
	for v := range args.fields[field] {
		if err := op(v); err != nil {
			return err
		}
	}
	return nil
}

// Clone returns a copy of args.
func (args Args) Clone() Args {
	fields := maps.Clone(args.fields)
	for k, values := range fields {
		fields[k] = maps.Clone(values)
	}
	return Args{fields: fields}
}

func deprecatedArgs(d map[string][]string) map[string]map[string]bool {
	m := make(map[string]map[string]bool, len(d))
	for k, v := range d {
		values := make(map[string]bool, len(v))
		for _, vv := range v {
			values[vv] = true
		}
		m[k] = values
	}
	return m
}
