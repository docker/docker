//go:build linux || freebsd

package images

import (
	"context"
	"errors"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/image"
)

// GetLayerFolders returns the layer folders from an image RootFS
func (i *ImageService) GetLayerFolders(img *image.Image, rwLayer container.RWLayer, containerID string) ([]string, error) {
	// Windows specific
	panic("not implemented")
}

// GetContainerLayerSize returns the real size & virtual size of the container.
func (i *ImageService) GetContainerLayerSize(ctx context.Context, containerID string) (int64, int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}

	var (
		sizeRw, sizeRootfs int64
		err                error
	)

	// Safe to index by runtime.GOOS as Unix hosts don't support multiple
	// container operating systems.
	rwlayer, err := i.layerStore.GetRWLayer(containerID)
	if err != nil {
		log.G(ctx).Errorf("Failed to compute size of container rootfs %v: %v", containerID, err)
		return sizeRw, sizeRootfs, nil
	}
	defer i.layerStore.ReleaseRWLayer(rwlayer)

	if cLayer, ok := rwlayer.(interface {
		SizeWithContext(context.Context) (int64, error)
	}); ok {
		sizeRw, err = cLayer.SizeWithContext(ctx)
	} else {
		sizeRw, err = rwlayer.Size()
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return 0, 0, err
		}
		log.G(ctx).Errorf("Driver %s couldn't return diff size of container %s: %s",
			i.layerStore.DriverName(), containerID, err)
		// FIXME: GetSize should return an error. Not changing it now in case
		// there is a side-effect.
		sizeRw = -1
	}

	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}

	if parent := rwlayer.Parent(); parent != nil {
		sizeRootfs = parent.Size()
		if sizeRw != -1 {
			sizeRootfs += sizeRw
		}
	}
	return sizeRw, sizeRootfs, nil
}
