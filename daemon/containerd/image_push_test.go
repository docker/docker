package containerd

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/moby/moby/v2/internal/testutil/specialimage"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

type pushTestCase struct {
	name               string
	indexPlatforms     []ocispec.Platform // all platforms supported by the image
	availablePlatforms []ocispec.Platform // platforms available locally
	requestPlatform    *ocispec.Platform  // platform requested by the client (not the platform selected for push!)
	check              func(t *testing.T, img c8dimages.Image, pushDescriptor ocispec.Descriptor, err error)
	daemonPlatform     *ocispec.Platform
}

func TestImagePushIndex(t *testing.T) {
	ctx := namespaces.WithNamespace(t.Context(), "testing-"+t.Name())

	csDir := t.TempDir()
	store := &blobsDirContentStore{blobs: filepath.Join(csDir, "blobs/sha256")}

	linuxAmd64 := platforms.MustParse("linux/amd64")
	darwinArm64 := platforms.MustParse("darwin/arm64")
	windowsAmd64 := platforms.MustParse("windows/amd64")

	linuxArm64 := platforms.MustParse("linux/arm64")
	linuxArmv5 := platforms.MustParse("linux/arm/v5")
	linuxArmv7 := platforms.MustParse("linux/arm/v7")

	// Image service will have the daemon host platform mocked to linux/amd64.
	// Unless test cases specify a different platform.
	defaultDaemonPlatform := linuxAmd64

	for _, tc := range []pushTestCase{
		// No explicit platform requested
		{
			name: "none requested, all present",

			indexPlatforms:     []ocispec.Platform{linuxAmd64, darwinArm64, windowsAmd64},
			availablePlatforms: []ocispec.Platform{linuxAmd64, darwinArm64, windowsAmd64},
			check:              wholeIndexSelected,
		},
		{
			name: "none requested, one present",

			indexPlatforms:     []ocispec.Platform{linuxAmd64, darwinArm64, windowsAmd64},
			availablePlatforms: []ocispec.Platform{linuxAmd64},
			check:              singleManifestSelected(linuxAmd64),
		},
		{
			name: "none requested, two present, daemon platform available",

			indexPlatforms:     []ocispec.Platform{linuxAmd64, darwinArm64, windowsAmd64},
			availablePlatforms: []ocispec.Platform{linuxAmd64, darwinArm64},
			check:              multipleCandidates,
		},
		{
			name: "none requested, two present, daemon platform NOT available",

			indexPlatforms:     []ocispec.Platform{linuxAmd64, darwinArm64, windowsAmd64},
			availablePlatforms: []ocispec.Platform{darwinArm64, windowsAmd64},
			check:              multipleCandidates,
		},

		// Specific platform requested
		{
			name: "linux/amd64 requested, all present",

			indexPlatforms:     []ocispec.Platform{linuxAmd64, darwinArm64, windowsAmd64},
			availablePlatforms: []ocispec.Platform{linuxAmd64, darwinArm64, windowsAmd64},
			requestPlatform:    &linuxAmd64,
			check:              singleManifestSelected(linuxAmd64),
		},
		{
			name: "linux/amd64 requested, but not present",

			indexPlatforms:     []ocispec.Platform{linuxAmd64, darwinArm64, windowsAmd64},
			availablePlatforms: []ocispec.Platform{darwinArm64, windowsAmd64},
			requestPlatform:    &linuxAmd64,
			check:              candidateNotFound,
		},

		// Variant tests
		{
			name: "linux/arm/v5 requested, but not in index",

			indexPlatforms:     []ocispec.Platform{linuxAmd64, linuxArmv7},
			availablePlatforms: []ocispec.Platform{linuxAmd64, linuxArmv7},
			requestPlatform:    &linuxArmv5,
			check:              candidateNotFound,
		},
		{
			name: "linux/arm/v5 requested, but not available",

			indexPlatforms:     []ocispec.Platform{linuxArm64, linuxArmv7, linuxArmv5},
			availablePlatforms: []ocispec.Platform{linuxArm64, linuxArmv7},
			requestPlatform:    &linuxArmv5,
			check:              candidateNotFound,
		},
		{
			name: "linux/arm/v7 requested, but not available",

			indexPlatforms:     []ocispec.Platform{linuxArm64, linuxArmv7, linuxArmv5},
			availablePlatforms: []ocispec.Platform{linuxArm64, linuxArmv5},
			requestPlatform:    &linuxArmv7,
			check:              candidateNotFound,
		},
		{
			name: "linux/arm/v7 requested on v7 daemon, but not available",

			indexPlatforms:     []ocispec.Platform{linuxArm64, linuxArmv7, linuxArmv5},
			availablePlatforms: []ocispec.Platform{linuxArm64, linuxArmv5},
			daemonPlatform:     &linuxArmv7,
			requestPlatform:    &linuxArmv7,
			check:              candidateNotFound,
		},
		{
			name: "linux/arm/v7 requested on v5 daemon, all available",

			indexPlatforms:     []ocispec.Platform{linuxArm64, linuxArmv7, linuxArmv5},
			availablePlatforms: []ocispec.Platform{linuxArm64, linuxArmv7, linuxArmv5},
			daemonPlatform:     &linuxArmv5,
			requestPlatform:    &linuxArmv7,
			check:              singleManifestSelected(linuxArmv7),
		},
		{
			name: "linux/arm/v5 requested on v7 daemon, all available",

			indexPlatforms:     []ocispec.Platform{linuxArm64, linuxArmv7, linuxArmv5},
			availablePlatforms: []ocispec.Platform{linuxArm64, linuxArmv7, linuxArmv5},
			daemonPlatform:     &linuxArmv7,
			requestPlatform:    &linuxArmv5,
			check:              singleManifestSelected(linuxArmv5),
		},
		{
			name: "none requested on v5 daemon, arm64 not available",

			indexPlatforms:     []ocispec.Platform{linuxArm64, linuxArmv7, linuxArmv5},
			availablePlatforms: []ocispec.Platform{linuxArmv7, linuxArmv5},
			daemonPlatform:     &linuxArmv5,
			requestPlatform:    nil,
			check:              multipleCandidates,
		},
		{
			name: "none requested on v7 daemon, arm64 not available",

			indexPlatforms:     []ocispec.Platform{linuxArm64, linuxArmv7, linuxArmv5},
			availablePlatforms: []ocispec.Platform{linuxArmv7, linuxArmv5},
			daemonPlatform:     &linuxArmv7,
			requestPlatform:    nil,
			check:              multipleCandidates,
		},
		{
			name: "none requested on v7 daemon, v7 not available",

			indexPlatforms:     []ocispec.Platform{linuxArm64, linuxArmv7, linuxArmv5},
			availablePlatforms: []ocispec.Platform{linuxArm64, linuxArmv5},
			daemonPlatform:     &linuxArmv7,
			requestPlatform:    nil,
			check:              multipleCandidates,
		},

		{
			name: "none requested on v7 daemon, v5 in index but not v7, all present",

			indexPlatforms:     []ocispec.Platform{linuxArm64, linuxArmv5},
			availablePlatforms: []ocispec.Platform{linuxArm64, linuxArmv5},
			daemonPlatform:     &linuxArmv7,
			requestPlatform:    nil,
			check:              wholeIndexSelected,
		},
		{
			name: "none requested on v7 daemon, v5 in index but not v7, v5 present",

			indexPlatforms:     []ocispec.Platform{linuxArm64, linuxArmv5},
			availablePlatforms: []ocispec.Platform{linuxArmv5},
			daemonPlatform:     &linuxArmv7,
			requestPlatform:    nil,
			check:              singleManifestSelected(linuxArmv5),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			imgSvc := fakeImageService(t, ctx, store)
			// Mock the daemon platform.
			daemonPlatform := defaultDaemonPlatform
			if tc.daemonPlatform != nil {
				daemonPlatform = *tc.daemonPlatform
			}
			imgSvc.defaultPlatformOverride = &daemonPlatform

			idx, _, err := specialimage.MultiPlatform(csDir, "multiplatform:latest", tc.indexPlatforms)
			assert.NilError(t, err)

			imgs := imagesFromIndex(idx)
			assert.Assert(t, is.Len(imgs, 1))

			img := imgs[0]
			_, err = imgSvc.images.Create(ctx, img)
			assert.NilError(t, err)

			for _, platform := range tc.indexPlatforms {
				if slices.ContainsFunc(tc.availablePlatforms, platforms.OnlyStrict(platform).Match) {
					continue
				}
				assert.NilError(t, deletePlatform(ctx, imgSvc, img, platform))
			}

			desc, err := imgSvc.getPushDescriptor(ctx, img, tc.requestPlatform)

			tc.check(t, img, desc, err)
		})
	}
}

