package images

import (
	"context"
	"fmt"
	"strconv"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/events"
	imagetypes "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/v2/daemon/internal/filters"
	"github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/internal/layer"
	"github.com/moby/moby/v2/daemon/internal/pruneprogress"
	"github.com/moby/moby/v2/daemon/internal/timestamp"
	"github.com/moby/moby/v2/daemon/server/imagebackend"
	"github.com/moby/moby/v2/errdefs"
	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

var imagesAcceptedFilters = map[string]bool{
	"dangling": true,
	"label":    true,
	"label!":   true,
	"until":    true,
}

// errPruneRunning is returned when a prune request is received while
// one is in progress
var errPruneRunning = errdefs.Conflict(errors.New("a prune operation is already running"))

// ImagePrune removes unused images
func (i *ImageService) ImagePrune(ctx context.Context, opts pruneprogress.Options) (*imagetypes.PruneReport, error) {
	pruneFilters := opts.Filters
	if !i.pruneRunning.CompareAndSwap(false, true) {
		return nil, errPruneRunning
	}
	defer i.pruneRunning.Store(false)

	// make sure that only accepted filters have been received
	err := pruneFilters.Validate(imagesAcceptedFilters)
	if err != nil {
		return nil, err
	}

	rep := &imagetypes.PruneReport{}

	danglingOnly, err := pruneFilters.GetBoolOrDefault("dangling", true)
	if err != nil {
		return nil, err
	}

	until, err := getUntilFromPruneFilters(pruneFilters)
	if err != nil {
		return nil, err
	}

	var allImages map[image.ID]*image.Image
	if danglingOnly {
		allImages = i.imageStore.Heads()
	} else {
		allImages = i.imageStore.Map()
	}

	// Filter intermediary images and get their unique size
	allLayers := i.layerStore.Map()
	topImages := map[image.ID]*image.Image{}
	for id, img := range allImages {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			dgst := digest.Digest(id)
			if len(i.referenceStore.References(dgst)) == 0 && len(i.imageStore.Children(id)) != 0 {
				continue
			}
			if !until.IsZero() && (img.Created == nil || img.Created.After(until)) {
				continue
			}
			if img.Config != nil && !matchLabels(pruneFilters, img.Config.Labels) {
				continue
			}
			topImages[id] = img
		}
	}

	canceled := false
deleteImagesLoop:
	for id := range topImages {
		select {
		case <-ctx.Done():
			// we still want to calculate freed size and return the data
			canceled = true
			break deleteImagesLoop
		default:
		}

		deletedImages := []imagetypes.DeleteResponse{}
		refs := i.referenceStore.References(id.Digest())
		if len(refs) > 0 {
			shouldDelete := !danglingOnly
			if !shouldDelete {
				hasTag := false
				for _, ref := range refs {
					if _, ok := ref.(reference.NamedTagged); ok {
						hasTag = true
						break
					}
				}

				// Only delete if it has no references which is a valid NamedTagged.
				shouldDelete = !hasTag
			}

			if shouldDelete {
				for _, ref := range refs {
					imgDel, err := i.ImageDelete(ctx, ref.String(), imagebackend.RemoveOptions{
						PruneChildren: true,
					})
					if imageDeleteFailed(ref.String(), err) {
						continue
					}
					deletedImages = append(deletedImages, imgDel...)
				}
			}
		} else {
			hex := id.Digest().Encoded()
			imgDel, err := i.ImageDelete(ctx, hex, imagebackend.RemoveOptions{
				PruneChildren: true,
			})
			if imageDeleteFailed(hex, err) {
				continue
			}
			deletedImages = append(deletedImages, imgDel...)
		}

		rep.ImagesDeleted = append(rep.ImagesDeleted, deletedImages...)
		for _, deleted := range deletedImages {
			if deleted.Untagged != "" {
				pruneprogress.Notify(opts.Progress, deleted.Untagged, "untagged")
			}
			if deleted.Deleted != "" {
				pruneprogress.Notify(opts.Progress, deleted.Deleted, "deleted")
			}
		}
	}

	// Compute how much space was freed
	for _, d := range rep.ImagesDeleted {
		if d.Deleted != "" {
			if l, ok := allLayers[layer.ChainID(d.Deleted)]; ok {
				rep.SpaceReclaimed += uint64(l.DiffSize())
			}
		}
	}

	if canceled {
		log.G(ctx).Debugf("ImagePrune operation cancelled: %#v", *rep)
	}
	i.eventsService.Log(events.ActionPrune, events.ImageEventType, events.Actor{
		Attributes: map[string]string{
			"reclaimed": strconv.FormatUint(rep.SpaceReclaimed, 10),
		},
	})
	return rep, nil
}

func imageDeleteFailed(ref string, err error) bool {
	switch {
	case err == nil:
		return false
	case cerrdefs.IsConflict(err), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return true
	default:
		log.G(context.TODO()).Warnf("failed to prune image %s: %v", ref, err)
		return true
	}
}

func matchLabels(pruneFilters filters.Args, labels map[string]string) bool {
	if !pruneFilters.MatchKVList("label", labels) {
		return false
	}
	// By default MatchKVList will return true if field (like 'label!') does not exist
	// So we have to add additional Contains("label!") check
	if pruneFilters.Contains("label!") {
		if pruneFilters.MatchKVList("label!", labels) {
			return false
		}
	}
	return true
}

func getUntilFromPruneFilters(pruneFilters filters.Args) (time.Time, error) {
	if !pruneFilters.Contains("until") {
		return time.Time{}, nil
	}
	untilFilters := pruneFilters.Get("until")
	if len(untilFilters) > 1 {
		return time.Time{}, errdefs.InvalidParameter(errors.New("more than one until filter specified"))
	}
	t, err := timestamp.Parse(untilFilters[0], time.Now())
	if err != nil {
		return time.Time{}, errdefs.InvalidParameter(fmt.Errorf("invalid value for 'until' filter: %w", err))
	}
	return t, nil
}
