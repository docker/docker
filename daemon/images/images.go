package images // import "github.com/docker/docker/daemon/images"

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/pkg/errors"

	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/container"
	"github.com/docker/docker/image"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/system"
)

var acceptedImageFilterTags = map[string]bool{
	"dangling":  true,
	"label":     true,
	"before":    true,
	"since":     true,
	"reference": true,
}

// byCreated is a temporary type used to sort a list of images by creation
// time.
type byCreated []*types.ImageSummary

func (r byCreated) Len() int           { return len(r) }
func (r byCreated) Swap(i, j int)      { r[i], r[j] = r[j], r[i] }
func (r byCreated) Less(i, j int) bool { return r[i].Created < r[j].Created }

// Map returns a map of all images in the ImageStore
func (i *ImageService) Map() map[image.ID]*image.Image {
	return i.imageStore.Map()
}

// Images returns a filtered list of images.
func (i *ImageService) Images(_ context.Context, opts types.ImageListOptions) ([]*types.ImageSummary, error) {
	if err := opts.Filters.Validate(acceptedImageFilterTags); err != nil {
		return nil, err
	}

	var danglingOnly bool
	if opts.Filters.Contains("dangling") {
		if opts.Filters.ExactMatch("dangling", "true") {
			danglingOnly = true
		} else if !opts.Filters.ExactMatch("dangling", "false") {
			return nil, invalidFilter{"dangling", opts.Filters.Get("dangling")}
		}
	}

	var (
		beforeFilter, sinceFilter *image.Image
		err                       error
	)
	err = opts.Filters.WalkValues("before", func(value string) error {
		beforeFilter, err = i.GetImage(value, nil)
		return err
	})
	if err != nil {
		return nil, err
	}

	err = opts.Filters.WalkValues("since", func(value string) error {
		sinceFilter, err = i.GetImage(value, nil)
		return err
	})
	if err != nil {
		return nil, err
	}

	var selectedImages map[image.ID]*image.Image
	if danglingOnly {
		selectedImages = i.imageStore.Heads()
	} else {
		selectedImages = i.imageStore.Map()
	}

	var (
		summaries     = make([]*types.ImageSummary, 0, len(selectedImages))
		summaryMap    map[*image.Image]*types.ImageSummary
		allContainers []*container.Container
	)
	for id, img := range selectedImages {
		if beforeFilter != nil {
			if img.Created.Equal(beforeFilter.Created) || img.Created.After(beforeFilter.Created) {
				continue
			}
		}

		if sinceFilter != nil {
			if img.Created.Equal(sinceFilter.Created) || img.Created.Before(sinceFilter.Created) {
				continue
			}
		}

		if opts.Filters.Contains("label") {
			// Very old image that do not have image.Config (or even labels)
			if img.Config == nil {
				continue
			}
			// We are now sure image.Config is not nil
			if !opts.Filters.MatchKVList("label", img.Config.Labels) {
				continue
			}
		}

		// Skip any images with an unsupported operating system to avoid a potential
		// panic when indexing through the layerstore. Don't error as we want to list
		// the other images. This should never happen, but here as a safety precaution.
		if !system.IsOSSupported(img.OperatingSystem()) {
			continue
		}

		var size int64
		if layerID := img.RootFS.ChainID(); layerID != "" {
			l, err := i.layerStore.Get(layerID)
			if err != nil {
				// The layer may have been deleted between the call to `Map()` or
				// `Heads()` and the call to `Get()`, so we just ignore this error
				if err == layer.ErrLayerDoesNotExist {
					continue
				}
				return nil, err
			}

			size, err = l.Size()
			layer.ReleaseAndLog(i.layerStore, l)
			if err != nil {
				return nil, err
			}
		}

		summary := newImageSummary(img, size)

		for _, ref := range i.referenceStore.References(id.Digest()) {
			if opts.Filters.Contains("reference") {
				var found bool
				var matchErr error
				for _, pattern := range opts.Filters.Get("reference") {
					found, matchErr = reference.FamiliarMatch(pattern, ref)
					if matchErr != nil {
						return nil, matchErr
					}
					if found {
						break
					}
				}
				if !found {
					continue
				}
			}
			if _, ok := ref.(reference.Canonical); ok {
				summary.RepoDigests = append(summary.RepoDigests, reference.FamiliarString(ref))
			}
			if _, ok := ref.(reference.NamedTagged); ok {
				summary.RepoTags = append(summary.RepoTags, reference.FamiliarString(ref))
			}
		}
		if summary.RepoDigests == nil && summary.RepoTags == nil {
			if opts.All || len(i.imageStore.Children(id)) == 0 {

				if opts.Filters.Contains("dangling") && !danglingOnly {
					// dangling=false case, so dangling image is not needed
					continue
				}
				if opts.Filters.Contains("reference") { // skip images with no references if filtering by reference
					continue
				}
				summary.RepoDigests = []string{"<none>@<none>"}
				summary.RepoTags = []string{"<none>:<none>"}
			} else {
				continue
			}
		} else if danglingOnly && len(summary.RepoTags) > 0 {
			continue
		}

		if opts.ContainerCount {
			// Lazily init allContainers.
			if allContainers == nil {
				allContainers = i.containers.List()
			}

			// Get container count
			var containers int64
			for _, c := range allContainers {
				if c.ImageID == id {
					containers++
				}
			}
			// NOTE: By default, Containers is -1, or "not set"
			summary.Containers = containers
		}

		if opts.ContainerCount || opts.SharedSize {
			// Lazily init summaryMap.
			if summaryMap == nil {
				summaryMap = make(map[*image.Image]*types.ImageSummary, len(selectedImages))
			}
			summaryMap[img] = summary
		}
		summaries = append(summaries, summary)
	}

	if opts.SharedSize {
		allLayers := i.layerStore.Map()
		layerRefs := make(map[layer.ChainID]int, len(allLayers))

		allImages := selectedImages
		if danglingOnly {
			// If danglingOnly is true, then selectedImages include only dangling images,
			// but we need to consider all existing images to correctly perform reference counting.
			// If danglingOnly is false, selectedImages (and, hence, allImages) is already equal to i.imageStore.Map()
			// and we can avoid performing an otherwise redundant method call.
			allImages = i.imageStore.Map()
		}
		// Count layer references across all known images
		for _, img := range allImages {
			rootFS := *img.RootFS
			rootFS.DiffIDs = nil
			for _, id := range img.RootFS.DiffIDs {
				rootFS.Append(id)
				layerRefs[rootFS.ChainID()]++
			}
		}

		// Get Shared sizes
		for img, summary := range summaryMap {
			rootFS := *img.RootFS
			rootFS.DiffIDs = nil

			// Indicate that we collected shared size information (default is -1, or "not set")
			summary.SharedSize = 0
			for _, id := range img.RootFS.DiffIDs {
				rootFS.Append(id)
				chid := rootFS.ChainID()

				if layerRefs[chid] > 1 {
					if _, ok := allLayers[chid]; !ok {
						return nil, fmt.Errorf("layer %v was not found (corruption?)", chid)
					}
					diffSize, err := allLayers[chid].DiffSize()
					if err != nil {
						return nil, err
					}
					summary.SharedSize += diffSize
				}
			}
		}
	}

	sort.Sort(sort.Reverse(byCreated(summaries)))

	return summaries, nil
}

