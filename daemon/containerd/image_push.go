package containerd

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	containerdlabels "github.com/containerd/containerd/v2/pkg/labels"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/auxprogress"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/v2/daemon/internal/metrics"
	"github.com/moby/moby/v2/daemon/internal/progress"
	"github.com/moby/moby/v2/daemon/internal/streamformatter"
	"github.com/moby/moby/v2/daemon/server/imagebackend"
	"github.com/moby/moby/v2/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/semaphore"
)

// PushImage initiates a push operation of the image pointed to by sourceRef.
// If reference is untagged, all tags from the reference repository are pushed.
// Image manifest (or index) is pushed as is, which will probably fail if you
// don't have all content referenced by the index.
// Cross-repo mounts will be attempted for non-existing blobs.
//
// It will also add distribution source labels to the pushed content
// pointing to the new target repository. This will allow subsequent pushes
// to perform cross-repo mounts of the shared content when pushing to a different
// repository on the same registry.
func (i *ImageService) PushImage(ctx context.Context, sourceRef reference.Named, options imagebackend.PushOptions) (retErr error) {
	if len(options.Platforms) > 1 {
		// TODO(thaJeztah): add support for pushing multiple platforms
		return cerrdefs.ErrInvalidArgument.WithMessage("multiple platforms is not supported")
	}
	start := time.Now()
	defer func() {
		if retErr == nil {
			metrics.ImageActions.WithValues("push").UpdateSince(start)
		}
	}()
	var platform *ocispec.Platform
	if len(options.Platforms) > 0 {
		p := options.Platforms[0]
		platform = &p
	}
	out := streamformatter.NewJSONProgressOutput(options.OutStream, false)
	progress.Messagef(out, "", "The push refers to repository [%s]", sourceRef.Name())

	if _, tagged := sourceRef.(reference.Tagged); !tagged {
		if _, digested := sourceRef.(reference.Digested); !digested {
			// Image is not tagged nor digested, that means all tags push was requested.

			// Find all images with the same repository.
			imgs, err := i.getAllImagesWithRepository(ctx, sourceRef)
			if err != nil {
				return err
			}

			if len(imgs) == 0 {
				return fmt.Errorf("An image does not exist locally with the tag: %s", reference.FamiliarName(sourceRef))
			}

			for _, img := range imgs {
				named, err := reference.ParseNamed(img.Name)
				if err != nil {
					// This shouldn't happen, but log a warning just in case.
					log.G(ctx).WithFields(log.Fields{
						"image":     img.Name,
						"sourceRef": sourceRef,
					}).Warn("refusing to push an invalid tag")
					continue
				}

				if err := i.pushRef(ctx, named, platform, options.MetaHeaders, options.AuthConfig, out); err != nil {
					return err
				}
			}

			return nil
		}
	}

	return i.pushRef(ctx, sourceRef, platform, options.MetaHeaders, options.AuthConfig, out)
}

