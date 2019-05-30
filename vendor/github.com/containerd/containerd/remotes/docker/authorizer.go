/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context/ctxhttp"
)

type dockerAuthorizer struct {
	credentials func(string) (string, string, error)

	client *http.Client
	mu     sync.Mutex

	auth map[string]string
}

// NewAuthorizer creates a Docker authorizer using the provided function to
// get credentials for the token server or basic auth.
func NewAuthorizer(client *http.Client, f func(string) (string, string, error)) Authorizer {
	if client == nil {
		client = http.DefaultClient
	}
	return &dockerAuthorizer{
		credentials: f,
		client:      client,
		auth:        map[string]string{},
	}
}

func (a *dockerAuthorizer) Authorize(ctx context.Context, req *http.Request) error {
	// TODO: Lookup matching challenge and scope rather than just host
	if auth := a.getAuth(req.URL.Host); auth != "" {
		req.Header.Set("Authorization", auth)
	}

	return nil
}

func (a *dockerAuthorizer) AddResponses(ctx context.Context, responses []*http.Response) error {
	last := responses[len(responses)-1]
	host := last.Request.URL.Host
	for _, c := range parseAuthHeader(last.Header) {
		if c.scheme == bearerAuth {
			if err := invalidAuthorization(c, responses); err != nil {
				// TODO: Clear token
				a.setAuth(host, "")
				return err
			}

			// TODO(dmcg): Store challenge, not token
			// Move token fetching to authorize
			return a.setTokenAuth(ctx, host, c.parameters)
		} else if c.scheme == basicAuth && a.credentials != nil {
			// TODO: Resolve credentials on authorize
			username, secret, err := a.credentials(host)
			if err != nil {
				return err
			}
			if username != "" && secret != "" {
				auth := username + ":" + secret
				a.setAuth(host, fmt.Sprintf("Basic %s", base64.StdEncoding.EncodeToString([]byte(auth))))
				return nil
			}
		}
	}

	return errors.Wrap(errdefs.ErrNotImplemented, "failed to find supported auth scheme")
}

func (a *dockerAuthorizer) getAuth(host string) string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.auth[host]
}

func (a *dockerAuthorizer) setAuth(host string, auth string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	changed := a.auth[host] != auth
	a.auth[host] = auth

	return changed
}

func (a *dockerAuthorizer) setTokenAuth(ctx context.Context, host string, params map[string]string) error {
	realm, ok := params["realm"]
	if !ok {
		return errors.New("no realm specified for token auth challenge")
	}

	realmURL, err := url.Parse(realm)
	if err != nil {
		return errors.Wrap(err, "invalid token auth challenge realm")
	}

	to := tokenOptions{
		realm:   realmURL.String(),
		service: params["service"],
	}

	to.scopes = getTokenScopes(ctx, params)
	if len(to.scopes) == 0 {
		return errors.Errorf("no scope specified for token auth challenge")
	}

	if a.credentials != nil {
		to.username, to.secret, err = a.credentials(host)
		if err != nil {
			return err
		}
	}

	var token string
	if to.secret != "" {
		// Credential information is provided, use oauth POST endpoint
		token, err = a.fetchTokenWithOAuth(ctx, to)
		if err != nil {
			return errors.Wrap(err, "failed to fetch oauth token")
		}
	} else {
		// Do request anonymously
		token, err = a.fetchToken(ctx, to)
		if err != nil {
			return errors.Wrap(err, "failed to fetch anonymous token")
		}
	}
	a.setAuth(host, fmt.Sprintf("Bearer %s", token))

	return nil
}

type tokenOptions struct {
	realm    string
	service  string
	scopes   []string
	username string
	secret   string
}

type postTokenResponse struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresIn    int       `json:"expires_in"`
	IssuedAt     time.Time `json:"issued_at"`
	Scope        string    `json:"scope"`
}

func (a *dockerAuthorizer) fetchTokenWithOAuth(ctx context.Context, to tokenOptions) (string, error) {
	form := url.Values{}
	form.Set("scope", strings.Join(to.scopes, " "))
	form.Set("service", to.service)
	// TODO: Allow setting client_id
	form.Set("client_id", "containerd-client")

	if to.username == "" {
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", to.secret)
	} else {
		form.Set("grant_type", "password")
		form.Set("username", to.username)
		form.Set("password", to.secret)
	}

	resp, err := ctxhttp.Post(
		ctx, a.client, to.realm,
		"application/x-www-form-urlencoded; charset=utf-8",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// Registries without support for POST may return 404 for POST /v2/token.
	// As of September 2017, GCR is known to return 404.
	// As of February 2018, JFrog Artifactory is known to return 401.
	if (resp.StatusCode == 405 && to.username != "") || resp.StatusCode == 404 || resp.StatusCode == 401 {
		return a.fetchToken(ctx, to)
	} else if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		b, _ := ioutil.ReadAll(io.LimitReader(resp.Body, 64000)) // 64KB
		log.G(ctx).WithFields(logrus.Fields{
			"status": resp.Status,
			"body":   string(b),
		}).Debugf("token request failed")
		// TODO: handle error body and write debug output
		return "", errors.Errorf("unexpected status: %s", resp.Status)
	}

	decoder := json.NewDecoder(resp.Body)

	var tr postTokenResponse
	if err = decoder.Decode(&tr); err != nil {
		return "", fmt.Errorf("unable to decode token response: %s", err)
	}

	return tr.AccessToken, nil
}

type getTokenResponse struct {
	Token        string    `json:"token"`
	AccessToken  string    `json:"access_token"`
	ExpiresIn    int       `json:"expires_in"`
	IssuedAt     time.Time `json:"issued_at"`
	RefreshToken string    `json:"refresh_token"`
}

// getToken fetches a token using a GET request
func (a *dockerAuthorizer) fetchToken(ctx context.Context, to tokenOptions) (string, error) {
	req, err := http.NewRequest("GET", to.realm, nil)
	if err != nil {
		return "", err
	}

	reqParams := req.URL.Query()

	if to.service != "" {
		reqParams.Add("service", to.service)
	}

	for _, scope := range to.scopes {
		reqParams.Add("scope", scope)
	}

	if to.secret != "" {
		req.SetBasicAuth(to.username, to.secret)
	}

	req.URL.RawQuery = reqParams.Encode()

	resp, err := ctxhttp.Do(ctx, a.client, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		// TODO: handle error body and write debug output
		return "", errors.Errorf("unexpected status: %s", resp.Status)
	}

	decoder := json.NewDecoder(resp.Body)

	var tr getTokenResponse
	if err = decoder.Decode(&tr); err != nil {
		return "", fmt.Errorf("unable to decode token response: %s", err)
	}

	// `access_token` is equivalent to `token` and if both are specified
	// the choice is undefined.  Canonicalize `access_token` by sticking
	// things in `token`.
	if tr.AccessToken != "" {
		tr.Token = tr.AccessToken
	}

	if tr.Token == "" {
		return "", ErrNoToken
	}

	return tr.Token, nil
}

func invalidAuthorization(c challenge, responses []*http.Response) error {
	errStr := c.parameters["error"]
	if errStr == "" {
		return nil
	}

	n := len(responses)
	if n == 1 || (n > 1 && !sameRequest(responses[n-2].Request, responses[n-1].Request)) {
		return nil
	}

	return errors.Wrapf(ErrInvalidAuthorization, "server message: %s", errStr)
}

func sameRequest(r1, r2 *http.Request) bool {
	if r1.Method != r2.Method {
		return false
	}
	if *r1.URL != *r2.URL {
		return false
	}
	return true
}
