package containerd

import (
	"context"
	"errors"
	"sort"
	"strings"

	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/tracing"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/distribution/reference"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/v2/daemon/internal/filters"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/errdefs"
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
func (i *ImageService) ImagePrune(ctx context.Context, fltrs filters.Args) (*image.PruneReport, error) {
	if !i.pruneRunning.CompareAndSwap(false, true) {
		return nil, errPruneRunning
	}
	defer i.pruneRunning.Store(false)

	err := fltrs.Validate(imagesAcceptedFilters)
	if err != nil {
		return nil, err
	}

	danglingOnly, err := fltrs.GetBoolOrDefault("dangling", true)
	if err != nil {
		return nil, err
	}
	// dangling=false will filter out dangling images like in image list.
	// Remove it, because in this context dangling=false means that we're
	// pruning NOT ONLY dangling (`docker image prune -a`) instead of NOT DANGLING.
	// This will be handled by the danglingOnly parameter of pruneUnused.
	for _, v := range fltrs.Get("dangling") {
		fltrs.Del("dangling", v)
	}

	filterFunc, err := i.setupFilters(ctx, fltrs)
	if err != nil {
		return nil, err
	}

	// Prune leases
	leaseManager := i.client.LeasesService()
	pullLeases, err := leaseManager.List(ctx, pruneLeaseFilter)
	if err != nil {
		return nil, err
	}

	// Take a snapshot of active leases. Leases that are still in use by
	// an in-flight operation must not be deleted, otherwise the GC triggered
	// by SynchronousDelete will remove content that is still being written,
	// corrupting concurrent operations.
	activeLeases := i.activeLeaseSnapshot()

	for i, lease := range pullLeases {
		if _, active := activeLeases[lease.ID]; active {
			log.G(ctx).WithFields(log.Fields{"lease": lease.ID}).Debug("Skipping lease in use by an active operation")
			continue
		}
		var opts []leases.DeleteOpt
		if i == len(pullLeases)-1 {
			opts = append(opts, leases.SynchronousDelete)
		}
		if err := leaseManager.Delete(ctx, lease, opts...); err != nil {
			return nil, err
		}
	}

	return i.pruneUnused(ctx, filterFunc, danglingOnly)
}

// pruneUnused deletes images that are dangling or unused by any container.
// The behavior is controlled by the danglingOnly parameter.
// If danglingOnly is true, only dangling images are deleted.
// Otherwise, all images unused by any container are deleted.
//
// Additionally, the filterFunc parameter is used to filter images that should
// be considered for deletion.
//
// Container created with images specified by an ID only (e.g. `docker run 82d1e9d`)
// will keep at least one image tag with that ID.
//
// In case a digested and tagged reference was used (e.g. `docker run alpine:latest@sha256:82d1e9d7ed48a7523bdebc18cf6290bdb97b82302a8a9c27d4fe885949ea94d1`),
// the alpine:latest image will be kept.
func (i *ImageService) pruneUnused(ctx context.Context, filterFunc imageFilterFunc, danglingOnly bool) (*image.PruneReport, error) {
	ctx, span := tracing.StartSpan(ctx, "ImageService.pruneUnused")
	span.SetAttributes(tracing.Attribute("danglingOnly", danglingOnly))
	defer span.End()

	allImages, err := i.images.List(ctx)
	if err != nil {
		return nil, err
	}

	// How many images make reference to a particular target digest.
	digestRefCount := map[digest.Digest]int{}
	// Images considered for pruning.
	imagesToPrune := map[string]c8dimages.Image{}
	for _, img := range allImages {
		digestRefCount[img.Target.Digest]++

		if !danglingOnly || isDanglingImage(img) {
			canBePruned := filterFunc(img)
			log.G(ctx).WithFields(log.Fields{
				"image":       img.Name,
				"canBePruned": canBePruned,
			}).Debug("considering image for pruning")

			if canBePruned {
				imagesToPrune[img.Name] = img
			}
		}
	}

	usedDigests := filterImagesUsedByContainers(ctx, i.containers.List(), imagesToPrune)

	// Sort images by name to make the behavior deterministic and consistent with graphdrivers.
	sorted := make([]string, 0, len(imagesToPrune))
	for name := range imagesToPrune {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	// Make sure we don't delete the last image of a particular digest used by any container.
	for _, name := range sorted {
		img := imagesToPrune[name]
		dgst := img.Target.Digest

		if digestRefCount[dgst] > 1 {
			digestRefCount[dgst]--
			continue
		}

		if _, isUsed := usedDigests[dgst]; isUsed {
			delete(imagesToPrune, name)
		}
	}

	return i.pruneAll(ctx, imagesToPrune)
}

// filterImagesUsedByContainers removes image names that are used by containers
// and returns a map of used image digests.
func filterImagesUsedByContainers(ctx context.Context,
	allContainers []*container.Container,
	imagesToPrune map[string]c8dimages.Image,
) (usedDigests map[digest.Digest]struct{}) {
	ctx, span := tracing.StartSpan(ctx, "filterImagesUsedByContainers")
	span.SetAttributes(tracing.Attribute("count", len(allContainers)))
	defer span.End()

	// Image specified by digests that are used by containers.
	usedDigests = map[digest.Digest]struct{}{}

	// Exclude images used by existing containers
	for _, ctr := range allContainers {
		// If the original image was force deleted, make sure we don't delete the dangling image
		delete(imagesToPrune, danglingImageName(ctr.ImageID.Digest()))

		// Config.Image is the image reference passed by user.
		// Config.ImageID is the resolved content digest based on the user's Config.Image.
		// For example: container created by:
		//           `docker run alpine` will have Config.Image="alpine"
		//           `docker run 82d1e9d` will have Config.Image="82d1e9d"
		// but both will have ImageID="sha256:82d1e9d7ed48a7523bdebc18cf6290bdb97b82302a8a9c27d4fe885949ea94d1"
		imageDgst := ctr.ImageID.Digest()

		// If user used an full or truncated ID instead of an explicit image name, mark the digest as used.
		normalizedImageID := "sha256:" + strings.TrimPrefix(ctr.Config.Image, "sha256:")
		fullOrTruncatedID := strings.HasPrefix(imageDgst.String(), normalizedImageID)
		digestedRef := strings.HasSuffix(ctr.Config.Image, "@"+imageDgst.String())
		if fullOrTruncatedID || digestedRef {
			usedDigests[imageDgst] = struct{}{}
		}

		ref, err := reference.ParseNormalizedNamed(ctr.Config.Image)
		log.G(ctx).WithFields(log.Fields{
			"ctr":          ctr.ID,
			"imageRef":     ref,
			"imageID":      imageDgst,
			"nameParseErr": err,
		}).Debug("filtering container's image")
		if err == nil {
			// If user provided a specific image name, exclude that image.
			name := reference.TagNameOnly(ref)
			delete(imagesToPrune, name.String())

			// Also exclude repo:tag image if repo:tag@sha256:digest reference was used.
			_, isDigested := name.(reference.Digested)
			tagged, isTagged := name.(reference.NamedTagged)
			if isDigested && isTagged {
				named, _ := reference.ParseNormalizedNamed(tagged.Name())
				namedTagged, _ := reference.WithTag(named, tagged.Tag())
				delete(imagesToPrune, namedTagged.String())
			}
		}
	}

	return usedDigests
}

// pruneAll deletes all images in the imagesToPrune map.
func (i *ImageService) pruneAll(ctx context.Context, imagesToPrune map[string]c8dimages.Image) (*image.PruneReport, error) {
	report := image.PruneReport{}

	ctx, span := tracing.StartSpan(ctx, "ImageService.pruneAll")
	span.SetAttributes(tracing.Attribute("count", len(imagesToPrune)))
	defer span.End()

	var errs []error
	for _, img := range imagesToPrune {
		log.G(ctx).WithField("image", img).Debug("pruning image")

		snapshotter := i.snapshotterService(i.snapshotter)
		snapshotSizes := map[string]int64{}
		blobs := []ocispec.Descriptor{}

		err := i.walkPresentChildren(ctx, img.Target, func(ctx context.Context, desc ocispec.Descriptor) error {
			blobs = append(blobs, desc)
			if !c8dimages.IsConfigType(desc.MediaType) {
				return nil
			}

			var cfg dockerspec.DockerOCIImage
			if err := readJSON(ctx, i.content, desc, &cfg); err != nil {
				if cerrdefs.IsNotFound(err) {
					return nil
				}
				return err
			}

			for _, chainID := range identity.ChainIDs(cfg.RootFS.DiffIDs) {
				id := chainID.String()
				if _, ok := snapshotSizes[id]; ok {
					continue
				}
				usage, err := snapshotter.Usage(ctx, id)
				if err != nil {
					if cerrdefs.IsNotFound(err) {
						snapshotSizes[id] = 0
						continue
					}
					return err
				}
				snapshotSizes[id] = usage.Size
			}
			return nil
		})
		if err != nil {
			errs = append(errs, err)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return &report, errors.Join(errs...)
			}
			continue
		}

		err = i.images.Delete(ctx, img.Name, c8dimages.SynchronousDelete())
		if err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, err)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return &report, errors.Join(errs...)
			}
			continue
		}

		familiarName := imageFamiliarName(img)
		i.logImageEvent(img, familiarName, events.ActionUnTag)
		report.ImagesDeleted = append(report.ImagesDeleted,
			image.DeleteResponse{
				Untagged: familiarName,
			},
		)

		var deleted bool
		// Check which blobs have been deleted and sum their sizes
		for _, blob := range blobs {
			_, err := i.content.ReaderAt(ctx, blob)

			if cerrdefs.IsNotFound(err) {
				if c8dimages.IsManifestType(blob.MediaType) || c8dimages.IsIndexType(blob.MediaType) {
					deleted = true
					report.ImagesDeleted = append(report.ImagesDeleted,
						image.DeleteResponse{
							Deleted: blob.Digest.String(),
						},
					)
				}
				report.SpaceReclaimed += uint64(blob.Size)
			}
		}
		for id, size := range snapshotSizes {
			_, err := snapshotter.Usage(ctx, id)
			if cerrdefs.IsNotFound(err) {
				report.SpaceReclaimed += uint64(size)
			}
		}
		if deleted {
			i.logImageEvent(img, familiarName, events.ActionDelete)
		}
	}

	return &report, errors.Join(errs...)
}
