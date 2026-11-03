package opts

import (
	"fmt"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestValidateIPAddress(t *testing.T) {
	tests := []struct {
		doc         string
		input       string
		expectedOut string
		expectedErr string
	}{
		{
			doc:         "IPv4 loopback",
			input:       `127.0.0.1`,
			expectedOut: `127.0.0.1`,
		},
		{
			doc:         "IPv4 loopback with whitespace",
			input:       ` 127.0.0.1 `,
			expectedErr: `IP address is not correctly formatted: ParseAddr(" 127.0.0.1 "): unexpected character (at " 127.0.0.1 ")`,
		},
		{
			doc:         "IPv6 loopback long form",
			input:       `0:0:0:0:0:0:0:1`,
			expectedOut: `::1`,
		},
		{
			doc:         "IPv6 loopback",
			input:       `::1`,
			expectedOut: `::1`,
		},
		{
			doc:         "IPv6 loopback with whitespace",
			input:       ` ::1 `,
			expectedErr: `IP address is not correctly formatted: ParseAddr(" ::1 "): each colon-separated field must have at least one digit (at " ::1 ")`,
		},
		{
			doc:         "IPv6 lowercase",
			input:       `2001:db8::68`,
			expectedOut: `2001:db8::68`,
		},
		{
			doc:         "IPv6 uppercase",
			input:       `2001:DB8::68`,
			expectedOut: `2001:db8::68`,
		},
		{
			doc:         "IPv6 with brackets",
			input:       `[::1]`,
			expectedErr: `IP address is not correctly formatted: ParseAddr("[::1]"): each colon-separated field must have at least one digit (at "[::1]")`,
		},
		{
			doc:         "IPv4 partial",
			input:       `127`,
			expectedErr: `IP address is not correctly formatted: ParseAddr("127"): unable to parse IP`,
		},
		{
			doc:         "random invalid string",
			input:       `random invalid string`,
			expectedErr: `IP address is not correctly formatted: ParseAddr("random invalid string"): unable to parse IP`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			actualOut, actualErr := ValidateIPAddress(tc.input)
			assert.Check(t, is.Equal(tc.expectedOut, actualOut))
			if tc.expectedErr == "" {
				assert.Check(t, actualErr)
			} else {
				assert.Check(t, is.Error(actualErr, tc.expectedErr))
			}
		})
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
		t.Error(`o.Get("bar") == false`)
	}
	if o.Get("baz") {
		t.Error(`o.Get("baz") == true`)
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
	// Re-using logOptsValidator (used by MapOpts)
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
		t.Error(`o.Get("max-file=2") == false`)
	}
	if o.Get("baz") {
		t.Error(`o.Get("baz") == true`)
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
	tests := []struct {
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

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ValidateLabel(tc.label)

			if tc.expectedErr != "" {
				assert.Error(t, err, tc.expectedErr)
			} else {
				assert.NilError(t, err)
			}
			if tc.expectedResult != "" {
				assert.Check(t, is.Equal(result, tc.expectedResult))
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
	t.Run("name and alias", func(t *testing.T) {
		name, alias, err := ParseLink("name:alias")
		assert.Check(t, err)
		assert.Check(t, is.Equal(name, "name"))
		assert.Check(t, is.Equal(alias, "alias"))
	})
	t.Run("short format", func(t *testing.T) {
		name, alias, err := ParseLink("name")
		assert.Check(t, err)
		assert.Check(t, is.Equal(name, "name"))
		assert.Check(t, is.Equal(alias, "name"))
	})
	t.Run("empty string", func(t *testing.T) {
		_, _, err := ParseLink("")
		assert.Check(t, is.Error(err, "empty string specified for links"))
	})
	t.Run("more than two colons", func(t *testing.T) {
		_, _, err := ParseLink("link:alias:wrong")
		assert.Check(t, is.Error(err, "bad format for links: link:alias:wrong"))
	})
	t.Run("legacy format", func(t *testing.T) {
		name, alias, err := ParseLink("/foo:/c1/bar")
		assert.Check(t, err)
		assert.Check(t, is.Equal(name, "foo"))
		assert.Check(t, is.Equal(alias, "bar"))
	})
}

func TestMapMapOpts(t *testing.T) {
	tmpMap := make(map[string]map[string]string)
	validator := func(val string) (string, error) {
		if strings.HasPrefix(val, "invalid-key=") {
			return "", fmt.Errorf("invalid key %s", val)
		}
		return val, nil
	}
	o := NewMapMapOpts(tmpMap, validator)
	o.Set("r1=k11=v11")
	assert.Check(t, is.DeepEqual(tmpMap, map[string]map[string]string{"r1": {"k11": "v11"}}))

	o.Set("r2=k21=v21")
	assert.Check(t, is.Len(tmpMap, 2))

	if err := o.Set("invalid-syntax"); err == nil {
		t.Error("invalid mapping syntax is not being caught")
	}

	if err := o.Set("k=invalid-syntax"); err == nil {
		t.Error("invalid value syntax is not being caught")
	}

	o.Set("r1=k12=v12")
	assert.Check(t, is.DeepEqual(tmpMap["r1"], map[string]string{"k11": "v11", "k12": "v12"}))

	if o.Set(`invalid-key={"k":"v"}`) == nil {
		t.Error("validator is not being called")
	}
}
