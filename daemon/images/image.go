package images // import "github.com/docker/docker/daemon/images"

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"

	"github.com/containerd/containerd/content"
	cerrdefs "github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/platforms"
	"github.com/docker/distribution/reference"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/image"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

const (
	// LabelImageParent is Docker's parent image ID
	// Stored on the image blob (config or manifest)
	LabelImageParent = "containerd.io/gc.ref.content.parent"

	// LabelLayerPrefix is used as the label prefix for layer stores
	// Stores the layer reference in the given layerstore.
	// The value always represents the digest of the ChainID
	LabelLayerPrefix = "docker.io/layer."
)

var shortID = regexp.MustCompile(`^([a-f0-9]{4,64})$`)

// ErrImageDoesNotExist is error returned when no image can be found for a reference.
type ErrImageDoesNotExist struct {
	ref reference.Reference
}

func (e ErrImageDoesNotExist) Error() string {
	ref := e.ref
	if named, ok := ref.(reference.Named); ok {
		ref = reference.TagNameOnly(named)
	}
	return fmt.Sprintf("No such image: %s", reference.FamiliarString(ref))
}

// NotFound implements the NotFound interface
func (e ErrImageDoesNotExist) NotFound() {}

// ResolveImage searches for an image based on the given
// reference or identifier. Returns the descriptor of
// the image, could be manifest list, manifest, or config.
func (i *ImageService) ResolveImage(ctx context.Context, refOrID string) (d ocispec.Descriptor, err error) {
	d, _, err = i.resolveImageName(ctx, refOrID)
	return
}

func (i *ImageService) resolveImageName(ctx context.Context, refOrID string) (ocispec.Descriptor, reference.Named, error) {
	parsed, err := reference.ParseAnyReference(refOrID)
	if err != nil {
		return ocispec.Descriptor{}, nil, errdefs.InvalidParameter(err)
	}

	is := i.client.ImageService()

	namedRef, ok := parsed.(reference.Named)
	if !ok {
		digested, ok := parsed.(reference.Digested)
		if !ok {
			return ocispec.Descriptor{}, nil, errdefs.InvalidParameter(errors.New("bad reference"))
		}

		imgs, err := is.List(ctx, fmt.Sprintf("target.digest==%s", digested.Digest()))
		if err != nil {
			return ocispec.Descriptor{}, nil, errors.Wrap(err, "failed to lookup digest")
		}
		if len(imgs) == 0 {
			return ocispec.Descriptor{}, nil, errdefs.NotFound(errors.New("image not found with digest"))
		}

		return imgs[0].Target, nil, nil
	}

	// If the identifier could be a short ID, attempt to match
	if shortID.MatchString(refOrID) {
		filters := []string{
			fmt.Sprintf("name==%q", namedRef.String()),
			fmt.Sprintf(`target.digest~=/sha256:%s[0-9a-fA-F]{%d}/`, refOrID, 64-len(refOrID)),
		}
		imgs, err := is.List(ctx, filters...)
		if err != nil {
			return ocispec.Descriptor{}, nil, err
		}

		if len(imgs) == 0 {
			return ocispec.Descriptor{}, nil, errdefs.NotFound(errors.New("list returned no images"))
		}
		if len(imgs) > 1 {
			ref := namedRef.String()
			digests := map[digest.Digest]struct{}{}
			for _, img := range imgs {
				if img.Name == ref {
					return img.Target, nil, nil
				}
				digests[img.Target.Digest] = struct{}{}
			}

			if len(digests) > 1 {
				return ocispec.Descriptor{}, nil, errdefs.NotFound(errors.New("ambiguous reference"))
			}
		}
		return imgs[0].Target, nil, nil
	}
	namedRef = reference.TagNameOnly(namedRef)
	img, err := is.Get(ctx, namedRef.String())
	if err != nil {
		// TODO(containerd): error translation can use common function
		if !cerrdefs.IsNotFound(err) {
			return ocispec.Descriptor{}, nil, err
		}
		return ocispec.Descriptor{}, nil, errdefs.NotFound(errors.New("id not found"))
	}

	return img.Target, namedRef, nil
}

