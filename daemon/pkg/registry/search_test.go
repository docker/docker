package registry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/netip"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/distribution/registry/client/transport"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/v2/daemon/internal/filters"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func spawnTestRegistrySession(t *testing.T) (*http.Client, *v1Endpoint) {
	t.Helper()
	endpoint, err := newV1Endpoint(t.Context(), makeIndex("/v1/"), nil)
	if err != nil {
		t.Fatal(err)
	}
	authConfig := &registry.AuthConfig{}
	userAgent := "docker test client"
	var tr http.RoundTripper = debugTransport{newTransport(nil), t.Log}
	tr = transport.NewTransport(newAuthTransport(tr, authConfig, false), Headers(userAgent, nil)...)
	client := httpClient(tr)

	if err := authorizeClient(t.Context(), client, authConfig, endpoint); err != nil {
		t.Fatal(err)
	}
	// In a normal scenario for the v1 registry, the client should send a `X-Docker-Token: true`
	// header while authenticating, in order to retrieve a token that can be later used to
	// perform authenticated actions.
	//
	// The mock v1 registry does not support that, (TODO(tiborvass): support it), instead,
	// it will consider authenticated any request with the header `X-Docker-Token: fake-token`.
	//
	// Because we know that the client's transport is an `*authTransport` we simply cast it,
	// in order to set the internal cached token to the fake token, and thus send that fake token
	// upon every subsequent requests.
	client.Transport.(*authTransport).token = []string{"fake-token"}
	return client, endpoint
}

type debugTransport struct {
	http.RoundTripper
	log func(...any)
}

func (tr debugTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	dump, err := httputil.DumpRequestOut(req, false)
	if err != nil {
		tr.log("could not dump request")
	}
	tr.log(string(dump))
	resp, err := tr.RoundTripper.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	dump, err = httputil.DumpResponse(resp, false)
	if err != nil {
		tr.log("could not dump response")
	}
	tr.log(string(dump))
	return resp, err
}

func TestSearchRepositories(t *testing.T) {
	client, ep := spawnTestRegistrySession(t)
	results, err := searchRepositories(t.Context(), client, ep, "fakequery", 25)
	if err != nil {
		t.Fatal(err)
	}
	if results == nil {
		t.Fatal("Expected non-nil SearchResults object")
	}
	assert.Equal(t, results.NumResults, 1, "Expected 1 search results")
	assert.Equal(t, results.Query, "fakequery", "Expected 'fakequery' as query")
	assert.Equal(t, results.Results[0].StarCount, 42, "Expected 'fakeimage' to have 42 stars")
}

func TestSearchErrors(t *testing.T) {
	errorCases := []struct {
		filtersArgs       filters.Args
		shouldReturnError bool
		expectedError     string
	}{
		{
			expectedError:     "unexpected status code 500",
			shouldReturnError: true,
		},
		{
			filtersArgs:   filters.NewArgs(filters.Arg("type", "custom")),
			expectedError: "invalid filter 'type'",
		},
		{
			filtersArgs:   filters.NewArgs(filters.Arg("is-automated", "invalid")),
			expectedError: "invalid filter 'is-automated=[invalid]'",
		},
		{
			filtersArgs: filters.NewArgs(
				filters.Arg("is-automated", "true"),
				filters.Arg("is-automated", "false"),
			),
			expectedError: "invalid filter 'is-automated",
		},
		{
			filtersArgs:   filters.NewArgs(filters.Arg("is-official", "invalid")),
			expectedError: "invalid filter 'is-official=[invalid]'",
		},
		{
			filtersArgs: filters.NewArgs(
				filters.Arg("is-official", "true"),
				filters.Arg("is-official", "false"),
			),
			expectedError: "invalid filter 'is-official",
		},
		{
			filtersArgs:   filters.NewArgs(filters.Arg("stars", "invalid")),
			expectedError: "invalid filter 'stars=invalid'",
		},
		{
			filtersArgs: filters.NewArgs(
				filters.Arg("stars", "1"),
				filters.Arg("stars", "invalid"),
			),
			expectedError: "invalid filter 'stars=invalid'",
		},
	}
	for _, tc := range errorCases {
		t.Run(tc.expectedError, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !tc.shouldReturnError {
					t.Errorf("unexpected HTTP request")
				}
				http.Error(w, "no search for you", http.StatusInternalServerError)
			}))
			defer srv.Close()

			// Construct the search term by cutting the 'http://' prefix off srv.URL.
			term := srv.URL[7:] + "/term"

			reg, err := NewService(ServiceOptions{})
			assert.NilError(t, err)
			_, err = reg.Search(t.Context(), tc.filtersArgs, term, 0, nil, map[string][]string{})
			assert.ErrorContains(t, err, tc.expectedError)
			if tc.shouldReturnError {
				assert.Check(t, cerrdefs.IsUnknown(err), "got: %T: %v", err, err)
				return
			}
			assert.Check(t, cerrdefs.IsInvalidArgument(err), "got: %T: %v", err, err)
		})
	}
}

