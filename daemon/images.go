package daemon

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/image"
	"github.com/docker/docker/layer"
	"github.com/pkg/errors"
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
func (daemon *Daemon) Map() map[image.ID]*image.Image {
	return daemon.imageStore.Map()
}

// Images returns a filtered list of images. filterArgs is a JSON-encoded set
// of filter arguments which will be interpreted by api/types/filters.
// filter is a shell glob string applied to repository names. The argument
// named all controls whether all images in the graph are filtered, or just
// the heads.
func (daemon *Daemon) Images(imageFilters filters.Args, all bool, withExtraAttrs bool) ([]*types.ImageSummary, error) {
	var (
		allImages    map[image.ID]*image.Image
		err          error
		danglingOnly bool
	)

	if err := imageFilters.Validate(acceptedImageFilterTags); err != nil {
		return nil, err
	}

	if imageFilters.Include("dangling") {
		if imageFilters.ExactMatch("dangling", "true") {
			danglingOnly = true
		} else if !imageFilters.ExactMatch("dangling", "false") {
			return nil, fmt.Errorf("Invalid filter 'dangling=%s'", imageFilters.Get("dangling"))
		}
	}
	if danglingOnly {
		allImages = daemon.imageStore.Heads()
	} else {
		allImages = daemon.imageStore.Map()
	}

	var beforeFilter, sinceFilter *image.Image
	err = imageFilters.WalkValues("before", func(value string) error {
		beforeFilter, err = daemon.GetImage(value)
		return err
	})
	if err != nil {
		return nil, err
	}

	err = imageFilters.WalkValues("since", func(value string) error {
		sinceFilter, err = daemon.GetImage(value)
		return err
	})
	if err != nil {
		return nil, err
	}

	images := []*types.ImageSummary{}
	var imagesMap map[*image.Image]*types.ImageSummary
	var layerRefs map[layer.ChainID]int
	var allLayers map[layer.ChainID]layer.Layer
	var containerCounts map[image.ID]int64

	for id, img := range allImages {
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

		if imageFilters.Include("label") {
			// Very old image that do not have image.Config (or even labels)
			if img.Config == nil {
				continue
			}
			// We are now sure image.Config is not nil
			if !imageFilters.MatchKVList("label", img.Config.Labels) {
				continue
			}
		}

		// Get all references (RepoTags, RepoDigests) for the image
		refs := daemon.referenceStore.References(id.Digest())

		// An image is "dangling" if it doesn't have references, and does not have children
		isDangling := len(refs) == 0 && len(daemon.imageStore.Children(id)) == 0

		if isDangling && !all  {
			continue
		}
		if imageFilters.Include("dangling") && danglingOnly != isDangling {
			// either dangling=true or dangling=false was set, and image doesn't match the filter
			continue
		}

		newImage := newImage(img)
		didMatch := false

		for _, ref := range refs {
			if _, ok := ref.(reference.Canonical); ok {
				newImage.RepoDigests = append(newImage.RepoDigests, ref.String())
			}
			if _, ok := ref.(reference.NamedTagged); ok {
				newImage.RepoTags = append(newImage.RepoTags, ref.String())
			}
			if !didMatch && imageFilters.Include("reference") {
				var found bool
				var matchErr error
				for _, pattern := range imageFilters.Get("reference") {
					found, matchErr = reference.Match(pattern, ref)
					if matchErr != nil {
						return nil, matchErr
					}
					if found {
						didMatch = true
						break
					}
				}
			}
		}
		if imageFilters.Include("reference") && !didMatch {
			continue
		}

		// Collect image's VirtualSize
		layerID := img.RootFS.ChainID()
		if layerID != "" {
			l, err := daemon.layerStore.Get(layerID)
			if err != nil {
				return nil, err
			}

			newImage.VirtualSize, err = l.Size()
			layer.ReleaseAndLog(daemon.layerStore, l)
			if err != nil {
				return nil, err
			}
		}

		if withExtraAttrs {
			// lazily init variables
			if imagesMap == nil {
				allLayers = daemon.layerStore.Map()
				imagesMap = make(map[*image.Image]*types.ImageSummary)
				layerRefs = make(map[layer.ChainID]int)
				containerCounts = make(map[image.ID]int64)

				for _, c := range daemon.List() {
					if _, ok := containerCounts[c.ImageID]; !ok {
						containerCounts[c.ImageID] = 1
						continue
					}
					containerCounts[c.ImageID]++
				}
			}

			// Get container count
			newImage.Containers = 0
			if _, ok := containerCounts[id]; ok {
				newImage.Containers = containerCounts[id]
			}

			// count layer references
			rootFS := *img.RootFS
			rootFS.DiffIDs = nil
			for _, id := range img.RootFS.DiffIDs {
				rootFS.Append(id)
				chid := rootFS.ChainID()
				layerRefs[chid]++
				if _, ok := allLayers[chid]; !ok {
					return nil, fmt.Errorf("layer %v was not found (corruption?)", chid)
				}
			}
			imagesMap[img] = newImage
		}

		images = append(images, newImage)
	}

	if withExtraAttrs {
		// Get Shared and Unique sizes
		for img, newImage := range imagesMap {
			rootFS := *img.RootFS
			rootFS.DiffIDs = nil

			newImage.Size = 0
			newImage.SharedSize = 0
			for _, id := range img.RootFS.DiffIDs {
				rootFS.Append(id)
				chid := rootFS.ChainID()

				diffSize, err := allLayers[chid].DiffSize()
				if err != nil {
					return nil, err
				}

				if layerRefs[chid] > 1 {
					newImage.SharedSize += diffSize
				} else {
					newImage.Size += diffSize
				}
			}
		}
	}

	sort.Sort(sort.Reverse(byCreated(images)))

	return images, nil
}

