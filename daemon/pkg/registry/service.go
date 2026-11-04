package registry

import (
	"context"
	"crypto/tls"
	"errors"
	"net/url"
	"strings"
	"sync"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/registry"
)

// Service is a registry service. It tracks configuration data such as a list
// of mirrors.
type Service struct {
	config *serviceConfig
	mu     sync.RWMutex
}

// NewService returns a new instance of [Service] ready to be installed into
// an engine.
func NewService(options ServiceOptions) (*Service, error) {
	config, err := newServiceConfig(options)
	if err != nil {
		return nil, err
	}

	return &Service{config: config}, err
}

// ServiceConfig returns a copy of the public registry service's configuration.
func (s *Service) ServiceConfig() *registry.ServiceConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config.copy()
}

// ReplaceConfig prepares a transaction which will atomically replace the
// registry service's configuration when the returned commit function is called.
func (s *Service) ReplaceConfig(options ServiceOptions) (commit func(), _ error) {
	config, err := newServiceConfig(options)
	if err != nil {
		return nil, err
	}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.config = config
	}, nil
}

// Auth contacts the public registry with the provided credentials,
// and returns OK if authentication was successful.
// It can be used to verify the validity of a client's credentials.
func (s *Service) Auth(ctx context.Context, authConfig *registry.AuthConfig, userAgent string) (token string, _ error) {
	registryHostName := IndexHostname

	if authConfig.ServerAddress != "" {
		var err error
		registryHostName, err = parseRegistryHostName(authConfig.ServerAddress)
		if err != nil {
			return "", err
		}
	}

	// Lookup endpoints for authentication but exclude mirrors to prevent
	// sending credentials of the upstream registry to a mirror.
	s.mu.RLock()
	endpoints, err := s.lookupV2Endpoints(ctx, registryHostName, false)
	s.mu.RUnlock()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		return "", invalidParam(err)
	}

	var lastErr error
	for _, endpoint := range endpoints {
		authToken, err := loginV2(ctx, authConfig, endpoint, userAgent)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || cerrdefs.IsUnauthorized(err) {
				// Failed to authenticate; don't continue with (non-TLS) endpoints.
				return "", err
			}
			// Try next endpoint
			log.G(ctx).WithFields(log.Fields{
				"error":    err,
				"endpoint": endpoint,
			}).Infof("Error logging in to endpoint, trying next endpoint")
			lastErr = err
			continue
		}

		return authToken, nil
	}

	return "", lastErr
}

func parseRegistryHostName(serverAddress string) (string, error) {
	if !strings.Contains(serverAddress, "://") {
		// url.Parse treats input without a scheme as path, not hostname.
		serverAddress = "https://" + serverAddress
	}
	u, err := url.Parse(serverAddress)
	if err != nil {
		return "", invalidParamWrapf(err, `invalid server address: unable to parse`)
	}
	switch u.Scheme {
	case "http", "https":
		// all good
		return u.Host, nil
	default:
		return "", invalidParamf(`invalid server address %q: unsupported URL scheme %q: must be "http" or "https"`, serverAddress, u.Scheme)
	}
}

// ResolveAuthConfig looks up authentication for the given reference from the
// given authConfigs.
//
// IMPORTANT: This function is for internal use and should not be used by external projects.
func (s *Service) ResolveAuthConfig(authConfigs map[string]registry.AuthConfig, ref reference.Named) registry.AuthConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Simplified version of "newIndexInfo" without handling of insecure
	// registries and mirrors, as we don't need that information to resolve
	// the auth-config.
	indexName := normalizeIndexName(reference.Domain(ref))
	registryInfo, ok := s.config.IndexConfigs[indexName]
	if !ok {
		registryInfo = &registry.IndexInfo{Name: indexName}
	}
	return resolveAuthConfig(authConfigs, registryInfo)
}

// APIEndpoint represents a remote API endpoint
type APIEndpoint struct {
	Mirror    bool
	URL       *url.URL
	TLSConfig *tls.Config
}

// LookupPullEndpoints creates a list of v2 endpoints to try to pull from, in order of preference.
// It gives preference to mirrors over the actual registry, and HTTPS over plain HTTP.
func (s *Service) LookupPullEndpoints(hostname string) ([]APIEndpoint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.lookupV2Endpoints(context.TODO(), hostname, true)
}

// LookupPushEndpoints creates a list of v2 endpoints to try to push to, in order of preference.
// It gives preference to HTTPS over plain HTTP. Mirrors are not included.
func (s *Service) LookupPushEndpoints(hostname string) ([]APIEndpoint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.lookupV2Endpoints(context.TODO(), hostname, false)
}

// IsInsecureRegistry returns true if the registry at given host is configured as
// insecure registry.
func (s *Service) IsInsecureRegistry(host string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.config.isSecureIndex(host)
}
