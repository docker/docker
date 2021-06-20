package volume // import "github.com/docker/docker/api/server/router/volume"

import (
	"context"

	"github.com/docker/docker/volume/service/opts"
	// TODO return types need to be refactored into pkg
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
)

// Backend is the methods that need to be implemented to provide
// volume specific functionality
type Backend interface {
	List(ctx context.Context, filter filters.Args) ([]*types.Volume, []string, error)
	Get(ctx context.Context, name string, opts ...opts.GetOption) (*types.Volume, error)
	Create(ctx context.Context, name, driverName string, opts ...opts.CreateOption) (*types.Volume, error)
	Remove(ctx context.Context, name string, opts ...opts.RemoveOption) error
	Prune(ctx context.Context, pruneFilters filters.Args) (*types.VolumesPruneReport, error)
}

// ClusterBackend is the backend used for Swarm Cluster Volumes. Regular
// volumes go through the volume service, but to avoid across-dependency
// between the cluster package and the volume package, we simply provide two
// backends here.
type ClusterBackend interface {
	GetVolume(nameOrId string) (types.Volume, error)
	GetVolumes(options types.VolumeListOptions) ([]*types.Volume, error)
	CreateVolume(volume volume.VolumeCreateBody) (*types.Volume, error)
	RemoveVolume(nameOrId string) error
	UpdateVolume(nameOrId string, version uint64, volume volume.VolumeUpdateBody) error
}
