package imagerefchecker

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/moby/buildkit/cache"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

type Opt struct {
	ImageStore   images.Store
	ContentStore content.Store
}

// New creates new image reference checker that can be used to see if a reference
// is being used by any of the images in the image store
func New(opt Opt) cache.ExternalRefCheckerFunc {
	return func() (cache.ExternalRefChecker, error) {
		return &Checker{opt: opt}, nil
	}
}

type Checker struct {
	opt    Opt
	once   sync.Once
	images map[string]struct{}
	cache  map[string]bool
}

func (c *Checker) Exists(key string, blobs []digest.Digest) bool {
	if c.opt.ImageStore == nil {
		return false
	}

	// An empty digest means that a layer in the chain has no blob, so it was
	// never exported and no image can reference it. Without this check, the
	// key of such a chain would be the same as the key of its parent chain.
	if slices.Contains(blobs, "") {
		return false
	}

	c.once.Do(c.init)

	if b, ok := c.cache[key]; ok {
		return b
	}

	_, ok := c.images[layerKey(blobs)]
	c.cache[key] = ok
	return ok
}

func (c *Checker) init() {
	c.images = map[string]struct{}{}
	c.cache = map[string]bool{}

	imgs, err := c.opt.ImageStore.List(context.TODO())
	if err != nil {
		return
	}

	var mu sync.Mutex

	for _, img := range imgs {
		if err := images.Dispatch(context.TODO(), images.Handlers(layersHandler(c.opt.ContentStore, func(layers []ocispecs.Descriptor) {
			mu.Lock()
			c.registerLayers(layers)
			mu.Unlock()
		})), nil, img.Target); err != nil {
			return
		}
	}
}

func (c *Checker) registerLayers(l []ocispecs.Descriptor) {
	if k := layerKey(toDigests(l)); k != "" {
		c.images[k] = struct{}{}
	}
}

func toDigests(layers []ocispecs.Descriptor) []digest.Digest {
	digests := make([]digest.Digest, len(layers))
	for i, l := range layers {
		digests[i] = l.Digest
	}
	return digests
}

func layerKey(layers []digest.Digest) string {
	b := &strings.Builder{}
	for _, l := range layers {
		b.Write([]byte(l))
	}
	return b.String()
}

func layersHandler(provider content.Provider, f func([]ocispecs.Descriptor)) images.HandlerFunc {
	return func(ctx context.Context, desc ocispecs.Descriptor) ([]ocispecs.Descriptor, error) {
		switch desc.MediaType {
		case images.MediaTypeDockerSchema2Manifest, ocispecs.MediaTypeImageManifest:
			p, err := content.ReadBlob(ctx, provider, desc)
			if err != nil {
				return nil, nil
			}

			var manifest ocispecs.Manifest
			if err := json.Unmarshal(p, &manifest); err != nil {
				return nil, err
			}

			f(manifest.Layers)
			return nil, nil
		case images.MediaTypeDockerSchema2ManifestList, ocispecs.MediaTypeImageIndex:
			p, err := content.ReadBlob(ctx, provider, desc)
			if err != nil {
				return nil, nil
			}

			var index ocispecs.Index
			if err := json.Unmarshal(p, &index); err != nil {
				return nil, err
			}

			return index.Manifests, nil
		default:
			return nil, errors.Errorf("encountered unknown type %v", desc.MediaType)
		}
	}
}