func deletePlatform(ctx context.Context, imgSvc *ImageService, img c8dimages.Image, platform ocispec.Platform) error {
	var blobs []ocispec.Descriptor
	pm := platforms.OnlyStrict(platform)
	err := imgSvc.walkImageManifests(ctx, img, func(im *ImageManifest) error {
		imPlatform, err := im.ImagePlatform(ctx)
		if err != nil {
			return fmt.Errorf("failed to determine platform of image manifest %v: %w", im.Target(), err)
		}

		if !pm.Match(imPlatform) {
			return nil
		}

		return imgSvc.walkPresentChildren(ctx, im.Target(), func(ctx context.Context, d ocispec.Descriptor) error {
			blobs = append(blobs, d)
			return nil
		})
	})
	if err != nil {
		return fmt.Errorf("failed to walk image manifests: %w", err)
	}

	for _, d := range blobs {
		err := imgSvc.content.Delete(ctx, d.Digest)
		if err != nil {
			return fmt.Errorf("failed to delete blob %v: %w", d.Digest, err)
		}
	}

	return nil
}

// wholeIndexSelected asserts that the push descriptor candidate is for the whole index.
func wholeIndexSelected(t *testing.T, img c8dimages.Image, pushDescriptor ocispec.Descriptor, err error) {
	assert.NilError(t, err)
	assert.Check(t, is.Equal(pushDescriptor.Digest, img.Target.Digest))
}

