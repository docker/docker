package opts // import "github.com/docker/docker/opts"

import (
	"fmt"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestValidateIPAddress(t *testing.T) {
	if ret, err := ValidateIPAddress(`1.2.3.4`); err != nil || ret == "" {
		t.Fatalf("ValidateIPAddress(`1.2.3.4`) got %s %s", ret, err)
	}

	if ret, err := ValidateIPAddress(`127.0.0.1`); err != nil || ret == "" {
		t.Fatalf("ValidateIPAddress(`127.0.0.1`) got %s %s", ret, err)
	}

	if ret, err := ValidateIPAddress(`::1`); err != nil || ret == "" {
		t.Fatalf("ValidateIPAddress(`::1`) got %s %s", ret, err)
	}

	if ret, err := ValidateIPAddress(`127`); err == nil || ret != "" {
		t.Fatalf("ValidateIPAddress(`127`) got %s %s", ret, err)
	}

	if ret, err := ValidateIPAddress(`random invalid string`); err == nil || ret != "" {
		t.Fatalf("ValidateIPAddress(`random invalid string`) got %s %s", ret, err)
	}

}

func TestMapOpts(t *testing.T) {
	tmpMap := make(map[string]string)
	o := NewMapOpts(tmpMap, logOptsValidator)
	o.Set("max-size=1")
	if o.String() != "map[max-size:1]" {
		t.Errorf("%s != [map[max-size:1]", o.String())
	}

	o.Set("max-file=2")
	if len(tmpMap) != 2 {
		t.Errorf("map length %d != 2", len(tmpMap))
	}

	if tmpMap["max-file"] != "2" {
		t.Errorf("max-file = %s != 2", tmpMap["max-file"])
	}

	if tmpMap["max-size"] != "1" {
		t.Errorf("max-size = %s != 1", tmpMap["max-size"])
	}
	if o.Set("dummy-val=3") == nil {
		t.Error("validator is not being called")
	}
}

func TestListOptsWithoutValidator(t *testing.T) {
	o := NewListOpts(nil)
	o.Set("foo")
	if o.String() != "[foo]" {
		t.Errorf("%s != [foo]", o.String())
	}
	o.Set("bar")
	if o.Len() != 2 {
		t.Errorf("%d != 2", o.Len())
	}
	o.Set("bar")
	if o.Len() != 3 {
		t.Errorf("%d != 3", o.Len())
	}
	if !o.Get("bar") {
		t.Error("o.Get(\"bar\") == false")
	}
	if o.Get("baz") {
		t.Error("o.Get(\"baz\") == true")
	}
	o.Delete("foo")
	if o.String() != "[bar bar]" {
		t.Errorf("%s != [bar bar]", o.String())
	}
	listOpts := o.GetAll()
	if len(listOpts) != 2 || listOpts[0] != "bar" || listOpts[1] != "bar" {
		t.Errorf("Expected [[bar bar]], got [%v]", listOpts)
	}
	mapListOpts := o.GetMap()
	if len(mapListOpts) != 1 {
		t.Errorf("Expected [map[bar:{}]], got [%v]", mapListOpts)
	}

}

func TestListOptsWithValidator(t *testing.T) {
	// Re-using logOptsvalidator (used by MapOpts)
	o := NewListOpts(logOptsValidator)
	o.Set("foo")
	if o.String() != "" {
		t.Errorf(`%s != ""`, o.String())
	}
	o.Set("foo=bar")
	if o.String() != "" {
		t.Errorf(`%s != ""`, o.String())
	}
	o.Set("max-file=2")
	if o.Len() != 1 {
		t.Errorf("%d != 1", o.Len())
	}
	if !o.Get("max-file=2") {
		t.Error("o.Get(\"max-file=2\") == false")
	}
	if o.Get("baz") {
		t.Error("o.Get(\"baz\") == true")
	}
	o.Delete("max-file=2")
	if o.String() != "" {
		t.Errorf(`%s != ""`, o.String())
	}
}

func TestValidateDNSSearch(t *testing.T) {
	valid := []string{
		`.`,
		`a`,
		`a.`,
		`1.foo`,
		`17.foo`,
		`foo.bar`,
		`foo.bar.baz`,
		`foo.bar.`,
		`foo.bar.baz`,
		`foo1.bar2`,
		`foo1.bar2.baz`,
		`1foo.2bar.`,
		`1foo.2bar.baz`,
		`foo-1.bar-2`,
		`foo-1.bar-2.baz`,
		`foo-1.bar-2.`,
		`foo-1.bar-2.baz`,
		`1-foo.2-bar`,
		`1-foo.2-bar.baz`,
		`1-foo.2-bar.`,
		`1-foo.2-bar.baz`,
	}

	invalid := []string{
		``,
		` `,
		`  `,
		`17`,
		`17.`,
		`.17`,
		`17-.`,
		`17-.foo`,
		`.foo`,
		`foo-.bar`,
		`-foo.bar`,
		`foo.bar-`,
		`foo.bar-.baz`,
		`foo.-bar`,
		`foo.-bar.baz`,
		`foo.bar.baz.this.should.fail.on.long.name.because.it.is.longer.thanitshouldbethis.should.fail.on.long.name.because.it.is.longer.thanitshouldbethis.should.fail.on.long.name.because.it.is.longer.thanitshouldbethis.should.fail.on.long.name.because.it.is.longer.thanitshouldbe`,
	}

	for _, domain := range valid {
		if ret, err := ValidateDNSSearch(domain); err != nil || ret == "" {
			t.Fatalf("ValidateDNSSearch(`"+domain+"`) got %s %s", ret, err)
		}
	}

	for _, domain := range invalid {
		if ret, err := ValidateDNSSearch(domain); err == nil || ret != "" {
			t.Fatalf("ValidateDNSSearch(`"+domain+"`) got %s %s", ret, err)
		}
	}
}

func TestValidateLabel(t *testing.T) {
	testCases := []struct {
		name           string
		label          string
		expectedResult string
		expectedErr    string
	}{
		{
			name:        "label with bad attribute format",
			label:       "label",
			expectedErr: "bad attribute format: label",
		},
		{
			name:           "label with general format",
			label:          "key1=value1",
			expectedResult: "key1=value1",
		},
		{
			name:           "label with more than one =",
			label:          "key1=value1=value2",
			expectedResult: "key1=value1=value2",
		},
		{
			name:           "label with one more",
			label:          "key1=value1=value2=value3",
			expectedResult: "key1=value1=value2=value3",
		},
		{
			name:           "label with no reserved com.docker.*",
			label:          "com.dockerpsychnotreserved.label=value",
			expectedResult: "com.dockerpsychnotreserved.label=value",
		},
		{
			name:           "label with no reserved io.docker.*",
			label:          "io.dockerproject.not=reserved",
			expectedResult: "io.dockerproject.not=reserved",
		},
		{
			name:           "label with no reserved org.dockerproject.*",
			label:          "org.docker.not=reserved",
			expectedResult: "org.docker.not=reserved",
		},
		{
			name:        "label with reserved com.docker.*",
			label:       "com.docker.feature=enabled",
			expectedErr: "label com.docker.feature=enabled is not allowed: the namespaces com.docker.*, io.docker.*, and org.dockerproject.* are reserved for internal use",
		},
		{
			name:        "label with reserved upcase com.docker.* ",
			label:       "COM.docker.feature=enabled",
			expectedErr: "label COM.docker.feature=enabled is not allowed: the namespaces com.docker.*, io.docker.*, and org.dockerproject.* are reserved for internal use",
		},
		{
			name:        "label with reserved io.docker.*",
			label:       "io.docker.configuration=0",
			expectedErr: "label io.docker.configuration=0 is not allowed: the namespaces com.docker.*, io.docker.*, and org.dockerproject.* are reserved for internal use",
		},
		{
			name:        "label with reserved upcase io.docker.*",
			label:       "io.DOCKER.CONFIGURATion=0",
			expectedErr: "label io.DOCKER.CONFIGURATion=0 is not allowed: the namespaces com.docker.*, io.docker.*, and org.dockerproject.* are reserved for internal use",
		},
		{
			name:        "label with reserved org.dockerproject.*",
			label:       "org.dockerproject.setting=on",
			expectedErr: "label org.dockerproject.setting=on is not allowed: the namespaces com.docker.*, io.docker.*, and org.dockerproject.* are reserved for internal use",
		},
		{
			name:        "label with reserved upcase org.dockerproject.*",
			label:       "Org.Dockerproject.Setting=on",
			expectedErr: "label Org.Dockerproject.Setting=on is not allowed: the namespaces com.docker.*, io.docker.*, and org.dockerproject.* are reserved for internal use",
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			result, err := ValidateLabel(testCase.label)

			if testCase.expectedErr != "" {
				assert.Error(t, err, testCase.expectedErr)
			} else {
				assert.NilError(t, err)
			}
			if testCase.expectedResult != "" {
				assert.Check(t, is.Equal(result, testCase.expectedResult))
			}
		})

	}
}

