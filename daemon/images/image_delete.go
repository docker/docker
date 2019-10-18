package images // import "github.com/docker/docker/daemon/images"

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/containerd/containerd/content"
	cerrdefs "github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/log"
	creference "github.com/containerd/containerd/reference"
	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/container"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/image"
	"github.com/docker/docker/pkg/stringid"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

type conflictType int

const (
	conflictRunningContainer conflictType = 1 << iota
	conflictActiveReference
	conflictStoppedContainer
	conflictHard = conflictRunningContainer
	conflictSoft = conflictActiveReference | conflictStoppedContainer
)

// ImageDelete deletes the image referenced by the given imageRef from this
// daemon. The given imageRef can be an image ID, ID prefix, or a repository
// reference (with an optional tag or digest, defaulting to the tag name
// "latest"). There is differing behavior depending on whether the given
// imageRef is a repository reference or not.
//
// If the given imageRef is a repository reference then that repository
// reference will be removed. However, if there exists any containers which
// were created using the same image reference then the repository reference
// cannot be removed unless either there are other repository references to the
// same image or force is true. Following removal of the repository reference,
// the referenced image itself will attempt to be deleted as described below
// but quietly, meaning any image delete conflicts will cause the image to not
// be deleted and the conflict will not be reported.
//
// There may be conflicts preventing deletion of an image and these conflicts
// are divided into two categories grouped by their severity:
//
// Hard Conflict:
// 	- a pull or build using the image.
// 	- any descendant image.
// 	- any running container using the image.
//
// Soft Conflict:
// 	- any stopped container using the image.
// 	- any repository tag or digest references to the image.
//      - TODO(containerd): has label "io.cri-containerd.image==managed"
//
// The image cannot be removed if there are any hard conflicts and can be
// removed if there are soft conflicts only if force is true.
//
// If prune is true, ancestor images will each attempt to be deleted quietly,
// meaning any delete conflicts will cause the image to not be deleted and the
// conflict will not be reported.
//
func (i *ImageService) ImageDelete(ctx context.Context, imageRef string, force, prune bool) ([]types.ImageDeleteResponseItem, error) {
	start := time.Now()

	img, err := i.ResolveImage(ctx, imageRef)
	if err != nil {
		return nil, err
	}

	imgID := img.Digest.String()

	// TODO(containerd): Use containerd filter on list containers
	using := func(c *container.Container) bool {
		return digest.Digest(c.ImageID) == img.Digest
	}

	is := i.client.ImageService()
	imgs, err := is.List(ctx, fmt.Sprintf("target.digest==%s", img.Digest))
	if err != nil {
		return nil, err
	}

	if !isImageIDPrefix(imgID, imageRef) {
		var deletedRefs []string

		// A repository reference was given and should be removed
		// first. We can only remove this reference if either force is
		// true, there are multiple repository references to this
		// image, or there are no containers using the given reference.
		if !force && isSingleReference(ctx, imgs) {
			if container := i.containers.First(using); container != nil {
				// If we removed the repository reference then
				// this image would remain "dangling" and since
				// we really want to avoid that the client must
				// explicitly force its removal.
				err := errors.Errorf("conflict: unable to remove repository reference %q (must force) - container %s is using its referenced image %s", imageRef, stringid.TruncateID(container.ID), stringid.TruncateID(imgID))
				return nil, errdefs.Conflict(err)
			}
		}

		// TODO(containerd): normalize ref then use containerd reference parsing
		parsedRef, err := reference.ParseNormalizedNamed(imageRef)
		if err != nil {
			return nil, err
		}
		imageRef = parsedRef.String()
		locator := parsedRef.Name()

		deletedRefs = append(deletedRefs, imageRef)

		// If a tag reference was removed and the only remaining
		// references to the same repository are digest references,
		// then clean up those digest references.
		if !isCanonicalReference(imageRef) {
			foundRepoTagRef := false
			canonicalRefs := []string{}
			for _, img := range imgs {
				if imageRef == img.Name {
					continue
				}

				spec, err := creference.Parse(img.Name)
				if err != nil {
					log.G(ctx).WithError(err).WithField("name", img.Name).Warnf("ignoring bad name")
					continue
				}
				if locator == spec.Locator {
					if !isCanonicalReference(img.Name) {
						foundRepoTagRef = true
						break
					}
					canonicalRefs = append(canonicalRefs, img.Name)
				}

			}
			if !foundRepoTagRef {
				// Remove canonical references from same repository
				deletedRefs = append(deletedRefs, canonicalRefs...)
			}
		}

		// If it has remaining references then the untag finishes the remove
		// and there is no need to check for parent reference removal
		if len(imgs)-len(deletedRefs) > 0 {
			records := []types.ImageDeleteResponseItem{}
			for _, ref := range deletedRefs {
				if err := is.Delete(ctx, ref); err != nil && !errdefs.IsNotFound(err) {
					return records, errors.Wrapf(err, "failed to delete ref: %s", ref)
				}
				var fref string
				if !strings.HasPrefix(ref, "<") {
					pref, err := reference.ParseNormalizedNamed(ref)
					if err != nil {
						return records, errors.Wrapf(err, "failed to parse ref: %s", ref)
					}
					fref = reference.FamiliarString(pref)
					i.LogImageEvent(ctx, imgID, fref, "untag")
					records = append(records, types.ImageDeleteResponseItem{Untagged: fref})
				}
			}
			return records, nil
		}

		c := conflictHard
		if !force {
			c |= conflictSoft
		}
		if conflict := i.checkImageDeleteConflict(img, c, false); conflict != nil {
			log.G(ctx).Debugf("%s: ignoring conflict: %#v", img.Digest, conflict)
			// TODO(containerd): Keep one reference to prevent deletion?
		}

	} else {
		// If an ID reference was given AND there is at most one tag
		// reference to the image AND all references are within one
		// repository, then remove all references.

		c := conflictHard
		active := false
		if !force {
			// If not forced, fail on soft conflicts
			c |= conflictSoft

			active = !isSingleReference(ctx, imgs)
		}

		if conflict := i.checkImageDeleteConflict(img, c, active); conflict != nil {
			return nil, conflict
		}
	}

	cs := i.client.ContentStore()

	layers := map[string][]digest.Digest{}
	seenParents := map[string]struct{}{}
	var wh images.HandlerFunc = func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		switch desc.MediaType {
		case images.MediaTypeDockerSchema2Config, ocispec.MediaTypeImageConfig:
			info, err := cs.Info(ctx, desc.Digest)
			if err != nil {
				return nil, err
			}

			var parents []ocispec.Descriptor
			for k, v := range info.Labels {
				if k == LabelImageParent {
					// Since parent relationship are client defined by labels rather
					// than a hash tree, ensure parents do not mistakenly loop
					if _, ok := seenParents[v]; !ok {
						log.G(ctx).WithField("image", imgID).WithField("config", desc.Digest.String()).Debugf("deleted image has parent: %s", v)
						parents = append(parents, ocispec.Descriptor{
							MediaType: images.MediaTypeDockerSchema2Config,
							Digest:    digest.Digest(v),
							// Size can be safely ignored here
						})
						seenParents[v] = struct{}{}
					}
				} else if strings.HasPrefix(k, LabelLayerPrefix) {
					driver := k[len(LabelLayerPrefix):]
					layers[driver] = append(layers[driver], digest.Digest(v))
				}
			}
			return parents, nil
		}
		return nil, nil
	}

	if err := images.Walk(ctx, images.Handlers(images.ChildrenHandler(cs), wh), img); err != nil {
		if !cerrdefs.IsNotFound(err) {
			return nil, err
		}
		log.G(ctx).WithError(err).Warnf("missing object, some layer removals may be excluded")
	}

	// Remove all references
	records := []types.ImageDeleteResponseItem{}
	for j, img := range imgs {
		var opts []images.DeleteOpt
		if j == len(imgs)-1 {
			opts = append(opts, images.SynchronousDelete())
		}
		if err := is.Delete(ctx, img.Name, opts...); err != nil && !errdefs.IsNotFound(err) {
			return records, errors.Wrapf(err, "failed to delete ref: %s", img.Name)
		}
		if !strings.HasPrefix(img.Name, "<") {
			pref, err := reference.ParseNormalizedNamed(img.Name)
			if err != nil {
				return records, errors.Wrapf(err, "failed to parse ref: %s", img.Name)
			}
			fref := reference.FamiliarString(pref)
			i.LogImageEvent(ctx, imgID, fref, "untag")
			records = append(records, types.ImageDeleteResponseItem{Untagged: fref})
		}
	}

	// Lookup image to see if it was deleted
	if _, err := cs.Info(ctx, img.Digest); err != nil {
		if !cerrdefs.IsNotFound(err) {
			return records, errors.Wrap(err, "failed to lookup image in content store")
		}
		records = append(records, types.ImageDeleteResponseItem{Deleted: imgID})

		if len(layers) > 0 {
			c, err := i.getCache(ctx)
			if err != nil {
				log.G(ctx).WithError(err).Errorf("unable to get cache, skipping layer removal")
			}

			c.m.Lock()
			for name, chainIDs := range layers {
				ls, ok := i.layerStores[name]
				if !ok {
					log.G(ctx).WithField("driver", name).Warnf("layer store not configured for referenced layers, skipping removal")
					continue
				}
				retained := c.layers[name]

				key := LabelLayerPrefix + name
				var filters []string
				unmarked := map[digest.Digest]struct{}{}
				for _, chainID := range chainIDs {
					filters = append(filters, fmt.Sprintf("labels.%q==%s", key, chainID))
					unmarked[chainID] = struct{}{}
				}

				// Mark referenced layers by removing from unmarked
				err := cs.Walk(ctx, func(i content.Info) error {
					v := i.Labels[key]
					if v != "" {
						log.G(ctx).WithField("key", key).Debugf("Still there after removal...")
						delete(unmarked, digest.Digest(v))
					}
					return nil
				}, filters...)
				if err != nil {
					log.G(ctx).WithError(err).WithField("driver", name).Errorf("mark failed, skipping layer removal")
				}

				for chainID := range unmarked {
					l, ok := retained[chainID]
					if ok {
						metadata, err := ls.Release(l)
						if err != nil {
							log.G(ctx).WithError(err).WithField("driver", name).WithField("layer", chainID).Errorf("layer release failed")
						}
						for _, m := range metadata {
							log.G(ctx).WithField("driver", name).WithField("layer", m.ChainID).Infof("layer removed")
						}
						delete(retained, chainID)
					} else {
						log.G(ctx).WithField("driver", name).WithField("id", chainID.String()).Warnf("referenced layer not retained")
					}
				}
			}
			c.m.Unlock()
		}
	}

	imageActions.WithValues("delete").UpdateSince(start)

	return records, nil
}

