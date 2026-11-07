package containerd

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/version"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/distribution/reference"
	registrytypes "github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/v2/daemon/pkg/registry"
	"github.com/moby/moby/v2/dockerversion"
	"github.com/moby/moby/v2/pkg/useragent"
)

// credsPolicy controls what happens when a request's registry host does not
// match the host the credentials were configured for.
type credsPolicy int

const (
	// credsLenient sends no credentials to non-matching hosts; used for
	// pulls, which may contact mirrors or fallback hosts.
	credsLenient credsPolicy = iota

	// credsStrict fails instead of proceeding anonymously; used for
	// pushes, which only ever contact the target registry.
	credsStrict
)

// resolverAuth carries the parameters shared between the resolver's host
// lookup and the authorizers it creates for each host.
type resolverAuth struct {
	authConfig registrytypes.AuthConfig
	ref        reference.Named
	policy     credsPolicy
	userAgent  string
}

func (i *ImageService) newResolverFromAuthConfig(ctx context.Context, authConfig *registrytypes.AuthConfig, ref reference.Named, metaHeaders http.Header, policy credsPolicy) (remotes.Resolver, docker.StatusTracker) {
	tracker := docker.NewInMemoryTracker()

	headers := http.Header{}
	if metaHeaders != nil {
		headers = metaHeaders.Clone()
	}
	headers.Set("User-Agent", dockerversion.DockerUserAgent(ctx, useragent.VersionInfo{Name: "containerd-client", Version: version.Version}, useragent.VersionInfo{Name: "storage-driver", Version: i.snapshotter}))

	hosts := i.registryHosts
	if authConfig != nil {
		auth := resolverAuth{
			authConfig: *authConfig,
			ref:        ref,
			policy:     policy,
			userAgent:  headers.Get("User-Agent"),
		}
		var mu sync.Mutex
		// Keep each host, client, and authorizer together so registry and token
		// requests use matching transport settings across resolver phases.
		hostsByName := map[string][]docker.RegistryHost{}
		hosts = func(name string) ([]docker.RegistryHost, error) {
			mu.Lock()
			defer mu.Unlock()

			if hosts, ok := hostsByName[name]; ok {
				return hosts, nil
			}
			hosts, err := hostsWrapper(ctx, i.registryHosts, auth, name)
			if err != nil {
				return nil, err
			}
			hostsByName[name] = hosts
			return hosts, nil
		}
	}

	return docker.NewResolver(docker.ResolverOptions{
		Hosts:   hosts,
		Tracker: tracker,
		Headers: headers,
	}), tracker
}

func hostsWrapper(ctx context.Context, hostsFn docker.RegistryHosts, auth resolverAuth, name string) ([]docker.RegistryHost, error) {
	hosts, err := hostsFn(name)
	if err != nil {
		return nil, err
	}
	cfgHost := credHost(auth.authConfig, auth.ref)
	for i := range hosts {
		hosts[i].Authorizer = authorizerFromAuthConfig(auth, hosts[i].Client)
	}
	primeAuthChallenge(ctx, hosts, auth, cfgHost)
	return hosts, nil
}

// credHost returns the registry host the credentials belong to.
func credHost(authConfig registrytypes.AuthConfig, ref reference.Named) string {
	cfgHost := registry.ConvertToHostname(authConfig.ServerAddress)
	if cfgHost == "" {
		cfgHost = reference.Domain(ref)
	}
	if cfgHost == registry.IndexHostname || cfgHost == registry.IndexName {
		cfgHost = registry.DefaultRegistryHost
	}
	return cfgHost
}

// normalizeRegistryHost lower-cases a registry host and strips default
// ports (:443/:80), so credential matching tolerates cosmetic differences
// between a configured server address and the actual request host.
func normalizeRegistryHost(host string) string {
	host = strings.ToLower(host)
	if h, port, err := net.SplitHostPort(host); err == nil && (port == "443" || port == "80") {
		if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
			// Re-bracket bare IPv6 addresses.
			h = "[" + h + "]"
		}
		return h
	}
	return host
}

func hostMatches(cfgHost, host string) bool {
	return normalizeRegistryHost(cfgHost) == normalizeRegistryHost(host)
}

