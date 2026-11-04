package images

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"time"

	"github.com/distribution/reference"
	imagetypes "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/internal/layer"
	"github.com/moby/moby/v2/daemon/internal/timestamp"
	"github.com/moby/moby/v2/daemon/server/imagebackend"
	"github.com/moby/moby/v2/errdefs"
)

var acceptedImageFilterTags = map[string]bool{
	"dangling":  true,
	"label":     true,
	"before":    true,
	"since":     true,
	"reference": true,
	"until":     true,
}

// byCreated is a temporary type used to sort a list of images by creation
// time.
type byCreated []imagetypes.Summary

func (r byCreated) Len() int           { return len(r) }
func (r byCreated) Swap(i, j int)      { r[i], r[j] = r[j], r[i] }
func (r byCreated) Less(i, j int) bool { return r[i].Created < r[j].Created }

// Images returns a filtered list of images.
func (i *ImageService) Images(ctx context.Context, opts imagebackend.ListOptions) ([]imagetypes.Summary, error) {
	if err := opts.Filters.Validate(acceptedImageFilterTags); err != nil {
		return nil, err
	}

	danglingOnly, err := opts.Filters.GetBoolOrDefault("dangling", false)
	if err != nil {
		return nil, err
	}

	var beforeFilter, sinceFilter time.Time
	err = opts.Filters.WalkValues("before", func(value string) error {
		img, err := i.GetImage(ctx, value, imagebackend.GetImageOpts{})
		if err != nil {
			return err
		}
		// Resolve multiple values to the oldest image,
		// equivalent to ANDing all the values together.
		if img.Created != nil && (beforeFilter.IsZero() || beforeFilter.After(*img.Created)) {
			beforeFilter = *img.Created
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	now := time.Now()
	err = opts.Filters.WalkValues("until", func(value string) error {
		ts, err := timestamp.Parse(value, now)
		if err != nil {
			return errdefs.InvalidParameter(fmt.Errorf("invalid value for 'until' filter: %w", err))
		}
		if beforeFilter.IsZero() || beforeFilter.After(ts) {
			beforeFilter = ts
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	err = opts.Filters.WalkValues("since", func(value string) error {
		img, err := i.GetImage(ctx, value, imagebackend.GetImageOpts{})
		if err != nil {
			return err
		}
		// Resolve multiple values to the newest image,
		// equivalent to ANDing all the values together.
		if img.Created != nil && sinceFilter.Before(*img.Created) {
			sinceFilter = *img.Created
		}
		return nil
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
		summaryMap    = make(map[*image.Image]*imagetypes.Summary, len(selectedImages))
		allContainers []*container.Container
	)
	for id, img := range selectedImages {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if !beforeFilter.IsZero() && (img.Created == nil || !img.Created.Before(beforeFilter)) {
			continue
		}
		if !sinceFilter.IsZero() && (img.Created == nil || !img.Created.After(sinceFilter)) {
			continue
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
		// panic when indexing through the layerStore. Don't error as we want to list
		// the other images. This should never happen, but here as a safety precaution.
		if err := image.CheckOS(img.OperatingSystem()); err != nil {
			continue
		}

		var size int64
		if layerID := img.RootFS.ChainID(); layerID != "" {
			l, err := i.layerStore.Get(layerID)
			if err != nil {
				// The layer may have been deleted between the call to `Map()` or
				// `Heads()` and the call to `Get()`, so we just ignore this error
				if errors.Is(err, layer.ErrLayerDoesNotExist) {
					continue
				}
				return nil, err
			}

			size = l.Size()
			layer.ReleaseAndLog(i.layerStore, l)
		}

		summary := newImageSummary(img, size)

		for _, ref := range i.referenceStore.References(id.Digest()) {
			if opts.Filters.Contains("reference") {
				// Match the filter pattern against both the familiar
				// and canonical forms of the image reference (with
				// and without tag), so that e.g. "alpine" and
				// "docker.io/library/alpine" (and their glob
				// variants) both match.
				targets := []string{
					reference.FamiliarString(ref),
					reference.FamiliarName(ref),
					reference.TagNameOnly(ref).String(),
					ref.Name(),
				}
				var found bool
				for _, pattern := range opts.Filters.Get("reference") {
					for _, target := range targets {
						matched, err := path.Match(pattern, target)
						if err != nil {
							return nil, err
						}
						if matched {
							found = true
							break
						}
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
			} else {
				continue
			}
		} else if danglingOnly && len(summary.RepoTags) > 0 {
			continue
		}

		// Lazily init allContainers.
		if allContainers == nil {
			allContainers = i.containers.List()
		}

		// Get container count
		var containersCount int64
		for _, c := range allContainers {
			if c.ImageID == id {
				containersCount++
			}
		}
		summary.Containers = containersCount
		summaryMap[img] = summary
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
				chID := rootFS.ChainID()

				if layerRefs[chID] > 1 {
					if _, ok := allLayers[chID]; !ok {
						return nil, fmt.Errorf("layer %v was not found (corruption?)", chID)
					}
					summary.SharedSize += allLayers[chID].DiffSize()
				}
			}
		}
	}

	summaries := make([]imagetypes.Summary, 0, len(summaryMap))
	for _, summary := range summaryMap {
		if summary.RepoTags == nil {
			summary.RepoTags = []string{}
		}
		if summary.RepoDigests == nil {
			summary.RepoDigests = []string{}
		}
		summaries = append(summaries, *summary)
	}
	sort.Sort(sort.Reverse(byCreated(summaries)))

	return summaries, nil
}

func newImageSummary(image *image.Image, size int64) *imagetypes.Summary {
	var created int64
	if image.Created != nil {
		created = image.Created.Unix()
	}
	summary := &imagetypes.Summary{
		ParentID: image.Parent.String(),
		ID:       image.ID().String(),
		Created:  created,
		Size:     size,
		// -1 indicates that the value has not been set (avoids ambiguity
		// between 0 (default) and "not set"). We cannot use a pointer (nil)
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