func isCanonicalReference(ref string) bool {
	// TODO(containerd): Use a regex
	return strings.ContainsAny(ref, "@")
}

// isSingleReference returns true when all references are from one repository
// and there is at most one tag. Returns false for empty input.
func isSingleReference(ctx context.Context, imgs []images.Image) bool {
	if len(imgs) <= 1 {
		return len(imgs) == 1
	}
	var singleRef string
	canonicalRefs := map[string]struct{}{}
	for _, img := range imgs {
		if isCanonicalReference(img.Name) {
			ref, err := creference.Parse(img.Name)
			if err != nil {
				log.G(ctx).WithField("name", img.Name).Warnf("ignoring unparseable reference")
				continue
			}
			canonicalRefs[ref.Locator] = struct{}{}
		} else if singleRef == "" {
			singleRef = img.Name
		} else {
			return false
		}
	}
	if len(canonicalRefs) != 1 {
		return false
	}

	if singleRef != "" {
		ref, err := creference.Parse(singleRef)
		if err == nil {
			_, ok := canonicalRefs[ref.Locator]
			return ok
		}
		log.G(ctx).WithField("name", singleRef).Warnf("ignoring unparseable reference")
	}
	return true

}

// isImageIDPrefix returns whether the given possiblePrefix is a prefix of the
// given imageID.
func isImageIDPrefix(imageID, possiblePrefix string) bool {
	if strings.HasPrefix(imageID, possiblePrefix) {
		return true
	}

	if i := strings.IndexRune(imageID, ':'); i >= 0 {
		return strings.HasPrefix(imageID[i+1:], possiblePrefix)
	}

	return false
}

