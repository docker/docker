package client // import "github.com/docker/docker/client"

import (
	"bytes"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"testing"

	"github.com/docker/docker/api"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client/context"
	"github.com/docker/docker/pkg/contextstore"
	"gotest.tools/assert"
	is "gotest.tools/assert/cmp"
	"gotest.tools/env"
	"gotest.tools/skip"
)

func TestNewEnvClient(t *testing.T) {
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
		apiclient, err := NewEnvClient()
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

func TestClientWithContext(t *testing.T) {
	caBytes, err := ioutil.ReadFile("testdata/ca.pem")
	assert.Check(t, err)
	certBytes, err := ioutil.ReadFile("testdata/cert.pem")
	assert.Check(t, err)
	keyBytes, err := ioutil.ReadFile("testdata/key.pem")
	assert.Check(t, err)
	storeDir, err := ioutil.TempDir("", "test-client-with-context")
	assert.Check(t, err)
	defer os.RemoveAll(storeDir)
	type contextDef struct {
		name          string
		host          string
		apiVersion    string
		ca            []byte
		cert          []byte
		key           []byte
		skipTLSVerify bool
	}
	contexts := []contextDef{
		{
			name: "all-default",
		},
		{
			name: "host-only",
			host: "https://notunixsocket",
		},
		{
			name:       "apiversion-only",
			apiVersion: "1.22",
		},
		{
			name: "with-tls",
			host: "https://notunixsocket",
			ca:   caBytes,
			key:  keyBytes,
			cert: certBytes,
		},
		{
			name:          "with-tls-skipverify",
			host:          "https://notunixsocket",
			ca:            caBytes,
			key:           keyBytes,
			cert:          certBytes,
			skipTLSVerify: true,
		},
		{
			name:          "with-skipverify-only",
			host:          "https://notunixsocket",
			skipTLSVerify: true,
		},
	}
	s, err := contextstore.NewStore(storeDir)
	assert.Check(t, err)
	for _, c := range contexts {
		t.Run(c.name, func(t *testing.T) {
			err = context.SetDockerEndpoint(s, c.name, c.host, c.apiVersion, c.ca, c.cert, c.key, c.skipTLSVerify)
			assert.Check(t, err)
			client, err := NewClientWithOpts(WithContextStoreOrEnv(storeDir, c.name))
			assert.Check(t, err)
			if c.host == "" {
				assert.Equal(t, client.host, DefaultDockerHost)
			} else {
				assert.Equal(t, client.host, c.host)
				// assume https
				tr := client.client.Transport.(*http.Transport)
				if c.ca == nil && c.cert == nil && c.key == nil && !c.skipTLSVerify {
					assert.Check(t, tr.TLSClientConfig == nil)
				} else {
					assert.Check(t, tr.TLSClientConfig != nil)
					if c.skipTLSVerify {
						assert.Check(t, tr.TLSClientConfig.InsecureSkipVerify)
					}
					if c.ca != nil {
						assert.Check(t, tr.TLSClientConfig.RootCAs != nil)
					} else {
						assert.Check(t, tr.TLSClientConfig.RootCAs == nil)
					}
					if c.cert != nil && c.key != nil {
						assert.Check(t, tr.TLSClientConfig.Certificates != nil)
					} else {
						assert.Check(t, tr.TLSClientConfig.Certificates == nil)
					}
				}
			}
			if c.apiVersion == "" {
				assert.Equal(t, client.ClientVersion(), api.DefaultVersion)
			} else {
				assert.Equal(t, client.ClientVersion(), c.apiVersion)
			}
		})
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

	for _, testcase := range testcases {
		c := Client{version: testcase.version, basePath: "/"}
		actual := c.getAPIPath(testcase.path, testcase.query)
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

func TestNewEnvClientSetsDefaultVersion(t *testing.T) {
	defer env.PatchAll(t, map[string]string{
		"DOCKER_HOST":        "",
		"DOCKER_API_VERSION": "",
		"DOCKER_TLS_VERIFY":  "",
		"DOCKER_CERT_PATH":   "",
	})()

	client, err := NewEnvClient()
	if err != nil {
		t.Fatal(err)
	}
	assert.Check(t, is.Equal(client.version, api.DefaultVersion))

	expected := "1.22"
	os.Setenv("DOCKER_API_VERSION", expected)
	client, err = NewEnvClient()
	if err != nil {
		t.Fatal(err)
	}
	assert.Check(t, is.Equal(expected, client.version))
}

// TestNegotiateAPIVersionEmpty asserts that client.Client can
// negotiate a compatible APIVersion when omitted
func TestNegotiateAPIVersionEmpty(t *testing.T) {
	defer env.PatchAll(t, map[string]string{"DOCKER_API_VERSION": ""})()

	client, err := NewEnvClient()
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
	client, err := NewEnvClient()
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

	client, err := NewEnvClient()
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
