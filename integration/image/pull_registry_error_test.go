package image

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/moby/moby/api/types/auxprogress"
	"github.com/moby/moby/client"
	"github.com/opencontainers/go-digest"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
	"gotest.tools/v3/skip"
)

// TestImagePullRegistryErrorAux tests that OCI registry errors are propagated
// through the aux progress stream when pulling fails.
func TestImagePullRegistryErrorAux(t *testing.T) {
	skip.If(t, testEnv.IsRemoteDaemon, "cannot run daemon when remote daemon")
	skip.If(t, testEnv.DaemonInfo.OSType == "windows", "We don't run a test registry on Windows")
	skip.If(t, testEnv.IsRootless, "Rootless has a different view of localhost")
	skip.If(t, !testEnv.UsingSnapshotter(), "Not supported for graphdrivers yet")

	ctx := setupTest(t)

	testCases := []struct {
		name         string
		responseBody string
		statusCode   int
		expectErrors auxprogress.OCIRegistryErrors
	}{
		{
			name:       "UNAUTHORIZED error",
			statusCode: http.StatusUnauthorized,
			responseBody: `{
				"errors": [
					{"code": "UNAUTHORIZED", "message": "authentication required"}
				]
			}`,
			expectErrors: auxprogress.OCIRegistryErrors{
				Errors: []auxprogress.OCIRegistryError{
					{Code: "UNAUTHORIZED", Message: "authentication required"},
				},
			},
		},
		{
			name:       "DENIED error",
			statusCode: http.StatusForbidden,
			responseBody: `{
				"errors": [
					{"code": "DENIED", "message": "access forbidden"}
				]
			}`,
			expectErrors: auxprogress.OCIRegistryErrors{
				Errors: []auxprogress.OCIRegistryError{
					{Code: "DENIED", Message: "access forbidden"},
				},
			},
		},
		{
			name:       "multiple errors",
			statusCode: http.StatusForbidden,
			responseBody: `{
				"errors": [
					{"code": "DENIED", "message": "access denied"},
					{"code": "UNAUTHORIZED", "message": "not authenticated"}
				]
			}`,
			expectErrors: auxprogress.OCIRegistryErrors{
				Errors: []auxprogress.OCIRegistryError{
					{Code: "DENIED", Message: "access denied"},
					{Code: "UNAUTHORIZED", Message: "not authenticated"},
				},
			},
		},
		{
			name:       "DENIED error with detail",
			statusCode: http.StatusForbidden,
			responseBody: `{
				"errors": [
					{"code": "DENIED", "message": "manifest pull has failed policy validation", "detail": "policy validation failed"}
				]
			}`,
			expectErrors: auxprogress.OCIRegistryErrors{
				Errors: []auxprogress.OCIRegistryError{
					{
						Code:    "DENIED",
						Message: "manifest pull has failed policy validation",
						Detail:  "policy validation failed",
					},
				},
			},
		},
	}

	manifestDigest := digest.SHA256.FromString("{}")
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create a mock registry server that returns the OCI error
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Logf("Mock registry received: %s %s", r.Method, r.URL.Path)

				// Handle /v2/ ping
				if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
					w.WriteHeader(http.StatusOK)
					return
				}

				handleManifestRequest := func() {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodHead {
						w.Header().Set("Docker-Content-Digest", manifestDigest.String())
						w.Header().Set("Content-Length", "2")
						w.WriteHeader(http.StatusOK)
						return
					}
					w.WriteHeader(tc.statusCode)
					_, _ = w.Write([]byte(tc.responseBody))
					t.Logf("manifests: Returning %d for %s %s", tc.statusCode, r.Method, r.URL.Path)
				}

				// Return error for manifest requests
				if strings.Contains(r.URL.Path, "/manifests/") {
					handleManifestRequest()
					return
				}

				// If manifests returns error, the client will try to fetch it from the blobs endpoint.
				if strings.Contains(r.URL.Path, "/blobs/"+manifestDigest.String()) {
					handleManifestRequest()
					return
				}

				t.Logf("Returning 404 for %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}))
			defer ts.Close()

			// Extract host:port from the test server URL
			registryHost := strings.TrimPrefix(ts.URL, "http://")
			imageRef := path.Join(registryHost, "test/image:latest")

			apiClient := testEnv.APIClient()
			res, err := apiClient.ImagePull(ctx, imageRef, client.ImagePullOptions{})
			t.Logf("ImagePull error: %v", err)
			if err != nil {
				var derrs docker.Errors
				if !errors.As(err, &derrs) {
					assert.NilError(t, err, "Initiating pull request failed, %T: %v", err, err)
				}
				return
			}
			defer res.Close()

			// Collect aux messages from the JSON stream
			var auxMessages []auxprogress.OCIRegistryErrors
			for msg, err := range res.JSONMessages(ctx) {
				if err != nil {
					t.Logf("JSONMessages error: %v", err)
					break
				}
				if msg.Aux != nil {
					var ociErrs auxprogress.OCIRegistryErrors
					if err := json.Unmarshal(*msg.Aux, &ociErrs); err == nil && len(ociErrs.Errors) > 0 {
						auxMessages = append(auxMessages, ociErrs)
					}
				}
			}

			// Verify we received the expected aux messages
			assert.Assert(t, is.Len(auxMessages, 1), "expected 1 aux message with OCI errors")
			assert.Check(t, is.DeepEqual(auxMessages[0], tc.expectErrors))
		})
	}
}