// ImageDeleteConflict holds a soft or hard conflict and an associated error.
// Implements the error interface.
type imageDeleteConflict struct {
	hard    bool
	imgID   image.ID
	message string
}

func (idc *imageDeleteConflict) Error() string {
	var forceMsg string
	if idc.hard {
		forceMsg = "cannot be forced"
	} else {
		forceMsg = "must be forced"
	}

	return fmt.Sprintf("conflict: unable to delete %s (%s) - %s", stringid.TruncateID(idc.imgID.String()), forceMsg, idc.message)
}

func (idc *imageDeleteConflict) Conflict() {}

// checkImageDeleteConflict determines whether there are any conflicts
// preventing deletion of the given image from this daemon. A hard conflict is
// any image which has the given image as a parent or any running container
// using the image. A soft conflict is any tags/digest referencing the given
// image or any stopped container using the image. If ignoreSoftConflicts is
// true, this function will not check for soft conflict conditions.
func (i *ImageService) checkImageDeleteConflict(img ocispec.Descriptor, mask conflictType, active bool) *imageDeleteConflict {
	if mask&conflictRunningContainer != 0 {
		// Check if any running container is using the image.
		running := func(c *container.Container) bool {
			return digest.Digest(c.ImageID) == img.Digest && c.IsRunning()
		}
		if container := i.containers.First(running); container != nil {
			return &imageDeleteConflict{
				imgID:   image.ID(img.Digest),
				hard:    true,
				message: fmt.Sprintf("image is being used by running container %s", stringid.TruncateID(container.ID)),
			}
		}
	}

	// Check if any repository tags/digest reference this image.
	if mask&conflictActiveReference != 0 && active {
		return &imageDeleteConflict{
			imgID:   image.ID(img.Digest),
			message: "image is referenced in multiple repositories",
		}
	}

	if mask&conflictStoppedContainer != 0 {
		// Check if any stopped containers reference this image.
		stopped := func(c *container.Container) bool {
			return !c.IsRunning() && digest.Digest(c.ImageID) == img.Digest
		}
		if container := i.containers.First(stopped); container != nil {
			return &imageDeleteConflict{
				imgID:   image.ID(img.Digest),
				message: fmt.Sprintf("image is being used by stopped container %s", stringid.TruncateID(container.ID)),
			}
		}
	}

	return nil
}
