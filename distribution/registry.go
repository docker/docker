package distribution // import "github.com/docker/docker/distribution"

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/docker/distribution"
	"github.com/docker/distribution/manifest/schema2"
	"github.com/docker/distribution/reference"
	"github.com/docker/distribution/registry/client"
	"github.com/docker/distribution/registry/client/auth"
	"github.com/docker/distribution/registry/client/transport"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/dockerversion"
	"github.com/docker/docker/registry"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ImageTypes represents the schema2 config types for images
var ImageTypes = []string{
	schema2.MediaTypeImageConfig,
	ocispec.MediaTypeImageConfig,
	// Handle unexpected values from https://github.com/docker/distribution/issues/1621
	// (see also https://github.com/docker/docker/issues/22378,
	// https://github.com/docker/docker/issues/30083)
	"application/octet-stream",
	"application/json",
	"text/html",
	// Treat defaulted values as images, newer types cannot be implied
	"",
}

// PluginTypes represents the schema2 config types for plugins
var PluginTypes = []string{
	schema2.MediaTypePluginConfig,
}

var mediaTypeClasses map[string]string

func init() {
	// initialize media type classes with all know types for
	// plugin
	mediaTypeClasses = map[string]string{}
	for _, t := range ImageTypes {
		mediaTypeClasses[t] = "image"
	}
	for _, t := range PluginTypes {
		mediaTypeClasses[t] = "plugin"
	}
}

// NewV2Repository returns a repository (v2 only). It creates an HTTP transport
// providing timeout settings and authentication support, and also verifies the
// remote API version.
func NewV2Repository(
	ctx context.Context, repoInfo *registry.RepositoryInfo, endpoint registry.APIEndpoint,
	metaHeaders http.Header, authConfig *types.AuthConfig, actions ...string,
) (repo distribution.Repository, err error) {
	repoName := repoInfo.Name.Name()
	// If endpoint does not support CanonicalName, use the RemoteName instead
	if endpoint.TrimHostname {
		repoName = reference.Path(repoInfo.Name)
	}

	direct := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		DualStack: true,
	}

	base := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         direct.DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     endpoint.TLSConfig,
	}

	modifiers := registry.Headers(dockerversion.DockerUserAgent(ctx), metaHeaders)
	authTransport := transport.NewTransport(base, modifiers...)

	challengeManager, err := registry.PingV2Registry(endpoint.URL, authTransport)
	if err != nil {
		transportOK := false
		if responseErr, ok := err.(registry.PingResponseError); ok {
			transportOK = true
			err = responseErr.Err
		}
		return nil, fallbackError{
			err:         err,
			transportOK: transportOK,
		}
	}

	if authConfig.RegistryToken != "" {
		passThruTokenHandler := &existingTokenHandler{token: authConfig.RegistryToken}
		modifiers = append(modifiers, auth.NewAuthorizer(challengeManager, passThruTokenHandler))
	} else {
		scope := auth.RepositoryScope{
			Repository: repoName,
			Actions:    actions,
			Class:      repoInfo.Class,
		}

		creds := registry.NewStaticCredentialStore(authConfig)
		tokenHandlerOptions := auth.TokenHandlerOptions{
			Transport:   authTransport,
			Credentials: creds,
			Scopes:      []auth.Scope{scope},
			ClientID:    registry.AuthClientID,
		}
		tokenHandler := auth.NewTokenHandlerWithOptions(tokenHandlerOptions)
		basicHandler := auth.NewBasicHandler(creds)
		modifiers = append(modifiers, auth.NewAuthorizer(challengeManager, tokenHandler, basicHandler))
	}
	tr := transport.NewTransport(base, modifiers...)

	repoNameRef, err := reference.WithName(repoName)
	if err != nil {
		return nil, fallbackError{
			err:         err,
			transportOK: true,
		}
	}

	repo, err = client.NewRepository(repoNameRef, endpoint.URL.String(), tr)
	if err != nil {
		err = fallbackError{
			err:         err,
			transportOK: true,
		}
	}
	return
}

type existingTokenHandler struct {
	token string
}

func (th *existingTokenHandler) Scheme() string {
	return "bearer"
}

func (th *existingTokenHandler) AuthorizeRequest(req *http.Request, params map[string]string) error {
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", th.token))
	return nil
}
