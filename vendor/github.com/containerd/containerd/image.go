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
	"fmt"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/rootfs"
	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// Image describes an image used by containers
type Image interface {
	// Name of the image
	Name() string
	// Target descriptor for the image content
	Target() ocispec.Descriptor
	// Labels of the image
	Labels() map[string]string
	// Unpack unpacks the image's content into a snapshot
	Unpack(context.Context, string) error
	// RootFS returns the unpacked diffids that make up images rootfs.
	RootFS(ctx context.Context) ([]digest.Digest, error)
	// Size returns the total size of the image's packed resources.
	Size(ctx context.Context) (int64, error)
	// Config descriptor for the image.
	Config(ctx context.Context) (ocispec.Descriptor, error)
	// IsUnpacked returns whether or not an image is unpacked.
	IsUnpacked(context.Context, string) (bool, error)
	// ContentStore provides a content store which contains image blob data
	ContentStore() content.Store
}

var _ = (Image)(&image{})

// NewImage returns a client image object from the metadata image
func NewImage(client *Client, i images.Image) Image {
	return &image{
		client:   client,
		i:        i,
		platform: platforms.Default(),
	}
}

// NewImageWithPlatform returns a client image object from the metadata image
func NewImageWithPlatform(client *Client, i images.Image, platform platforms.MatchComparer) Image {
	return &image{
		client:   client,
		i:        i,
		platform: platform,
	}
}

type image struct {
	client *Client

	i        images.Image
	platform platforms.MatchComparer
}

func (i *image) Name() string {
	return i.i.Name
}

func (i *image) Target() ocispec.Descriptor {
	return i.i.Target
}

func (i *image) Labels() map[string]string {
	return i.i.Labels
}

func (i *image) RootFS(ctx context.Context) ([]digest.Digest, error) {
	provider := i.client.ContentStore()
	return i.i.RootFS(ctx, provider, i.platform)
}

func (i *image) Size(ctx context.Context) (int64, error) {
	provider := i.client.ContentStore()
	return i.i.Size(ctx, provider, i.platform)
}

func (i *image) Config(ctx context.Context) (ocispec.Descriptor, error) {
	provider := i.client.ContentStore()
	return i.i.Config(ctx, provider, i.platform)
}

func (i *image) IsUnpacked(ctx context.Context, snapshotterName string) (bool, error) {
	sn := i.client.SnapshotService(snapshotterName)
	cs := i.client.ContentStore()

	diffs, err := i.i.RootFS(ctx, cs, i.platform)
	if err != nil {
		return false, err
	}

	chainID := identity.ChainID(diffs)
	_, err = sn.Stat(ctx, chainID.String())
	if err == nil {
		return true, nil
	} else if !errdefs.IsNotFound(err) {
		return false, err
	}

	return false, nil
}

func (i *image) Unpack(ctx context.Context, snapshotterName string) error {
	ctx, done, err := i.client.WithLease(ctx)
	if err != nil {
		return err
	}
	defer done(ctx)

	layers, err := i.getLayers(ctx, i.platform)
	if err != nil {
		return err
	}

	var (
		sn = i.client.SnapshotService(snapshotterName)
		a  = i.client.DiffService()
		cs = i.client.ContentStore()

		chain    []digest.Digest
		unpacked bool
	)
	for _, layer := range layers {
		unpacked, err = rootfs.ApplyLayer(ctx, layer, chain, sn, a)
		if err != nil {
			return err
		}

		if unpacked {
			// Set the uncompressed label after the uncompressed
			// digest has been verified through apply.
			cinfo := content.Info{
				Digest: layer.Blob.Digest,
				Labels: map[string]string{
					"containerd.io/uncompressed": layer.Diff.Digest.String(),
				},
			}
			if _, err := cs.Update(ctx, cinfo, "labels.containerd.io/uncompressed"); err != nil {
				return err
			}
		}

		chain = append(chain, layer.Diff.Digest)
	}

	desc, err := i.i.Config(ctx, cs, i.platform)
	if err != nil {
		return err
	}

	rootfs := identity.ChainID(chain).String()

	cinfo := content.Info{
		Digest: desc.Digest,
		Labels: map[string]string{
			fmt.Sprintf("containerd.io/gc.ref.snapshot.%s", snapshotterName): rootfs,
		},
	}

	_, err = cs.Update(ctx, cinfo, fmt.Sprintf("labels.containerd.io/gc.ref.snapshot.%s", snapshotterName))
	return err
}

func (i *image) getLayers(ctx context.Context, platform platforms.MatchComparer) ([]rootfs.Layer, error) {
	cs := i.client.ContentStore()

	manifest, err := images.Manifest(ctx, cs, i.i.Target, platform)
	if err != nil {
		return nil, err
	}

	diffIDs, err := i.i.RootFS(ctx, cs, platform)
	if err != nil {
		return nil, errors.Wrap(err, "failed to resolve rootfs")
	}
	if len(diffIDs) != len(manifest.Layers) {
		return nil, errors.Errorf("mismatched image rootfs and manifest layers")
	}
	layers := make([]rootfs.Layer, len(diffIDs))
	for i := range diffIDs {
		layers[i].Diff = ocispec.Descriptor{
			// TODO: derive media type from compressed type
			MediaType: ocispec.MediaTypeImageLayer,
			Digest:    diffIDs[i],
		}
		layers[i].Blob = manifest.Layers[i]
	}
	return layers, nil
}

func (i *image) ContentStore() content.Store {
	return i.client.ContentStore()
}
