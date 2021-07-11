package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
)

// SystemDiskUsage returns information about the daemon data disk usage
func (daemon *Daemon) SystemDiskUsage(ctx context.Context, containers, images, volumes, layerSize bool) (
	*types.DiskUsage, error) {

	if !atomic.CompareAndSwapInt32(&daemon.diskUsageRunning, 0, 1) {
		return nil, fmt.Errorf("a disk usage operation is already running")
	}
	defer atomic.StoreInt32(&daemon.diskUsageRunning, 0)

	var err error
	usage := &types.DiskUsage{}

	if containers {
		// Retrieve container list
		usage.Containers, err = daemon.Containers(&types.ContainerListOptions{
			Size: true,
			All:  true,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve container list: %v", err)
		}
	}

	if images {
		// Get all top images with extra attributes
		usage.Images, err = daemon.imageService.Images(filters.NewArgs(), false, true)
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve image list: %v", err)
		}
	}

	if volumes {
		usage.Volumes, err = daemon.volumes.LocalVolumesSize(ctx)
		if err != nil {
			return nil, err
		}
	}

	if layerSize {
		usage.LayersSize, err = daemon.imageService.LayerDiskUsage(ctx)
		if err != nil {
			return nil, err
		}
	}

	return usage, nil
}
