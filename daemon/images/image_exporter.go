package images // import "github.com/docker/docker/daemon/images"

import (
	"context"
	"io"

	"github.com/containerd/containerd/images/archive"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// ExportImage exports a list of images to the given output stream. The
// exported images are archived into a tar when written to the output
// stream. All images with the given tag and all versions containing
// the same tag are exported. names is the set of tags to export, and
// outStream is the writer which the images are written to.
func (i *ImageService) ExportImage(ctx context.Context, names []string, w io.Writer) error {
	images := map[digest.Digest]struct {
		target ocispec.Descriptor
		names  []string
	}{}

	for _, name := range names {
		desc, named, err := i.resolveImageName(ctx, name)
		if err != nil {
			return errors.Wrapf(err, "failed to resolve %s to an image", name)
		}
		ei, ok := images[desc.Digest]
		if !ok {
			ei.target = desc
		}
		if named != nil {
			ei.names = append(ei.names, named.String())
		}
		images[desc.Digest] = ei
	}

	opts := []archive.ExportOpt{
		archive.WithPlatform(i.platforms),
	}

	for _, img := range images {
		opts = append(opts, archive.WithManifest(img.target, img.names...))
	}

	// Add each manifest
	return archive.Export(ctx, i.client.ContentStore(), w, opts...)
}