func (i *ImageService) pushRef(ctx context.Context, targetRef reference.Named, platform *ocispec.Platform, metaHeaders map[string][]string, authConfig *registry.AuthConfig, out progress.Output) (retErr error) {
	leasedCtx, release, err := i.client.WithLease(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := release(context.WithoutCancel(leasedCtx)); err != nil {
			log.G(ctx).WithField("image", targetRef).WithError(err).Warn("failed to release lease created for push")
		}
	}()

	img, err := i.images.Get(ctx, targetRef.String())
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return errdefs.NotFound(fmt.Errorf("tag does not exist: %s", reference.FamiliarString(targetRef)))
		}
		return errdefs.System(err)
	}

	target := img.Target
	if platform != nil {
		target, err = i.getPushDescriptor(ctx, img, platform)
		if err != nil {
			return err
		}
	}

	store := i.content
	resolver, tracker := i.newResolverFromAuthConfig(ctx, authConfig, targetRef, metaHeaders)
	pp := pushProgress{Tracker: tracker}
	jobsQueue := newJobs()
	finishProgress := jobsQueue.showProgress(ctx, out, combinedProgress([]progressUpdater{
		&pp,
		&pullProgress{showExists: false, store: store},
	}))
	defer func() {
		finishProgress()
		if retErr == nil {
			if tagged, ok := targetRef.(reference.Tagged); ok {
				progress.Messagef(out, "", "%s: digest: %s size: %d", tagged.Tag(), target.Digest, target.Size)
			}
		}
	}()

	i.transferLimitMu.Lock()
	limiter := i.uploadLimiter
	i.transferLimitMu.Unlock()

	mountableBlobs, err := findMissingMountable(ctx, store, jobsQueue, target, targetRef, limiter)
	if err != nil {
		return err
	}

	// Create a store which fakes the local existence of possibly mountable blobs.
	// Otherwise they can't be pushed at all.
	realStore := store
	wrapped := wrapWithFakeMountableBlobs(store, mountableBlobs)
	store = wrapped
	addLayerJobs := c8dimages.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		if showBlobProgress(desc) {
			jobsQueue.Add(desc)
		}

		return nil, nil
	})

	handlerWrapper := func(h c8dimages.Handler) c8dimages.Handler {
		return c8dimages.Handlers(addLayerJobs, h)
	}

	push := func(ctx context.Context, desc ocispec.Descriptor) error {
		ref := targetRef

		if _, digested := ref.(reference.Digested); !digested {
			// Annotate ref with digest to push only push tag for single digest
			ref, err = reference.WithDigest(ref, target.Digest)
			if err != nil {
				return err
			}
		}
		pusher, err := resolver.Pusher(ctx, ref.String())
		if err != nil {
			return err
		}

		return remotes.PushContent(ctx, pusher, desc, store, limiter, platforms.All, handlerWrapper)
	}

	err = push(ctx, target)
	if err != nil {
		// If push failed because of a missing content, no specific platform was requested
		// and the target is an index, select a platform-specific manifest to push instead.
		if cerrdefs.IsNotFound(err) && c8dimages.IsIndexType(target.MediaType) && platform == nil {
			var newTarget ocispec.Descriptor
			newTarget, err = i.getPushDescriptor(ctx, img, nil)
			if err != nil {
				return err
			}

			// Retry only if the new push candidate is different from the previous one.
			if newTarget.Digest != target.Digest {
				orgTarget := target
				target = newTarget
				pp.TurnNotStartedIntoUnavailable()
				err = push(ctx, target)

				if err == nil {
					progress.Aux(out, auxprogress.ManifestPushedInsteadOfIndex{
						ManifestPushedInsteadOfIndex: true,
						OriginalIndex:                orgTarget,
						SelectedManifest:             newTarget,
					})
				}
			}
		}

		if err != nil {
			if !cerrdefs.IsNotFound(err) {
				return translateRegistryError(ctx, err)
			}
			progress.Aux(out, auxprogress.ContentMissing{
				ContentMissing: true,
				Desc:           target,
			})
			return errdefs.NotFound(err)
		}
	}

	appendDistributionSourceLabel(ctx, realStore, targetRef, target)

	i.LogImageEvent(ctx, reference.FamiliarString(targetRef), reference.FamiliarName(targetRef), events.ActionPush)

	return nil
}

func (i *ImageService) getPushDescriptor(ctx context.Context, img c8dimages.Image, platform *ocispec.Platform) (ocispec.Descriptor, error) {
	pm := i.matchRequestedOrDefault(platforms.OnlyStrict, platform)

	anyMissing := false

	var presentMatchingManifests []*ImageManifest
	err := i.walkReachableImageManifests(ctx, img, func(im *ImageManifest) error {
		available, err := im.CheckContentAvailable(ctx)
		if err != nil {
			return fmt.Errorf("failed to determine availability of image manifest %s: %w", im.Target().Digest, err)
		}

		if !available {
			anyMissing = true
			return nil
		}

		if im.IsAttestation() {
			return nil
		}

		imgPlatform, err := im.ImagePlatform(ctx)
		if err != nil {
			return fmt.Errorf("failed to determine platform of image %s: %w", img.Name, err)
		}

		if !pm.Match(imgPlatform) {
			return nil
		}

		presentMatchingManifests = append(presentMatchingManifests, im)
		return nil
	})
	if err != nil {
		return ocispec.Descriptor{}, errdefs.System(err)
	}

	switch len(presentMatchingManifests) {
	case 0:
		err := &errPlatformNotFound{imageRef: imageFamiliarName(img)}
		if pm.Requested != nil {
			err.wanted = *pm.Requested
		}
		return ocispec.Descriptor{}, err
	case 1:
		// Only one manifest is available AND matching the requested platform.

		if platform != nil {
			// Explicit platform was requested
			return presentMatchingManifests[0].Target(), nil
		}

		// No specific platform was requested, but only one manifest is available.
		if anyMissing {
			return presentMatchingManifests[0].Target(), nil
		}

		// Index has only one manifest anyway, select the full index.
		return img.Target, nil
	default:
		if platform == nil {
			if !anyMissing {
				// No specific platform requested, and all manifests are available, select the full index.
				return img.Target, nil
			}

			return ocispec.Descriptor{}, errdefs.Conflict(errors.Errorf("multiple matching manifests found but no specific platform requested"))
		}

		return ocispec.Descriptor{}, errdefs.Conflict(errors.Errorf("multiple manifests found for platform %s", platforms.FormatAll(*platform)))
	}
}

