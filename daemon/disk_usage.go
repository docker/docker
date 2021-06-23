package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/docker/docker/api/server/router/system"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
)

// SystemDiskUsage returns information about the daemon data disk usage
func (daemon *Daemon) SystemDiskUsage(ctx context.Context, opts system.DiskUsageOptions) (*types.DiskUsage, error) {
	if !atomic.CompareAndSwapInt32(&daemon.diskUsageRunning, 0, 1) {
		return nil, fmt.Errorf("a disk usage operation is already running")
	}
	defer atomic.StoreInt32(&daemon.diskUsageRunning, 0)

	var err error

	var containers []*types.Container
	if opts.Containers {
		// Retrieve container list
		containers, err = daemon.Containers(&types.ContainerListOptions{
			Size: true,
			All:  true,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve container list: %v", err)
		}
	}

	var (
		images     []*types.ImageSummary
		layersSize int64
	)
	if opts.Images {
		// Get all top images with extra attributes
		images, err = daemon.imageService.Images(filters.NewArgs(), false, true)
		if err != nil {
			return nil, fmt.Errorf("failed to retrieve image list: %v", err)
		}

		layersSize, err = daemon.imageService.LayerDiskUsage(ctx)
		if err != nil {
			return nil, err
		}
	}

	var volumes []*types.Volume
	if opts.Volumes {
		volumes, err = daemon.volumes.LocalVolumesSize(ctx)
		if err != nil {
			return nil, err
		}
	}
	return &types.DiskUsage{
		LayersSize: layersSize,
		Containers: containers,
		Volumes:    volumes,
		Images:     images,
	}, nil
}