func TestIsReservedLabelNamespace(t *testing.T) {
	tests := []struct {
		label    string
		expected bool
	}{
		{
			label:    "my-label",
			expected: false,
		},
		{
			label:    "com.dockerpsychnotreserved.label",
			expected: false,
		},
		{
			label:    "io.dockerproject.not",
			expected: false,
		},
		{
			label:    "org.docker.not",
			expected: false,
		},
		{
			label:    "com.docker",
			expected: true,
		},
		{
			label:    "com.docker.",
			expected: true,
		},
		{
			label:    "com.docker.feature",
			expected: true,
		},
		{
			label:    "COM.docker.feature",
			expected: true,
		},
		{
			label:    "io.docker",
			expected: true,
		},
		{
			label:    "io.docker.",
			expected: true,
		},
		{
			label:    "io.docker.feature",
			expected: true,
		},
		{
			label:    "io.docker.feature",
			expected: true,
		},
		{
			label:    "org.dockerproject",
			expected: true,
		},
		{
			label:    "org.dockerproject.",
			expected: true,
		},
		{
			label:    "org.dockerproject.feature",
			expected: true,
		},
		{
			label:    "org.dockerproject.feature",
			expected: true,
		},
	}

	for _, tc := range tests {
		testCase := tc
		t.Run(testCase.label, func(t *testing.T) {
			result := IsReservedLabelNamespace(testCase.label)
			assert.Equal(t, result, testCase.expected)
		})
	}
}