func TestSearch(t *testing.T) {
	const term = "term"
	successCases := []struct {
		name            string
		filtersArgs     filters.Args
		registryResults []registry.SearchResult
		expectedResults []registry.SearchResult
	}{
		{
			name:            "empty results",
			registryResults: []registry.SearchResult{},
			expectedResults: []registry.SearchResult{},
		},
		{
			name: "no filter",
			registryResults: []registry.SearchResult{
				{
					Name:        "name",
					Description: "description",
				},
			},
			expectedResults: []registry.SearchResult{
				{
					Name:        "name",
					Description: "description",
				},
			},
		},
		{
			name:        "is-automated=true, no results",
			filtersArgs: filters.NewArgs(filters.Arg("is-automated", "true")),
			registryResults: []registry.SearchResult{
				{
					Name:        "name",
					Description: "description",
				},
			},
			expectedResults: []registry.SearchResult{},
		},
		{
			name:        "is-automated=true",
			filtersArgs: filters.NewArgs(filters.Arg("is-automated", "true")),
			registryResults: []registry.SearchResult{
				{
					Name:        "name",
					Description: "description",
					IsAutomated: true, //nolint:staticcheck // ignore SA1019 (field is deprecated).
				},
			},
			expectedResults: []registry.SearchResult{},
		},
		{
			name:        "is-automated=false, IsAutomated reset to false",
			filtersArgs: filters.NewArgs(filters.Arg("is-automated", "false")),
			registryResults: []registry.SearchResult{
				{
					Name:        "name",
					Description: "description",
					IsAutomated: true, //nolint:staticcheck // ignore SA1019 (field is deprecated).
				},
			},
			expectedResults: []registry.SearchResult{
				{
					Name:        "name",
					Description: "description",
					IsAutomated: false, //nolint:staticcheck // ignore SA1019 (field is deprecated).
				},
			},
		},
		{
			name:        "is-automated=false",
			filtersArgs: filters.NewArgs(filters.Arg("is-automated", "false")),
			registryResults: []registry.SearchResult{
				{
					Name:        "name",
					Description: "description",
				},
			},
			expectedResults: []registry.SearchResult{
				{
					Name:        "name",
					Description: "description",
				},
			},
		},
		{
			name:        "is-official=true, no results",
			filtersArgs: filters.NewArgs(filters.Arg("is-official", "true")),
			registryResults: []registry.SearchResult{
				{
					Name:        "name",
					Description: "description",
				},
			},
			expectedResults: []registry.SearchResult{},
		},
		{
			name:        "is-official=true",
			filtersArgs: filters.NewArgs(filters.Arg("is-official", "true")),
			registryResults: []registry.SearchResult{
				{
					Name:        "name",
					Description: "description",
					IsOfficial:  true,
				},
			},
			expectedResults: []registry.SearchResult{
				{
					Name:        "name",
					Description: "description",
					IsOfficial:  true,
				},
			},
		},
		{
			name:        "is-official=false, no results",
			filtersArgs: filters.NewArgs(filters.Arg("is-official", "false")),
			registryResults: []registry.SearchResult{
				{
					Name:        "name",
					Description: "description",
					IsOfficial:  true,
				},
			},
			expectedResults: []registry.SearchResult{},
		},
		{
			name:        "is-official=false",
			filtersArgs: filters.NewArgs(filters.Arg("is-official", "false")),
			registryResults: []registry.SearchResult{
				{
					Name:        "name",
					Description: "description",
					IsOfficial:  false,
				},
			},
			expectedResults: []registry.SearchResult{
				{
					Name:        "name",
					Description: "description",
					IsOfficial:  false,
				},
			},
		},
		{
			name:        "stars=0",
			filtersArgs: filters.NewArgs(filters.Arg("stars", "0")),
			registryResults: []registry.SearchResult{
				{
					Name:        "name",
					Description: "description",
					StarCount:   0,
				},
			},
			expectedResults: []registry.SearchResult{
				{
					Name:        "name",
					Description: "description",
					StarCount:   0,
				},
			},
		},
		{
			name:        "stars=0, no results",
			filtersArgs: filters.NewArgs(filters.Arg("stars", "1")),
			registryResults: []registry.SearchResult{
				{
					Name:        "name",
					Description: "description",
					StarCount:   0,
				},
			},
			expectedResults: []registry.SearchResult{},
		},
		{
			name:        "stars=1",
			filtersArgs: filters.NewArgs(filters.Arg("stars", "1")),
			registryResults: []registry.SearchResult{
				{
					Name:        "name0",
					Description: "description0",
					StarCount:   0,
				},
				{
					Name:        "name1",
					Description: "description1",
					StarCount:   1,
				},
			},
			expectedResults: []registry.SearchResult{
				{
					Name:        "name1",
					Description: "description1",
					StarCount:   1,
				},
			},
		},
		{
			name: "stars=1, is-official=true, is-automated=true",
			filtersArgs: filters.NewArgs(
				filters.Arg("stars", "1"),
				filters.Arg("is-official", "true"),
				filters.Arg("is-automated", "true"),
			),
			registryResults: []registry.SearchResult{
				{
					Name:        "name0",
					Description: "description0",
					StarCount:   0,
					IsOfficial:  true,
					IsAutomated: true, //nolint:staticcheck // ignore SA1019 (field is deprecated).
				},
				{
					Name:        "name1",
					Description: "description1",
					StarCount:   1,
					IsOfficial:  false,
					IsAutomated: true, //nolint:staticcheck // ignore SA1019 (field is deprecated).
				},
				{
					Name:        "name2",
					Description: "description2",
					StarCount:   1,
					IsOfficial:  true,
				},
				{
					Name:        "name3",
					Description: "description3",
					StarCount:   2,
					IsOfficial:  true,
					IsAutomated: true, //nolint:staticcheck // ignore SA1019 (field is deprecated).
				},
			},
			expectedResults: []registry.SearchResult{},
		},
	}
	for _, tc := range successCases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-type", "application/json")
				json.NewEncoder(w).Encode(registry.SearchResults{
					Query:      term,
					NumResults: len(tc.registryResults),
					Results:    tc.registryResults,
				})
			}))
			defer srv.Close()

			// Construct the search term by cutting the 'http://' prefix off srv.URL.
			searchTerm := srv.URL[7:] + "/" + term

			reg, err := NewService(ServiceOptions{})
			assert.NilError(t, err)
			results, err := reg.Search(t.Context(), tc.filtersArgs, searchTerm, 0, nil, map[string][]string{})
			assert.NilError(t, err)
			assert.DeepEqual(t, results, tc.expectedResults)
		})
	}
}

