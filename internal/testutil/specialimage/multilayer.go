package specialimage

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/google/uuid"
	"github.com/moby/go-archive"
	"github.com/moby/go-archive/compression"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type SingleFileLayer struct {
	Name    string
	Content []byte
}

func MultiLayer(dir string) (*ocispec.Index, error) {
	return MultiLayerCustom(dir, "multilayer:latest", []SingleFileLayer{
		{Name: "foo", Content: []byte("1")},
		{Name: "bar", Content: []byte("2")},
		{Name: "hello", Content: []byte("world")},
	})
}

func MultiLayerCustom(dir string, imageRef string, layers []SingleFileLayer) (*ocispec.Index, error) {
	var layerDescs []ocispec.Descriptor
	var layerDgsts []digest.Digest
	var layerBlobs []string
	for _, layer := range layers {
		layerDesc, err := writeLayerWithOneFile(dir, layer.Name, layer.Content)
		if err != nil {
			return nil, err
		}

		layerDescs = append(layerDescs, layerDesc)
		layerDgsts = append(layerDgsts, layerDesc.Digest)
		layerBlobs = append(layerBlobs, blobPath(layerDesc))
	}

	configDesc, err := writeJsonBlob(dir, ocispec.MediaTypeImageConfig, ocispec.Image{
		Platform: platforms.DefaultSpec(),
		Config: ocispec.ImageConfig{
			Env: []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		},
		RootFS: ocispec.RootFS{
			Type:    "layers",
			DiffIDs: layerDgsts,
		},
	})
	if err != nil {
		return nil, err
	}

	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    layerDescs,
	}

	legacyManifests := []manifestItem{
		{
			Config:   blobPath(configDesc),
			RepoTags: []string{imageRef},
			Layers:   layerBlobs,
		},
	}

	ref, err := reference.ParseNormalizedNamed(imageRef)
	if err != nil {
		return nil, err
	}
	return singlePlatformImage(dir, ref, manifest, legacyManifests)
}

// Legacy manifest item (manifests.json)
type manifestItem struct {
	Config   string
	RepoTags []string
	Layers   []string
}

func singlePlatformImage(dir string, ref reference.Named, manifest ocispec.Manifest, legacyManifests []manifestItem) (*ocispec.Index, error) {
	manifestDesc, err := writeJsonBlob(dir, ocispec.MediaTypeImageManifest, manifest)
	if err != nil {
		return nil, err
	}

	if ref != nil {
		manifestDesc.Annotations = map[string]string{
			c8dimages.AnnotationImageName: ref.String(),
		}

		if tagged, ok := ref.(reference.Tagged); ok {
			manifestDesc.Annotations[ocispec.AnnotationRefName] = tagged.Tag()
		}
	}

	if err := writeJson(legacyManifests, filepath.Join(dir, "manifest.json")); err != nil {
		return nil, err
	}

	return ociImage(dir, ref, manifestDesc)
}

func ociImage(dir string, ref reference.Named, target ocispec.Descriptor) (*ocispec.Index, error) {
	idx := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{target},
	}
	if err := writeJson(idx, filepath.Join(dir, "index.json")); err != nil {
		return nil, err
	}

	err := os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion": "1.0.0"}`), 0o644)
	if err != nil {
		return nil, err
	}

	return &idx, nil
}

func fileArchive(dir string, name string, content []byte) (io.ReadCloser, error) {
	tmp, err := os.MkdirTemp("", "")
	if err != nil {
		return nil, err
	}

	if err := os.WriteFile(filepath.Join(tmp, name), content, 0o644); err != nil {
		return nil, err
	}

	return archive.Tar(tmp, compression.None)
}

func writeLayerWithOneFile(dir string, filename string, content []byte) (ocispec.Descriptor, error) {
	rd, err := fileArchive(dir, filename, content)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	defer rd.Close()

	return writeBlob(dir, ocispec.MediaTypeImageLayer, rd)
}

func writeJsonBlob(dir string, mt string, obj any) (ocispec.Descriptor, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	return writeBlob(dir, mt, bytes.NewReader(b))
}

func writeJson(obj any, path string) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}

	return os.WriteFile(path, b, 0o644)
}

func writeBlob(dir string, mt string, rd io.Reader) (_ ocispec.Descriptor, outErr error) {
	digester := digest.Canonical.Digester()
	hashTee := io.TeeReader(rd, digester.Hash())

	blobsPath := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobsPath, 0o755); err != nil {
		return ocispec.Descriptor{}, err
	}

	tmpPath := filepath.Join(blobsPath, uuid.New().String())
	file, err := os.Create(tmpPath)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	defer func() {
		if outErr != nil {
			file.Close()
			os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(file, hashTee); err != nil {
		return ocispec.Descriptor{}, err
	}

	digest := digester.Digest()

	stat, err := os.Stat(tmpPath)
	if err != nil {
		return ocispec.Descriptor{}, err
	}

	file.Close()
	if err := os.Rename(tmpPath, filepath.Join(blobsPath, digest.Encoded())); err != nil {
		return ocispec.Descriptor{}, err
	}

	return ocispec.Descriptor{
		MediaType: mt,
		Digest:    digest,
		Size:      stat.Size(),
	}, nil
}

func blobPath(desc ocispec.Descriptor) string {
	return "blobs/sha256/" + desc.Digest.Encoded()
}
