package containerd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/containerd/v2/core/leases"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/moby/go-archive/compression"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/v2/daemon/images"
	"github.com/moby/moby/v2/daemon/internal/streamformatter"
	"github.com/moby/moby/v2/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// ExportImage exports a list of images to the given output stream. The
// exported images are archived into a tar when written to the output
// stream. All images with the given tag and all versions containing
// the same tag are exported. names is the set of tags to export, and
// outStream is the writer which the images are written to.
//
// TODO(thaJeztah): produce JSON stream progress response and image events; see https://github.com/moby/moby/issues/43910
func (i *ImageService) ExportImage(ctx context.Context, names []string, platformList []ocispec.Platform, outStream io.Writer) error {
	// Get the platform matcher for the requested platforms (matches all platforms if none specified)
	pm := matchAnyWithPreference(i.hostPlatformMatcher(), platformList)
	referrers := newReferrersForExport(i.content)

	opts := []archive.ExportOpt{
		archive.WithSkipNonDistributableBlobs(),

		// This makes the exported archive also include `manifest.json`
		// when the image is a manifest list. It is needed for backwards
		// compatibility with Docker image format.
		// The containerd will choose only one manifest for the `manifest.json`.
		// Our preference is to have it point to the default platform.
		// Example:
		//  Daemon is running on linux/arm64
		//  When we export linux/amd64 and linux/arm64, manifest.json will point to linux/arm64.
		//  When we export linux/amd64 only, manifest.json will point to linux/amd64.
		// Note: This only matters when importing this archive into non-containerd Docker.
		// Importing the same archive into containerd, will not restrict the platforms.
		archive.WithPlatform(pm),
		archive.WithSkipMissing(i.content),
		archive.WithReferrersProvider(referrers),
	}

	ctx, done, err := i.withLease(ctx, false)
	if err != nil {
		return errdefs.System(err)
	}
	defer done()

	addLease := func(ctx context.Context, target ocispec.Descriptor) error {
		return i.leaseContent(ctx, i.content, target)
	}

	exportImage := func(ctx context.Context, img c8dimages.Image, ref reference.Named) error {
		target := img.Target

		// If a single platform is requested, export the manifest for the specific platform only
		// (single-level index). Otherwise export the full index (two-level, nested). Note that
		// since opts includes WithPlatform and WithSkipMissing, the index will contain the
		// requested platforms only, and only if they are available in the content store.
		if len(platformList) == 1 {
			newTarget, err := i.getPushDescriptor(ctx, img, &platformList[0])
			if err != nil {
				return errors.Wrap(err, "no suitable export target found")
			}
			target = newTarget
		}

		if err := addLease(ctx, target); err != nil {
			return err
		}

		if ref != nil {
			opts = append(opts, archive.WithManifest(target, ref.String()))

			log.G(ctx).WithFields(log.Fields{
				"target": target,
				"name":   ref,
			}).Debug("export image")
		} else {
			orgTarget := target
			target.Annotations = make(map[string]string)

			for k, v := range orgTarget.Annotations {
				switch k {
				case c8dimages.AnnotationImageName, ocispec.AnnotationRefName:
					// Strip image name/tag annotations from the descriptor.
					// Otherwise containerd will use it as name.
				default:
					target.Annotations[k] = v
				}
			}

			opts = append(opts, archive.WithManifest(target))

			log.G(ctx).WithFields(log.Fields{
				"target": target,
			}).Debug("export image without name")
		}

		i.LogImageEvent(ctx, target.Digest.String(), target.Digest.String(), events.ActionSave)
		return nil
	}

	exportRepository := func(ctx context.Context, ref reference.Named) error {
		imgs, err := i.getAllImagesWithRepository(ctx, ref)
		if err != nil {
			return errdefs.System(fmt.Errorf("failed to list all images from repository %s: %w", ref.Name(), err))
		}

		if len(imgs) == 0 {
			return images.ErrImageDoesNotExist{Ref: ref}
		}

		for _, img := range imgs {
			ref, err := reference.ParseNamed(img.Name)
			if err != nil {
				log.G(ctx).WithFields(log.Fields{
					"image": img.Name,
					"error": err,
				}).Warn("couldn't parse image name as a valid named reference")
				continue
			}

			if err := exportImage(ctx, img, ref); err != nil {
				return err
			}
		}

		return nil
	}

	for _, name := range names {
		img, resolveErr := i.resolveImage(ctx, name)

		// Check if the requested name is a truncated digest of the resolved descriptor.
		// If yes, that means that the user specified a specific image ID so
		// it's not referencing a repository.
		specificDigestResolved := false
		if resolveErr == nil {
			nameWithoutDigestAlgorithm := strings.TrimPrefix(name, img.Target.Digest.Algorithm().String()+":")
			specificDigestResolved = strings.HasPrefix(img.Target.Digest.Encoded(), nameWithoutDigestAlgorithm)
		}

		log.G(ctx).WithFields(log.Fields{
			"name":                   name,
			"img":                    img,
			"resolveErr":             resolveErr,
			"specificDigestResolved": specificDigestResolved,
		}).Debug("export requested")

		ref, refErr := reference.ParseNormalizedNamed(name)

		if refErr == nil {
			if _, ok := ref.(reference.Digested); ok {
				specificDigestResolved = true
			}
		} else if resolveErr != nil {
			// Both reference parsing and image resolution failed,
			// so the name is not a valid image reference. Return
			// the format error so the API gives a 400 instead of
			// a misleading "not found" response.
			return errdefs.InvalidParameter(refErr)
		}

		if resolveErr != nil || !specificDigestResolved {
			// Name didn't resolve to anything, or name wasn't explicitly referencing a digest
			if refErr == nil && reference.IsNameOnly(ref) {
				// Reference is valid, but doesn't include a specific tag.
				// Export all images with the same repository.
				if err := exportRepository(ctx, ref); err != nil {
					return err
				}
				continue
			}
		}

		if resolveErr != nil {
			return resolveErr
		}
		if refErr != nil {
			return errdefs.InvalidParameter(refErr)
		}

		// If user exports a specific digest, it shouldn't have a tag.
		if specificDigestResolved {
			ref = nil
		}

		if err := exportImage(ctx, img, ref); err != nil {
			return err
		}
	}

	opts = append(opts, archive.WithReferrersProvider(referrers))

	return i.client.Export(ctx, outStream, opts...)
}

