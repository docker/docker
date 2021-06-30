package images // import "github.com/docker/docker/daemon/images"

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/pkg/errors"

	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
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

// rangeImageLayerChainIDs calls f sequentially for chain ID of each layer image consists of.
// If f returns false, range stops the iteration.
func rangeImageLayerChainIDs(img *image.Image, f func(layer.ChainID) bool) {
	rootFS := image.RootFS{
		Type: img.RootFS.Type,
	}
	for _, id := range img.RootFS.DiffIDs {
		rootFS.Append(id)
		if !f(rootFS.ChainID()) {
			return
		}
	}
}

// Images returns a filtered list of images. filterArgs is a JSON-encoded set
// of filter arguments which will be interpreted by api/types/filters.
// filter is a shell glob string applied to repository names. The argument
// named all controls whether all images in the graph are filtered, or just
// the heads.
func (i *ImageService) Images(imageFilters filters.Args, all bool, withExtraAttrs bool) ([]*types.ImageSummary, error) {
	if err := imageFilters.Validate(acceptedImageFilterTags); err != nil {
		return nil, err
	}

	var danglingOnly bool
	if imageFilters.Contains("dangling") {
		if imageFilters.ExactMatch("dangling", "true") {
			danglingOnly = true
		} else if !imageFilters.ExactMatch("dangling", "false") {
			return nil, invalidFilter{"dangling", imageFilters.Get("dangling")}
		}
	}

	var (
		beforeFilter, sinceFilter *image.Image
		err                       error
	)
	err = imageFilters.WalkValues("before", func(value string) error {
		beforeFilter, err = i.GetImage(value, nil)
		return err
	})
	if err != nil {
		return nil, err
	}

	err = imageFilters.WalkValues("since", func(value string) error {
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
imageLoop:
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

		if imageFilters.Contains("label") {
			// Very old image that do not have image.Config (or even labels)
			if img.Config == nil {
				continue
			}
			// We are now sure image.Config is not nil
			if !imageFilters.MatchKVList("label", img.Config.Labels) {
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
		v, ok := i.imageSizeCache.Load(id)
		if ok {
			size = v.(int64)
		} else if layerID := img.RootFS.ChainID(); layerID != "" {
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

			i.imageSizeCache.Store(id, size)
		}

		references := i.referenceStore.References(id.Digest())
		var repoDigests []string
		var repoTags []string
		for i, ref := range references {
			if imageFilters.Contains("reference") {
				var found bool
				var matchErr error
				for _, pattern := range imageFilters.Get("reference") {
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
				if repoDigests == nil {
					// Lazily init repoDigests
					repoDigests = make([]string, 0, len(references)-i) // i references were skipped
				}
				repoDigests = append(repoDigests, reference.FamiliarString(ref))
			}
			if _, ok := ref.(reference.NamedTagged); ok {
				if danglingOnly {
					continue imageLoop
				}
				if repoTags == nil {
					// Lazily init repoTags
					repoTags = make([]string, 0, len(references)-i) // i references were skipped
				}
				repoTags = append(repoTags, reference.FamiliarString(ref))
			}
		}
		if len(repoDigests) == 0 && len(repoTags) == 0 {
			switch {
			case !all && len(i.imageStore.Children(id)) > 0, // all=false and image is not a head, skip
				imageFilters.Contains("dangling") && !danglingOnly, // image is dangling and dangling=false, skip
				imageFilters.Contains("reference"):                 // image has no references and filtering by reference requested, skip
				continue

			default:
				repoDigests = []string{"<none>@<none>"}
				repoTags = []string{"<none>:<none>"}
			}
		}

		summary := newImageSummary(img, size, repoDigests, repoTags)
		if withExtraAttrs {
			// Lazily init summaryMap and allContainers
			if summaryMap == nil {
				summaryMap = make(map[*image.Image]*types.ImageSummary, len(selectedImages))
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

			summaryMap[img] = summary
		}
		summaries = append(summaries, summary)
	}

	if withExtraAttrs {
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
			rangeImageLayerChainIDs(img, func(chid layer.ChainID) bool {
				layerRefs[chid]++
				return true
			})
		}

		// Get Shared sizes
		for img, summary := range summaryMap {
			var (
				sharedSize int64
				err        error
			)
			rangeImageLayerChainIDs(img, func(chid layer.ChainID) bool {
				if layerRefs[chid] <= 1 {
					return true
				}
				if _, ok := allLayers[chid]; !ok {
					err = fmt.Errorf("layer %v was not found (corruption?)", chid)
					return false
				}

				var diffSize int64
				diffSize, err = allLayers[chid].DiffSize()
				if err != nil {
					return false
				}
				sharedSize += diffSize
				return true
			})
			if err != nil {
				return nil, err
			}
			// NOTE: By default, SharedSize is -1, or "not set"
			summary.SharedSize = sharedSize
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

func newImageSummary(image *image.Image, size int64, repoDigests, repoTags []string) *types.ImageSummary {
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

		RepoDigests: repoDigests,
		RepoTags:    repoTags,
	}
	if image.Config != nil {
		summary.Labels = image.Config.Labels
	}
	return summary
}
