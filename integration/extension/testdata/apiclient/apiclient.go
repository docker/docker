// Package apiclient is the daemon API client integration-test fixture.
package apiclient

import (
	"context"

	"github.com/moby/extensions"
	greeterv0 "github.com/moby/extensions/example/greeter/v0"
	greeterpb "github.com/moby/extensions/example/greeter/v0/protogen"
	servicev0 "github.com/moby/extensions/extpoints/service/v0"
	"github.com/moby/moby/client"
	daemonapiv0 "github.com/moby/moby/v2/extpoints/daemonapi/v0"
	servicegrpcv0 "github.com/moby/moby/v2/extpoints/servicegrpc/v0"
	"google.golang.org/grpc"
)

// ID is the extension id and binary name.
const ID = "org.mobyproject.example.apiclient.v1"

type extension struct {
	api *client.Client
}

// Greet replies with the ID of the daemon reached through the API client.
func (e *extension) Greet(ctx context.Context, _ *greeterv0.HelloRequest) (*greeterv0.HelloReply, error) {
	info, err := e.api.Info(ctx, client.InfoOptions{})
	if err != nil {
		return nil, err
	}
	return &greeterv0.HelloReply{Message: info.Info.ID}, nil
}

func (e *extension) RegisterServices(r grpc.ServiceRegistrar) {
	greeterpb.ServerPoint.Register(r, e)
}

func (e *extension) Declaration() extensions.Declaration {
	return extensions.Declaration{
		ID: ID,
		Providers: []extensions.Provider{
			servicegrpcv0.Point.Provide(e),
			servicev0.Offer(servicegrpcv0.Point),
		},
		Dependencies: []extensions.Dependency{
			daemonapiv0.Point.Dependency(),
		},
		Init: func(_ context.Context, _ extensions.Config, resolver extensions.Resolver) error {
			provider, err := daemonapiv0.Point.Single(resolver)
			if err != nil {
				return err
			}
			api, err := daemonapiv0.NewClient(provider)
			if err != nil {
				return err
			}
			e.api = api
			return nil
		},
		Shutdown: func(context.Context) error {
			return e.api.Close()
		},
	}
}

// Extension publishes a greeter service backed by the daemon API.
var Extension extensions.Extension = &extension{}
