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

// New declares a provider for the API endpoint the daemon serves at host.
func New(host string) extensions.Extension {
	return extensions.New(extensions.Declaration{
		ID: "org.mobyproject.daemon.api.v0",
		Providers: []extensions.Provider{
			daemonapiv0.Point.Provide(endpoint{host: host}),
		},
	})
}

type endpoint struct {
	host string
}

func (e endpoint) Get(context.Context, *daemonapiv0.GetRequest) (*daemonapiv0.GetResponse, error) {
	return &daemonapiv0.GetResponse{Host: e.host}, nil
}
