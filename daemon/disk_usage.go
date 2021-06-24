package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/server/router/system"
	"github.com/docker/docker/api/types"
	"golang.org/x/sync/errgroup"
)

func (daemon *Daemon) containerDiskUsage(ctx context.Context) ([]*types.Container, error) {
	containers, err := daemon.Containers(&types.ContainerListOptions{
		Size: true,
		All:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve container list: %v", err)
	}
	return containers, nil
}

// ContainerDiskUsage returns information about container data disk usage.
func (daemon *Daemon) ContainerDiskUsage(ctx context.Context) ([]*types.Container, error) {
	v, err := daemon.containerDiskUsageSingleton.Do(ctx)
	if err != nil {
		return nil, err
	}
	return v.([]*types.Container), nil
}

// SystemDiskUsage returns information about the daemon data disk usage.
// The caller must not mutate returned values, since
func (daemon *Daemon) SystemDiskUsage(ctx context.Context, opts system.DiskUsageOptions) (*types.DiskUsage, error) {
	eg, ctx := errgroup.WithContext(ctx)

	var containers []*types.Container
	if opts.Containers {
		eg.Go(func() error {
			var err error
			containers, err = daemon.ContainerDiskUsage(ctx)
			return err
		})
	}

	var (
		images     []*types.ImageSummary
		layersSize int64
	)
	if opts.Images {
		eg.Go(func() error {
			var err error
			images, err = daemon.imageService.ImageDiskUsage(ctx)
			return err
		})
		eg.Go(func() error {
			var err error
			layersSize, err = daemon.imageService.LayerDiskUsage(ctx)
			return err
		})
	}

	var volumes []*types.Volume
	if opts.Volumes {
		eg.Go(func() error {
			var err error
			volumes, err = daemon.volumes.LocalVolumesSize(ctx)
			return err
		})
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}
	return &types.DiskUsage{
		LayersSize: layersSize,
		Containers: containers,
		Volumes:    volumes,
		Images:     images,
	}, nil
}
