package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/docker/docker/api/server/router/system"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/errdefs"
)

// SystemDiskUsage returns information about the daemon data disk usage
func (daemon *Daemon) SystemDiskUsage(ctx context.Context, opts system.DiskUsageOptions) (*types.DiskUsage, error) {
	if !atomic.CompareAndSwapInt32(&daemon.diskUsageRunning, 0, 1) {
		return nil, fmt.Errorf("a disk usage operation is already running")
	}
	defer atomic.StoreInt32(&daemon.diskUsageRunning, 0)

	var (
		containers []*types.Container
		images     []*types.ImageSummary
		layersSize int64
		volumes    []*types.Volume
		err        error
	)
	if len(opts.ObjectTypes) == 0 {
		opts.ObjectTypes = map[types.DiskUsageObject]struct{}{
			types.ContainerObject: {},
			types.ImageObject:     {},
			types.VolumeObject:    {},
		}
	}
	for typ := range opts.ObjectTypes {
		switch typ {
		case types.ContainerObject:
			// Retrieve container list
			containers, err = daemon.Containers(&types.ContainerListOptions{
				Size: true,
				All:  true,
			})
			if err != nil {
				return nil, fmt.Errorf("failed to retrieve container list: %v", err)
			}

		case types.ImageObject:
			// Get all top images with extra attributes
			images, err = daemon.imageService.Images(filters.NewArgs(), false, true)
			if err != nil {
				return nil, fmt.Errorf("failed to retrieve image list: %v", err)
			}

			layersSize, err = daemon.imageService.LayerDiskUsage(ctx)
			if err != nil {
				return nil, err
			}

		case types.VolumeObject:
			volumes, err = daemon.volumes.LocalVolumesSize(ctx)
			if err != nil {
				return nil, err
			}

		case types.BuildCacheObject:
			// NOTE: build-cache data usage is currently collected via
			// direct call to buildkit.Builder.DiskUsage in systemRouter.getDiskUsage().

		default:
			return nil, errdefs.InvalidParameter(fmt.Errorf("unknown object type: %s", typ))
		}
	}
	return &types.DiskUsage{
		LayersSize: layersSize,
		Containers: containers,
		Volumes:    volumes,
		Images:     images,
	}, nil
}
