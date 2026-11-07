/*
 *
 * Copyright 2026 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package google

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/internal/transport"
	"google.golang.org/grpc/status"
)

// The HTTP transport timeout values match newDefaultHTTPClient in the
// GCE metadata client package (cloud.google.com/go/compute/metadata):
//
// Proxy is nil to bypass environment proxies per gRFC A83, a 2s dial
// timeout fails fast if the local metadata server is unreachable, TCP
// keep-alive is set to 30s, and idle pooled connections close after 60s.
var defaultMetadataHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   2 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		IdleConnTimeout: 60 * time.Second,
	},
}

// idTokenFetcher fetches ID tokens from the GCE Metadata Server.
type idTokenFetcher struct {
	client       *http.Client // unproxied HTTP client for metadata server communication
	metadataHost string       // hostname of the GCE metadata server
}

// newIDTokenFetcher creates a new idTokenFetcher configured to send direct,
// unproxied HTTP requests to the GCE metadata server.
func newIDTokenFetcher() *idTokenFetcher {
	host := os.Getenv("GCE_METADATA_HOST")
	if host == "" {
		host = "metadata.google.internal"
	}

	return &idTokenFetcher{
		metadataHost: host,
		client:       defaultMetadataHTTPClient,
	}
}

// fetchIDToken fetches an ID token for the given audience from the GCE
// Metadata Server.
//
// It takes a context controlling HTTP request cancellation and an audience
// string for the requested token. It returns the raw JWT token string,
// expiration timestamp parsed from the token's "exp" claim, and an error
// mapped to a gRPC status if the HTTP request or JWT parsing fails:
//   - If the HTTP request fails with a status that maps to gRPC UNAVAILABLE
//     according to HTTP to gRPC status code mappings, it returns UNAVAILABLE.
//   - All other HTTP error status codes and JWT parsing failures map to
//     UNAUTHENTICATED.
//   - Non-HTTP request failures are mapped to UNAVAILABLE.
func (f *idTokenFetcher) fetchIDToken(ctx context.Context, audience string) (string, time.Time, error) {
	reqURL := fmt.Sprintf("http://%s/computeMetadata/v1/instance/service-accounts/default/identity?audience=%s&format=full", f.metadataHost, url.QueryEscape(audience))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", time.Time{}, status.Errorf(codes.Unavailable, "credentials: failed to fetch ID token: %v", err)
	}
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := f.client.Do(req)
	if err != nil {
		return "", time.Time{}, status.Errorf(codes.Unavailable, "credentials: failed to fetch ID token: %v", err)
	}
	defer resp.Body.Close()

	// Limit the response body read to 1 MB to prevent unbounded memory
	// allocation if the server returns an unexpectedly large response, matching
	// the limit used by the golang.org/x/oauth2 package when reading token
	// responses.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", time.Time{}, status.Errorf(codes.Unavailable, "credentials: failed to fetch ID token: %v", err)
	}

	// Per gRFC A83, if the returned HTTP status maps to UNAVAILABLE in the
	// standard HTTP to gRPC status code mapping, return UNAVAILABLE; otherwise,
	// return UNAUTHENTICATED.
	if resp.StatusCode != http.StatusOK {
		switch transport.HTTPStatusConvTab[resp.StatusCode] {
		case codes.Unavailable:
			return "", time.Time{}, status.Errorf(codes.Unavailable, "credentials: failed to fetch ID token: HTTP status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		default:
			return "", time.Time{}, status.Errorf(codes.Unauthenticated, "credentials: failed to fetch ID token: HTTP status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
	}

	// Handle the HTTP 200 OK case.
	rawJWT := strings.TrimSpace(string(body))
	expiry, err := parseJWTExpiry(rawJWT)
	if err != nil {
		return "", time.Time{}, status.Errorf(codes.Unauthenticated, "credentials: failed to fetch ID token: %v", err)
	}

	return rawJWT, expiry, nil
}

type jwtPayload struct {
	// Per RFC 7519 Section 2, JWT NumericDate values (such as "exp") are JSON
	// numeric values that may contain non-integer fractional seconds. Using
	// float64 ensures json.Unmarshal succeeds even if fractional seconds are
	// present.
	Exp float64 `json:"exp"`
}

// parseJWTExpiry parses a JWT string to extract its expiration timestamp
// ("exp" claim).
//
// A compact JWT consists of three dot-separated, Base64URL-encoded parts:
//  1. Header: Contains metadata like algorithm and token type. Ignored here.
//  2. Payload: Contains token claims in JSON format. Decoded to extract the
//     expiration timestamp ("exp").
//  3. Signature: Cryptographic signature. Ignored here.
//
// Per gRFC A83, if the JWT format is invalid or the "exp" claim cannot be
// extracted, an error is returned.
func parseJWTExpiry(jwtStr string) (time.Time, error) {
	// A valid JWT must have 3 parts: header, payload, and signature.
	parts := strings.Split(jwtStr, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("invalid JWT format: expected 3 parts, got %d", len(parts))
	}

	// Per the JWT specification (RFC 7519 / RFC 7515), compact JWT segments use
	// Base64URL encoding with all trailing '=' padding characters omitted.
	// base64.RawURLEncoding is unpadded Base64URL encoding and decodes these
	// strings directly without requiring manual padding.
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to decode JWT payload: %v", err)
	}

	var payload jwtPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return time.Time{}, fmt.Errorf("failed to unmarshal JWT payload: %v", err)
	}

	if payload.Exp <= 0 {
		return time.Time{}, fmt.Errorf("missing or invalid 'exp' claim in JWT payload")
	}

	return time.Unix(int64(payload.Exp), 0), nil
}
