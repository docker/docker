package images

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/docker/docker/container"
	"github.com/docker/docker/layer"
	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func testDeleteImages(ctx context.Context, t *testing.T, is *ImageService) {

	type testImage struct {
		names []string
		image construct

		// Index of parent relative to current image, must be negative
		parent int

		// Tags expected after deletion
		expected []string

		// Whether the image object is expected to exist after deletion
		deleted bool

		// Whether layers are expected to exist after deletion
		layers bool
	}
	type testDelete struct {
		ref      string
		id       int // index of image to delete, if ref is empty
		force    bool
		prune    bool
		untagged []string
		deleted  []int // indexs of images deleted
	}

	type testCase struct {
		name       string
		images     []testImage
		deletes    []testDelete
		containers []*container.Container
	}

	for _, tc := range []testCase{
		{
			name: "RemoveSingleTags",
			images: []testImage{
				{
					names:   []string{"docker.io/library/img1:latest"},
					image:   randomManifest(2),
					deleted: true,
				},
				{
					names:    []string{"docker.io/library/img2:latest"},
					image:    randomManifest(2),
					expected: []string{"docker.io/library/img2:latest"},
					layers:   true,
				},
				{
					names:    []string{"docker.io/library/img3:latest", "docker.io/library/img4:latest"},
					image:    randomManifest(3),
					expected: []string{"docker.io/library/img4:latest"},
					layers:   true,
				},
			},
			deletes: []testDelete{
				{
					ref:      "img1:latest",
					untagged: []string{"img1:latest", "img1:latest@0"},
					deleted:  []int{0},
				},
				{
					ref:      "img3:latest",
					untagged: []string{"img3:latest", "img3:latest@2"},
				},
			},
		},
		{
			name: "RemoveParentFirst",
			images: []testImage{
				{
					names:   []string{"docker.io/library/img1:latest"},
					image:   randomManifest(2),
					deleted: true,
				},
				{
					names:   []string{"docker.io/library/img2:latest"},
					image:   randomManifest(2),
					parent:  -1,
					deleted: true,
				},
			},
			deletes: []testDelete{
				{
					ref:      "img1:latest",
					untagged: []string{"img1:latest", "img1:latest@0"},
					deleted:  []int{0},
				},
				{
					ref:      "img2:latest",
					untagged: []string{"img2:latest", "img2:latest@1"},
					deleted:  []int{1},
				},
			},
		},
		{
			name: "RemoveChild",
			images: []testImage{
				{
					names:    []string{"docker.io/library/img1:latest"},
					image:    randomManifest(2),
					expected: []string{"docker.io/library/img1:latest"},
					layers:   true,
				},
				{
					names:   []string{"docker.io/library/img2:latest"},
					image:   randomManifest(2),
					parent:  -1,
					deleted: true,
				},
			},
			deletes: []testDelete{
				{
					ref:      "img2:latest",
					untagged: []string{"img2:latest", "img2:latest@1"},
					deleted:  []int{1},
				},
			},
		},
		{
			name: "RemoveParent",
			images: []testImage{
				{
					names:   []string{"docker.io/library/img1:latest"},
					image:   randomManifest(2),
					deleted: true,
					layers:  true,
				},
				{
					names:    []string{"docker.io/library/img2:latest"},
					expected: []string{"docker.io/library/img2:latest"},
					image:    randomManifest(2),
					parent:   -1,
					layers:   true,
				},
			},
			deletes: []testDelete{
				{
					ref:      "img1:latest",
					untagged: []string{"img1:latest", "img1:latest@0"},
					deleted:  []int{0},
				},
			},
		},
	} {

		var created []string
		t.Run(tc.name, func(t *testing.T) {
			var imgs []ocispec.Descriptor
			type finalState struct {
				digest  digest.Digest
				deleted bool

				layersDeleted bool
				// TODO(containerd): store by platform
				config  digest.Digest
				diffIDs []digest.Digest
			}
			var states []finalState

			expected := map[string]*ocispec.Descriptor{}

			cis := is.client.ImageService()
			cs := is.client.ContentStore()
			ctx, cleanup, err := is.client.WithLease(ctx)
			if err != nil {
				t.Fatal(err)
			}

			for _, imagec := range tc.images {
				var desc ocispec.Descriptor
				if err := imagec.image(&desc)(ctx, cs); err != nil {
					t.Error(err)
					break
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
						expected[img.Name] = nil
					}

					img.Name = img.Name + "@" + desc.Digest.String()
					_, err = cis.Create(ctx, img)
					if err != nil {
						t.Error(err)
						break
					}
					created = append(created, img.Name)
					expected[img.Name] = nil
				}

				for _, tag := range imagec.expected {
					expected[tag] = &desc
					expected[tag+"@"+desc.Digest.String()] = &desc
				}

				// TODO(containerd): Handle multiplatform cases (for each?)
				m, err := images.Manifest(ctx, cs, desc, is.platforms)
				if err != nil {
					t.Fatal(err)
				}

				if err := is.unpack(ctx, m.Config, m.Layers, nil, nil, nil); err != nil {
					t.Fatal(err)
				}

				if imagec.parent < 0 {
					parentImg := states[len(states)+imagec.parent]

					info := content.Info{
						Digest: m.Config.Digest,
						Labels: map[string]string{
							LabelImageParent: parentImg.config.String(),
						},
					}
					_, err := cs.Update(ctx, info, "labels."+LabelImageParent)
					if err != nil {
						t.Fatal(err)
					}
				}

				diffIDs, err := images.RootFS(ctx, cs, m.Config)
				if err != nil {
					t.Fatal(err)
				}
				states = append(states, finalState{
					digest:  desc.Digest,
					deleted: imagec.deleted,

					layersDeleted: !imagec.layers,
					config:        m.Config.Digest,
					diffIDs:       diffIDs,
				})

				imgs = append(imgs, desc)
			}
			if err := cleanup(ctx); err != nil {
				t.Fatal(err)
			}
			if t.Failed() {
				t.FailNow()
			}

			is.containers = mockContainerStore{tc.containers}
			for _, del := range tc.deletes {
				ref := del.ref
				if ref == "" {
					ref = imgs[del.id].Digest.String()
				}
				items, err := is.ImageDelete(ctx, ref, del.force, del.prune)
				if err != nil {
					t.Fatal(err)
				}
				if expected := len(del.deleted) + len(del.untagged); len(items) != expected {
					t.Errorf("Wrong number of items: expected %d, actual %v", expected, items)
				} else {
					untags := map[string]struct{}{}
					for _, ut := range del.untagged {
						untags[formatTag(ut, imgs)] = struct{}{}
					}
					deletes := map[string]struct{}{}
					for _, idx := range del.deleted {
						deletes[imgs[idx].Digest.String()] = struct{}{}
					}
					for _, item := range items {
						if item.Deleted != "" {
							if _, ok := deletes[item.Deleted]; !ok {
								t.Errorf("Unexpected delete: %s", item.Deleted)
							} else {
								delete(deletes, item.Deleted)
							}
						}
						if item.Untagged != "" {
							if _, ok := untags[item.Untagged]; !ok {
								t.Errorf("Unexpected untag: %s", item.Untagged)
							} else {
								delete(untags, item.Untagged)
							}
						}
					}
				}
			}

			for _, state := range states {
				_, err := cs.Info(ctx, state.digest)
				if err != nil {
					if !errdefs.IsNotFound(err) {
						t.Fatal(err)
					}
					if !state.deleted {
						t.Errorf("Missing image %s", state.digest)
					}
				} else if state.deleted {
					t.Errorf("Expected image %s to be deleted", state.digest)
				}
				if len(state.diffIDs) > 0 {
					chainID := identity.ChainID(state.diffIDs)
					ls := is.layerStores["vfs"]
					l, err := ls.Get(layer.ChainID(chainID))
					if err != nil {
						if err != layer.ErrLayerDoesNotExist {
							t.Fatal(err)
						}
						if !state.layersDeleted {
							t.Errorf("Missing image %s layer", state.digest)
						}
					} else {
						layer.ReleaseAndLog(ls, l)
						if state.layersDeleted {
							t.Errorf("Expected image %s layers to be deleted", state.digest)
						}
					}
				}
			}

			istore := is.client.ImageService()
			for name, desc := range expected {
				img, err := istore.Get(ctx, name)
				if err != nil {
					if !errdefs.IsNotFound(err) {
						t.Fatal(err)
					}
					if desc != nil {
						t.Errorf("Missing tag %s", name)
					}
				} else if desc == nil {
					t.Errorf("Expected tag %s to be deleted", name)
				} else if desc.Digest != img.Target.Digest {
					t.Errorf("Wrong tag for %s: got %s, expected %s", name, img.Target.Digest, desc.Digest)
				}
			}

		})
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

func formatTag(t string, imgs []ocispec.Descriptor) string {
	if i := strings.IndexByte(t, '@'); i >= 0 {
		idx, err := strconv.Atoi(t[i+1:])
		if err != nil {
			panic(err)
		}
		t = t[:i+1] + imgs[idx].Digest.String()
	}
	return t
}

type mockContainerStore struct {
	containers []*container.Container
}

func (mockContainerStore) First(container.StoreFilter) *container.Container {
	return nil
}

func (s mockContainerStore) List() []*container.Container {
	return s.containers
}

func (mockContainerStore) Get(string) *container.Container {
	return nil
}
