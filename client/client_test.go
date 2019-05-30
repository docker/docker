package client // import "github.com/docker/docker/client"

import (
	"bytes"
	"context"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/docker/docker/api"
	"github.com/docker/docker/api/types"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/env"
	"gotest.tools/skip"
)

func TestNewClientWithOpsFromEnv(t *testing.T) {
	skip.If(t, runtime.GOOS == "windows")

	testcases := []struct {
		doc             string
		envs            map[string]string
		expectedError   string
		expectedVersion string
	}{
		{
			doc:             "default api version",
			envs:            map[string]string{},
			expectedVersion: api.DefaultVersion,
		},
		{
			doc: "invalid cert path",
			envs: map[string]string{
				"DOCKER_CERT_PATH": "invalid/path",
			},
			expectedError: "Could not load X509 key pair: open invalid/path/cert.pem: no such file or directory",
		},
		{
			doc: "default api version with cert path",
			envs: map[string]string{
				"DOCKER_CERT_PATH": "testdata/",
			},
			expectedVersion: api.DefaultVersion,
		},
		{
			doc: "default api version with cert path and tls verify",
			envs: map[string]string{
				"DOCKER_CERT_PATH":  "testdata/",
				"DOCKER_TLS_VERIFY": "1",
			},
			expectedVersion: api.DefaultVersion,
		},
		{
			doc: "default api version with cert path and host",
			envs: map[string]string{
				"DOCKER_CERT_PATH": "testdata/",
				"DOCKER_HOST":      "https://notaunixsocket",
			},
			expectedVersion: api.DefaultVersion,
		},
		{
			doc: "invalid docker host",
			envs: map[string]string{
				"DOCKER_HOST": "host",
			},
			expectedError: "unable to parse docker host `host`",
		},
		{
			doc: "invalid docker host, with good format",
			envs: map[string]string{
				"DOCKER_HOST": "invalid://url",
			},
			expectedVersion: api.DefaultVersion,
		},
		{
			doc: "override api version",
			envs: map[string]string{
				"DOCKER_API_VERSION": "1.22",
			},
			expectedVersion: "1.22",
		},
	}

	defer env.PatchAll(t, nil)()
	for _, c := range testcases {
		env.PatchAll(t, c.envs)
		apiclient, err := NewClientWithOpts(FromEnv)
		if c.expectedError != "" {
			assert.Check(t, is.Error(err, c.expectedError), c.doc)
		} else {
			assert.Check(t, err, c.doc)
			version := apiclient.ClientVersion()
			assert.Check(t, is.Equal(c.expectedVersion, version), c.doc)
		}

		if c.envs["DOCKER_TLS_VERIFY"] != "" {
			// pedantic checking that this is handled correctly
			tr := apiclient.client.Transport.(*http.Transport)
			assert.Assert(t, tr.TLSClientConfig != nil, c.doc)
			assert.Check(t, is.Equal(tr.TLSClientConfig.InsecureSkipVerify, false), c.doc)
		}
	}
}

func TestGetAPIPath(t *testing.T) {
	testcases := []struct {
		version  string
		path     string
		query    url.Values
		expected string
	}{
		{"", "/containers/json", nil, "/containers/json"},
		{"", "/containers/json", url.Values{}, "/containers/json"},
		{"", "/containers/json", url.Values{"s": []string{"c"}}, "/containers/json?s=c"},
		{"1.22", "/containers/json", nil, "/v1.22/containers/json"},
		{"1.22", "/containers/json", url.Values{}, "/v1.22/containers/json"},
		{"1.22", "/containers/json", url.Values{"s": []string{"c"}}, "/v1.22/containers/json?s=c"},
		{"v1.22", "/containers/json", nil, "/v1.22/containers/json"},
		{"v1.22", "/containers/json", url.Values{}, "/v1.22/containers/json"},
		{"v1.22", "/containers/json", url.Values{"s": []string{"c"}}, "/v1.22/containers/json?s=c"},
		{"v1.22", "/networks/kiwl$%^", nil, "/v1.22/networks/kiwl$%25%5E"},
	}

	ctx := context.TODO()
	for _, testcase := range testcases {
		c := Client{version: testcase.version, basePath: "/"}
		actual := c.getAPIPath(ctx, testcase.path, testcase.query)
		assert.Check(t, is.Equal(actual, testcase.expected))
	}
}