// SquashImage creates a new image with the diff of the specified image and the specified parent.
// This new image contains only the layers from it's parent + 1 extra layer which contains the diff of all the layers in between.
// The existing image(s) is not destroyed.
// If no parent is specified, a new image with the diff of all the specified image's layers merged into a new layer that has no parents.
func (daemon *Daemon) SquashImage(id, parent string) (string, error) {
	img, err := daemon.imageStore.Get(image.ID(id))
	if err != nil {
		return "", err
	}

	var parentImg *image.Image
	var parentChainID layer.ChainID
	if len(parent) != 0 {
		parentImg, err = daemon.imageStore.Get(image.ID(parent))
		if err != nil {
			return "", errors.Wrap(err, "error getting specified parent layer")
		}
		parentChainID = parentImg.RootFS.ChainID()
	} else {
		rootFS := image.NewRootFS()
		parentImg = &image.Image{RootFS: rootFS}
	}

	l, err := daemon.layerStore.Get(img.RootFS.ChainID())
	if err != nil {
		return "", errors.Wrap(err, "error getting image layer")
	}
	defer daemon.layerStore.Release(l)

	ts, err := l.TarStreamFrom(parentChainID)
	if err != nil {
		return "", errors.Wrapf(err, "error getting tar stream to parent")
	}
	defer ts.Close()

	newL, err := daemon.layerStore.Register(ts, parentChainID)
	if err != nil {
		return "", errors.Wrap(err, "error registering layer")
	}
	defer daemon.layerStore.Release(newL)

	var newImage image.Image
	newImage = *img
	newImage.RootFS = nil

	var rootFS image.RootFS
	rootFS = *parentImg.RootFS
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

	newImgID, err := daemon.imageStore.Create(b)
	if err != nil {
		return "", errors.Wrap(err, "error creating new image after squash")
	}
	return string(newImgID), nil
}

func newImage(image *image.Image) *types.ImageSummary {
	newImage := new(types.ImageSummary)
	newImage.ParentID = image.Parent.String()
	newImage.ID = image.ID().String()
	newImage.Created = image.Created.Unix()
	newImage.Size = -1
	newImage.VirtualSize = -1
	newImage.SharedSize = -1
	newImage.Containers = -1
	if image.Config != nil {
		newImage.Labels = image.Config.Labels
	}
	return newImage
}
