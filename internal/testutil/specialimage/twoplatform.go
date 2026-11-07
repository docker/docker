package specialimage

import (
	"os"
	"path/filepath"

	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TwoPlatform(dir string) (*ocispec.Index, error) {
	const imageRef = "twoplatform:latest"
	ref, err := reference.ParseNormalizedNamed(imageRef)
	if err != nil {
		return nil, err
	}

	manifest1Desc, _, err := oneLayerPlatformManifest(dir, platforms.MustParse("linux/amd64"), FileInLayer{Path: "bash", Content: []byte("layer1")})
	if err != nil {
		return nil, err
	}

	manifest2Desc, _, err := oneLayerPlatformManifest(dir, platforms.MustParse("linux/arm64"), FileInLayer{Path: "bash", Content: []byte("layer2")})
	if err != nil {
		return nil, err
	}

	return multiPlatformImage(dir, ref, ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{manifest1Desc, manifest2Desc},
	})
}

type FileInLayer struct {
	Path    string
	Content []byte
}

func oneLayerPlatformManifest(dir string, platform ocispec.Platform, f FileInLayer) (ocispec.Descriptor, manifestItem, error) {
	layerDesc, err := writeLayerWithOneFile(dir, f.Path, f.Content)
	if err != nil {
		return ocispec.Descriptor{}, manifestItem{}, err
	}

	img := ocispec.Image{
		Platform: platform,
		Config: ocispec.ImageConfig{
			Env: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		},
		RootFS: ocispec.RootFS{
			Type:    "layers",
			DiffIDs: []digest.Digest{layerDesc.Digest},
		},
	}

	configDesc, err := writeJsonBlob(dir, ocispec.MediaTypeImageConfig, img)
	if err != nil {
		return ocispec.Descriptor{}, manifestItem{}, err
	}

	manifestDesc, err := writeJsonBlob(dir, ocispec.MediaTypeImageManifest, ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{layerDesc},
	})
	if err != nil {
		return ocispec.Descriptor{}, manifestItem{}, err
	}
	manifestDesc.Platform = &platform

	return manifestDesc, manifestItem{
		Config: blobPath(configDesc),
		Layers: []string{blobPath(layerDesc)},
	}, nil
}

func multiPlatformImage(dir string, ref reference.Named, target ocispec.Index) (*ocispec.Index, error) {
	targetDesc, err := writeJsonBlob(dir, ocispec.MediaTypeImageIndex, target)
	if err != nil {
		return nil, err
	}

	if ref != nil {
		targetDesc.Annotations = map[string]string{
			c8dimages.AnnotationImageName: ref.String(),
		}

		if tagged, ok := ref.(reference.Tagged); ok {
			targetDesc.Annotations[ocispec.AnnotationRefName] = tagged.Tag()
		}
	}

	index := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{targetDesc},
	}

	if err := writeJson(index, filepath.Join(dir, "index.json")); err != nil {
		return nil, err
	}

	err = os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion": "1.0.0"}`), 0o644)
	if err != nil {
		return nil, err
	}

	return &index, nil
}
