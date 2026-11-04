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
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/google/internal"
	"google.golang.org/grpc/internal/backoff"
	"google.golang.org/grpc/status"
)

const (
	// preemptiveRefresh is the window before a token's actual expiration during
	// which the token is considered stale. Requests using a stale but still
	// valid token will trigger a background asynchronous refresh. This avoids
	// blocking the current RPC, preventing periodic latency spikes during token
	// refresh.
	preemptiveRefresh = 1 * time.Minute

	// metadataTimeout is the timeout for the call to the metadata server to
	// prevent the background token fetch from hanging indefinitely in case
	// of connection issues.
	metadataTimeout = 60 * time.Second
)

type gcpServiceAccountIdentityCallCreds struct {
	// The following fields are initialized at creation time and are read-only
	// after that.
	ctx          context.Context
	audience     string
	backoff      backoff.Strategy
	fetchIDToken func(context.Context, string) (string, time.Time, error)

	// The following fields are protected by mu.
	mu                     sync.Mutex
	token                  string        // cached raw JWT token value
	tokenExpiry            time.Time     // timestamp after which the cached token is considered invalid
	preemptiveTokenRefresh time.Time     // timestamp after which background preemptive refresh is triggered
	fetching               chan struct{} // used to ensure a single in-progress background token fetch
	nextRetryTime          time.Time     // timestamp after which we can attempt the next token fetch
	retryAttempt           int           // consecutive fetch failure count used to compute backoff delay
	lastErr                error         // cached error returned from the most recent token fetch attempt
}

func init() {
	internal.BackoffStrategy = backoff.DefaultExponential
	internal.IDTokenFetcher = func() func(context.Context, string) (string, time.Time, error) {
		return newIDTokenFetcher().fetchIDToken
	}
}

// NewServiceAccountIdentityCredentials creates a PerRPCCredentials that
// authenticates using a GCP Service Account Identity JWT token for the given
// audience.
//
// This credential fetches the ID token from the GCE metadata server and is
// only valid for use in environments running on GCP. The ctx and audience
// parameters cannot be empty.
//
// The credentials object starts asynchronous background token fetches to
// refresh expired tokens. The provided context propagates cancellation to
// these background tasks. Users should not pass an RPC-scoped context here,
// but rather a context that is valid for the entire lifetime of the
// credentials and should cancel the context when they are done.
//
// # Experimental
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func NewServiceAccountIdentityCredentials(ctx context.Context, audience string) (credentials.PerRPCCredentials, error) {
	if ctx == nil {
		return nil, fmt.Errorf("credentials: ctx cannot be nil")
	}

	if audience == "" {
		return nil, fmt.Errorf("credentials: audience cannot be empty")
	}

	return &gcpServiceAccountIdentityCallCreds{
		ctx:          ctx,
		audience:     audience,
		backoff:      internal.BackoffStrategy,
		fetchIDToken: internal.IDTokenFetcher(),
	}, nil
}

