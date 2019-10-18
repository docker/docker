package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
)

// SystemDiskUsage returns information about the daemon data disk usage
func (daemon *Daemon) SystemDiskUsage(ctx context.Context) (*types.DiskUsage, error) {
	if !atomic.CompareAndSwapInt32(&daemon.diskUsageRunning, 0, 1) {
		return nil, fmt.Errorf("a disk usage operation is already running")
	}
	defer atomic.StoreInt32(&daemon.diskUsageRunning, 0)

	// Retrieve container list
	allContainers, err := daemon.Containers(ctx, &types.ContainerListOptions{
		Size: true,
		All:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve container list: %v", err)
	}

	// Get all top images with extra attributes
	allImages, err := daemon.imageService.Images(ctx, filters.NewArgs(), false, true)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve image list: %v", err)
	}

	localVolumes, err := daemon.volumes.LocalVolumesSize(ctx)
	if err != nil {
		return nil, err
	}

	allLayersSize, err := daemon.imageService.LayerDiskUsage(ctx)
	if err != nil {
		return nil, err
	}

	return &types.DiskUsage{
		LayersSize: allLayersSize,
		Containers: allContainers,
		Volumes:    localVolumes,
		Images:     allImages,
	}, nil
}
