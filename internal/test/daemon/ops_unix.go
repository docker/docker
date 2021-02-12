// +build !windows

package daemon

import (
	"path/filepath"
	"runtime"

	"github.com/docker/docker/daemon/images"
	"github.com/docker/docker/layer"

	// register graph drivers
	_ "github.com/docker/docker/daemon/graphdriver/register"
	"github.com/docker/docker/pkg/idtools"
)

// WithImageService sets imageService options
func WithImageService(d *Daemon) {
	layerStores := make(map[string]layer.Store)
	os := runtime.GOOS
	layerStores[os], _ = layer.NewStoreFromOptions(layer.StoreOptions{
		Root:                      d.Root,
		MetadataStorePathTemplate: filepath.Join(d.RootDir(), "image", "%s", "layerdb"),
		GraphDriver:               d.storageDriver,
		GraphDriverOptions:        nil,
		IDMapping:                 &idtools.IdentityMapping{},
		PluginGetter:              nil,
		ExperimentalEnabled:       false,
		OS:                        os,
	})
	d.imageService = images.NewImageService(images.ImageServiceConfig{
		LayerStores: layerStores,
	})
}
