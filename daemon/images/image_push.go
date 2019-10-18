package images // import "github.com/docker/docker/daemon/images"

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/platforms"
	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api/types"
	progressutils "github.com/docker/docker/distribution/utils"
	"github.com/docker/docker/pkg/progress"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// PushImage initiates a push operation on the repository named localName.
func (i *ImageService) PushImage(ctx context.Context, image, tag string, metaHeaders map[string][]string, authConfig *types.AuthConfig, outStream io.Writer) error {
	start := time.Now()
	ref, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return err
	}
	is := i.client.ImageService()

	var imgs []images.Image
	if tag != "" {
		// Push by digest is not supported, so only tags are supported.
		ref, err = reference.WithTag(ref, tag)
		if err != nil {
			return err
		}

		img, err := is.Get(ctx, ref.String())
		if err != nil {
			return errors.Wrap(err, "unable to get image")
		}
		imgs = append(imgs, img)
	} else {
		// TODO(containerd): Escape '.' in ref
		imgs, err := is.List(ctx, fmt.Sprintf("name~=^%s:.*$", ref.String()))
		if err != nil {
			return errors.Wrap(err, "unable to get image")
		}
		if len(imgs) == 0 {
			return errors.Wrap(errdefs.ErrNotFound, "no matching images")
		}
	}

	// Include a buffer so that slow client connections don't affect
	// transfer performance.
	progressChan := make(chan progress.Progress, 100)

	writesDone := make(chan struct{})

	ctx, cancelFunc := context.WithCancel(ctx)

	go func() {
		progressutils.WriteDistributionProgress(cancelFunc, outStream, progressChan)
		close(writesDone)
	}()

	// TODO(containerd): Handle authConfig
	// TODO(containerd): Handle metaHeaders
	opts := []containerd.RemoteOpt{}

	for _, img := range imgs {
		// TODO(containerd): Check for migrations to do

		switch img.Target.MediaType {
		case images.MediaTypeDockerSchema2ManifestList, ocispec.MediaTypeImageIndex:
			// TODO(containerd): Don't resolve manifest, rely on cross manifest push
			// or push all platforms
			p, err := content.ReadBlob(ctx, i.client.ContentStore(), img.Target)
			if err != nil {
				return errors.Wrap(err, "unable to read manifest list")
			}

			var idx ocispec.Index
			if err := json.Unmarshal(p, &idx); err != nil {
				return err
			}

			platform := platforms.Default()
			var descs []ocispec.Descriptor
			for _, d := range idx.Manifests {
				if d.Platform == nil || platform.Match(*d.Platform) {
					descs = append(descs, d)
				}
			}
			if len(descs) > 0 {
				sort.SliceStable(descs, func(i, j int) bool {
					if descs[i].Platform == nil {
						return false
					}
					if descs[j].Platform == nil {
						return true
					}
					return platform.Less(*descs[i].Platform, *descs[j].Platform)
				})
				img.Target = descs[0]
			}

		default:
			// Keep target
		}
		if err = i.client.Push(ctx, img.Name, img.Target, opts...); err != nil {
			err = errors.Wrap(err, "failed to push")
			break
		}
	}

	//close(progressChan)
	//<-writesDone
	imageActions.WithValues("push").UpdateSince(start)

	return err
}