// leaseContent will add a resource to the lease for each child of the descriptor making sure that it and
// its children won't be deleted while the lease exists
func (i *ImageService) leaseContent(ctx context.Context, store content.Store, desc ocispec.Descriptor) error {
	lid, ok := leases.FromContext(ctx)
	if !ok {
		return nil
	}
	lease := leases.Lease{ID: lid}
	leasesManager := i.client.LeasesService()
	return c8dimages.Walk(ctx, c8dimages.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		_, err := store.Info(ctx, desc.Digest)
		if err != nil {
			if errors.Is(err, cerrdefs.ErrNotFound) {
				return nil, nil
			}
			return nil, errdefs.System(err)
		}

		r := leases.Resource{
			ID:   desc.Digest.String(),
			Type: "content",
		}
		if err := leasesManager.AddResource(ctx, lease, r); err != nil {
			return nil, errdefs.System(err)
		}

		return c8dimages.Children(ctx, store, desc)
	}), desc)
}

// importImages preserves previous image targets while importing the archive.
// The caller must hold a lease in ctx until the import and reference cleanup finish.
func (i *ImageService) importImages(ctx context.Context, inTar io.Reader, pm platforms.MatchComparer, skipMissing bool) ([]c8dimages.Image, error) {
	previousImagesByTarget := map[digest.Digest]c8dimages.Image{}
	opts := []containerd.ImportOpt{
		containerd.WithImportPlatform(pm),

		containerd.WithImportReferrers(newReferrersForImport(i.content)),

		// Create an additional image with dangling name for imported images...
		containerd.WithDigestRef(danglingImageName),
		// ... but only if they don't have a name or it's invalid.
		containerd.WithSkipDigestRef(func(nameFromArchive string) bool {
			if nameFromArchive == "" {
				return false
			}

			ref, err := reference.ParseNormalizedNamed(nameFromArchive)
			if err != nil {
				return false
			}

			// Keep the previous image content leased until the import reveals
			// whether the name moved to a different target.
			if img, err := i.images.Get(ctx, ref.String()); err == nil {
				if _, ok := previousImagesByTarget[img.Target.Digest]; !ok {
					previousImagesByTarget[img.Target.Digest] = img
					if err := i.leaseContent(ctx, i.content, img.Target); err != nil {
						log.G(ctx).WithError(err).Warnf("failed to lease the previous image for %s", img.Name)
						if err := i.ensureDanglingImage(ctx, img); err != nil {
							log.G(ctx).WithError(err).Warnf("failed to keep the previous image for %s as dangling", img.Name)
						}
					}
				}
			} else if !cerrdefs.IsNotFound(err) {
				log.G(ctx).WithError(err).Warn("failed to retrieve image: %w", err)
			}
			return true
		}),
	}

	if skipMissing {
		// Allow variants to be missing if no specific platform is requested.
		opts = append(opts, containerd.WithSkipMissing())
	}

	imgs, err := i.client.Import(ctx, inTar, opts...)
	cleanupCtx := context.WithoutCancel(ctx)
	for target, previous := range previousImagesByTarget {
		refs, listErr := i.images.List(cleanupCtx, "target.digest=="+target.String())
		if listErr != nil {
			log.G(ctx).WithError(listErr).WithField("digest", target).Warn("failed to retrieve references to previous image")
			continue
		}

		hasNamedRef := false
		for _, ref := range refs {
			if !isDanglingImage(ref) {
				hasNamedRef = true
				break
			}
		}

		if !hasNamedRef {
			if err := i.ensureDanglingImage(cleanupCtx, previous); err != nil {
				log.G(ctx).WithError(err).WithField("digest", target).Warn("failed to keep previous image as dangling")
			}
			continue
		}

		if err := i.images.Delete(cleanupCtx, danglingImageName(target)); err != nil && !cerrdefs.IsNotFound(err) {
			log.G(ctx).WithError(err).WithField("digest", target).Warn("failed to remove redundant dangling image")
		}
	}

	return imgs, err
}

