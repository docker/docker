// Copyright 2025 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package headers

import (
	"context"
	"net/http"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/internal"
	"cloud.google.com/go/auth/internal/regionalaccessboundary"
)

type regionalAccessBoundaryProvider interface {
	GetHeaderValue(ctx context.Context, reqURL string, token *auth.Token) string
}

// SetAuthHeader uses the provided token to set the Authorization and regional
// access boundary headers on a request. If the token.Type is empty, the type is
// assumed to be Bearer.
func SetAuthHeader(token *auth.Token, req *http.Request) {
	typ := token.Type
	if typ == "" {
		typ = internal.TokenTypeBearer
	}
	req.Header.Set("Authorization", typ+" "+token.Value)

	if provider, ok := token.Metadata[regionalaccessboundary.ProviderKey].(regionalAccessBoundaryProvider); ok {
		if headerVal := provider.GetHeaderValue(req.Context(), req.URL.String(), token); headerVal != "" {
			req.Header.Set("x-allowed-locations", headerVal)
		}
	}
}

// SetAuthMetadata uses the provided token to set the Authorization and regional
// access boundary metadata. If the token.Type is empty, the type is assumed to be
// Bearer.
func SetAuthMetadata(ctx context.Context, token *auth.Token, reqURL string, m map[string]string) {
	typ := token.Type
	if typ == "" {
		typ = internal.TokenTypeBearer
	}
	m["authorization"] = typ + " " + token.Value

	if provider, ok := token.Metadata[regionalaccessboundary.ProviderKey].(regionalAccessBoundaryProvider); ok {
		if headerVal := provider.GetHeaderValue(ctx, reqURL, token); headerVal != "" {
			m["x-allowed-locations"] = headerVal
		}
	}
}
