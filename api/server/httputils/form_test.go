package httputils

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/go-check/check"
)

// Hook up gocheck into the "go test" runner.
func Test(t *testing.T) { check.TestingT(t) }

type DockerSuite struct{}

var _ = check.Suite(&DockerSuite{})

func (s *DockerSuite) TestBoolValue(c *check.C) {
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"no":    false,
		"false": false,
		"none":  false,
		"1":     true,
		"yes":   true,
		"true":  true,
		"one":   true,
		"100":   true,
	}

	for n, e := range cases {
		v := url.Values{}
		v.Set("test", n)
		r, _ := http.NewRequest("POST", "", nil)
		r.Form = v

		a := BoolValue(r, "test")
		if a != e {
			c.Fatalf("Value: %s, expected: %v, actual: %v", n, e, a)
		}
	}
}

func (s *DockerSuite) TestBoolValueOrDefault(c *check.C) {
	r, _ := http.NewRequest("GET", "", nil)
	if !BoolValueOrDefault(r, "queryparam", true) {
		c.Fatal("Expected to get true default value, got false")
	}

	v := url.Values{}
	v.Set("param", "")
	r, _ = http.NewRequest("GET", "", nil)
	r.Form = v
	if BoolValueOrDefault(r, "param", true) {
		c.Fatal("Expected not to get true")
	}
}

func (s *DockerSuite) TestInt64ValueOrZero(c *check.C) {
	cases := map[string]int64{
		"":     0,
		"asdf": 0,
		"0":    0,
		"1":    1,
	}

	for n, e := range cases {
		v := url.Values{}
		v.Set("test", n)
		r, _ := http.NewRequest("POST", "", nil)
		r.Form = v

		a := Int64ValueOrZero(r, "test")
		if a != e {
			c.Fatalf("Value: %s, expected: %v, actual: %v", n, e, a)
		}
	}
}

func (s *DockerSuite) TestInt64ValueOrDefault(c *check.C) {
	cases := map[string]int64{
		"":   -1,
		"-1": -1,
		"42": 42,
	}

	for n, e := range cases {
		v := url.Values{}
		v.Set("test", n)
		r, _ := http.NewRequest("POST", "", nil)
		r.Form = v

		a, err := Int64ValueOrDefault(r, "test", -1)
		if a != e {
			c.Fatalf("Value: %s, expected: %v, actual: %v", n, e, a)
		}
		if err != nil {
			c.Fatalf("Error should be nil, but received: %s", err)
		}
	}
}

func (s *DockerSuite) TestInt64ValueOrDefaultWithError(c *check.C) {
	v := url.Values{}
	v.Set("test", "invalid")
	r, _ := http.NewRequest("POST", "", nil)
	r.Form = v

	_, err := Int64ValueOrDefault(r, "test", -1)
	if err == nil {
		c.Fatalf("Expected an error.")
	}
}
