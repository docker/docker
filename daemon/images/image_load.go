package images // import "github.com/docker/docker/daemon/images"
import (
	"context"
	"encoding/json"
	"io"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/images/archive"
	"github.com/containerd/containerd/log"
	"github.com/docker/docker/pkg/progress"
	"github.com/docker/docker/pkg/streamformatter"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// LoadImage uploads a set of images into the repository. This is the
// complement of ImageExport.  The input stream is an uncompressed tar
// ball containing images and metadata.
func (i *ImageService) LoadImage(ctx context.Context, inTar io.ReadCloser, outStream io.Writer, quiet bool) error {
	var p progress.Output
	if !quiet {
		p = streamformatter.NewJSONProgressOutput(outStream, false)
	}

	ctx, done, err := i.client.WithLease(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := done(context.Background()); err != nil {
			log.G(ctx).WithError(err).Errorf("lease release failed")
		}
	}()

	cs := i.client.ContentStore()
	index, err := archive.ImportIndex(ctx, cs, inTar)
	if err != nil {
		// TODO(containerd): Handle unrecognized type for older
		// docker images. Update import index to return an error
		// which has all the blobs written
		return err
	}

	var (
		imgs []images.Image
		is   = i.client.ImageService()
	)

	// TODO(containerd): Provide option for naming OCI index
	//imgs = append(imgs, images.Image{
	//	Name:   iopts.indexName,
	//	Target: index,
	//})

	var handler images.HandlerFunc = func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		// Only save images at top level
		if desc.Digest != index.Digest {
			return images.Children(ctx, cs, desc)
		}

		b, err := content.ReadBlob(ctx, cs, desc)
		if err != nil {
			return nil, err
		}

		var idx ocispec.Index
		if err := json.Unmarshal(b, &idx); err != nil {
			return nil, err
		}

		for _, m := range idx.Manifests {
			ref := m.Annotations[images.AnnotationImageName]
			if ref == "" {
				ref = m.Annotations[ocispec.AnnotationRefName]
				if ref == "" {
					log.G(ctx).Debugf("image skipped, no name for %s", m.Digest.String())
				} else {
					// TODO: Support OCI ref names by providing
					// default repository through API
					log.G(ctx).Debugf("image only containers OCI ref name %q, repository is missing for %s", ref, m.Digest.String())
				}
				continue
			}

			if p != nil {
				progress.Message(p, ref, "Importing")
			}

			mfst, err := images.Manifest(ctx, cs, m, i.platforms)
			if err != nil {
				return nil, err
			}

			if err := i.unpack(ctx, mfst.Config, mfst.Layers, p, nil, nil); err != nil {
				return nil, errors.Wrap(err, "failed to unpack image")
			}

			imgID := m.Digest.String()
			imgs = append(imgs, images.Image{
				Name:   ref,
				Target: m,
			}, images.Image{
				Name:   ref + "@" + imgID,
				Target: m,
			})

			imgs = append(imgs)

			i.LogImageEvent(ctx, imgID, imgID, "load")
		}

		return idx.Manifests, nil
	}

	handler = images.SetChildrenLabels(cs, handler)
	handler = images.FilterPlatforms(handler, i.platforms)
	if err := images.Walk(ctx, handler, index); err != nil {
		return err
	}

	for i := range imgs {
		img, err := is.Update(ctx, imgs[i], "target")
		if err != nil {
			if !errdefs.IsNotFound(err) {
				return err
			}

			img, err = is.Create(ctx, imgs[i])
			if err != nil {
				return err
			}
		}
		imgs[i] = img
	}

	return nil
}
