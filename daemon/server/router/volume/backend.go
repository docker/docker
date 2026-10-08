package volume

import (
	"context"

	"github.com/moby/moby/v2/daemon/internal/pruneprogress"

	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/v2/daemon/internal/filters"
	"github.com/moby/moby/v2/daemon/server/volumebackend"
	"github.com/moby/moby/v2/daemon/volume/service/opts"
)

// Backend is the methods that need to be implemented to provide
// volume specific functionality
type Backend interface {
	List(ctx context.Context, filter filters.Args) ([]volume.Volume, []string, error)
	Get(ctx context.Context, name string, opts ...opts.GetOption) (*volume.Volume, error)
	Create(ctx context.Context, name, driverName string, opts ...opts.CreateOption) (*volume.Volume, error)
	Remove(ctx context.Context, name string, opts ...opts.RemoveOption) error
	Prune(ctx context.Context, opts pruneprogress.Options) (*volume.PruneReport, error)
}

// ClusterBackend is the backend used for Swarm Cluster Volumes. Regular
// volumes go through the volume service, but to avoid across-dependency
// between the cluster package and the volume package, we simply provide two
// backends here.
type ClusterBackend interface {
	GetVolume(nameOrID string) (volume.Volume, error)
	GetVolumes(options volumebackend.ListOptions) ([]volume.Volume, error)
	CreateVolume(volume volume.CreateRequest) (*volume.Volume, error)
	RemoveVolume(nameOrID string, force bool) error
	UpdateVolume(nameOrID string, version uint64, volume volumebackend.UpdateOptions) error
	IsManager() bool
}
