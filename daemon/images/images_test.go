package images

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func testListImages(ctx context.Context, t *testing.T, is *ImageService) {
	type testImage struct {
		names []string
		image construct
		// TODO(containerd): unpack
		// TODO(containerd): parent index
	}
	type imageCheck func(*testing.T, *types.ImageSummary, []ocispec.Descriptor)

	type checkOpt func(*types.ImageSummary, []ocispec.Descriptor)

	withID := func(i int) checkOpt {
		return func(s *types.ImageSummary, images []ocispec.Descriptor) {
			s.ID = images[i].Digest.String()
		}
	}

	withTags := func(tags ...string) checkOpt {
		return func(s *types.ImageSummary, images []ocispec.Descriptor) {
			s.RepoTags = tags
		}
	}

	withDigests := func(i int, tags ...string) checkOpt {
		return func(s *types.ImageSummary, images []ocispec.Descriptor) {
			digests := make([]string, len(tags))
			for j := range tags {
				digests[j] = fmt.Sprintf("%s@%s", tags[j], images[i].Digest.String())
			}
			s.RepoDigests = digests
		}
	}

	check := func(opts ...checkOpt) imageCheck {
		return func(t *testing.T, a *types.ImageSummary, images []ocispec.Descriptor) {
			t.Helper()

			var e types.ImageSummary
			for _, opt := range opts {
				opt(&e, images)
			}

			if e.ID != "" && e.ID != a.ID {
				t.Errorf("unexpected id: expected %s, actual %s", e.ID, a.ID)
			}
			if e.RepoTags != nil && !reflect.DeepEqual(e.RepoTags, a.RepoTags) {
				t.Errorf("unexpected tags:, expected %v, actual %v", e.RepoTags, a.RepoTags)
			}
			if e.RepoDigests != nil && !reflect.DeepEqual(e.RepoDigests, a.RepoDigests) {
				t.Errorf("unexpected digests:, expected %v, actual %v", e.RepoDigests, a.RepoDigests)
			}
		}
	}

	type testCase struct {
		name     string
		images   []testImage
		expected []imageCheck
		// TODO(containerd): filters, all, extra args
	}

	for _, tcase := range []testCase{
		{
			name: "SingleImageSingleTag",
			images: []testImage{
				{
					names: []string{"docker.io/library/someimage:latest"},
					image: randomManifest(1),
				},
			},
			expected: []imageCheck{
				check(withID(0), withTags("someimage:latest"), withDigests(0, "someimage:latest")),
			},
		},
		{
			name: "MultiImageSingleTag",
			images: []testImage{
				{
					names: []string{"docker.io/library/someimage:latest"},
					image: randomManifest(1),
				},
				{
					names: []string{"docker.io/library/someimage:latest"},
					image: randomManifest(2),
				},
			},
			expected: []imageCheck{
				check(withID(1), withTags("someimage:latest"), withDigests(1, "someimage:latest")),
			},
		},
	} {
		ctx, cleanup, err := is.client.WithLease(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var created []string
		t.Run(tcase.name, func(t *testing.T) {
			var imgs []ocispec.Descriptor
			cis := is.client.ImageService()
			for _, imagec := range tcase.images {
				var desc ocispec.Descriptor
				if err := imagec.image(&desc)(ctx, is.client.ContentStore()); err != nil {
					t.Fatal(err)
				}

				for _, name := range imagec.names {
					img := images.Image{
						Name:   name,
						Target: desc,
					}
					_, err = cis.Create(ctx, img)
					if err != nil {
						if !errdefs.IsAlreadyExists(err) {
							t.Fatal(err)
						}
						if _, err := cis.Update(ctx, img); err != nil {
							t.Fatal(err)
						}
					} else {
						created = append(created, img.Name)
					}

					img.Name = img.Name + "@" + desc.Digest.String()
					_, err = cis.Create(ctx, img)
					if err != nil {
						t.Fatal(err)
					}
					created = append(created, img.Name)

				}
				// TODO(containerd): Unpack image?
				// TODO(containerd): Set parent
				imgs = append(imgs, desc)
			}

			listed, err := is.Images(ctx, filters.NewArgs(), false, false)
			if err != nil {
				t.Fatal(err)
			}

			if len(listed) != len(tcase.expected) {
				t.Fatalf("unexpected number of images: expected %d, actual %d", len(tcase.expected), len(listed))
			}

			for i := range listed {
				tcase.expected[i](t, listed[i], imgs)
			}
		})
		if err := cleanup(ctx); err != nil {
			t.Fatal(err)
		}
		cis := is.client.ImageService()
		for i, name := range created {
			var opts []images.DeleteOpt
			if i == len(created)-1 {
				opts = append(opts, images.SynchronousDelete())
			}
			if err := cis.Delete(ctx, name, opts...); err != nil && !errdefs.IsNotFound(err) {
				t.Fatal(err)
			}
		}
	}
}