// SquashImage creates a new image with the diff of the specified image and the specified parent.
// This new image contains only the layers from it's parent + 1 extra layer which contains the diff of all the layers in between.
// The existing image(s) is not destroyed.
// If no parent is specified, a new image with the diff of all the specified image's layers merged into a new layer that has no parents.
func (i *ImageService) SquashImage(id, parent string) (string, error) {

	var (
		img *image.Image
		err error
	)
	if img, err = i.imageStore.Get(image.ID(id)); err != nil {
		return "", err
	}

	var parentImg *image.Image
	var parentChainID layer.ChainID
	if len(parent) != 0 {
		parentImg, err = i.imageStore.Get(image.ID(parent))
		if err != nil {
			return "", errors.Wrap(err, "error getting specified parent layer")
		}
		parentChainID = parentImg.RootFS.ChainID()
	} else {
		rootFS := image.NewRootFS()
		parentImg = &image.Image{RootFS: rootFS}
	}
	if !system.IsOSSupported(img.OperatingSystem()) {
		return "", errors.Wrap(err, system.ErrNotSupportedOperatingSystem.Error())
	}
	l, err := i.layerStore.Get(img.RootFS.ChainID())
	if err != nil {
		return "", errors.Wrap(err, "error getting image layer")
	}
	defer i.layerStore.Release(l)

	ts, err := l.TarStreamFrom(parentChainID)
	if err != nil {
		return "", errors.Wrapf(err, "error getting tar stream to parent")
	}
	defer ts.Close()

	newL, err := i.layerStore.Register(ts, parentChainID)
	if err != nil {
		return "", errors.Wrap(err, "error registering layer")
	}
	defer i.layerStore.Release(newL)

	newImage := *img
	newImage.RootFS = nil

	rootFS := *parentImg.RootFS
	rootFS.DiffIDs = append(rootFS.DiffIDs, newL.DiffID())
	newImage.RootFS = &rootFS

	for i, hi := range newImage.History {
		if i >= len(parentImg.History) {
			hi.EmptyLayer = true
		}
		newImage.History[i] = hi
	}

	now := time.Now()
	var historyComment string
	if len(parent) > 0 {
		historyComment = fmt.Sprintf("merge %s to %s", id, parent)
	} else {
		historyComment = fmt.Sprintf("create new from %s", id)
	}

	newImage.History = append(newImage.History, image.History{
		Created: now,
		Comment: historyComment,
	})
	newImage.Created = now

	b, err := json.Marshal(&newImage)
	if err != nil {
		return "", errors.Wrap(err, "error marshalling image config")
	}

	newImgID, err := i.imageStore.Create(b)
	if err != nil {
		return "", errors.Wrap(err, "error creating new image after squash")
	}
	return string(newImgID), nil
}

func newImageSummary(image *image.Image, size int64) *types.ImageSummary {
	summary := &types.ImageSummary{
		ParentID:    image.Parent.String(),
		ID:          image.ID().String(),
		Created:     image.Created.Unix(),
		Size:        size,
		VirtualSize: size,
		// -1 indicates that the value has not been set (avoids ambiguity
		// between 0 (default) and "not set". We cannot use a pointer (nil)
		// for this, as the JSON representation uses "omitempty", which would
		// consider both "0" and "nil" to be "empty".
		SharedSize: -1,
		Containers: -1,
	}
	if image.Config != nil {
		summary.Labels = image.Config.Labels
	}
	return summary
}
