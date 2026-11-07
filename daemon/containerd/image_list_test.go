package containerd

import (
	"context"
	"math/rand"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/log/logtest"
	"github.com/containerd/platforms"
	imagetypes "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/server/imagebackend"
	"github.com/moby/moby/v2/internal/testutil/specialimage"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func imagesFromIndex(index ...*ocispec.Index) []c8dimages.Image {
	var imgs []c8dimages.Image
	for _, idx := range index {
		for _, desc := range idx.Manifests {
			imgs = append(imgs, c8dimages.Image{
				Name:   desc.Annotations[c8dimages.AnnotationImageName],
				Target: desc,
			})
		}
	}
	return imgs
}

func BenchmarkImageList(b *testing.B) {
	populateStore := func(ctx context.Context, is *ImageService, dir string,
		count int,
		// % chance for each image to spawn containers
		containerChance int,
		// Maximum container count if the image is decided to spawn containers (chance above)
		maxContainerCount int,
	) {
		// Use constant seed for reproducibility
		src := rand.NewSource(1982731263716)

		for i := range count {
			platform := platforms.DefaultSpec()

			// 20% is other architecture than the host
			if i%5 == 0 {
				platform.Architecture = "other"
			}

			idx, err := specialimage.RandomSinglePlatform(dir, platform, src)
			assert.NilError(b, err)

			r1 := int(src.Int63())
			r2 := int(src.Int63())

			imgs := imagesFromIndex(idx)
			for _, desc := range imgs {
				_, err := is.images.Create(ctx, desc)
				assert.NilError(b, err)

				if r1%100 >= containerChance {
					continue
				}

				containersCount := r2 % maxContainerCount
				for range containersCount {
					id := digest.FromString(desc.Name + strconv.Itoa(i)).String()

					target := desc.Target
					is.containers.Add(id, &container.Container{
						ID:            id,
						ImageManifest: &target,
					})
				}
			}
		}
	}

	for _, count := range []int{10, 100, 1000} {
		csDir := b.TempDir()

		ctx := namespaces.WithNamespace(b.Context(), "testing-"+strconv.Itoa(count))

		cs := &delayedStore{
			store:    &blobsDirContentStore{blobs: filepath.Join(csDir, "blobs/sha256")},
			overhead: 500 * time.Microsecond,
		}

		imgSvc := fakeImageService(b, ctx, cs)

		// Every generated image has a 10% chance to spawn up to 5 containers
		const containerChance = 10
		const maxContainerCount = 5
		populateStore(ctx, imgSvc, csDir, count, containerChance, maxContainerCount)

		b.Run(strconv.Itoa(count)+"-images", func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				_, err := imgSvc.Images(ctx, imagebackend.ListOptions{All: true, SharedSize: true})
				assert.NilError(b, err)
			}
		})
	}
}

