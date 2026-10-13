package daemonapiv0

import (
	"context"
	"fmt"

	"github.com/moby/moby/client"
)

// NewClient returns an Engine API client connected to the endpoint api
// reports.
// Resolve api with Point.Single during initialization, after declaring
// Point.Dependency().
// opts are applied after the endpoint, so they may override it.
//
// The daemon serves the API only after every extension has initialized, so do
// not make API calls from Init; they would block daemon startup.
// The caller owns the returned client and must close it.
func NewClient(ctx context.Context, api DaemonAPI, opts ...client.Opt) (*client.Client, error) {
	resp, err := api.Get(ctx, &GetRequest{})
	if err != nil {
		return nil, fmt.Errorf("get daemon API endpoint: %w", err)
	}
	return client.New(append([]client.Opt{client.WithHost(resp.Host)}, opts...)...)
}
