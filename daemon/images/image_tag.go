package images // import "github.com/docker/docker/daemon/images"

import (
	"context"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/docker/distribution/reference"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// TagImage creates the tag specified by newTag, pointing to the image named
// imageName (alternatively, imageName can also be an image ID).
func (i *ImageService) TagImage(ctx context.Context, imageName, repository, tag string) (string, error) {
	desc, err := i.ResolveImage(ctx, imageName)
	if err != nil {
		return "", err
	}

	newTag, err := reference.ParseNormalizedNamed(repository)
	if err != nil {
		return "", err
	}
	if tag != "" {
		if newTag, err = reference.WithTag(reference.TrimNamed(newTag), tag); err != nil {
			return "", err
		}
	}

	err = i.TagImageWithReference(ctx, desc, newTag)
	return reference.FamiliarString(newTag), err
}

// TagImageWithReference adds the given reference to the image ID provided.
func (i *ImageService) TagImageWithReference(ctx context.Context, target ocispec.Descriptor, newTag reference.Reference) error {
	im := images.Image{
		Name:   newTag.String(),
		Target: target,
	}

	is := i.client.ImageService()
	_, err := is.Create(ctx, im)
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			_, err = i.client.ImageService().Update(ctx, im)
		}
		if err != nil {
			return errors.Wrap(err, "failed to create image")
		}
	}

	i.LogImageEvent(ctx, target.Digest.String(), reference.FamiliarString(newTag), "tag")

	return nil
}