func TestImageListCheckTotalSize(t *testing.T) {
	ctx := namespaces.WithNamespace(t.Context(), "testing")

	blobsDir := t.TempDir()
	cs := &blobsDirContentStore{blobs: filepath.Join(blobsDir, "blobs/sha256")}

	twoplatform, mfstsDescs, err := specialimage.MultiPlatform(blobsDir, "test:latest", []ocispec.Platform{
		{OS: "linux", Architecture: "arm64"},
		{OS: "linux", Architecture: "amd64"},
	})
	assert.NilError(t, err)

	ctx = logtest.WithT(ctx, t)
	service := fakeImageService(t, ctx, cs)

	img, err := service.images.Create(ctx, imagesFromIndex(twoplatform)[0])
	assert.NilError(t, err)

	all, err := service.Images(ctx, imagebackend.ListOptions{Manifests: true, SharedSize: true})
	assert.NilError(t, err)

	assert.Check(t, is.Len(all, 1))
	assert.Check(t, is.Len(all[0].Manifests, 2))

	// TODO: The test snapshotter doesn't do anything, so the size is always 0.
	assert.Check(t, is.Equal(all[0].Manifests[0].ImageData.Size.Unpacked, int64(0)))
	assert.Check(t, is.Equal(all[0].Manifests[1].ImageData.Size.Unpacked, int64(0)))

	mfstArm64 := mfstsDescs[0]
	mfstAmd64 := mfstsDescs[1]

	indexSize := blobSize(t, ctx, cs, twoplatform.Manifests[0].Digest)
	arm64ManifestSize := blobSize(t, ctx, cs, mfstArm64.Digest)
	amd64ManifestSize := blobSize(t, ctx, cs, mfstAmd64.Digest)

	var arm64Mfst, amd64Mfst ocispec.Manifest
	assert.NilError(t, readJSON(ctx, cs, mfstArm64, &arm64Mfst))
	assert.NilError(t, readJSON(ctx, cs, mfstAmd64, &amd64Mfst))

	// MultiPlatform should produce a single layer. If these fail, the test needs to be adjusted.
	assert.Assert(t, is.Len(arm64Mfst.Layers, 1))
	assert.Assert(t, is.Len(amd64Mfst.Layers, 1))

	arm64ConfigSize := blobSize(t, ctx, cs, arm64Mfst.Config.Digest)
	amd64ConfigSize := blobSize(t, ctx, cs, amd64Mfst.Config.Digest)

	arm64LayerSize := blobSize(t, ctx, cs, arm64Mfst.Layers[0].Digest)
	amd64LayerSize := blobSize(t, ctx, cs, amd64Mfst.Layers[0].Digest)

	allTotalSize := indexSize +
		arm64ManifestSize + amd64ManifestSize +
		arm64ConfigSize + amd64ConfigSize +
		arm64LayerSize + amd64LayerSize

	assert.Check(t, is.Equal(all[0].Size, allTotalSize-indexSize))

	assert.Check(t, is.Equal(all[0].Manifests[0].Size.Content, arm64ManifestSize+arm64ConfigSize+arm64LayerSize))
	assert.Check(t, is.Equal(all[0].Manifests[1].Size.Content, amd64ManifestSize+amd64ConfigSize+amd64LayerSize))

	// TODO: This should also include the Size.Unpacked, but the test snapshotter doesn't do anything yet
	assert.Check(t, is.Equal(all[0].Manifests[0].Size.Total, amd64ManifestSize+amd64ConfigSize+amd64LayerSize))
	assert.Check(t, is.Equal(all[0].Manifests[1].Size.Total, amd64ManifestSize+amd64ConfigSize+amd64LayerSize))

	t.Run("without layers", func(t *testing.T) {
		var layers []ocispec.Descriptor
		err = service.walkPresentChildren(ctx, img.Target, func(ctx context.Context, desc ocispec.Descriptor) error {
			if c8dimages.IsLayerType(desc.MediaType) {
				layers = append(layers, desc)
			}
			return nil
		})
		assert.NilError(t, err)

		for _, layer := range layers {
			err := cs.Delete(ctx, layer.Digest)
			assert.NilError(t, err, "failed to delete layer %s", layer.Digest)
		}

		all, err := service.Images(ctx, imagebackend.ListOptions{Manifests: true, SharedSize: true})
		assert.NilError(t, err)

		assert.Assert(t, is.Len(all, 1))
		assert.Check(t, is.Equal(all[0].Size, allTotalSize-indexSize-arm64LayerSize-amd64LayerSize))

		assert.Assert(t, is.Len(all[0].Manifests, 2))
		assert.Check(t, is.Equal(all[0].Manifests[0].Size.Content, arm64ManifestSize+arm64ConfigSize))
		assert.Check(t, is.Equal(all[0].Manifests[1].Size.Content, amd64ManifestSize+amd64ConfigSize))
	})
}

