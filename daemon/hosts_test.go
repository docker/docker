package daemon

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestConfigureRegistryHosts(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	assert.NilError(t, err)

	tests := []struct {
		name     string
		host     string
		insecure bool
		wantHTTP bool
	}{
		{
			name:     "localhost insecure",
			host:     net.JoinHostPort("localhost", port),
			insecure: true,
			wantHTTP: true,
		},
		{
			name: "localhost secure",
			host: net.JoinHostPort("localhost", port),
		},
		{
			name:     "loopback insecure",
			host:     net.JoinHostPort("127.0.0.1", port),
			insecure: true,
			wantHTTP: true,
		},
		{
			name: "loopback secure",
			host: net.JoinHostPort("127.0.0.1", port),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hosts, err := configureRegistryHosts(tc.host, tc.insecure)
			assert.NilError(t, err)
			assert.Equal(t, len(hosts), 1)

			host := hosts[0]
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, host.Scheme+"://"+host.Host+"/v2/", http.NoBody)
			assert.NilError(t, err)

			resp, err := host.Client.Do(req)
			if resp != nil {
				defer func() { _ = resp.Body.Close() }()
			}

			if tc.wantHTTP {
				assert.NilError(t, err)
				assert.Equal(t, resp.StatusCode, http.StatusOK)
			} else {
				assert.Assert(t, err != nil)
			}
		})
	}
}

func TestMirrorsToHosts(t *testing.T) {
	pullCaps := docker.HostCapabilityPull | docker.HostCapabilityResolve | docker.HostCapabilityReferrers
	allCaps := docker.HostCapabilityPull | docker.HostCapabilityResolve | docker.HostCapabilityPush | docker.HostCapabilityReferrers
	defaultRegistry := testRegistryHost("https", "registry-1.docker.com", "/v2", allCaps)
	for _, tc := range []struct {
		mirrors  []string
		dhost    docker.RegistryHost
		expected []docker.RegistryHost
	}{
		{
			mirrors: []string{"https://localhost:5000"},
			dhost:   defaultRegistry,
			expected: []docker.RegistryHost{
				testRegistryHost("https", "localhost:5000", "/v2", pullCaps),
				defaultRegistry,
			},
		},
		{
			mirrors: []string{"http://localhost:5000"},
			dhost:   defaultRegistry,
			expected: []docker.RegistryHost{
				testRegistryHost("http", "localhost:5000", "/v2", pullCaps),
				defaultRegistry,
			},
		},
		{
			mirrors: []string{"http://localhost:5000/v2"},
			dhost:   defaultRegistry,
			expected: []docker.RegistryHost{
				testRegistryHost("http", "localhost:5000", "/v2", pullCaps),
				defaultRegistry,
			},
		},
		{
			mirrors: []string{"localhost:5000"},
			dhost:   defaultRegistry,
			expected: []docker.RegistryHost{
				testRegistryHost("https", "localhost:5000", "/v2", pullCaps),
				defaultRegistry,
			},
		},
		{
			mirrors: []string{"localhost:5000/trailingslash/"},
			dhost:   defaultRegistry,
			expected: []docker.RegistryHost{
				testRegistryHost("https", "localhost:5000", "/trailingslash/v2", pullCaps),
				defaultRegistry,
			},
		},
		{
			mirrors: []string{"localhost:5000/2trailingslash//"},
			dhost:   defaultRegistry,
			expected: []docker.RegistryHost{
				testRegistryHost("https", "localhost:5000", "/2trailingslash/v2", pullCaps),
				defaultRegistry,
			},
		},
		{
			mirrors: []string{"localhost:5000/v2/"},
			dhost:   defaultRegistry,
			expected: []docker.RegistryHost{
				testRegistryHost("https", "localhost:5000", "/v2", pullCaps),
				defaultRegistry,
			},
		},
		{
			mirrors: []string{"localhost:5000/base"},
			dhost:   defaultRegistry,
			expected: []docker.RegistryHost{
				testRegistryHost("https", "localhost:5000", "/base/v2", pullCaps),
				defaultRegistry,
			},
		},
		{
			// Legacy mirror configuration always appended /v2, keep functionality the same
			mirrors: []string{"localhost:5000/v2/base"},
			dhost:   defaultRegistry,
			expected: []docker.RegistryHost{
				testRegistryHost("https", "localhost:5000", "/v2/base/v2", pullCaps),
				defaultRegistry,
			},
		},
	} {
		actual := mirrorsToRegistryHosts(tc.mirrors, tc.dhost)

		assert.Check(t, is.DeepEqual(actual, tc.expected))
	}
}

func testRegistryHost(scheme, host, path string, caps docker.HostCapabilities) docker.RegistryHost {
	return docker.RegistryHost{
		Host:         host,
		Scheme:       scheme,
		Path:         path,
		Capabilities: caps,
	}
}
