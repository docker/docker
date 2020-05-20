package plugin

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"

	reference "github.com/containerd/containerd/reference/docker"
	"github.com/containerd/containerd/remotes"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/dockerversion"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// scope builds the correct auth scope for the registry client to authorize against
// By default the client currently only does a "repository:" scope with out a classifier, e.g. "(plugin)"
// Without this, the client will not be able to authorize the request
func scope(ref reference.Named, push bool) string {
	scope := "repository(plugin):" + reference.Path(reference.TrimNamed(ref)) + ":pull"
	if push {
		scope += ",push"
	}
	return scope
}

func (pm *Manager) newResolver(ctx context.Context, tracker docker.StatusTracker, auth *types.AuthConfig, headers http.Header, httpFallback bool) (remotes.Resolver, error) {
	if headers == nil {
		headers = http.Header{}
	}
	headers.Add("User-Agent", dockerversion.DockerUserAgent(ctx))

	return docker.NewResolver(docker.ResolverOptions{
		Tracker: tracker,
		Headers: headers,
		Hosts:   pm.registryHostsFn(auth, httpFallback),
	}), nil
}

func registryHTTPClient(config *tls.Config) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSClientConfig:     config,
			TLSHandshakeTimeout: 10 * time.Second,
			IdleConnTimeout:     30 * time.Second,
		},
	}
}

func (pm *Manager) registryHostsFn(auth *types.AuthConfig, httpFallback bool) docker.RegistryHosts {
	return func(hostname string) ([]docker.RegistryHost, error) {
		eps, err := pm.config.RegistryService.LookupPullEndpoints(hostname)
		if err != nil {
			return nil, errors.Wrapf(err, "error resolving repository for %s", hostname)
		}

		hosts := make([]docker.RegistryHost, 0, len(eps))

		for _, ep := range eps {
			// forced http fallback is used only for push since the containerd pusher only ever uses the first host we
			// pass to it.
			// So it is the callers responsibility to retry with this flag set.
			if httpFallback && ep.URL.Scheme != "http" {
				logrus.WithField("registryHost", hostname).WithField("endpoint", ep).Debugf("Skipping non-http endpoint")
				continue
			}

			caps := docker.HostCapabilityPull | docker.HostCapabilityResolve
			if !ep.Mirror {
				caps = caps | docker.HostCapabilityPush
			}

			host, err := docker.DefaultHost(ep.URL.Host)
			if err != nil {
				return nil, err
			}

			client := registryHTTPClient(ep.TLSConfig)
			hosts = append(hosts, docker.RegistryHost{
				Host:         host,
				Scheme:       ep.URL.Scheme,
				Client:       client,
				Path:         "/v2",
				Capabilities: caps,
				Authorizer: docker.NewDockerAuthorizer(
					docker.WithAuthClient(client),
					docker.WithAuthCreds(func(_ string) (string, string, error) {
						if auth.IdentityToken != "" {
							return "", auth.IdentityToken, nil
						}
						return auth.Username, auth.Password, nil
					}),
				),
			})
		}
		logrus.WithField("registryHost", hostname).WithField("hosts", hosts).Debug("Resolved registry hosts")

		return hosts, nil
	}
}