func TestImageDiskUsageWithIndexTarget(t *testing.T) {
	ctx := namespaces.WithNamespace(t.Context(), "testing-"+t.Name())

	blobsDir := t.TempDir()
	idx, _, err := specialimage.MultiPlatform(blobsDir, "test:latest", []ocispec.Platform{
		{OS: "linux", Architecture: "arm64"},
		{OS: "linux", Architecture: "amd64"},
	})
	assert.NilError(t, err)

	cs := &blobsDirContentStore{blobs: filepath.Join(blobsDir, "blobs/sha256")}
	service := fakeImageService(t, ctx, cs)

	img, err := service.images.Create(ctx, imagesFromIndex(idx)[0])
	assert.NilError(t, err)

	var expected int64
	visited := map[digest.Digest]struct{}{}
	err = service.walkPresentChildren(ctx, img.Target, func(_ context.Context, desc ocispec.Descriptor) error {
		if _, ok := visited[desc.Digest]; ok {
			return nil
		}
		visited[desc.Digest] = struct{}{}

		expected += desc.Size
		return nil
	})
	assert.NilError(t, err)

	usage, err := service.ImageDiskUsage(ctx)
	assert.NilError(t, err)
	assert.Check(t, is.Equal(usage, expected))
}

func blobSize(t *testing.T, ctx context.Context, cs content.Store, dgst digest.Digest) int64 {
	info, err := cs.Info(ctx, dgst)
	assert.NilError(t, err)
	return info.Size
}

func TestImageListIdentityUsesCacheOnly(t *testing.T) {
	ctx := namespaces.WithNamespace(t.Context(), "testing-"+t.Name())

	blobsDir := t.TempDir()
	idx, err := specialimage.MultiLayer(blobsDir)
	assert.NilError(t, err)

	cs := &blobsDirContentStore{blobs: filepath.Join(blobsDir, "blobs/sha256")}
	service := fakeImageService(t, ctx, cs)

	_, err = service.images.Create(ctx, imagesFromIndex(idx)[0])
	assert.NilError(t, err)

	all, err := service.Images(ctx, imagebackend.ListOptions{Identity: true})
	assert.NilError(t, err)
	assert.Check(t, is.Len(all, 1))
	assert.Check(t, is.Len(all[0].Manifests, 1))
	assert.Check(t, all[0].Manifests[0].ImageData != nil)
	assert.Check(t, all[0].Manifests[0].ImageData.Identity == nil)
}

func TestImageListIdentityIsManifestScoped(t *testing.T) {
	ctx := namespaces.WithNamespace(t.Context(), "testing-"+t.Name())

	blobsDir := t.TempDir()
	idx, err := specialimage.MultiLayer(blobsDir)
	assert.NilError(t, err)

	cs := &blobsDirContentStore{blobs: filepath.Join(blobsDir, "blobs/sha256")}
	service := fakeImageService(t, ctx, cs)

	img, err := service.images.Create(ctx, imagesFromIndex(idx)[0])
	assert.NilError(t, err)

	summary, err := service.multiPlatformSummary(ctx, img, matchAnyWithPreference(platforms.Default(), nil))
	assert.NilError(t, err)
	assert.Assert(t, is.Len(summary.manifestIdentityRefs, 1))

	var ref manifestIdentityRef
	for _, candidate := range summary.manifestIdentityRefs {
		ref = candidate
		break
	}

	key := imageIdentityCacheKey(img.Target.Digest.String(), ref.manifest.Target().Digest.String(), platforms.FormatAll(ref.platform))
	assert.NilError(t, service.updateImageIdentityCache(ctx, key, &imagetypes.SignatureIdentity{Name: "cached"}, time.Minute))

	all, err := service.Images(ctx, imagebackend.ListOptions{Identity: true})
	assert.NilError(t, err)
	assert.Check(t, is.Len(all, 1))
	assert.Check(t, is.Len(all[0].Manifests, 1))
	if assert.Check(t, all[0].Manifests[0].ImageData != nil) && assert.Check(t, all[0].Manifests[0].ImageData.Identity != nil) {
		assert.Check(t, is.Len(all[0].Manifests[0].ImageData.Identity.Signature, 1))
		assert.Check(t, is.Equal(all[0].Manifests[0].ImageData.Identity.Signature[0].Name, "cached"))
	}
}