func logOptsValidator(val string) (string, error) {
	allowedKeys := map[string]string{"max-size": "1", "max-file": "2"}
	vals := strings.Split(val, "=")
	if allowedKeys[vals[0]] != "" {
		return val, nil
	}
	return "", fmt.Errorf("invalid key %s", vals[0])
}

func TestNamedListOpts(t *testing.T) {
	var v []string
	o := NewNamedListOptsRef("foo-name", &v, nil)

	o.Set("foo")
	if o.String() != "[foo]" {
		t.Errorf("%s != [foo]", o.String())
	}
	if o.Name() != "foo-name" {
		t.Errorf("%s != foo-name", o.Name())
	}
	if len(v) != 1 {
		t.Errorf("expected foo to be in the values, got %v", v)
	}
}

func TestNamedMapOpts(t *testing.T) {
	tmpMap := make(map[string]string)
	o := NewNamedMapOpts("max-name", tmpMap, nil)

	o.Set("max-size=1")
	if o.String() != "map[max-size:1]" {
		t.Errorf("%s != [map[max-size:1]", o.String())
	}
	if o.Name() != "max-name" {
		t.Errorf("%s != max-name", o.Name())
	}
	if _, exist := tmpMap["max-size"]; !exist {
		t.Errorf("expected map-size to be in the values, got %v", tmpMap)
	}
}

func TestParseLink(t *testing.T) {
	name, alias, err := ParseLink("name:alias")
	if err != nil {
		t.Fatalf("Expected not to error out on a valid name:alias format but got: %v", err)
	}
	if name != "name" {
		t.Fatalf("Link name should have been name, got %s instead", name)
	}
	if alias != "alias" {
		t.Fatalf("Link alias should have been alias, got %s instead", alias)
	}
	// short format definition
	name, alias, err = ParseLink("name")
	if err != nil {
		t.Fatalf("Expected not to error out on a valid name only format but got: %v", err)
	}
	if name != "name" {
		t.Fatalf("Link name should have been name, got %s instead", name)
	}
	if alias != "name" {
		t.Fatalf("Link alias should have been name, got %s instead", alias)
	}
	// empty string link definition is not allowed
	if _, _, err := ParseLink(""); err == nil || !strings.Contains(err.Error(), "empty string specified for links") {
		t.Fatalf("Expected error 'empty string specified for links' but got: %v", err)
	}
	// more than two colons are not allowed
	if _, _, err := ParseLink("link:alias:wrong"); err == nil || !strings.Contains(err.Error(), "bad format for links: link:alias:wrong") {
		t.Fatalf("Expected error 'bad format for links: link:alias:wrong' but got: %v", err)
	}
}
