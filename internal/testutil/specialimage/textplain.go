package specialimage

import (
	"strings"

	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/distribution/reference"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TextPlain creates an non-container image that only contains a text/plain blob.
func TextPlain(dir string) (*ocispec.Index, error) {
	ref, err := reference.ParseNormalizedNamed("tianon/test:text-plain")
	if err != nil {
		return nil, err
	}

	emptyJsonDesc, err := writeBlob(dir, "text/plain", strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}

	configDesc := emptyJsonDesc
	configDesc.MediaType = "application/vnd.oci.empty.v1+json"

	desc, err := writeJsonBlob(dir, ocispec.MediaTypeImageManifest, ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers: []ocispec.Descriptor{
			emptyJsonDesc,
		},
	})
	if err != nil {
		return nil, err
	}
	desc.Annotations = map[string]string{
		c8dimages.AnnotationImageName: ref.String(),
	}

	return ociImage(dir, nil, desc)
}
