package distribution

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/distribution/reference"
	registrytypes "github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/pkg/registry"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestNoMatchesErr(t *testing.T) {
	err := noMatchesErr{}
	assert.Check(t, is.ErrorContains(err, "no matching manifest for "+runtime.GOOS))

	err = noMatchesErr{ocispec.Platform{
		Architecture: "arm64",
		OS:           "windows",
		OSVersion:    "10.0.17763",
		Variant:      "v8",
	}}
	assert.Check(t, is.Error(err, "no matching manifest for windows(10.0.17763)/arm64/v8 in the manifest list entries"))
}

func TestInvalidManifestClassErr(t *testing.T) {
	err := invalidManifestClassError{mediaType: "application/vnd.docker.container.image.v1+json", class: "image"}
	assert.Check(t, is.Error(err, `unsupported remote descriptor: image config ("application/vnd.docker.container.image.v1+json")`))
}

func TestPullSchema2Config(t *testing.T) {
	ctx := t.Context()

	const imageJSON = `{
	"architecture": "amd64",
	"os": "linux",
	"config": {},
	"rootfs": {
		"type": "layers",
		"diff_ids": []
	}
}`
	expectedDigest := digest.Digest("sha256:66ad98165d38f53ee73868f82bd4eed60556ddfee824810a4062c4f777b20a5b")

	tests := []struct {
		name           string
		handler        func(callCount int, w http.ResponseWriter)
		expectError    string
		expectAttempts uint64
	}{
		{
			name: "success first time",
			handler: func(callCount int, w http.ResponseWriter) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(imageJSON))
			},
			expectAttempts: 1,
		},
		{
			name: "500 status",
			handler: func(callCount int, w http.ResponseWriter) {
				if callCount == 1 {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(imageJSON))
			},
			expectAttempts: 2,
		},
		{
			name: "EOF",
			handler: func(callCount int, w http.ResponseWriter) {
				if callCount == 1 {
					panic("intentional panic")
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(imageJSON))
			},
			expectAttempts: 2,
		},
		{
			name: "unauthorized",
			handler: func(callCount int, w http.ResponseWriter) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("you need to be authenticated"))
			},
			expectError:    "unauthorized: you need to be authenticated",
			expectAttempts: 1,
		},
		{
			name: "unauthorized JSON",
			handler: func(callCount int, w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`					{ "errors":	[{"code": "UNAUTHORIZED", "message": "you need to be authenticated", "detail": "more detail"}]}`))
			},
			expectError:    "unauthorized: you need to be authenticated",
			expectAttempts: 1,
		},
		{
			name: "unauthorized JSON no body",
			handler: func(callCount int, w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
			},
			expectError:    "unauthorized: authentication required",
			expectAttempts: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var callCount atomic.Uint64
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Logf("HTTP %s %s", r.Method, r.URL.Path)
				defer r.Body.Close()
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v2":
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodGet && r.URL.Path == "/v2/library/testremotename/blobs/"+expectedDigest.String():
					tc.handler(int(callCount.Add(1)), w)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer ts.Close()

			p := testNewPuller(t, ts.URL)

			config, err := p.pullSchema2Config(ctx, expectedDigest)
			if tc.expectError == "" {
				if err != nil {
					t.Fatal(err)
				}

				_, err = image.NewFromJSON(config)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil {
					t.Fatalf("expected error to contain %q", tc.expectError)
				}
				if !strings.Contains(err.Error(), tc.expectError) {
					t.Fatalf("expected error=%q to contain %q", err, tc.expectError)
				}
			}

			if cc := callCount.Load(); cc != tc.expectAttempts {
				t.Fatalf("got callCount=%d but expected=%d", cc, tc.expectAttempts)
			}
		})
	}
}

func testNewPuller(t *testing.T, rawurl string) *puller {
	t.Helper()

	uri, err := url.Parse(rawurl)
	assert.NilError(t, err, "could not parse url from test server: %v", rawurl)

	repoName, err := reference.ParseNormalizedNamed("testremotename")
	assert.NilError(t, err)

	imagePullConfig := &ImagePullConfig{
		Config: Config{
			AuthConfig: &registrytypes.AuthConfig{
				RegistryToken: secretRegistryToken,
			},
		},
	}

	p := newPuller(registry.APIEndpoint{URL: uri}, repoName, imagePullConfig, nil)
	p.repo, err = newRepository(t.Context(), repoName, p.endpoint, p.config.MetaHeaders, p.config.AuthConfig, "pull")
	assert.NilError(t, err)
	return p
}