func TestImageList(t *testing.T) {
	ctx := namespaces.WithNamespace(t.Context(), "testing")

	blobsDir := t.TempDir()

	toContainerdImage := func(t *testing.T, imageFunc specialimage.SpecialImageFunc) c8dimages.Image {
		idx, err := imageFunc(blobsDir)
		assert.NilError(t, err)

		return imagesFromIndex(idx)[0]
	}

	multilayer := toContainerdImage(t, specialimage.MultiLayer)
	twoplatform := toContainerdImage(t, specialimage.TwoPlatform)
	emptyIndex := toContainerdImage(t, specialimage.EmptyIndex)
	configTarget := toContainerdImage(t, specialimage.ConfigTarget)
	textplain := toContainerdImage(t, specialimage.TextPlain)
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

	for _, tc := range []struct {
		name   string
		images []c8dimages.Image

		check func(*testing.T, []imagetypes.Summary)
	}{
		{
			name:   "one multi-layer image",
			images: []c8dimages.Image{multilayer},
			check: func(t *testing.T, all []imagetypes.Summary) {
				assert.Check(t, is.Len(all, 1))

				if assert.Check(t, all[0].Descriptor != nil) {
					assert.Check(t, is.DeepEqual(*all[0].Descriptor, multilayer.Target))
				}
				assert.Check(t, is.Equal(all[0].ID, multilayer.Target.Digest.String()))
				assert.Check(t, is.DeepEqual(all[0].RepoTags, []string{"multilayer:latest"}))

				assert.Check(t, is.Len(all[0].Manifests, 1))
				assert.Check(t, all[0].Manifests[0].Available)
				assert.Check(t, is.Equal(all[0].Manifests[0].Kind, imagetypes.ManifestKindImage))
			},
		},
		{
			name:   "one image with two platforms is still one entry",
			images: []c8dimages.Image{twoplatform},
			check: func(t *testing.T, all []imagetypes.Summary) {
				assert.Check(t, is.Len(all, 1))

				if assert.Check(t, all[0].Descriptor != nil) {
					assert.Check(t, is.DeepEqual(*all[0].Descriptor, twoplatform.Target))
				}
				assert.Check(t, is.Equal(all[0].ID, twoplatform.Target.Digest.String()))

				assert.Check(t, is.DeepEqual(all[0].RepoTags, []string{"twoplatform:latest"}))

				i := all[0]
				assert.Check(t, is.Len(i.Manifests, 2))

				assert.Check(t, is.Equal(i.Manifests[0].Kind, imagetypes.ManifestKindImage))
				if assert.Check(t, i.Manifests[0].ImageData != nil) {
					assert.Check(t, is.Equal(i.Manifests[0].ImageData.Platform.Architecture, "amd64"))
				}
				assert.Check(t, is.Equal(i.Manifests[1].Kind, imagetypes.ManifestKindImage))
				if assert.Check(t, i.Manifests[1].ImageData != nil) {
					assert.Check(t, is.Equal(i.Manifests[1].ImageData.Platform.Architecture, "arm64"))
				}
			},
		},
		{
			name:   "two images are two entries",
			images: []c8dimages.Image{multilayer, twoplatform},
			check: func(t *testing.T, all []imagetypes.Summary) {
				assert.Check(t, is.Len(all, 2))

				if assert.Check(t, all[0].Descriptor != nil) {
					assert.Check(t, is.DeepEqual(*all[0].Descriptor, multilayer.Target))
				}
				assert.Check(t, is.Equal(all[0].ID, multilayer.Target.Digest.String()))

				assert.Check(t, is.DeepEqual(all[0].RepoTags, []string{"multilayer:latest"}))

				if assert.Check(t, all[1].Descriptor != nil) {
					assert.Check(t, is.DeepEqual(*all[1].Descriptor, twoplatform.Target))
				}
				assert.Check(t, is.Equal(all[1].ID, twoplatform.Target.Digest.String()))

				assert.Check(t, is.DeepEqual(all[1].RepoTags, []string{"twoplatform:latest"}))

				assert.Check(t, is.Len(all[0].Manifests, 1))
				assert.Check(t, is.Len(all[1].Manifests, 2))

				assert.Check(t, is.Equal(all[0].Manifests[0].Kind, imagetypes.ManifestKindImage))

				assert.Check(t, is.Equal(all[1].Manifests[0].Kind, imagetypes.ManifestKindImage))
				assert.Check(t, is.Equal(all[1].Manifests[1].Kind, imagetypes.ManifestKindImage))
			},
		},
		{
			name:   "three images, one is an empty index",
			images: []c8dimages.Image{multilayer, emptyIndex, twoplatform},
			check: func(t *testing.T, all []imagetypes.Summary) {
				assert.Check(t, is.Len(all, 3))
			},
		},
		{
			name:   "one good image, second has config as a target",
			images: []c8dimages.Image{multilayer, configTarget},
			check: func(t *testing.T, all []imagetypes.Summary) {
				assert.Check(t, is.Len(all, 2))

				sort.Slice(all, func(i, j int) bool {
					return slices.Contains(all[i].RepoTags, "multilayer:latest")
				})

				if assert.Check(t, all[0].Descriptor != nil) {
					assert.Check(t, is.DeepEqual(*all[0].Descriptor, multilayer.Target))
				}
				assert.Check(t, is.Equal(all[0].ID, multilayer.Target.Digest.String()))

				assert.Check(t, is.Len(all[0].Manifests, 1))

				if assert.Check(t, all[1].Descriptor != nil) {
					assert.Check(t, is.DeepEqual(*all[1].Descriptor, configTarget.Target))
				}
				assert.Check(t, is.Len(all[1].Manifests, 0))
			},
		},
		{
			name:   "a non-container image manifest",
			images: []c8dimages.Image{textplain},
			check: func(t *testing.T, all []imagetypes.Summary) {
				assert.Check(t, is.Len(all, 1))

				if assert.Check(t, all[0].Descriptor != nil) {
					assert.Check(t, is.DeepEqual(*all[0].Descriptor, textplain.Target))
				}
				assert.Check(t, is.Equal(all[0].ID, textplain.Target.Digest.String()))

				assert.Assert(t, is.Len(all[0].Manifests, 1))
			},
		},
		{
			name:   "multi-platform with no platforms available locally",
			images: []c8dimages.Image{missingMultiPlatform},
			check: func(t *testing.T, all []imagetypes.Summary) {
				assert.Assert(t, is.Len(all, 1))
				assert.Check(t, is.Len(all[0].Manifests, 2))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := logtest.WithT(ctx, t)
			service := fakeImageService(t, ctx, cs)

			for _, img := range tc.images {
				_, err := service.images.Create(ctx, img)
				assert.NilError(t, err)
			}

			opts := imagebackend.ListOptions{
				Manifests:  true,
				SharedSize: true,
			}
			all, err := service.Images(ctx, opts)
			assert.NilError(t, err)

			sort.Slice(all, func(i, j int) bool {
				firstTag := func(idx int) string {
					if len(all[idx].RepoTags) > 0 {
						return all[idx].RepoTags[0]
					}
					return ""
				}
				return firstTag(i) < firstTag(j)
			})

			tc.check(t, all)
		})
	}
}

