package dockerfile // import "github.com/docker/docker/builder/dockerfile"

import (
	"context"

	"github.com/docker/docker/api/types/backend"
	"github.com/docker/docker/builder"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type getAndMountFunc func(string, bool, *ocispec.Platform) (*ocispec.Descriptor, builder.ROLayer, error)

// imageSources mounts images and provides a cache for mounted images. It tracks
// all images so they can be unmounted at the end of the build.
type imageSources struct {
	byImageID map[digest.Digest]*imageMount
	mounts    []*imageMount
	getImage  getAndMountFunc
}

func newImageSources(ctx context.Context, options builderOptions) *imageSources {
	getAndMount := func(idOrRef string, localOnly bool, platform *ocispec.Platform) (*ocispec.Descriptor, builder.ROLayer, error) {
		pullOption := backend.PullOptionNoPull
		if !localOnly {
			if options.Options.PullParent {
				pullOption = backend.PullOptionForcePull
			} else {
				pullOption = backend.PullOptionPreferLocal
			}
		}
		return options.Backend.GetImageAndReleasableLayer(ctx, idOrRef, backend.GetImageAndLayerOptions{
			PullOption: pullOption,
			AuthConfig: options.Options.AuthConfigs,
			Output:     options.ProgressWriter.Output,
			Platform:   platform,
		})
	}

	return &imageSources{
		byImageID: make(map[digest.Digest]*imageMount),
		getImage:  getAndMount,
	}
}

func (m *imageSources) Get(idOrRef string, localOnly bool, platform *ocispec.Platform) (*imageMount, error) {
	if dgst, err := digest.Parse(idOrRef); err == nil {
		if im, ok := m.byImageID[dgst]; ok {
			return im, nil
		}
	}

	image, layer, err := m.getImage(idOrRef, localOnly, platform)
	if err != nil {
		return nil, err
	}
	im := newImageMount(image, layer)
	m.Add(im)
	return im, nil
}

func (m *imageSources) Unmount() (retErr error) {
	for _, im := range m.mounts {
		if err := im.unmount(); err != nil {
			logrus.Error(err)
			retErr = err
		}
	}
	return
}

func (m *imageSources) Add(im *imageMount) {
	if im.image != nil {
		m.byImageID[im.image.Digest] = im
	} else {
		// TODO(containerd): Handle scratch images differently

		// TOD(containerd): Get build platform
		//// set the OS for scratch images
		//os := runtime.GOOS
		//// Windows does not support scratch except for LCOW
		//if runtime.GOOS == "windows" {
		//	os = "linux"
		//}
		//im.image = &dockerimage.Image{V1Image: dockerimage.V1Image{OS: os}}
	}
	m.mounts = append(m.mounts, im)
}

// imageMount is a reference to an image that can be used as a builder.Source
type imageMount struct {
	image *ocispec.Descriptor
	layer builder.ROLayer
}

func newImageMount(image *ocispec.Descriptor, layer builder.ROLayer) *imageMount {
	im := &imageMount{image: image, layer: layer}
	return im
}

func (im *imageMount) unmount() error {
	if im.layer == nil {
		return nil
	}
	if err := im.layer.Release(); err != nil {
		// TODO(containerd): cleaner output than %s
		return errors.Wrapf(err, "failed to unmount previous build image %s", im.image.Digest.String())
	}
	im.layer = nil
	return nil
}

func (im *imageMount) Image() *ocispec.Descriptor {
	return im.image
}

func (im *imageMount) NewRWLayer() (builder.RWLayer, error) {
	return im.layer.NewRWLayer()
}

// TODO(containerd): Remove this function, always use digest
func (im *imageMount) ImageID() string {
	var imageID string
	if im.image != nil {
		imageID = im.image.Digest.String()
	}
	return imageID
}