func appendDistributionSourceLabel(ctx context.Context, realStore content.Store, targetRef reference.Named, target ocispec.Descriptor) {
	appendSource, err := docker.AppendDistributionSourceLabel(realStore, targetRef.String())
	if err != nil {
		// This shouldn't happen at this point because the reference would have to be invalid
		// and if it was, then it would error out earlier.
		log.G(ctx).WithError(err).Warn("failed to create an handler that appends distribution source label to pushed content")
		return
	}

	handler := presentChildrenHandler(realStore, appendSource)
	if err := c8dimages.Dispatch(ctx, handler, nil, target); err != nil {
		// Shouldn't happen, but even if it would fail, then make it only a warning
		// because it doesn't affect the pushed data.
		log.G(ctx).WithError(err).Warn("failed to append distribution source labels to pushed content")
	}
}

// findMissingMountable will walk the target descriptor recursively and return
// missing contents with their distribution source which could potentially
// be cross-repo mounted.
func findMissingMountable(ctx context.Context, store content.Store, queue *jobs,
	target ocispec.Descriptor, targetRef reference.Named, limiter *semaphore.Weighted,
) (map[digest.Digest]distributionSource, error) {
	mountableBlobs := map[digest.Digest]distributionSource{}
	var mutex sync.Mutex

	sources, err := getDigestSources(ctx, store, target.Digest)
	if err != nil {
		if !cerrdefs.IsNotFound(err) {
			return nil, err
		}
		log.G(ctx).WithField("target", target).Debug("distribution source label not found")
		return mountableBlobs, nil
	}

	handler := func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		_, err := store.Info(ctx, desc.Digest)
		if err != nil {
			if !cerrdefs.IsNotFound(err) {
				return nil, errdefs.System(errors.Wrapf(err, "failed to get metadata of content %s", desc.Digest.String()))
			}

			for _, source := range sources {
				if canBeMounted(desc.MediaType, targetRef, source) {
					mutex.Lock()
					mountableBlobs[desc.Digest] = source
					mutex.Unlock()
					queue.Add(desc)
					break
				}
			}
			return nil, nil
		}

		return c8dimages.Children(ctx, store, desc)
	}

	err = c8dimages.Dispatch(ctx, c8dimages.HandlerFunc(handler), limiter, target)
	if err != nil {
		return nil, err
	}

	return mountableBlobs, nil
}

func getDigestSources(ctx context.Context, store content.Manager, digest digest.Digest) ([]distributionSource, error) {
	info, err := store.Info(ctx, digest)
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil, errdefs.NotFound(err)
		}
		return nil, errdefs.System(err)
	}

	sources := extractDistributionSources(info.Labels)
	if sources == nil {
		return nil, errdefs.NotFound(fmt.Errorf("label %q is not attached to %s", containerdlabels.LabelDistributionSource, digest.String()))
	}

	return sources, nil
}

func extractDistributionSources(labels map[string]string) []distributionSource {
	var sources []distributionSource

	// Check if this blob has a distributionSource label
	// if yes, read it as source
	for k, v := range labels {
		if reg, ok := strings.CutPrefix(k, containerdlabels.LabelDistributionSource); ok {
			for repo := range strings.SplitSeq(v, ",") {
				ref, err := reference.ParseNamed(reg + "/" + repo)
				if err != nil {
					continue
				}

				sources = append(sources, distributionSource{
					registryRef: ref,
				})
			}
		}
	}

	return sources
}

type distributionSource struct {
	registryRef reference.Named
}

// ToAnnotation returns key and value
func (source distributionSource) ToAnnotation() (string, string) {
	domain := reference.Domain(source.registryRef)
	v := reference.Path(source.registryRef)
	return containerdlabels.LabelDistributionSource + domain, v
}

func (source distributionSource) GetReference(dgst digest.Digest) (reference.Named, error) {
	return reference.WithDigest(source.registryRef, dgst)
}

// canBeMounted returns if the content with given media type can be cross-repo
// mounted when pushing it to a remote reference ref.
func canBeMounted(mediaType string, targetRef reference.Named, source distributionSource) bool {
	if c8dimages.IsManifestType(mediaType) {
		return false
	}
	if c8dimages.IsIndexType(mediaType) {
		return false
	}

	reg := reference.Domain(targetRef)
	// Remove :port suffix from domain
	// containerd distribution source label doesn't store port
	if portIdx := strings.LastIndex(reg, ":"); portIdx != -1 {
		reg = reg[:portIdx]
	}

	// If the source registry is the same as the one we are pushing to
	// then the cross-repo mount will work.
	return reg == reference.Domain(source.registryRef)
}
