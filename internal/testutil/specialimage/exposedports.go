package specialimage

import (
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	exposedPortRangeImageRef = "exposedportrange:latest"
	exposedPortRangeCmd      = "true"
	exposedPortRange         = "33060-33061/tcp"
	exposedPortRangeRootFS   = "layers"
)

// ExposedPortRange creates a minimal image whose config contains a Docker
// EXPOSE port range entry, matching images produced by some non-Docker builders.
func ExposedPortRange(dir string) (*ocispec.Index, error) {
	ref, err := reference.ParseNormalizedNamed(exposedPortRangeImageRef)
	if err != nil {
		return nil, err
	}

	configDesc, err := writeJsonBlob(dir, ocispec.MediaTypeImageConfig, ocispec.Image{
		Platform: platforms.DefaultSpec(),
		Config: ocispec.ImageConfig{
			Cmd: []string{exposedPortRangeCmd},
			ExposedPorts: map[string]struct{}{
				exposedPortRange: {},
			},
		},
		RootFS: ocispec.RootFS{
			Type: exposedPortRangeRootFS,
		},
	})
	if err != nil {
		return nil, err
	}

	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{},
	}

	legacyManifests := []manifestItem{
		{
			Config:   blobPath(configDesc),
			RepoTags: []string{exposedPortRangeImageRef},
		},
	}

	return singlePlatformImage(dir, ref, manifest, legacyManifests)
}