func TestNewIndexInfo(t *testing.T) {
	overrideLookupIP(t)

	// emptyServiceConfig is a default service-config for situations where
	// no config-file is available (e.g. when used in the CLI). It won't
	// have mirrors configured, but does have the default insecure registry
	// CIDRs for loopback interfaces configured.
	emptyServiceConfig := &serviceConfig{
		IndexConfigs: map[string]*registry.IndexInfo{
			IndexName: {
				Name:     IndexName,
				Mirrors:  []string{},
				Secure:   true,
				Official: true,
			},
		},
		InsecureRegistryCIDRs: []netip.Prefix{
			netip.MustParsePrefix("::1/128"),
			netip.MustParsePrefix("127.0.0.0/8"),
		},
	}

	expectedIndexInfos := map[string]*registry.IndexInfo{
		IndexName: {
			Name:     IndexName,
			Official: true,
			Secure:   true,
			Mirrors:  []string{},
		},
		"index." + IndexName: {
			Name:     IndexName,
			Official: true,
			Secure:   true,
			Mirrors:  []string{},
		},
		"example.com": {
			Name:     "example.com",
			Official: false,
			Secure:   true,
			Mirrors:  []string{},
		},
		"127.0.0.1:5000": {
			Name:     "127.0.0.1:5000",
			Official: false,
			Secure:   false, // Loopback is insecure in the default configuration.
			Mirrors:  []string{},
		},
	}
	t.Run("no mirrors", func(t *testing.T) {
		for indexName, expected := range expectedIndexInfos {
			t.Run(indexName, func(t *testing.T) {
				actual := newIndexInfo(emptyServiceConfig, indexName)
				assert.Check(t, is.DeepEqual(actual, expected))
			})
		}
	})

	expectedIndexInfos = map[string]*registry.IndexInfo{
		IndexName: {
			Name:     IndexName,
			Official: true,
			Secure:   true,
			Mirrors:  []string{"http://mirror1.local/", "http://mirror2.local/"},
		},
		"index." + IndexName: {
			Name:     IndexName,
			Official: true,
			Secure:   true,
			Mirrors:  []string{"http://mirror1.local/", "http://mirror2.local/"},
		},
		"example.com": {
			Name:     "example.com",
			Official: false,
			Secure:   false,
			Mirrors:  []string{},
		},
		"example.com:5000": {
			Name:     "example.com:5000",
			Official: false,
			Secure:   true,
			Mirrors:  []string{},
		},
		"127.0.0.1": {
			Name:     "127.0.0.1",
			Official: false,
			Secure:   true, // Explicit insecure-registry configuration disables the loopback defaults.
			Mirrors:  []string{},
		},
		"127.0.0.1:5000": {
			Name:     "127.0.0.1:5000",
			Official: false,
			Secure:   true, // Explicit insecure-registry configuration disables the loopback defaults.
			Mirrors:  []string{},
		},
		"127.255.255.255": {
			Name:     "127.255.255.255",
			Official: false,
			Secure:   true,
			Mirrors:  []string{},
		},
		"127.255.255.255:5000": {
			Name:     "127.255.255.255:5000",
			Official: false,
			Secure:   true,
			Mirrors:  []string{},
		},
		"::1": {
			Name:     "::1",
			Official: false,
			Secure:   true,
			Mirrors:  []string{},
		},
		"[::1]:5000": {
			Name:     "[::1]:5000",
			Official: false,
			Secure:   true,
			Mirrors:  []string{},
		},
		// IPv6 only has a single loopback address, so ::2 is not a loopback.
		"::2": {
			Name:     "::2",
			Official: false,
			Secure:   true,
			Mirrors:  []string{},
		},
		// IPv6 only has a single loopback address, so ::2 is not a loopback.
		"[::2]:5000": {
			Name:     "[::2]:5000",
			Official: false,
			Secure:   true,
			Mirrors:  []string{},
		},
		"other.com": {
			Name:     "other.com",
			Official: false,
			Secure:   true,
			Mirrors:  []string{},
		},
	}
	t.Run("mirrors", func(t *testing.T) {
		// Note that newServiceConfig calls ValidateMirror internally, which normalizes
		// mirror-URLs to have a trailing slash.
		config, err := newServiceConfig(ServiceOptions{
			Mirrors:            []string{"http://mirror1.local", "http://mirror2.local"},
			InsecureRegistries: []string{"example.com"},
		})
		assert.NilError(t, err)
		for indexName, expected := range expectedIndexInfos {
			t.Run(indexName, func(t *testing.T) {
				actual := newIndexInfo(config, indexName)
				assert.Check(t, is.DeepEqual(actual, expected))
			})
		}
	})

	expectedIndexInfos = map[string]*registry.IndexInfo{
		"example.com": {
			Name:     "example.com",
			Official: false,
			Secure:   false,
			Mirrors:  []string{},
		},
		"example.com:5000": {
			Name:     "example.com:5000",
			Official: false,
			Secure:   false,
			Mirrors:  []string{},
		},
		"127.0.0.1": {
			Name:     "127.0.0.1",
			Official: false,
			Secure:   true, // Only the explicitly configured CIDR is insecure.
			Mirrors:  []string{},
		},
		"127.0.0.1:5000": {
			Name:     "127.0.0.1:5000",
			Official: false,
			Secure:   true,
			Mirrors:  []string{},
		},
		"192.0.2.1:5000": {
			Name:     "192.0.2.1:5000",
			Official: false,
			Secure:   false,
			Mirrors:  []string{},
		},
		"198.51.100.1:5000": {
			Name:     "198.51.100.1:5000",
			Official: false,
			Secure:   true,
			Mirrors:  []string{},
		},
		"other.com": {
			Name:     "other.com",
			Official: false,
			Secure:   true,
			Mirrors:  []string{},
		},
	}
	t.Run("custom insecure", func(t *testing.T) {
		config, err := newServiceConfig(ServiceOptions{
			InsecureRegistries: []string{"192.0.2.0/24"},
		})
		assert.NilError(t, err)
		for indexName, expected := range expectedIndexInfos {
			t.Run(indexName, func(t *testing.T) {
				actual := newIndexInfo(config, indexName)
				assert.Check(t, is.DeepEqual(actual, expected))
			})
		}
	})
}
