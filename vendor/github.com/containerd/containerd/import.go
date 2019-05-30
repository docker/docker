/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package containerd

import (
	"context"
	"encoding/json"
	"io"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/images/archive"
	"github.com/containerd/containerd/platforms"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type importOpts struct {
	indexName    string
	imageRefT    func(string) string
	dgstRefT     func(digest.Digest) string
	allPlatforms bool
}

// ImportOpt allows the caller to specify import specific options
type ImportOpt func(*importOpts) error

// WithImageRefTranslator is used to translate the index reference
// to an image reference for the image store.
func WithImageRefTranslator(f func(string) string) ImportOpt {
	return func(c *importOpts) error {
		c.imageRefT = f
		return nil
	}
}

// WithDigestRef is used to create digest images for each
// manifest in the index.
func WithDigestRef(f func(digest.Digest) string) ImportOpt {
	return func(c *importOpts) error {
		c.dgstRefT = f
		return nil
	}
}

// WithIndexName creates a tag pointing to the imported index
func WithIndexName(name string) ImportOpt {
	return func(c *importOpts) error {
		c.indexName = name
		return nil
	}
}

// WithAllPlatforms is used to import content for all platforms.
func WithAllPlatforms(allPlatforms bool) ImportOpt {
	return func(c *importOpts) error {
		c.allPlatforms = allPlatforms
		return nil
	}
}

// Import imports an image from a Tar stream using reader.
// Caller needs to specify importer. Future version may use oci.v1 as the default.
// Note that unreferrenced blobs may be imported to the content store as well.
func (c *Client) Import(ctx context.Context, reader io.Reader, opts ...ImportOpt) ([]images.Image, error) {
	var iopts importOpts
	for _, o := range opts {
		if err := o(&iopts); err != nil {
			return nil, err
		}
	}

	ctx, done, err := c.WithLease(ctx)
	if err != nil {
		return nil, err
	}
	defer done(ctx)

	index, err := archive.ImportIndex(ctx, c.ContentStore(), reader)
	if err != nil {
		return nil, err
	}

	var (
		imgs []images.Image
		cs   = c.ContentStore()
		is   = c.ImageService()
	)

	if iopts.indexName != "" {
		imgs = append(imgs, images.Image{
			Name:   iopts.indexName,
			Target: index,
		})
	}
	var platformMatcher = platforms.All
	if !iopts.allPlatforms {
		platformMatcher = platforms.Default()
	}

	var handler images.HandlerFunc = func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		// Only save images at top level
		if desc.Digest != index.Digest {
			return images.Children(ctx, cs, desc)
		}

		p, err := content.ReadBlob(ctx, cs, desc)
		if err != nil {
			return nil, err
		}

		var idx ocispec.Index
		if err := json.Unmarshal(p, &idx); err != nil {
			return nil, err
		}

		for _, m := range idx.Manifests {
			if ref := m.Annotations[ocispec.AnnotationRefName]; ref != "" {
				if iopts.imageRefT != nil {
					ref = iopts.imageRefT(ref)
				}
				if ref != "" {
					imgs = append(imgs, images.Image{
						Name:   ref,
						Target: m,
					})
				}
			}
			if iopts.dgstRefT != nil {
				ref := iopts.dgstRefT(m.Digest)
				if ref != "" {
					imgs = append(imgs, images.Image{
						Name:   ref,
						Target: m,
					})
				}
			}
		}

		return idx.Manifests, nil
	}

	handler = images.FilterPlatforms(handler, platformMatcher)
	handler = images.SetChildrenLabels(cs, handler)
	if err := images.Walk(ctx, handler, index); err != nil {
		return nil, err
	}

	for i := range imgs {
		img, err := is.Update(ctx, imgs[i], "target")
		if err != nil {
			if !errdefs.IsNotFound(err) {
				return nil, err
			}

			img, err = is.Create(ctx, imgs[i])
			if err != nil {
				return nil, err
			}
		}
		imgs[i] = img
	}

	return imgs, nil
}