// GetRequestMetadata gets the current request metadata, refreshing tokens if
// required. This implementation follows the PerRPCCredentials interface.
//
// It guarantees that only one underlying token fetch will be executed
// concurrently. If a valid token is cached, it is returned immediately. If
// a fetch recently failed, the cached error is returned until the backoff
// interval expires. Otherwise, it initiates a new token fetch or blocks
// waiting for an already-in-progress fetch to complete.
func (c *gcpServiceAccountIdentityCallCreds) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	ri, _ := credentials.RequestInfoFromContext(ctx)
	if err := credentials.CheckSecurityLevel(ri.AuthInfo, credentials.PrivacyAndIntegrity); err != nil {
		return nil, fmt.Errorf("credentials: cannot send secure credentials on an insecure connection: %v", err)
	}

	if md, err := c.cachedRequestMetadata(true); md != nil || err != nil {
		return md, err
	}

	c.mu.Lock()
	// Now that we have the lock, did someone else finish the fetch while we
	// were waiting for the lock?
	md, err := c.cachedRequestMetadataLocked(false)
	if md != nil || err != nil {
		c.mu.Unlock()
		return md, err
	}

	// If no one is fetching, start it.
	if c.fetching == nil {
		c.fetching = make(chan struct{})
		go c.startFetch()
	}
	wait := c.fetching
	c.mu.Unlock()

	select {
	case <-wait:
		// Return the new cached result from the most recent fetch.
		md, err := c.cachedRequestMetadata(false)
		if md == nil && err == nil {
			return nil, status.Error(codes.Unavailable, "credentials: fetched token is expired")
		}
		return md, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// cachedRequestMetadata handles its own locking for the "fast path".
func (c *gcpServiceAccountIdentityCallCreds) cachedRequestMetadata(attemptPreemptiveRefresh bool) (map[string]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cachedRequestMetadataLocked(attemptPreemptiveRefresh)
}

// cachedRequestMetadataLocked returns the cached token if it is valid, or nil
// if it is not. If attemptPreemptiveRefresh is true, it will also trigger a
// background refresh if the token is stale but still valid. It returns the
// cached error if the last fetch failed and the backoff interval has not
// expired.
//
// Returns (nil, nil) if the token is not valid and a new fetch is required. The
// caller is responsible for initiating the fetch or waiting for an in-progress
// fetch to complete.
//
// It must be called with mu locked.
func (c *gcpServiceAccountIdentityCallCreds) cachedRequestMetadataLocked(attemptPreemptiveRefresh bool) (map[string]string, error) {
	if c.token != "" && c.isTokenValidLocked() {
		if attemptPreemptiveRefresh && c.isTokenStaleLocked() && c.fetching == nil {
			c.fetching = make(chan struct{})
			go c.startFetch()
		}

		return map[string]string{"authorization": "Bearer " + c.token}, nil
	}

	if c.lastErr != nil && time.Now().Before(c.nextRetryTime) {
		return nil, c.lastErr
	}

	return nil, nil
}

// RequireTransportSecurity indicates whether the credentials requires
// transport security.
func (c *gcpServiceAccountIdentityCallCreds) RequireTransportSecurity() bool {
	return true
}

// isTokenStaleLocked checks if the token falls within the
// preemptiveRefresh window. It must be called with mu locked.
func (c *gcpServiceAccountIdentityCallCreds) isTokenStaleLocked() bool {
	return c.preemptiveTokenRefresh.Before(time.Now())
}

// isTokenValidLocked checks if the token is not expired yet. It must be called
// with mu locked.
func (c *gcpServiceAccountIdentityCallCreds) isTokenValidLocked() bool {
	return c.tokenExpiry.After(time.Now())
}

// startFetch initiates a token fetch and updates the credential
// state upon completion.
func (c *gcpServiceAccountIdentityCallCreds) startFetch() {
	ctx, cancel := context.WithTimeout(c.ctx, metadataTimeout)
	defer cancel()
	token, exp, err := c.fetchIDToken(ctx, c.audience)

	c.mu.Lock()
	defer c.mu.Unlock()

	close(c.fetching)
	c.fetching = nil
	c.updateStateLocked(token, exp, err)
}

// updateStateLocked updates the credentials local token cache and
// backoff state based on the outcome of a background fetch attempt.
//
// If the fetch succeeded, the cached token is updated, and the backoff timers
// and error are reset. If the fetch failed, the error is cached and the next
// retry time is calculated using the backoff strategy.
//
// It must be called with mu locked.
func (c *gcpServiceAccountIdentityCallCreds) updateStateLocked(token string, exp time.Time, err error) {
	if err != nil {
		c.lastErr = err
		backoffDelay := c.backoff.Backoff(c.retryAttempt)
		c.retryAttempt++
		c.nextRetryTime = time.Now().Add(backoffDelay)
		return
	}
	c.lastErr = nil
	c.retryAttempt = 0
	c.nextRetryTime = time.Time{}
	c.token = token
	// Per gRFC A83, the cached token is considered invalid 30 seconds before its
	// actual expiration time to accommodate for clock skew.
	c.tokenExpiry = exp.Add(-30 * time.Second)
	c.preemptiveTokenRefresh = c.tokenExpiry.Add(-preemptiveRefresh)
}