// LoadImage uploads a set of images into the repository. This is the
// complement of ExportImage.  The input stream is an uncompressed tar
// ball containing images and metadata.
func (i *ImageService) LoadImage(ctx context.Context, inTar io.ReadCloser, platformList []ocispec.Platform, outStream io.Writer, quiet bool) error {
	decompressed, err := compression.DecompressStream(inTar)
	if err != nil {
		return errors.Wrap(err, "failed to decompress input tar archive")
	}
	defer decompressed.Close()

	ctx, done, err := i.withLease(ctx, true)
	if err != nil {
		return errdefs.System(err)
	}
	defer done()

	specificPlatforms := len(platformList) > 0

	// Get the platform matcher for the requested platforms (matches all platforms if none specified)
	pm := matchAnyWithPreference(i.hostPlatformMatcher(), platformList)

	imgs, err := i.importImages(ctx, decompressed, pm, !specificPlatforms)
	if err != nil {
		if specificPlatforms {
			platformNames := make([]string, 0, len(platformList))
			for _, p := range platformList {
				platformNames = append(platformNames, platforms.FormatAll(p))
			}
			log.G(ctx).WithFields(log.Fields{"error": err, "platforms": platformNames}).Debug("failed to import image to containerd")

			// Note: ErrEmptyWalk will not be returned in most cases as
			// index.json will contain a descriptor of the actual OCI index or
			// Docker manifest list, so the walk is never empty.
			// Even in case of a single-platform image, the manifest descriptor
			// doesn't have a platform set, so it won't be filtered out by the
			// FilterPlatform containerd handler.
			if errors.Is(err, c8dimages.ErrEmptyWalk) {
				return errdefs.NotFound(errors.Wrapf(err, "requested platform(s) (%v) not found", platformNames))
			}
			if cerrdefs.IsNotFound(err) {
				return errdefs.NotFound(errors.Wrapf(err, "requested platform(s) (%v) found, but some content is missing", platformNames))
			}
		}
		log.G(ctx).WithError(err).Debug("failed to import image to containerd")
		return errdefs.System(err)
	}

	if specificPlatforms {
		// Verify that the requested platform(s) are available for the loaded images.
		// While the ideal behavior here would be to verify whether the input
		// archive actually supplied them, we're not able to determine that
		// as the imported index is not returned by the import operation.
		platformNames := make([]string, 0, len(platformList))
		for _, p := range platformList {
			platformNames = append(platformNames, platforms.FormatAll(p))
		}
		if err := i.verifyImagesProvidePlatform(ctx, imgs, platformNames, pm); err != nil {
			return err
		}
	}

	progress := streamformatter.NewStdoutWriter(outStream)
	// Unpack only an image of the host platform
	unpackPm := i.hostPlatformMatcher()
	// If a load of specific platform is requested, unpack it
	if specificPlatforms {
		unpackPm = pm
	}

	for _, img := range imgs {
		name := img.Name
		loadedMsg := "Loaded image"

		if isDanglingImage(img) {
			name = img.Target.Digest.String()
			loadedMsg = "Loaded image ID"
		} else if named, err := reference.ParseNormalizedNamed(img.Name); err == nil {
			name = reference.FamiliarString(reference.TagNameOnly(named))
		}

		if !isDanglingImage(img) {
			i.warmImageIdentityCache(ctx, img)
		}

		err = i.unpackLoadedImage(ctx, img, name, unpackPm)

		fmt.Fprintf(progress, "%s: %s\n", loadedMsg, name)
		i.LogImageEvent(ctx, img.Target.Digest.String(), img.Target.Digest.String(), events.ActionLoad)

		if err != nil {
			// The image failed to unpack, but is already imported, log the error but don't fail the whole load.
			fmt.Fprintf(progress, "Error unpacking image %s: %v\n", name, err)
		}
	}

	return nil
}

