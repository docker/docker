package request // import "github.com/docker/docker/internal/test/request"

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/docker/docker/client"
	"github.com/docker/docker/internal/test"
	"github.com/docker/docker/internal/test/environment"
	"github.com/docker/docker/opts"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/go-connections/sockets"
	"github.com/docker/go-connections/tlsconfig"
	"github.com/pkg/errors"
	"gotest.tools/assert"
)

// NewAPIClient returns a docker API client configured from environment variables
func NewAPIClient(t assert.TestingT, ops ...client.Opt) client.APIClient {
	if ht, ok := t.(test.HelperT); ok {
		ht.Helper()
	}
	ops = append([]client.Opt{client.FromEnv}, ops...)
	clt, err := client.NewClientWithOpts(ops...)
	assert.NilError(t, err)
	return clt
}

// DaemonTime provides the current time on the daemon host
func DaemonTime(ctx context.Context, t assert.TestingT, client client.APIClient, testEnv *environment.Execution) time.Time {
	if ht, ok := t.(test.HelperT); ok {
		ht.Helper()
	}
	if testEnv.IsLocalDaemon() {
		return time.Now()
	}

	info, err := client.Info(ctx)
	assert.NilError(t, err)

	dt, err := time.Parse(time.RFC3339Nano, info.SystemTime)
	assert.NilError(t, err, "invalid time format in GET /info response")
	return dt
}

// DaemonUnixTime returns the current time on the daemon host with nanoseconds precision.
// It return the time formatted how the client sends timestamps to the server.
func DaemonUnixTime(ctx context.Context, t assert.TestingT, client client.APIClient, testEnv *environment.Execution) string {
	if ht, ok := t.(test.HelperT); ok {
		ht.Helper()
	}
	dt := DaemonTime(ctx, t, client, testEnv)
	return fmt.Sprintf("%d.%09d", dt.Unix(), int64(dt.Nanosecond()))
}

// Post creates and execute a POST request on the specified host and endpoint, with the specified request modifiers
func Post(endpoint string, modifiers ...func(*Options)) (*http.Response, io.ReadCloser, error) {
	return Do(endpoint, append(modifiers, Method(http.MethodPost))...)
}

// Delete creates and execute a DELETE request on the specified host and endpoint, with the specified request modifiers
func Delete(endpoint string, modifiers ...func(*Options)) (*http.Response, io.ReadCloser, error) {
	return Do(endpoint, append(modifiers, Method(http.MethodDelete))...)
}

// Get creates and execute a GET request on the specified host and endpoint, with the specified request modifiers
func Get(endpoint string, modifiers ...func(*Options)) (*http.Response, io.ReadCloser, error) {
	return Do(endpoint, modifiers...)
}

// Head creates and execute a HEAD request on the specified host and endpoint, with the specified request modifiers
func Head(endpoint string, modifiers ...func(*Options)) (*http.Response, io.ReadCloser, error) {
	return Do(endpoint, append(modifiers, Method(http.MethodHead))...)
}

// Do creates and execute a request on the specified endpoint, with the specified request modifiers
func Do(endpoint string, modifiers ...func(*Options)) (*http.Response, io.ReadCloser, error) {
	opts := &Options{
		host: DaemonHost(),
	}
	for _, mod := range modifiers {
		mod(opts)
	}
	req, err := newRequest(endpoint, opts)
	if err != nil {
		return nil, nil, err
	}
	client, err := newHTTPClient(opts.host)
	if err != nil {
		return nil, nil, err
	}
	resp, err := client.Do(req)
	var body io.ReadCloser
	if resp != nil {
		body = ioutils.NewReadCloserWrapper(resp.Body, func() error {
			defer resp.Body.Close()
			return nil
		})
	}
	return resp, body, err
}

// ReadBody read the specified ReadCloser content and returns it
func ReadBody(b io.ReadCloser) ([]byte, error) {
	defer b.Close()
	return ioutil.ReadAll(b)
}

// newRequest creates a new http Request to the specified host and endpoint, with the specified request modifiers
func newRequest(endpoint string, opts *Options) (*http.Request, error) {
	hostURL, err := client.ParseHostURL(opts.host)
	if err != nil {
		return nil, errors.Wrapf(err, "failed parsing url %q", opts.host)
	}
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create request")
	}

	if os.Getenv("DOCKER_TLS_VERIFY") != "" {
		req.URL.Scheme = "https"
	} else {
		req.URL.Scheme = "http"
	}
	req.URL.Host = hostURL.Host

	for _, config := range opts.requestModifiers {
		if err := config(req); err != nil {
			return nil, err
		}
	}

	return req, nil
}

// newHTTPClient creates an http client for the specific host
// TODO: Share more code with client.defaultHTTPClient
func newHTTPClient(host string) (*http.Client, error) {
	// FIXME(vdemeester) 10*time.Second timeout of SockRequest… ?
	hostURL, err := client.ParseHostURL(host)
	if err != nil {
		return nil, err
	}
	transport := new(http.Transport)
	if hostURL.Scheme == "tcp" && os.Getenv("DOCKER_TLS_VERIFY") != "" {
		// Setup the socket TLS configuration.
		tlsConfig, err := getTLSConfig()
		if err != nil {
			return nil, err
		}
		transport = &http.Transport{TLSClientConfig: tlsConfig}
	}
	transport.DisableKeepAlives = true
	err = sockets.ConfigureTransport(transport, hostURL.Scheme, hostURL.Host)
	return &http.Client{Transport: transport}, err
}

func getTLSConfig() (*tls.Config, error) {
	dockerCertPath := os.Getenv("DOCKER_CERT_PATH")

	if dockerCertPath == "" {
		return nil, errors.New("DOCKER_TLS_VERIFY specified, but no DOCKER_CERT_PATH environment variable")
	}

	option := &tlsconfig.Options{
		CAFile:   filepath.Join(dockerCertPath, "ca.pem"),
		CertFile: filepath.Join(dockerCertPath, "cert.pem"),
		KeyFile:  filepath.Join(dockerCertPath, "key.pem"),
	}
	tlsConfig, err := tlsconfig.Client(*option)
	if err != nil {
		return nil, err
	}

	return tlsConfig, nil
}

// DaemonHost return the daemon host string for this test execution
func DaemonHost() string {
	daemonURLStr := "unix://" + opts.DefaultUnixSocket
	if daemonHostVar := os.Getenv("DOCKER_HOST"); daemonHostVar != "" {
		daemonURLStr = daemonHostVar
	}
	return daemonURLStr
}

// SockConn opens a connection on the specified socket
func SockConn(timeout time.Duration, daemon string) (net.Conn, error) {
	daemonURL, err := url.Parse(daemon)
	if err != nil {
		return nil, errors.Wrapf(err, "could not parse url %q", daemon)
	}

	var c net.Conn
	switch daemonURL.Scheme {
	case "npipe":
		return npipeDial(daemonURL.Path, timeout)
	case "unix":
		return net.DialTimeout(daemonURL.Scheme, daemonURL.Path, timeout)
	case "tcp":
		if os.Getenv("DOCKER_TLS_VERIFY") != "" {
			// Setup the socket TLS configuration.
			tlsConfig, err := getTLSConfig()
			if err != nil {
				return nil, err
			}
			dialer := &net.Dialer{Timeout: timeout}
			return tls.DialWithDialer(dialer, daemonURL.Scheme, daemonURL.Host, tlsConfig)
		}
		return net.DialTimeout(daemonURL.Scheme, daemonURL.Host, timeout)
	default:
		return c, errors.Errorf("unknown scheme %v (%s)", daemonURL.Scheme, daemon)
	}
}