// primeAuthChallenge probes the credential-matching registry host for an
// auth challenge and registers it with that host's authorizer before the
// first request. Without it, requests are only authenticated after a 401,
// and registries answering unauthenticated requests with 403 instead (e.g.
// Artifactory blob uploads) fail the push without a retry. Mirrors what the
// legacy push path does through registry.PingV2Registry.
func primeAuthChallenge(ctx context.Context, hosts []docker.RegistryHost, auth resolverAuth, cfgHost string) {
	if auth.authConfig.Password == "" && auth.authConfig.IdentityToken == "" {
		// Nothing to prime with; registry tokens are attached
		// preemptively by bearerAuthorizer and need no challenge.
		return
	}
	for _, host := range hosts {
		if !hostMatches(cfgHost, host.Host) {
			// Never probe hosts the credentials would not be sent to.
			continue
		}
		if host.Client == nil || host.Authorizer == nil {
			return
		}

		pingPath := "/" + strings.Trim(host.Path, "/") + "/"
		if pingPath == "//" {
			pingPath = "/v2/"
		}
		pingURL := url.URL{Scheme: host.Scheme, Host: host.Host, Path: pingPath}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pingURL.String(), http.NoBody)
		if err != nil {
			log.G(ctx).WithError(err).Debug("failed to create registry auth probe request")
			return
		}
		if auth.userAgent != "" {
			req.Header.Set("User-Agent", auth.userAgent)
		}
		resp, err := host.Client.Do(req)
		if err != nil {
			log.G(ctx).WithError(err).WithField("host", host.Host).Debug("failed to probe registry for auth challenge")
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusUnauthorized {
			return
		}
		if err := host.Authorizer.AddResponses(ctx, []*http.Response{resp}); err != nil {
			log.G(ctx).WithError(err).WithField("host", host.Host).Debug("failed to register auth challenge from registry probe")
		}
		return
	}
}

func authorizerFromAuthConfig(auth resolverAuth, client *http.Client) docker.Authorizer {
	cfgHost := credHost(auth.authConfig, auth.ref)
	authConfig := auth.authConfig

	if authConfig.RegistryToken != "" {
		return &bearerAuthorizer{
			host:   cfgHost,
			bearer: authConfig.RegistryToken,
			strict: auth.policy == credsStrict,
		}
	}

	opts := []docker.AuthorizerOpt{
		docker.WithAuthCreds(func(host string) (string, string, error) {
			if !hostMatches(cfgHost, host) {
				if auth.policy == credsStrict && (authConfig.Password != "" || authConfig.IdentityToken != "") {
					return "", "", fmt.Errorf("credentials configured for registry %q do not match the registry host %q; refusing to continue without credentials", cfgHost, host)
				}
				log.G(context.TODO()).WithFields(log.Fields{
					"host":    host,
					"cfgHost": cfgHost,
				}).Warn("Host doesn't match")
				return "", "", nil
			}
			if authConfig.IdentityToken != "" {
				return "", authConfig.IdentityToken, nil
			}
			return authConfig.Username, authConfig.Password, nil
		}),
	}
	if client != nil {
		opts = append(opts, docker.WithAuthClient(client))
	}
	return docker.NewDockerAuthorizer(opts...)
}

type bearerAuthorizer struct {
	host   string
	bearer string
	strict bool
}

func (a *bearerAuthorizer) Authorize(ctx context.Context, req *http.Request) error {
	if !hostMatches(a.host, req.Host) {
		if a.strict {
			return fmt.Errorf("registry token configured for host %q does not match the registry host %q; refusing to continue without credentials", a.host, req.Host)
		}
		log.G(ctx).WithFields(log.Fields{
			"host":    req.Host,
			"cfgHost": a.host,
		}).Warn("Host doesn't match for bearer token")
		return nil
	}

	req.Header.Set("Authorization", "Bearer "+a.bearer)

	return nil
}

func (a *bearerAuthorizer) AddResponses(context.Context, []*http.Response) error {
	// Return not implemented to prevent retry of the request when bearer did not succeed
	return cerrdefs.ErrNotImplemented
}