func (i *ImageService) unpackLoadedImage(ctx context.Context, img c8dimages.Image, name string, pm platforms.Matcher) error {
	return i.walkImageManifests(ctx, img, func(platformImg *ImageManifest) error {
		logger := log.G(ctx).WithFields(log.Fields{
			"image":    name,
			"manifest": platformImg.Target().Digest,
		})

		if isPseudo, err := platformImg.IsPseudoImage(ctx); isPseudo || err != nil {
			if err != nil {
				logger.WithError(err).Warn("failed to read manifest")
			} else {
				logger.Debug("don't unpack non-image manifest")
			}
			return nil
		}

		imgPlat, err := platformImg.ImagePlatform(ctx)
		if err != nil {
			logger.WithError(err).Warn("failed to read image platform, skipping unpack")
			return nil
		}

		if !pm.Match(imgPlat) {
			return nil
		}

		unpacked, err := platformImg.IsUnpacked(ctx, i.snapshotter)
		if err != nil {
			logger.WithError(err).Warn("failed to check if image is unpacked")
			return nil
		}

		if !unpacked {
			err = platformImg.Unpack(ctx, i.snapshotter)
			if err != nil {
				return errdefs.System(err)
			}
		}
		logger.WithField("alreadyUnpacked", unpacked).WithError(err).Debug("unpack")
		return nil
	})
}

// verifyImagesProvidePlatform checks if the requested platform is loaded.
// If the requested platform is not loaded, it returns an error.
func (i *ImageService) verifyImagesProvidePlatform(ctx context.Context, imgs []c8dimages.Image, platformNames []string, pm platforms.Matcher) error {
	if len(imgs) == 0 {
		return errdefs.NotFound(fmt.Errorf("no images providing the requested platform(s) found: %v", platformNames))
	}
	var incompleteImgs []string
	for _, img := range imgs {
		hasRequestedPlatform := false
		err := i.walkImageManifests(ctx, img, func(platformImg *ImageManifest) error {
			imgPlat, err := platformImg.ImagePlatform(ctx)
			if err != nil {
				if cerrdefs.IsNotFound(err) {
					return nil
				}
				return errors.Wrapf(err, "failed to determine image platform")
			}

			if !pm.Match(imgPlat) {
				return nil
			}
			available, err := platformImg.CheckContentAvailable(ctx)
			if err != nil {
				return errors.Wrapf(err, "failed to determine image content availability for platform(s) %s", platformNames)
			}

			if available {
				hasRequestedPlatform = true
				return nil
			}
			return nil
		})
		if err != nil {
			return errdefs.System(err)
		}
		if !hasRequestedPlatform {
			incompleteImgs = append(incompleteImgs, imageFamiliarName(img))
		}
	}

	var msg string
	switch len(incompleteImgs) {
	case 0:
		// Success - All images provide the requested platform.
		return nil
	case 1:
		msg = "image %s was loaded, but doesn't provide the requested platform (%s)"
	default:
		msg = "images [%s] were loaded, but don't provide the requested platform (%s)"
	}

	return errdefs.NotFound(fmt.Errorf(msg, strings.Join(incompleteImgs, ", "), platformNames))
}

type referrersForImport struct {
	store      content.Store
	candidates *referrersList
}

func newReferrersForImport(store content.Store) *referrersForImport {
	return &referrersForImport{
		store:      store,
		candidates: newReferrersList(),
	}
}

func (r *referrersForImport) Referrers(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
	if r.candidates.readFrom(ctx, r.store, desc) != nil {
		return nil, nil
	}
	refs, ok := r.candidates.Get(desc.Digest)
	if !ok {
		return nil, nil
	}
	return refs, nil
}

type referrersForExport struct {
	store content.Store
}

func newReferrersForExport(store content.Store) *referrersForExport {
	return &referrersForExport{
		store: store,
	}
}

func (r *referrersForExport) Referrers(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
	info, err := r.store.Info(ctx, desc.Digest)
	if err != nil {
		if errors.Is(err, cerrdefs.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var refs []ocispec.Descriptor
	for k, v := range info.Labels {
		if strings.HasPrefix(k, "containerd.io/gc.ref.content.referrer.sha256.") {
			dgst, err := digest.Parse(v)
			if err != nil {
				continue
			}
			var desc ocispec.Descriptor
			desc.Digest = dgst
			info, err := r.store.Info(ctx, dgst)
			if err != nil {
				continue
			}
			desc.Size = info.Size
			// parse mediatype and artifact type
			dt, err := content.ReadBlob(ctx, r.store, ocispec.Descriptor{Digest: dgst})
			if err != nil {
				continue
			}
			var mfst ocispec.Manifest
			if err := json.Unmarshal(dt, &mfst); err != nil {
				continue
			}
			desc.MediaType = mfst.MediaType
			if mfst.ArtifactType != "" {
				desc.ArtifactType = mfst.ArtifactType
			}
			// TODO: we should only export signatures but cosign doesn't set artifact type on payload
			refs = append(refs, desc)
		}
	}

	return refs, nil
}
