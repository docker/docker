// Package extensionapi provides the daemon API endpoint to extensions.
//
// The endpoint is a temporary shortcut until extensions can depend on specific
// extension points for the daemon functionality they need.
// Give new functionality for extensions its own extension point instead of
// relying on this endpoint.
package extensionapi

import (
	"context"

	"github.com/moby/extensions"
	daemonapiv0 "github.com/moby/moby/v2/extpoints/daemonapi/v0"
)

// New declares a provider for the API endpoint returned by host.
// host is called each time an extension resolves the endpoint.
func New(host func(context.Context) (string, error)) extensions.Extension {
	return extensions.New(extensions.Declaration{
		ID: "org.mobyproject.daemon.api.v0",
		Providers: []extensions.Provider{
			daemonapiv0.Point.Provide(endpoint{host: host}),
		},
	})
}

type endpoint struct {
	host func(context.Context) (string, error)
}

func (e endpoint) Get(ctx context.Context, _ *daemonapiv0.GetRequest) (*daemonapiv0.GetResponse, error) {
	host, err := e.host(ctx)
	if err != nil {
		return nil, err
	}
	return &daemonapiv0.GetResponse{Host: host}, nil
}
