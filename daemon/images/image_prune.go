package images // import "github.com/docker/docker/daemon/images"

import (
	"context"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/errdefs"
	"github.com/pkg/errors"
)

//var imagesAcceptedFilters = map[string]bool{
//	"dangling": true,
//	"label":    true,
//	"label!":   true,
//	"until":    true,
//}

// errPruneRunning is returned when a prune request is received while
// one is in progress
//var errPruneRunning = errdefs.Conflict(errors.New("a prune operation is already running"))

// ImagesPrune removes unused images
func (i *ImageService) ImagesPrune(ctx context.Context, pruneFilters filters.Args) (*types.ImagesPruneReport, error) {
	return nil, errdefs.NotImplemented(errors.New("prune not implemented"))
	/*
			if !atomic.CompareAndSwapInt32(&i.pruneRunning, 0, 1) {
				return nil, errPruneRunning
			}
			defer atomic.StoreInt32(&i.pruneRunning, 0)

			// make sure that only accepted filters have been received
			err := pruneFilters.Validate(imagesAcceptedFilters)
			if err != nil {
				return nil, err
			}

			rep := &types.ImagesPruneReport{}

			danglingOnly := true
			if pruneFilters.Contains("dangling") {
				if pruneFilters.ExactMatch("dangling", "false") || pruneFilters.ExactMatch("dangling", "0") {
					danglingOnly = false
				} else if !pruneFilters.ExactMatch("dangling", "true") && !pruneFilters.ExactMatch("dangling", "1") {
					return nil, invalidFilter{"dangling", pruneFilters.Get("dangling")}
				}
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
			allLayers := make(map[layer.ChainID]layer.Layer)
			for _, ls := range i.layerStores {
				for k, v := range ls.Map() {
					allLayers[k] = v
				}
			}
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
					if !until.IsZero() && img.Created.After(until) {
						continue
					}
					if img.V1Image.Config != nil && !matchLabels(pruneFilters, img.V1Image.Config.Labels) {
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

				deletedImages := []types.ImageDeleteResponseItem{}
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

						// Only delete if it's untagged (i.e. repo:<none>)
						shouldDelete = !hasTag
					}

					if shouldDelete {
						for _, ref := range refs {
							imgDel, err := i.ImageDelete(ctx, ref.String(), false, true)
							if imageDeleteFailed(ref.String(), err) {
								continue
							}
							deletedImages = append(deletedImages, imgDel...)
						}
					}
				} else {
					hex := id.Digest().Hex()
					imgDel, err := i.ImageDelete(ctx, hex, false, true)
					if imageDeleteFailed(hex, err) {
						continue
					}
					deletedImages = append(deletedImages, imgDel...)
				}

				rep.ImagesDeleted = append(rep.ImagesDeleted, deletedImages...)
			}

			// Compute how much space was freed
			for _, d := range rep.ImagesDeleted {
				if d.Deleted != "" {
					chid := layer.ChainID(d.Deleted)
					if l, ok := allLayers[chid]; ok {
						diffSize, err := l.DiffSize()
						if err != nil {
							logrus.Warnf("failed to get layer %s size: %v", chid, err)
							continue
						}
						rep.SpaceReclaimed += uint64(diffSize)
					}
				}
			}

			if canceled {
				logrus.Debugf("ImagesPrune operation cancelled: %#v", *rep)
			}

			return rep, nil
	*/
}

/*
func imageDeleteFailed(ref string, err error) bool {
	switch {
	case err == nil:
		return false
	case errdefs.IsConflict(err):
		return true
	default:
		logrus.Warnf("failed to prune image %s: %v", ref, err)
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
	until := time.Time{}
	if !pruneFilters.Contains("until") {
		return until, nil
	}
	untilFilters := pruneFilters.Get("until")
	if len(untilFilters) > 1 {
		return until, fmt.Errorf("more than one until filter specified")
	}
	ts, err := timetypes.GetTimestamp(untilFilters[0], time.Now())
	if err != nil {
		return until, err
	}
	seconds, nanoseconds, err := timetypes.ParseTimestamps(ts, 0)
	if err != nil {
		return until, err
	}
	until = time.Unix(seconds, nanoseconds)
	return until, nil
}
*/
