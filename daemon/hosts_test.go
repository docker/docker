package daemon

import (
	"net/http"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

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

func TestRegistryHostsResponseHeaderTimeout(t *testing.T) {
	d := &Daemon{}
	hosts, err := d.RegistryHosts("registry.example.com")
	assert.NilError(t, err)
	assert.Assert(t, len(hosts) > 0)
	for _, h := range hosts {
		tr, ok := h.Client.Transport.(*http.Transport)
		assert.Assert(t, ok, "unexpected transport type %T", h.Client.Transport)
		assert.Check(t, is.Equal(tr.ResponseHeaderTimeout, time.Duration(0)))
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
