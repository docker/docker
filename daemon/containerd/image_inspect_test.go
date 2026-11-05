package containerd

import (
	"fmt"
	"path/filepath"
	"testing"

	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log/logtest"
	"github.com/containerd/platforms"
	"github.com/moby/moby/v2/daemon/server/imagebackend"
	"github.com/moby/moby/v2/internal/testutil/specialimage"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestImageInspect(t *testing.T) {
	ctx := namespaces.WithNamespace(t.Context(), "testing")

	blobsDir := t.TempDir()

	toContainerdImage := func(t *testing.T, imageFunc specialimage.SpecialImageFunc) c8dimages.Image {
		idx, err := imageFunc(blobsDir)
		assert.NilError(t, err)

		return imagesFromIndex(idx)[0]
	}

	missingMultiPlatform := toContainerdImage(t, func(dir string) (*ocispec.Index, error) {
		idx, _, err := specialimage.PartialMultiPlatform(dir, "missingmp:latest", specialimage.PartialOpts{
			Stored: nil,
			Missing: []ocispec.Platform{
				{OS: "linux", Architecture: "arm64"},
				{OS: "linux", Architecture: "amd64"},
			},
		})
		return idx, err
	})

	cs := &blobsDirContentStore{blobs: filepath.Join(blobsDir, "blobs/sha256")}

	t.Run("inspect image with manifests but missing platform blobs", func(t *testing.T) {
		ctx := logtest.WithT(ctx, t)
		service := fakeImageService(t, ctx, cs)

		_, err := service.images.Create(ctx, missingMultiPlatform)
		assert.NilError(t, err)

		for _, manifests := range []bool{true, false} {
			t.Run(fmt.Sprintf("manifests=%t", manifests), func(t *testing.T) {
				inspect, err := service.ImageInspect(ctx, missingMultiPlatform.Name, imagebackend.ImageInspectOpts{Manifests: manifests})
				assert.NilError(t, err)

				if manifests {
					assert.Check(t, is.Len(inspect.Manifests, 2))
				} else {
					assert.Check(t, is.Len(inspect.Manifests, 0))
				}
			})
		}
	})

	t.Run("inspect image with one layer missing", func(t *testing.T) {
		ctx := logtest.WithT(ctx, t)
		service := fakeImageService(t, ctx, cs)

		img := toContainerdImage(t, specialimage.MultiLayer)

		_, err := service.images.Create(ctx, img)
		assert.NilError(t, err)

		// Get the manifest to access the layers
		mfst, err := c8dimages.Manifest(ctx, cs, img.Target, nil)
		assert.NilError(t, err)
		assert.Check(t, len(mfst.Layers) > 0, "image should have at least one layer")

		// Delete the last layer from the content store
		lastLayer := mfst.Layers[len(mfst.Layers)-1]
		err = cs.Delete(ctx, lastLayer.Digest)
		assert.NilError(t, err)

		for _, platform := range []*ocispec.Platform{nil, new(platforms.DefaultSpec())} {
			inspect, err := service.ImageInspect(ctx, img.Name, imagebackend.ImageInspectOpts{Platform: platform})
			assert.NilError(t, err)

			assert.Check(t, inspect.Config != nil)
			assert.Check(t, is.Len(inspect.RootFS.Layers, len(mfst.Layers)))
		}
	})

	t.Run("inspect falls back to a pulled platform and rejects a requested platform that was not pulled", func(t *testing.T) {
		ctx := logtest.WithT(ctx, t)
		service := fakeImageService(t, ctx, cs)
		host := ocispec.Platform{OS: "linux", Architecture: "amd64"}
		service.defaultPlatformOverride = &host

		img := toContainerdImage(t, func(dir string) (*ocispec.Index, error) {
			idx, _, err := specialimage.PartialMultiPlatform(dir, "only386:latest", specialimage.PartialOpts{
				Stored:  []ocispec.Platform{{OS: "linux", Architecture: "386"}},
				Missing: []ocispec.Platform{host},
			})
			return idx, err
		})
		_, err := service.images.Create(ctx, img)
		assert.NilError(t, err)

		inspect, err := service.ImageInspect(ctx, img.Name, imagebackend.ImageInspectOpts{})
		assert.NilError(t, err)
		assert.Check(t, is.Equal(inspect.Architecture, "386"))
		assert.Check(t, is.Equal(inspect.RootFS.Type, "layers"))

		_, err = service.ImageInspect(ctx, img.Name, imagebackend.ImageInspectOpts{Platform: &host})
		assert.Check(t, is.ErrorType(err, cerrdefs.IsNotFound))
	})

	t.Run("inspect image size by platform", func(t *testing.T) {
		ctx := logtest.WithT(ctx, t)
		service := fakeImageService(t, ctx, cs)

		idx, manifestDescs, err := specialimage.MultiPlatform(blobsDir, "multiplatform:latest", []ocispec.Platform{
			{OS: "linux", Architecture: "amd64"},
			{OS: "linux", Architecture: "arm64"},
		})
		assert.NilError(t, err)
		multiPlatformImage := imagesFromIndex(idx)[0]

		_, err = service.images.Create(ctx, multiPlatformImage)
		assert.NilError(t, err)

		snapshotSizeByArch := map[string]int64{
			"amd64": 10_000_000,
			"arm64": 20_000_000,
		}
		snapshotter, ok := service.snapshotterServices[service.snapshotter].(*testSnapshotterService)
		assert.Assert(t, ok)
		snapshotter.usageByKey = make(map[string]snapshots.Usage, len(manifestDescs))
		for _, desc := range manifestDescs {
			assert.Assert(t, desc.Platform != nil)
			platformImage, err := service.NewImageManifest(ctx, multiPlatformImage, desc)
			assert.NilError(t, err)
			diffIDs, err := platformImage.RootFS(ctx)
			assert.NilError(t, err)
			snapshotter.usageByKey[identity.ChainID(diffIDs).String()] = snapshots.Usage{
				Size: snapshotSizeByArch[desc.Platform.Architecture],
			}
		}

		inspectAll, err := service.ImageInspect(ctx, multiPlatformImage.Name, imagebackend.ImageInspectOpts{Manifests: true})
		assert.NilError(t, err)
		assert.Check(t, is.Len(inspectAll.Manifests, 2))

		totalSizeByArch := make(map[string]int64, len(inspectAll.Manifests))
		for _, manifest := range inspectAll.Manifests {
			assert.Assert(t, manifest.ImageData != nil)
			arch := manifest.ImageData.Platform.Architecture
			snapshotSize, ok := snapshotSizeByArch[arch]
			assert.Assert(t, ok)
			assert.Equal(t, manifest.ImageData.Size.Unpacked, snapshotSize)
			assert.Equal(t, manifest.Size.Total, manifest.Size.Content+snapshotSize)
			totalSizeByArch[arch] = manifest.Size.Total
		}
		selectedSize, ok := totalSizeByArch[inspectAll.Architecture]
		assert.Assert(t, ok)
		assert.Equal(t, inspectAll.Size, selectedSize)

		// Test with amd64 platform
		amd64Platform := &ocispec.Platform{OS: "linux", Architecture: "amd64"}
		inspectAmd64, err := service.ImageInspect(ctx, multiPlatformImage.Name, imagebackend.ImageInspectOpts{
			Platform: amd64Platform,
		})
		assert.NilError(t, err)
		assert.Equal(t, inspectAmd64.Architecture, "amd64")
		assert.Equal(t, inspectAmd64.Os, "linux")
		assert.Equal(t, inspectAmd64.Size, totalSizeByArch["amd64"])

		// Test with arm64 platform
		arm64Platform := &ocispec.Platform{OS: "linux", Architecture: "arm64"}
		inspectArm64, err := service.ImageInspect(ctx, multiPlatformImage.Name, imagebackend.ImageInspectOpts{
			Platform: arm64Platform,
		})
		assert.NilError(t, err)
		assert.Equal(t, inspectArm64.Architecture, "arm64")
		assert.Equal(t, inspectArm64.Os, "linux")
		assert.Equal(t, inspectArm64.Size, totalSizeByArch["arm64"])
	})
}