// RuntimeImage represents a platform-specific image along with the
// image configuration and targeted image ID.
type RuntimeImage struct {
	Target      ocispec.Descriptor
	Config      ocispec.Descriptor
	ConfigBytes []byte
	Platform    ocispec.Platform
}

// ResolveRuntimeImage resolves an image down to the platform-specific
// runtime configuration for the image.
// A runtime image is platform specific.
// The platform is resolved based on availability in the image and
// the order preference of the backend storage drivers.
func (i *ImageService) ResolveRuntimeImage(ctx context.Context, desc ocispec.Descriptor) (RuntimeImage, error) {
	runtimeImages, err := i.runtimeImages(ctx, desc)
	if err != nil {
		return RuntimeImage{}, err
	}

	// filter platforms, do inplace filtering since small sized array
	for j := 0; j < len(runtimeImages); {
		if !i.platforms.Match(runtimeImages[j].Platform) {
			copy(runtimeImages[j:], runtimeImages[j+1:])
			runtimeImages = runtimeImages[:len(runtimeImages)-1]
		} else {
			j++
		}
	}

	sort.SliceStable(runtimeImages, func(j, k int) bool {
		return i.platforms.Less(runtimeImages[j].Platform, runtimeImages[k].Platform)
	})

	if len(runtimeImages) == 0 {
		return RuntimeImage{}, errdefs.NotFound(errors.New("no runtime image found"))
	}

	ri := runtimeImages[0]
	if len(ri.ConfigBytes) == 0 {
		ri.ConfigBytes, err = content.ReadBlob(ctx, i.client.ContentStore(), ri.Config)
		if err != nil {
			return RuntimeImage{}, err
		}
	}

	return ri, nil
}

// ResolveRuntimeConfig resolves the descriptor to a runtime image and returns the config
func (i *ImageService) ResolveRuntimeConfig(ctx context.Context, desc ocispec.Descriptor) ([]byte, error) {
	ri, err := i.ResolveRuntimeImage(ctx, desc)
	if err != nil {
		return nil, err
	}
	return ri.ConfigBytes, nil
}

func (i *ImageService) runtimeImages(ctx context.Context, image ocispec.Descriptor) ([]RuntimeImage, error) {
	var (
		imageMap      = map[digest.Digest]RuntimeImage{}
		runtimeImages []RuntimeImage
		cs            = i.client.ContentStore()
	)

	if err := images.Walk(ctx, images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		switch desc.MediaType {
		case images.MediaTypeDockerSchema2Config, ocispec.MediaTypeImageConfig:
			image, ok := imageMap[desc.Digest]
			if !ok {
				image = RuntimeImage{
					Target: desc,
				}
			}
			image.Config = desc

			p, err := content.ReadBlob(ctx, cs, desc)
			if err != nil {
				if cerrdefs.IsNotFound(err) {
					log.G(ctx).Debugf("image config missing: %s", desc.Digest.String())
					return nil, nil
				}
				return nil, err
			}

			if err := json.Unmarshal(p, &image.Platform); err != nil {
				return nil, err
			}

			if image.Platform.OS == "" {
				log.G(ctx).Warnf("image is missing platform: %s", desc.Digest.String())
				return nil, nil
			}

			image.Platform = platforms.Normalize(image.Platform)
			image.ConfigBytes = p

			runtimeImages = append(runtimeImages, image)
			return nil, nil
		case images.MediaTypeDockerSchema2Manifest, ocispec.MediaTypeImageManifest:
			p, err := content.ReadBlob(ctx, cs, desc)
			if err != nil {
				if cerrdefs.IsNotFound(err) {
					log.G(ctx).Debugf("image manifest missing: %s", desc.Digest.String())
					return nil, nil
				}
				return nil, err
			}

			var manifest ocispec.Manifest
			if err := json.Unmarshal(p, &manifest); err != nil {
				return nil, err
			}

			if image, ok := imageMap[desc.Digest]; ok {
				if image.Platform.OS != "" {
					// Use platform from manifest list
					image.Config = manifest.Config
					runtimeImages = append(runtimeImages, image)
					return nil, nil
				}
				// Map config to the runtime image
				imageMap[manifest.Config.Digest] = image
			} else {
				imageMap[manifest.Config.Digest] = RuntimeImage{
					Target: desc,
				}
			}

			return []ocispec.Descriptor{manifest.Config}, nil
		case images.MediaTypeDockerSchema2ManifestList, ocispec.MediaTypeImageIndex:
			p, err := content.ReadBlob(ctx, cs, desc)
			if err != nil {
				return nil, err
			}

			var idx ocispec.Index
			if err := json.Unmarshal(p, &idx); err != nil {
				return nil, err
			}

			for _, m := range idx.Manifests {
				ri := RuntimeImage{
					Target: desc,
				}
				if m.Platform != nil {
					ri.Platform = platforms.Normalize(*m.Platform)
				}
				imageMap[m.Digest] = ri
			}

			return idx.Manifests, nil

		}
		return nil, errdefs.NotFound(errors.Errorf("unexpected media type %v for %v", desc.MediaType, desc.Digest))
	}), image); err != nil {
		return nil, err
	}

	return runtimeImages, nil
}

