package specialimage

import (
	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ConfigTarget creates an image index with an image config being used as an
// image target instead of a manifest or index.
func ConfigTarget(dir string) (*ocispec.Index, error) {
	const imageRef = "config:latest"

	ref, err := reference.ParseNormalizedNamed(imageRef)
	if err != nil {
		return nil, err
	}

	desc, err := writeJsonBlob(dir, ocispec.MediaTypeImageConfig, ocispec.Image{
		Platform: platforms.MustParse("linux/amd64"),
		Config: ocispec.ImageConfig{
			Env: []string{"FOO=BAR"},
		},
	})
	if err != nil {
		return nil, err
	}
	desc.Annotations = map[string]string{
		c8dimages.AnnotationImageName: ref.String(),
	}

	return ociImage(dir, ref, desc)
}
