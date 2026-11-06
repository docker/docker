package images

import (
	"context"

	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/internal/layer"
	"github.com/pkg/errors"
)

// GetContainerLayerSize returns real size & virtual size
func (i *ImageService) GetContainerLayerSize(ctx context.Context, containerID string) (int64, int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	// TODO Windows
	return 0, 0, nil
}

// GetLayerFolders returns the layer folders from an image RootFS
func (i *ImageService) GetLayerFolders(img *image.Image, rwLayer container.RWLayer, containerID string) ([]string, error) {
	folders := []string{}
	rd := len(img.RootFS.DiffIDs)
	for index := 1; index <= rd; index++ {
		// FIXME: why does this mutate the RootFS?
		img.RootFS.DiffIDs = img.RootFS.DiffIDs[:index]
		if err := image.CheckOS(img.OperatingSystem()); err != nil {
			return nil, errors.Wrapf(err, "cannot get layerpath for ImageID %s", img.RootFS.ChainID())
		}
		layerPath, err := layer.GetLayerPath(i.layerStore, img.RootFS.ChainID())
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get layer path from graphdriver %s for ImageID %s", i.layerStore, img.RootFS.ChainID())
		}
		// Reverse order, expecting parent first
		folders = append([]string{layerPath}, folders...)
	}
	if rwLayer == nil {
		return nil, errors.New("RWLayer is unexpectedly nil")
	}
	m, err := rwLayer.Metadata()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get layer metadata")
	}
	return append(folders, m["dir"]), nil
}