// singleManifestSelected asserts that the push descriptor candidate is for a single platform-specific manifest.
func singleManifestSelected(platform ocispec.Platform) func(t *testing.T, img c8dimages.Image, pushDescriptor ocispec.Descriptor, err error) {
	pm := platforms.OnlyStrict(platform)
	return func(t *testing.T, img c8dimages.Image, pushDescriptor ocispec.Descriptor, err error) {
		assert.NilError(t, err)
		assert.Assert(t, is.Equal(pushDescriptor.MediaType, ocispec.MediaTypeImageManifest), "the push descriptor isn't for a manifest")
		assert.Assert(t, pushDescriptor.Platform != nil, "the push descriptor doesn't have a platform")
		assert.Assert(t, pm.Match(*pushDescriptor.Platform), "the push descriptor isn't for the selected platform")
	}
}

// candidateNotFound asserts that the no matching candidate was found.
func candidateNotFound(t *testing.T, _ c8dimages.Image, desc ocispec.Descriptor, err error) {
	assert.Check(t, cerrdefs.IsNotFound(err), "expected NotFound error, got %v, candidate: %v", err, desc.Platform)
}

// multipleCandidates asserts that multiple matching candidates were found and no decision could be made.
func multipleCandidates(t *testing.T, _ c8dimages.Image, desc ocispec.Descriptor, err error) {
	assert.Check(t, cerrdefs.IsConflict(err), "expected Conflict error, got %v, candidate: %v", err, desc.Platform)
}
