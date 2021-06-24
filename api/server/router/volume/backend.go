package volume // import "github.com/docker/docker/api/server/router/volume"

import (
	"context"

	"github.com/docker/docker/api/types" // TODO return types need to be refactored into pkg
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/volume/service/opts"
)

// Backend is the methods that need to be implemented to provide
// volume specific functionality
type Backend interface {
	List(ctx context.Context, filter filters.Args) ([]*types.Volume, []string, error)
	Get(ctx context.Context, name string, opts ...opts.GetOption) (*types.Volume, error)
	Create(ctx context.Context, name, driverName string, opts ...opts.CreateOption) (*types.Volume, error)
	Remove(ctx context.Context, name string, opts ...opts.RemoveOption) error
	Prune(ctx context.Context, pruneFilters filters.Args) (*types.VolumesPruneReport, error)
	LocalVolumesSize(ctx context.Context) ([]*types.Volume, error)
}
