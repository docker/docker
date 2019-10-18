package daemon

import (
	"github.com/containerd/containerd/platforms"
	"github.com/docker/docker/distribution"
	"github.com/docker/docker/distribution/metadata"
	"github.com/docker/docker/image"
	"github.com/docker/docker/layer"
	dockerreference "github.com/docker/docker/reference"
)

// DistributionServices provides daemon image storage services
type DistributionServices struct {
	DownloadManager   distribution.RootFSDownloadManager
	V2MetadataService metadata.V2MetadataService
	LayerStore        layer.Store
	ImageStore        image.Store
	ReferenceStore    dockerreference.Store
}

// DistributionServices returns services controlling daemon storage
// TODO(containerd): deprecated after migration
func (d *Daemon) DistributionServices() (DistributionServices, error) {
	ls, err := d.imageService.GetLayerStore(platforms.DefaultSpec())
	if err != nil {
		return DistributionServices{}, err
	}
	return DistributionServices{
		LayerStore: ls,
	}, nil
}
