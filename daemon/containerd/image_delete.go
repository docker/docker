package containerd

import (
	"context"
	"fmt"
	"strings"
	"time"

	c8dimages "github.com/containerd/containerd/v2/core/images"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/events"
	imagetypes "github.com/moby/moby/api/types/image"
	"github.com/moby/moby/v2/daemon/container"
	dimages "github.com/moby/moby/v2/daemon/images"
	"github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/internal/metrics"
	"github.com/moby/moby/v2/daemon/internal/stringid"
	"github.com/moby/moby/v2/daemon/server/imagebackend"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ImageDelete deletes the image referenced by the given imageRef from this
// daemon. The given imageRef can be an image ID, ID prefix, or a repository
// reference (with an optional tag or digest, defaulting to the tag name
// "latest"). There is differing behavior depending on whether the given
// imageRef is a repository reference or not.
//
// If the given imageRef is a repository reference then that repository
// reference is removed. However, if there exists any containers which
// were created using the same image reference then the repository reference
// cannot be removed unless either there are other repository references to the
// same image or force is true. Following removal of the repository reference,
// the referenced image itself is attempted to be deleted as described below
// but quietly, meaning any image delete conflicts will cause the image to not
// be deleted and the conflict will not be reported.
//
// There may be conflicts preventing deletion of an image and these conflicts
// are divided into two categories grouped by their severity:
//
// Hard Conflict:
//   - any running container using the image.
//
// Soft Conflict:
//   - any stopped container using the image.
//   - any repository tag or digest references to the image.
//
// The image cannot be removed if there are any hard conflicts and can be
// removed if there are soft conflicts only if force is true.
//
// If prune is true, ancestor images are attempted to be deleted quietly,
// meaning any delete conflicts will cause the image to not be deleted and the
// conflict will not be reported.
func (i *ImageService) ImageDelete(ctx context.Context, imageRef string, options imagebackend.RemoveOptions) (response []imagetypes.DeleteResponse, retErr error) {
	start := time.Now()
	defer func() {
		metrics.ImageDeletesCounter.Inc()
		if retErr == nil {
			metrics.ImageActions.WithValues("delete").UpdateSince(start)
		} else {
			reason := metrics.CategorizeErrorReason(retErr)
			metrics.ImageDeletesFailedCounter.WithValues(reason).Inc()
		}
	}()

	if len(options.Platforms) > 0 && !options.Force {
		return nil, cerrdefs.ErrInvalidArgument.WithMessage("Content will be removed from all images referencing this variant. Use --force to force delete.")
	}

	force := options.Force
	prune := options.PruneChildren

	var c conflictType
	if !force {
		c |= conflictSoft
	}

	img, all, err := i.resolveAllReferences(ctx, imageRef)
	if err != nil {
		return nil, err
	}

	var imgID image.ID
	if img == nil {
		if len(all) == 0 {
			parsed, _ := reference.ParseAnyReference(imageRef)
			return nil, dimages.ErrImageDoesNotExist{Ref: parsed}
		}
		imgID = image.ID(all[0].Target.Digest)
		var named reference.Named
		if !isImageIDPrefix(imgID.String(), imageRef) {
			if nn, err := reference.ParseNormalizedNamed(imageRef); err == nil {
				named = nn
			}
		}
		sameRef, err := i.getSameReferences(ctx, named, all)
		if err != nil {
			return nil, err
		}

		if len(sameRef) == 0 && named != nil {
			return nil, dimages.ErrImageDoesNotExist{Ref: named}
		}

		if len(sameRef) == len(all) && !force {
			c &= ^conflictActiveReference
		}
		if named != nil && len(sameRef) > 0 && len(sameRef) != len(all) {
			if len(options.Platforms) > 0 {
				return i.deleteImagePlatforms(ctx, img, imgID, options.Platforms)
			}
			return i.untagReferences(ctx, sameRef)
		}
	} else {
		imgID = image.ID(img.Target.Digest)
		explicitDanglingRef := strings.HasPrefix(imageRef, imageNameDanglingPrefix) && isDanglingImage(*img)
		if isImageIDPrefix(imgID.String(), imageRef) || explicitDanglingRef {
			if len(options.Platforms) > 0 {
				return i.deleteImagePlatforms(ctx, img, imgID, options.Platforms)
			}
			return i.deleteAll(ctx, imgID, all, c, prune)
		}
		parsedRef, err := reference.ParseNormalizedNamed(img.Name)
		if err != nil {
			return nil, err
		}

		sameRef, err := i.getSameReferences(ctx, parsedRef, all)
		if err != nil {
			return nil, err
		}
		if len(sameRef) != len(all) {
			if len(options.Platforms) > 0 {
				return i.deleteImagePlatforms(ctx, img, imgID, options.Platforms)
			}
			return i.untagReferences(ctx, sameRef)
		} else if len(all) > 1 && !force {
			// Since only a single used reference, remove all active
			//
			// TODO(dmcgowan): Consider keeping the conflict and changing active reference calculation in image checker; https://github.com/moby/moby/pull/46840
			c &= ^conflictActiveReference
		}

		using := func(c *container.Container) bool {
			if c.ImageID == imgID {
				if len(options.Platforms) == 0 {
					return true
				}
				for _, p := range options.Platforms {
					pm := platforms.OnlyStrict(p)
					if pm.Match(c.ImagePlatform) {
						return true
					}
				}

				// No match for the image reference, but continue to check if used as mounted image
			}

			for _, mp := range c.MountPoints {
				if mp.Type == "image" {
					if mp.Spec.Source == string(imgID) {
						return true
					}
				}
			}

			return false
		}
		// TODO(dmcgowan): Should this also check parentage here? https://github.com/moby/moby/pull/46840
		if ctr := i.containers.First(using); ctr != nil {
			if !force {
				// If we removed the repository reference then
				// this image would remain "dangling" and since
				// we really want to avoid that the client must
				// explicitly force its removal.
				return nil, &imageDeleteConflict{
					reference: reference.FamiliarString(parsedRef),
					used:      true,
					message:   fmt.Sprintf("container %s is using its referenced image %s", stringid.TruncateID(ctr.ID), stringid.TruncateID(imgID.String())),
				}
			}

			if len(options.Platforms) > 0 {
				return i.deleteImagePlatforms(ctx, img, imgID, options.Platforms)
			}

			// Delete all images
			if err := i.softImageDelete(ctx, *img, all); err != nil {
				return nil, err
			}

			familiarRef := reference.FamiliarString(parsedRef)
			i.logImageEvent(*img, familiarRef, events.ActionUnTag)
			return []imagetypes.DeleteResponse{{Untagged: familiarRef}}, nil
		}
	}

	if len(options.Platforms) > 0 {
		return i.deleteImagePlatforms(ctx, img, imgID, options.Platforms)
	}
	return i.deleteAll(ctx, imgID, all, c, prune)
}

// deleteImagePlatforms iterates over a slice of platforms and deletes each one for the given image.
func (i *ImageService) deleteImagePlatforms(ctx context.Context, img *c8dimages.Image, imgID image.ID, platformsToDel []ocispec.Platform) ([]imagetypes.DeleteResponse, error) {
	var accumulatedResponses []imagetypes.DeleteResponse
	for _, p := range platformsToDel {
		responses, err := i.deleteImagePlatformByImageID(ctx, img, imgID, &p)
		if err != nil {
			return nil, fmt.Errorf("failed to delete platform %s for image %s: %w", platforms.Format(p), imgID.String(), err)
		}
		accumulatedResponses = append(accumulatedResponses, responses...)
	}
	return accumulatedResponses, nil
}

func (i *ImageService) deleteImagePlatformByImageID(ctx context.Context, img *c8dimages.Image, imgID image.ID, platform *ocispec.Platform) ([]imagetypes.DeleteResponse, error) {
	pm := platforms.OnlyStrict(*platform)
	var target ocispec.Descriptor
	if img == nil {
		// Find any image with the same target
		// We're deleting by digest anyway so it doesn't matter - we just
		// need a c8d image object to pass to getBestPresentImageManifest
		i, err := i.resolveImage(ctx, imgID.String())
		if err != nil {
			return nil, err
		}
		img = &i
	}
	imgMfst, err := i.getBestPresentImageManifest(ctx, *img, pm)
	if err != nil {
		return nil, err
	}
	target = imgMfst.Target()

	var toDelete []ocispec.Descriptor
	err = i.walkPresentChildren(ctx, target, func(ctx context.Context, d ocispec.Descriptor) error {
		toDelete = append(toDelete, d)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// TODO(vvoland): Check if these are not used by other images with different target root images; https://github.com/moby/moby/pull/49982
	// The same manifest can be referenced by different image indexes.
	var response []imagetypes.DeleteResponse
	for _, d := range toDelete {
		if err := i.content.Delete(ctx, d.Digest); err != nil {
			if cerrdefs.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if c8dimages.IsIndexType(d.MediaType) || c8dimages.IsManifestType(d.MediaType) {
			response = append(response, imagetypes.DeleteResponse{Deleted: d.Digest.String()})
		}
	}
	return response, nil
}

// deleteAll deletes the image from the daemon, and if prune is true,
// also deletes dangling parents if there is no conflict in doing so.
// Parent images are removed quietly, and if there is any issue/conflict
// it is logged but does not halt execution/an error is not returned.
func (i *ImageService) deleteAll(ctx context.Context, imgID image.ID, all []c8dimages.Image, c conflictType, prune bool) (records []imagetypes.DeleteResponse, _ error) {
	var parents []c8dimages.Image
	if prune {
		// TODO(dmcgowan): Consider using GC labels to walk for deletion; https://github.com/moby/moby/pull/46840
		var err error
		parents, err = i.parents(ctx, imgID)
		if err != nil {
			log.G(ctx).WithError(err).Warn("failed to get image parents")
		}
	}

	for _, imageRef := range all {
		if err := i.imageDeleteHelper(ctx, imageRef, all, &records, c); err != nil {
			return records, err
		}
	}
	i.LogImageEvent(ctx, imgID.String(), imgID.String(), events.ActionDelete)
	records = append(records, imagetypes.DeleteResponse{Deleted: imgID.String()})

	for _, parent := range parents {
		if !isDanglingImage(parent) {
			break
		}
		if err := i.imageDeleteHelper(ctx, parent, all, &records, conflictSoft); err != nil {
			log.G(ctx).WithError(err).Warn("failed to remove image parent")
			break
		}
		parentID := parent.Target.Digest.String()
		i.LogImageEvent(ctx, parentID, parentID, events.ActionDelete)
		records = append(records, imagetypes.DeleteResponse{Deleted: parentID})
	}

	return records, nil
}

// isImageIDPrefix returns whether the given
// possiblePrefix is a prefix of the given imageID.
func isImageIDPrefix(imageID, possiblePrefix string) bool {
	if strings.HasPrefix(imageID, possiblePrefix) {
		return true
	}
	if i := strings.IndexRune(imageID, ':'); i >= 0 {
		return strings.HasPrefix(imageID[i+1:], possiblePrefix)
	}
	return false
}

// getSameReferences returns the set of images which are the same as:
// - the provided img if non-nil
// - OR the first named image found in the provided image set
// - OR the full set of provided images if no named references in the set
//
// References are considered the same if:
// - Both contain the same name and tag
// - Both contain the same name, one is untagged and no other differing tags in set
// - One is dangling
//
// Note: All imgs should have the same target, only the image name will be considered
// for determining whether images are the same.
func (i *ImageService) getSameReferences(ctx context.Context, named reference.Named, imgs []c8dimages.Image) ([]c8dimages.Image, error) {
	var (
		tag        string
		sameRef    []c8dimages.Image
		digestRefs = []c8dimages.Image{}
		allTags    bool
	)
	if named != nil {
		if tagged, ok := named.(reference.Tagged); ok {
			tag = tagged.Tag()
		} else if _, ok := named.(reference.Digested); ok {
			// If digest is explicitly provided, match all tags
			allTags = true
		}
	}
	for _, ref := range imgs {
		if !isDanglingImage(ref) {
			if repoRef, err := reference.ParseNamed(ref.Name); err == nil {
				if named == nil {
					named = repoRef
					if tagged, ok := named.(reference.Tagged); ok {
						tag = tagged.Tag()
					}
				} else if named.Name() != repoRef.Name() {
					continue
				} else if !allTags {
					if tagged, ok := repoRef.(reference.Tagged); ok {
						if tag == "" {
							tag = tagged.Tag()
						} else if tag != tagged.Tag() {
							// Same repo, different tag, do not include digest refs
							digestRefs = nil
							continue
						}
					} else {
						if digestRefs != nil {
							digestRefs = append(digestRefs, ref)
						}
						// Add digest refs at end if no other tags in the same name
						continue
					}
				}
			} else {
				// Ignore names which do not parse
				log.G(ctx).WithError(err).WithField("image", ref.Name).Info("failed to parse image name, ignoring")
			}
		}
		sameRef = append(sameRef, ref)
	}
	if digestRefs != nil {
		sameRef = append(sameRef, digestRefs...)
	}
	return sameRef, nil
}

type conflictType int

const (
	conflictRunningContainer conflictType = 1 << iota
	conflictActiveReference
	conflictStoppedContainer
	conflictHard = conflictRunningContainer
	conflictSoft = conflictActiveReference | conflictStoppedContainer
)

// imageDeleteHelper attempts to delete the given image from this daemon.
// If the image has any hard delete conflicts (running containers using
// the image) then it cannot be deleted. If the image has any soft delete
// conflicts (any tags/digests referencing the image or any stopped container
// using the image) then it can only be deleted if force is true. Any deleted
// images and untagged references are appended to the given records. If any
// error or conflict is encountered, it will be returned immediately without
// deleting the image.
func (i *ImageService) imageDeleteHelper(ctx context.Context, img c8dimages.Image, all []c8dimages.Image, records *[]imagetypes.DeleteResponse, extra conflictType) error {
	// First, determine if this image has any conflicts. Ignore soft conflicts
	// if force is true.
	c := conflictHard | extra

	imgID := image.ID(img.Target.Digest)

	err := i.checkImageDeleteConflict(ctx, imgID, all, c)
	if err != nil {
		return err
	}

	untaggedRef, err := reference.ParseAnyReference(img.Name)
	if err != nil {
		return err
	}

	if !isDanglingImage(img) && len(all) == 1 && extra&conflictActiveReference != 0 {
		children, err := i.Children(ctx, imgID)
		if err != nil {
			return err
		}
		if len(children) > 0 {
			_, err = i.images.Create(ctx, c8dimages.Image{
				Name:      danglingImageName(img.Target.Digest),
				Target:    img.Target,
				CreatedAt: time.Now(),
				Labels:    img.Labels,
			})
			if err != nil && !cerrdefs.IsAlreadyExists(err) {
				return fmt.Errorf("failed to create dangling image: %w", err)
			}
		}
	}

	// TODO(dmcgowan): Add with target; https://github.com/moby/moby/pull/46840
	err = i.images.Delete(ctx, img.Name, c8dimages.SynchronousDelete())
	if err != nil {
		return err
	}

	if !isDanglingImage(img) {
		i.logImageEvent(img, reference.FamiliarString(untaggedRef), events.ActionUnTag)
		*records = append(*records, imagetypes.DeleteResponse{Untagged: reference.FamiliarString(untaggedRef)})
	}

	return nil
}

// ImageDeleteConflict holds a soft or hard conflict and associated
// error. A hard conflict represents a running container using the
// image, while a soft conflict is any tags/digests referencing the
// given image or any stopped container using the image.
// Implements the error interface.
type imageDeleteConflict struct {
	hard      bool
	used      bool
	reference string
	message   string
}

func (idc *imageDeleteConflict) Error() string {
	var forceMsg string
	if idc.hard {
		forceMsg = "cannot be forced"
	} else {
		forceMsg = "must be forced"
	}
	return fmt.Sprintf("conflict: unable to delete %s (%s) - %s", idc.reference, forceMsg, idc.message)
}

func (*imageDeleteConflict) Conflict() {}

// untagReferences deletes the given image references and returns the appropriate response records
func (i *ImageService) untagReferences(ctx context.Context, refs []c8dimages.Image) ([]imagetypes.DeleteResponse, error) {
	var records []imagetypes.DeleteResponse
	for _, ref := range refs {
		// TODO(dmcgowan): Add with target; https://github.com/moby/moby/pull/46840
		err := i.images.Delete(ctx, ref.Name)
		if err != nil {
			return nil, err
		}
		if nn, err := reference.ParseNormalizedNamed(ref.Name); err == nil {
			familiarRef := reference.FamiliarString(nn)
			i.logImageEvent(ref, familiarRef, events.ActionUnTag)
			records = append(records, imagetypes.DeleteResponse{Untagged: familiarRef})
		}
	}
	return records, nil
}

// checkImageDeleteConflict returns a conflict representing
// any issue preventing deletion of the given image ID, and
// nil if there are none. It takes a bitmask representing a
// filter for which conflict types the caller cares about,
// and will only check for these conflict types.
func (i *ImageService) checkImageDeleteConflict(ctx context.Context, imgID image.ID, all []c8dimages.Image, mask conflictType) error {
	if mask&conflictRunningContainer != 0 {
		running := func(c *container.Container) bool {
			return c.ImageID == imgID && c.State.IsRunning()
		}
		if ctr := i.containers.First(running); ctr != nil {
			return &imageDeleteConflict{
				reference: stringid.TruncateID(imgID.String()),
				hard:      true,
				used:      true,
				message:   "image is being used by running container " + stringid.TruncateID(ctr.ID),
			}
		}
	}

	if mask&conflictStoppedContainer != 0 {
		stopped := func(c *container.Container) bool {
			return !c.State.IsRunning() && c.ImageID == imgID
		}
		if ctr := i.containers.First(stopped); ctr != nil {
			return &imageDeleteConflict{
				reference: stringid.TruncateID(imgID.String()),
				used:      true,
				message:   "image is being used by stopped container " + stringid.TruncateID(ctr.ID),
			}
		}
	}

	if mask&conflictActiveReference != 0 {
		// TODO(dmcgowan): Count unexpired references; https://github.com/moby/moby/pull/46840
		if len(all) > 1 {
			return &imageDeleteConflict{
				reference: stringid.TruncateID(imgID.String()),
				message:   "image is referenced in multiple repositories",
			}
		}
	}

	return nil
}