func TestParseHostURL(t *testing.T) {
	testcases := []struct {
		host        string
		expected    *url.URL
		expectedErr string
	}{
		{
			host:        "",
			expectedErr: "unable to parse docker host",
		},
		{
			host:        "foobar",
			expectedErr: "unable to parse docker host",
		},
		{
			host:     "foo://bar",
			expected: &url.URL{Scheme: "foo", Host: "bar"},
		},
		{
			host:     "tcp://localhost:2476",
			expected: &url.URL{Scheme: "tcp", Host: "localhost:2476"},
		},
		{
			host:     "tcp://localhost:2476/path",
			expected: &url.URL{Scheme: "tcp", Host: "localhost:2476", Path: "/path"},
		},
	}

	for _, testcase := range testcases {
		actual, err := ParseHostURL(testcase.host)
		if testcase.expectedErr != "" {
			assert.Check(t, is.ErrorContains(err, testcase.expectedErr))
		}
		assert.Check(t, is.DeepEqual(testcase.expected, actual))
	}
}

func TestNewClientWithOpsFromEnvSetsDefaultVersion(t *testing.T) {
	defer env.PatchAll(t, map[string]string{
		"DOCKER_HOST":        "",
		"DOCKER_API_VERSION": "",
		"DOCKER_TLS_VERIFY":  "",
		"DOCKER_CERT_PATH":   "",
	})()

	client, err := NewClientWithOpts(FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	assert.Check(t, is.Equal(client.version, api.DefaultVersion))

	expected := "1.22"
	os.Setenv("DOCKER_API_VERSION", expected)
	client, err = NewClientWithOpts(FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	assert.Check(t, is.Equal(expected, client.version))
}

// TestNegotiateAPIVersionEmpty asserts that client.Client can
// negotiate a compatible APIVersion when omitted
func TestNegotiateAPIVersionEmpty(t *testing.T) {
	defer env.PatchAll(t, map[string]string{"DOCKER_API_VERSION": ""})()

	client, err := NewClientWithOpts(FromEnv)
	assert.NilError(t, err)

	ping := types.Ping{
		APIVersion:   "",
		OSType:       "linux",
		Experimental: false,
	}

	// set our version to something new
	client.version = "1.25"

	// if no version from server, expect the earliest
	// version before APIVersion was implemented
	expected := "1.24"

	// test downgrade
	client.NegotiateAPIVersionPing(ping)
	assert.Check(t, is.Equal(expected, client.version))
}

// TestNegotiateAPIVersion asserts that client.Client can
// negotiate a compatible APIVersion with the server
func TestNegotiateAPIVersion(t *testing.T) {
	client, err := NewClientWithOpts(FromEnv)
	assert.NilError(t, err)

	expected := "1.21"
	ping := types.Ping{
		APIVersion:   expected,
		OSType:       "linux",
		Experimental: false,
	}

	// set our version to something new
	client.version = "1.22"

	// test downgrade
	client.NegotiateAPIVersionPing(ping)
	assert.Check(t, is.Equal(expected, client.version))

	// set the client version to something older, and verify that we keep the
	// original setting.
	expected = "1.20"
	client.version = expected
	client.NegotiateAPIVersionPing(ping)
	assert.Check(t, is.Equal(expected, client.version))

}

// TestNegotiateAPIVersionOverride asserts that we honor
// the environment variable DOCKER_API_VERSION when negotiating versions
func TestNegotiateAPVersionOverride(t *testing.T) {
	expected := "9.99"
	defer env.PatchAll(t, map[string]string{"DOCKER_API_VERSION": expected})()

	client, err := NewClientWithOpts(FromEnv)
	assert.NilError(t, err)

	ping := types.Ping{
		APIVersion:   "1.24",
		OSType:       "linux",
		Experimental: false,
	}

	// test that we honored the env var
	client.NegotiateAPIVersionPing(ping)
	assert.Check(t, is.Equal(expected, client.version))
}

func TestNegotiateAPIVersionAutomatic(t *testing.T) {
	var pingVersion string
	httpClient := newMockClient(func(req *http.Request) (*http.Response, error) {
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
		resp.Header.Set("API-Version", pingVersion)
		resp.Body = ioutil.NopCloser(strings.NewReader("OK"))
		return resp, nil
	})

	client, err := NewClientWithOpts(
		WithHTTPClient(httpClient),
		WithAPIVersionNegotiation(),
	)
	assert.NilError(t, err)

	ctx := context.Background()
	assert.Equal(t, client.ClientVersion(), api.DefaultVersion)

	// First request should trigger negotiation
	pingVersion = "1.35"
	_, _ = client.Info(ctx)
	assert.Equal(t, client.ClientVersion(), "1.35")

	// Once successfully negotiated, subsequent requests should not re-negotiate
	pingVersion = "1.25"
	_, _ = client.Info(ctx)
	assert.Equal(t, client.ClientVersion(), "1.35")
}

// TestNegotiateAPIVersionWithEmptyVersion asserts that initializing a client
// with an empty version string does still allow API-version negotiation
func TestNegotiateAPIVersionWithEmptyVersion(t *testing.T) {
	client, err := NewClientWithOpts(WithVersion(""))
	assert.NilError(t, err)

	client.NegotiateAPIVersionPing(types.Ping{APIVersion: "1.35"})
	assert.Equal(t, client.version, "1.35")
}

// TestNegotiateAPIVersionWithFixedVersion asserts that initializing a client
// with an fixed version disables API-version negotiation
func TestNegotiateAPIVersionWithFixedVersion(t *testing.T) {
	client, err := NewClientWithOpts(WithVersion("1.35"))
	assert.NilError(t, err)

	client.NegotiateAPIVersionPing(types.Ping{APIVersion: "1.31"})
	assert.Equal(t, client.version, "1.35")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (rtf roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return rtf(req)
}

type bytesBufferClose struct {
	*bytes.Buffer
}

func (bbc bytesBufferClose) Close() error {
	return nil
}

func TestClientRedirect(t *testing.T) {
	client := &http.Client{
		CheckRedirect: CheckRedirect,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.String() == "/bla" {
				return &http.Response{StatusCode: 404}, nil
			}
			return &http.Response{
				StatusCode: 301,
				Header:     map[string][]string{"Location": {"/bla"}},
				Body:       bytesBufferClose{bytes.NewBuffer(nil)},
			}, nil
		}),
	}

	cases := []struct {
		httpMethod  string
		expectedErr *url.Error
		statusCode  int
	}{
		{http.MethodGet, nil, 301},
		{http.MethodPost, &url.Error{Op: "Post", URL: "/bla", Err: ErrRedirect}, 301},
		{http.MethodPut, &url.Error{Op: "Put", URL: "/bla", Err: ErrRedirect}, 301},
		{http.MethodDelete, &url.Error{Op: "Delete", URL: "/bla", Err: ErrRedirect}, 301},
	}

	for _, tc := range cases {
		req, err := http.NewRequest(tc.httpMethod, "/redirectme", nil)
		assert.Check(t, err)
		resp, err := client.Do(req)
		assert.Check(t, is.Equal(tc.statusCode, resp.StatusCode))
		if tc.expectedErr == nil {
			assert.Check(t, is.Nil(err))
		} else {
			urlError, ok := err.(*url.Error)
			assert.Assert(t, ok, "%T is not *url.Error", err)
			assert.Check(t, is.Equal(*tc.expectedErr, *urlError))
		}
	}
}