func TestComputeSharedSizeIncludesSharedContentBlobs(t *testing.T) {
	sharedChainID := digest.FromString("shared-chain")
	uniqueChainID := digest.FromString("unique-chain")

	sharedBlob := ocispec.Descriptor{
		Digest: digest.FromString("shared-blob"),
		Size:   20,
	}
	uniqueBlob := ocispec.Descriptor{
		Digest: digest.FromString("unique-blob"),
		Size:   30,
	}

	sizeFn := func(d digest.Digest) (int64, error) {
		switch d {
		case sharedChainID:
			return 100, nil
		case uniqueChainID:
			return 200, nil
		default:
			t.Fatalf("unexpected chainID: %s", d)
			return 0, nil
		}
	}

	sharedSize, err := computeSharedSize(
		sharedSizeData{
			chainIDs: []digest.Digest{sharedChainID, uniqueChainID},
			content: []ocispec.Descriptor{
				sharedBlob,
				sharedBlob, // duplicate reference within the same image should not double count.
				uniqueBlob,
			},
		},
		map[digest.Digest]int{
			sharedChainID: 2,
			uniqueChainID: 1,
		},
		map[digest.Digest]int{
			sharedBlob.Digest: 2,
			uniqueBlob.Digest: 1,
		},
		sizeFn,
	)

	assert.NilError(t, err)
	assert.Check(t, is.Equal(sharedSize, int64(120)))
}