// GetImage returns an image corresponding to the image referred to by refOrID.
// Deprecated: Use (i *ImageService).GetImage instead.
// TODO(containerd): remove this function and replace with ResolveImage
func (i *ImageService) getDockerImage(refOrID string) (*image.Image, error) {
	ref, err := reference.ParseAnyReference(refOrID)
	if err != nil {
		return nil, errdefs.InvalidParameter(err)
	}

	var target ocispec.Descriptor
	cs := i.client.ContentStore()

	namedRef, ok := ref.(reference.Named)
	if !ok {
		digested, ok := ref.(reference.Digested)
		if !ok {
			return nil, ErrImageDoesNotExist{ref}
		}

		target.Digest = digested.Digest()

	} else {
		img, err := i.client.ImageService().Get(context.TODO(), namedRef.String())
		if err != nil {
			if !errdefs.IsNotFound(err) {
				return nil, errors.Wrapf(err, "unable to get image: %q", namedRef.String())
			}
			// TODO: If not found here, get all hashes of config and search for best match
			// Search based on ID
			//if id, err := i.imageStore.Search(refOrID); err == nil {
			//	img, err := i.imageStore.Get(id)
			//	if err != nil {
			//		return nil, ErrImageDoesNotExist{ref}
			//	}
			//	return img, nil
			//}
			return nil, ErrImageDoesNotExist{ref}
		}
		d, err := images.Config(context.TODO(), cs, img.Target, i.platforms)
		if err != nil {
			if errdefs.IsNotFound(err) {
				return nil, ErrImageDoesNotExist{ref}
			}
			return nil, errors.Wrap(err, "unable to resolve image")
		}
		target = d
	}

	img, err := i.getImage(context.TODO(), target)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, ErrImageDoesNotExist{ref}
		}
		return nil, err
	}

	return img, nil
}

// TODO(containerd): remove this function and replace with ResolveImage
func (i *ImageService) getImage(ctx context.Context, target ocispec.Descriptor) (*image.Image, error) {
	cs := i.client.ContentStore()

	b, err := content.ReadBlob(ctx, cs, target)
	if err != nil {
		return nil, errors.Wrap(err, "unable to read target blob")
	}

	var img image.Image
	if err := json.Unmarshal(b, &img); err != nil {
		return nil, errors.Wrap(err, "unable to unmarshal image config")
	}

	return &img, nil
}
