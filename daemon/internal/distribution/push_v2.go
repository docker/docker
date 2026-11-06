package distribution

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"

	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/log"
	"github.com/distribution/reference"
	"github.com/docker/distribution"
	"github.com/docker/distribution/manifest/schema2"
	"github.com/docker/distribution/registry/api/errcode"
	"github.com/docker/distribution/registry/client"
	"github.com/moby/moby/v2/daemon/internal/distribution/metadata"
	"github.com/moby/moby/v2/daemon/internal/distribution/xfer"
	"github.com/moby/moby/v2/daemon/internal/layer"
	"github.com/moby/moby/v2/daemon/internal/progress"
	"github.com/moby/moby/v2/daemon/internal/stringid"
	"github.com/moby/moby/v2/daemon/pkg/registry"
	"github.com/moby/moby/v2/pkg/ioutils"
	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

const (
	smallLayerMaximumSize  = 100 * (1 << 10) // 100KB
	middleLayerMaximumSize = 10 * (1 << 20)  // 10MB
)

// newPusher creates a new pusher for pushing to a v2 registry.
// The parameters are passed through to the underlying pusher implementation for
// use during the actual push operation.
func newPusher(ref reference.Named, endpoint registry.APIEndpoint, repoName reference.Named, config *ImagePushConfig) *pusher {
	return &pusher{
		metadataService: metadata.NewV2MetadataService(config.MetadataStore),
		ref:             ref,
		endpoint:        endpoint,
		repoName:        repoName,
		config:          config,
	}
}

type pusher struct {
	metadataService metadata.V2MetadataService
	ref             reference.Named
	endpoint        registry.APIEndpoint
	repoName        reference.Named
	config          *ImagePushConfig
	repo            distribution.Repository

	// pushState is state built by the Upload functions.
	pushState pushState
}

type pushState struct {
	sync.Mutex
	// remoteLayers is the set of layers known to exist on the remote side.
	// This avoids redundant queries when pushing multiple tags that
	// involve the same layers. It is also used to fill in digest and size
	// information when building the manifest.
	remoteLayers map[layer.DiffID]distribution.Descriptor
	hasAuthInfo  bool
}

// TODO(tiborvass): have push() take a reference to repository + tag, so that the pusher itself is repository-agnostic.
func (p *pusher) push(ctx context.Context) (err error) {
	p.pushState.remoteLayers = make(map[layer.DiffID]distribution.Descriptor)

	p.repo, err = newRepository(ctx, p.repoName, p.endpoint, p.config.MetaHeaders, p.config.AuthConfig, "push", "pull")
	p.pushState.hasAuthInfo = p.config.AuthConfig.RegistryToken != "" || (p.config.AuthConfig.Username != "" && p.config.AuthConfig.Password != "")
	if err != nil {
		log.G(ctx).Debugf("Error getting v2 registry: %v", err)
		return err
	}

	if err = p.pushRepository(ctx); err != nil {
		// [Service.LookupPushEndpoints] never returns mirror endpoint.
		if continueOnError(err, false) {
			return fallbackError{
				err:         err,
				transportOK: true,
			}
		}
	}
	return err
}

func (p *pusher) pushRepository(ctx context.Context) (err error) {
	if namedTagged, isNamedTagged := p.ref.(reference.NamedTagged); isNamedTagged {
		imageID, err := p.config.ReferenceStore.Get(p.ref)
		if err != nil {
			return fmt.Errorf("tag does not exist: %s", reference.FamiliarString(p.ref))
		}

		return p.pushTag(ctx, namedTagged, imageID)
	}

	if !reference.IsNameOnly(p.ref) {
		return errors.New("cannot push a digest reference")
	}

	// Push all tags
	pushed := 0
	for _, association := range p.config.ReferenceStore.ReferencesByName(p.ref) {
		if namedTagged, isNamedTagged := association.Ref.(reference.NamedTagged); isNamedTagged {
			pushed++
			if err := p.pushTag(ctx, namedTagged, association.ID); err != nil {
				return err
			}
		}
	}

	if pushed == 0 {
		return fmt.Errorf("no tags to push for %s", reference.FamiliarName(p.repoName))
	}

	return nil
}

func (p *pusher) pushTag(ctx context.Context, ref reference.NamedTagged, id digest.Digest) error {
	log.G(ctx).Debugf("Pushing repository: %s", reference.FamiliarString(ref))

	imgConfig, err := p.config.ImageStore.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("could not find image from tag %s: %v", reference.FamiliarString(ref), err)
	}

	rootfs, err := rootFSFromConfig(imgConfig)
	if err != nil {
		return fmt.Errorf("unable to get rootfs for image %s: %s", reference.FamiliarString(ref), err)
	}

	l, err := p.config.LayerStores.Get(rootfs.ChainID())
	if err != nil {
		return fmt.Errorf("failed to get top layer from image: %v", err)
	}
	defer l.Release()

	hmacKey, err := metadata.ComputeV2MetadataHMACKey(p.config.AuthConfig)
	if err != nil {
		return fmt.Errorf("failed to compute hmac key of auth config: %v", err)
	}

	var descriptors []xfer.UploadDescriptor

	descriptorTemplate := pushDescriptor{
		metadataService: p.metadataService,
		hmacKey:         hmacKey,
		repoName:        p.repoName,
		ref:             p.ref,
		repo:            p.repo,
		pushState:       &p.pushState,
	}

	// Loop bounds condition is to avoid pushing the base layer on Windows.
	for range rootfs.DiffIDs {
		descriptor := descriptorTemplate
		descriptor.layer = l
		descriptor.checkedDigests = make(map[digest.Digest]struct{})
		descriptors = append(descriptors, &descriptor)

		l = l.Parent()
	}

	if err := p.config.UploadManager.Upload(ctx, descriptors, p.config.ProgressOutput); err != nil {
		return err
	}

	// Try schema2 first
	builder := schema2.NewManifestBuilder(p.repo.Blobs(ctx), p.config.ConfigMediaType, imgConfig)
	manifest, err := manifestFromBuilder(ctx, builder, descriptors)
	if err != nil {
		return err
	}

	manSvc, err := p.repo.Manifests(ctx)
	if err != nil {
		return err
	}

	putOptions := []distribution.ManifestServiceOption{distribution.WithTag(ref.Tag())}
	if _, err = manSvc.Put(ctx, manifest, putOptions...); err != nil {
		if err.Error() == "tag invalid" {
			msg := "[DEPRECATED] support for pushing manifest v2 schema1 images has been removed. More information at https://docs.docker.com/registry/spec/deprecated-schema-v1/"
			log.G(ctx).WithError(err).Error(msg)
			err = errors.Wrap(err, msg)
		}
		return err
	}

	var canonicalManifest []byte

	switch v := manifest.(type) {
	case *schema2.DeserializedManifest:
		_, canonicalManifest, err = v.Payload()
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown manifest type %T", v)
	}

	manifestDigest := digest.FromBytes(canonicalManifest)
	progress.Messagef(p.config.ProgressOutput, "", "%s: digest: %s size: %d", ref.Tag(), manifestDigest, len(canonicalManifest))

	if err := addDigestReference(p.config.ReferenceStore, ref, manifestDigest, id); err != nil {
		return err
	}

	// pushResult contains the tag, manifest digest, and manifest size from the
	// push. It's used to signal this information to the trust code in the client
	// so it can sign the manifest if necessary.
	//
	// TODO(thaJeztah): this aux-type is only present for docker content trust, which is deprecated.
	type pushResult struct {
		Tag    string
		Digest string
		Size   int
	}

	// Signal digest to the trust client so it can sign the
	// push, if appropriate.
	progress.Aux(p.config.ProgressOutput, pushResult{Tag: ref.Tag(), Digest: manifestDigest.String(), Size: len(canonicalManifest)})

	return nil
}

func manifestFromBuilder(ctx context.Context, builder distribution.ManifestBuilder, descriptors []xfer.UploadDescriptor) (distribution.Manifest, error) {
	// descriptors is in reverse order; iterate backwards to get references
	// appended in the right order.
	for _, descriptor := range slices.Backward(descriptors) {
		if err := builder.AppendReference(descriptor.(*pushDescriptor)); err != nil {
			return nil, err
		}
	}

	return builder.Build(ctx)
}

type pushDescriptor struct {
	layer            PushLayer
	metadataService  metadata.V2MetadataService
	hmacKey          []byte
	repoName         reference.Named
	ref              reference.Named
	repo             distribution.Repository
	pushState        *pushState
	remoteDescriptor distribution.Descriptor
	// a set of digests whose presence has been checked in a target repository
	checkedDigests map[digest.Digest]struct{}
}

func (pd *pushDescriptor) Key() string {
	return "v2push:" + pd.ref.Name() + " " + pd.layer.DiffID().String()
}

func (pd *pushDescriptor) ID() string {
	return stringid.TruncateID(pd.layer.DiffID().String())
}

func (pd *pushDescriptor) DiffID() layer.DiffID {
	return pd.layer.DiffID()
}

func (pd *pushDescriptor) Upload(ctx context.Context, progressOutput progress.Output) (distribution.Descriptor, error) {
	diffID := pd.DiffID()

	pd.pushState.Lock()
	if descriptor, ok := pd.pushState.remoteLayers[diffID]; ok {
		// it is already known that the push is not needed and
		// therefore doing a stat is unnecessary
		pd.pushState.Unlock()
		progress.Update(progressOutput, pd.ID(), "Layer already exists")
		return descriptor, nil
	}
	pd.pushState.Unlock()

	maxMountAttempts, maxExistenceChecks, checkOtherRepositories := getMaxMountAndExistenceCheckAttempts(pd.layer)

	// Do we have any metadata associated with this layer's DiffID?
	metaData, err := pd.metadataService.GetMetadata(diffID)
	if err == nil {
		// check for blob existence in the target repository
		descriptor, exists, err := pd.layerAlreadyExists(ctx, progressOutput, diffID, true, 1, metaData)
		if exists || err != nil {
			return descriptor, err
		}
	}

	// if digest was empty or not saved, or if blob does not exist on the remote repository,
	// then push the blob.
	bs := pd.repo.Blobs(ctx)

	var layerUpload distribution.BlobWriter

	// Attempt to find another repository in the same registry to mount the layer from to avoid an unnecessary upload
	candidates := getRepositoryMountCandidates(pd.repoName, pd.hmacKey, maxMountAttempts, metaData)
	isUnauthorizedError := false
	for _, mc := range candidates {
		mountCandidate := mc
		log.G(ctx).Debugf("attempting to mount layer %s (%s) from %s", diffID, mountCandidate.Digest, mountCandidate.SourceRepository)
		createOpts := []distribution.BlobCreateOption{}

		if mountCandidate.SourceRepository != "" {
			namedRef, err := reference.ParseNormalizedNamed(mountCandidate.SourceRepository)
			if err != nil {
				log.G(ctx).WithError(err).Errorf("failed to parse source repository reference %v", reference.FamiliarString(namedRef))
				_ = pd.metadataService.Remove(mountCandidate)
				continue
			}

			// Candidates are always under same domain, create remote reference
			// with only path to set mount from with
			remoteRef, err := reference.WithName(reference.Path(namedRef))
			if err != nil {
				log.G(ctx).WithError(err).Errorf("failed to make remote reference out of %q", reference.Path(namedRef))
				continue
			}

			canonicalRef, err := reference.WithDigest(reference.TrimNamed(remoteRef), mountCandidate.Digest)
			if err != nil {
				log.G(ctx).WithError(err).Error("failed to make canonical reference")
				continue
			}

			createOpts = append(createOpts, client.WithMountFrom(canonicalRef))
		}

		// send the layer
		lu, err := bs.Create(ctx, createOpts...)
		switch err := err.(type) {
		case nil:
			// noop
		case distribution.ErrBlobMounted:
			progress.Updatef(progressOutput, pd.ID(), "Mounted from %s", err.From.Name())

			err.Descriptor.MediaType = c8dimages.MediaTypeDockerSchema2LayerGzip

			pd.pushState.Lock()
			pd.pushState.remoteLayers[diffID] = err.Descriptor
			pd.pushState.Unlock()

			// Cache mapping from this layer's DiffID to the blobsum
			if err := pd.metadataService.TagAndAdd(diffID, pd.hmacKey, metadata.V2Metadata{
				Digest:           err.Descriptor.Digest,
				SourceRepository: pd.repoName.Name(),
			}); err != nil {
				return distribution.Descriptor{}, xfer.DoNotRetry{Err: err}
			}
			return err.Descriptor, nil
		case errcode.Errors:
			for _, e := range err {
				switch e := e.(type) {
				case errcode.Error:
					if e.Code == errcode.ErrorCodeUnauthorized {
						// when unauthorized error that indicate user don't has right to push layer to register
						log.G(ctx).Debugln("failed to push layer to registry because unauthorized error")
						isUnauthorizedError = true
					}
				default:
				}
			}
		default:
			log.G(ctx).Infof("failed to mount layer %s (%s) from %s: %v", diffID, mountCandidate.Digest, mountCandidate.SourceRepository, err)
		}

		// when error is unauthorizedError and user don't hasAuthInfo that's the case user don't has right to push layer to register
		// and he hasn't login either, in this case candidate cache should be removed
		if mountCandidate.SourceRepository != "" &&
			(!isUnauthorizedError || pd.pushState.hasAuthInfo) &&
			(metadata.CheckV2MetadataHMAC(&mountCandidate, pd.hmacKey) ||
				mountCandidate.HMAC == "") {
			cause := "blob mount failure"
			if err != nil {
				cause = fmt.Sprintf("an error: %v", err.Error())
			}
			log.G(ctx).Debugf("removing association between layer %s and %s due to %s", mountCandidate.Digest, mountCandidate.SourceRepository, cause)
			_ = pd.metadataService.Remove(mountCandidate)
		}

		if lu != nil {
			// cancel previous upload
			cancelLayerUpload(ctx, mountCandidate.Digest, layerUpload)
			layerUpload = lu
		}
	}

	if maxExistenceChecks-len(pd.checkedDigests) > 0 {
		// do additional layer existence checks with other known digests if any
		descriptor, exists, err := pd.layerAlreadyExists(ctx, progressOutput, diffID, checkOtherRepositories, maxExistenceChecks-len(pd.checkedDigests), metaData)
		if exists || err != nil {
			return descriptor, err
		}
	}

	log.G(ctx).Debugf("Pushing layer: %s", diffID)
	if layerUpload == nil {
		layerUpload, err = bs.Create(ctx)
		if err != nil {
			return distribution.Descriptor{}, retryOnError(err)
		}
	}
	defer layerUpload.Close()
	// upload the blob
	return pd.uploadUsingSession(ctx, progressOutput, diffID, layerUpload)
}

func (pd *pushDescriptor) SetRemoteDescriptor(descriptor distribution.Descriptor) {
	pd.remoteDescriptor = descriptor
}

func (pd *pushDescriptor) Descriptor() distribution.Descriptor {
	return pd.remoteDescriptor
}

func (pd *pushDescriptor) uploadUsingSession(
	ctx context.Context,
	progressOutput progress.Output,
	diffID layer.DiffID,
	layerUpload distribution.BlobWriter,
) (distribution.Descriptor, error) {
	var reader io.ReadCloser

	contentReader, err := pd.layer.Open()
	if err != nil {
		return distribution.Descriptor{}, retryOnError(err)
	}

	reader = progress.NewProgressReader(ioutils.NewCancelReadCloser(ctx, contentReader), progressOutput, pd.layer.Size(), pd.ID(), "Pushing")

	switch m := pd.layer.MediaType(); m {
	case c8dimages.MediaTypeDockerSchema2Layer:
		compressedReader, compressionDone := compress(reader)
		defer func(closer io.Closer) {
			closer.Close()
			<-compressionDone
		}(reader)
		reader = compressedReader
	case c8dimages.MediaTypeDockerSchema2LayerGzip:
	default:
		reader.Close()
		return distribution.Descriptor{}, xfer.DoNotRetry{Err: fmt.Errorf("unsupported layer media type %s", m)}
	}

	digester := digest.Canonical.Digester()
	tee := io.TeeReader(reader, digester.Hash())

	nn, err := layerUpload.ReadFrom(tee)
	reader.Close()
	if err != nil {
		return distribution.Descriptor{}, retryOnError(err)
	}

	pushDigest := digester.Digest()
	if _, err := layerUpload.Commit(ctx, distribution.Descriptor{Digest: pushDigest}); err != nil {
		return distribution.Descriptor{}, retryOnError(err)
	}

	log.G(ctx).Debugf("uploaded layer %s (%s), %d bytes", diffID, pushDigest, nn)
	progress.Update(progressOutput, pd.ID(), "Pushed")

	// Cache mapping from this layer's DiffID to the blobsum
	if err := pd.metadataService.TagAndAdd(diffID, pd.hmacKey, metadata.V2Metadata{
		Digest:           pushDigest,
		SourceRepository: pd.repoName.Name(),
	}); err != nil {
		return distribution.Descriptor{}, xfer.DoNotRetry{Err: err}
	}

	desc := distribution.Descriptor{
		Digest:    pushDigest,
		MediaType: c8dimages.MediaTypeDockerSchema2LayerGzip,
		Size:      nn,
	}

	pd.pushState.Lock()
	pd.pushState.remoteLayers[diffID] = desc
	pd.pushState.Unlock()

	return desc, nil
}

// layerAlreadyExists checks if the registry already knows about any of the metadata passed in the "metadata"
// slice. If it finds one that the registry knows about, it returns the known digest and "true". If
// "checkOtherRepositories" is true, stat will be performed also with digests mapped to any other repository
// (not just the target one).
func (pd *pushDescriptor) layerAlreadyExists(
	ctx context.Context,
	progressOutput progress.Output,
	diffID layer.DiffID,
	checkOtherRepositories bool,
	maxExistenceCheckAttempts int,
	v2Metadata []metadata.V2Metadata,
) (_ distribution.Descriptor, exists bool, _ error) {
	// filter the metadata
	candidates := []metadata.V2Metadata{}
	for _, meta := range v2Metadata {
		if meta.SourceRepository != "" && !checkOtherRepositories && meta.SourceRepository != pd.repoName.Name() {
			continue
		}
		candidates = append(candidates, meta)
	}
	// sort the candidates by similarity
	sortV2MetadataByLikenessAndAge(pd.repoName, pd.hmacKey, candidates)

	digestToMetadata := make(map[digest.Digest]*metadata.V2Metadata)
	// an array of unique blob digests ordered from the best mount candidates to worst
	layerDigests := []digest.Digest{}
	for i := 0; i < len(candidates); i++ {
		if len(layerDigests) >= maxExistenceCheckAttempts {
			break
		}
		meta := &candidates[i]
		if _, ok := digestToMetadata[meta.Digest]; ok {
			// keep reference just to the first mapping (the best mount candidate)
			continue
		}
		if _, ok := pd.checkedDigests[meta.Digest]; ok {
			// existence of this digest has already been tested
			continue
		}
		digestToMetadata[meta.Digest] = meta
		layerDigests = append(layerDigests, meta.Digest)
	}

	var desc distribution.Descriptor

attempts:
	for _, dgst := range layerDigests {
		meta := digestToMetadata[dgst]
		log.G(ctx).Debugf("Checking for presence of layer %s (%s) in %s", diffID, dgst, pd.repoName.Name())
		var err error
		desc, err = pd.repo.Blobs(ctx).Stat(ctx, dgst)
		pd.checkedDigests[meta.Digest] = struct{}{}
		switch {
		case err == nil:
			if m, ok := digestToMetadata[desc.Digest]; !ok || m.SourceRepository != pd.repoName.Name() || !metadata.CheckV2MetadataHMAC(m, pd.hmacKey) {
				// cache mapping from this layer's DiffID to the blobsum
				if err := pd.metadataService.TagAndAdd(diffID, pd.hmacKey, metadata.V2Metadata{
					Digest:           desc.Digest,
					SourceRepository: pd.repoName.Name(),
				}); err != nil {
					return distribution.Descriptor{}, false, xfer.DoNotRetry{Err: err}
				}
			}
			desc.MediaType = c8dimages.MediaTypeDockerSchema2LayerGzip
			exists = true
			break attempts
		case errors.Is(err, distribution.ErrBlobUnknown):
			if meta.SourceRepository == pd.repoName.Name() {
				// remove the mapping to the target repository
				if err := pd.metadataService.Remove(*meta); err != nil {
					log.G(ctx).WithError(err).Debug("Failed remove metadata")
				}
			}
		default:
			log.G(ctx).WithError(err).Debugf("Failed to check for presence of layer %s (%s) in %s", diffID, dgst, pd.repoName.Name())
		}
	}

	if exists {
		progress.Update(progressOutput, pd.ID(), "Layer already exists")
		pd.pushState.Lock()
		pd.pushState.remoteLayers[diffID] = desc
		pd.pushState.Unlock()
	}

	return desc, exists, nil
}

// getMaxMountAndExistenceCheckAttempts returns a maximum number of cross repository mount attempts from
// source repositories of target registry, maximum number of layer existence checks performed on the target
// repository and whether the check shall be done also with digests mapped to different repositories. The
// decision is based on layer size. The smaller the layer, the fewer attempts shall be made because the cost
// of upload does not outweigh a latency.
func getMaxMountAndExistenceCheckAttempts(layer PushLayer) (maxMountAttempts, maxExistenceCheckAttempts int, checkOtherRepositories bool) {
	size := layer.Size()
	switch {
	// big blob
	case size > middleLayerMaximumSize:
		// 1st attempt to mount the blob few times
		// 2nd few existence checks with digests associated to any repository
		// then fallback to upload
		return 4, 3, true

	// middle sized blobs; if we could not get the size, assume we deal with middle sized blob
	case size > smallLayerMaximumSize:
		// 1st attempt to mount blobs of average size few times
		// 2nd try at most 1 existence check if there's an existing mapping to the target repository
		// then fallback to upload
		return 3, 1, false

	// small blobs, do a minimum number of checks
	default:
		return 1, 1, false
	}
}

// getRepositoryMountCandidates returns an array of v2 metadata items belonging to the given registry. The
// array is sorted from youngest to oldest. The resulting array will contain only metadata entries having
// registry part of SourceRepository matching the part of repoInfo.
func getRepositoryMountCandidates(
	repoInfo reference.Named,
	hmacKey []byte,
	maxCandidates int,
	v2Metadata []metadata.V2Metadata,
) []metadata.V2Metadata {
	candidates := []metadata.V2Metadata{}
	for _, meta := range v2Metadata {
		sourceRepo, err := reference.ParseNamed(meta.SourceRepository)
		if err != nil || reference.Domain(repoInfo) != reference.Domain(sourceRepo) {
			continue
		}
		// target repository is not a viable candidate
		if meta.SourceRepository == repoInfo.Name() {
			continue
		}
		candidates = append(candidates, meta)
	}

	sortV2MetadataByLikenessAndAge(repoInfo, hmacKey, candidates)
	if maxCandidates >= 0 && len(candidates) > maxCandidates {
		// select the youngest metadata
		candidates = candidates[:maxCandidates]
	}

	return candidates
}

// byLikeness is a sorting container for v2 metadata candidates for cross repository mount. The
// candidate "a" is preferred over "b":
//
//  1. if it was hashed using the same AuthConfig as the one used to authenticate to target repository and the
//     "b" was not
//  2. if a number of its repository path components exactly matching path components of target repository is higher
type byLikeness struct {
	arr            []metadata.V2Metadata
	hmacKey        []byte
	pathComponents []string
}

func (bla byLikeness) Less(i, j int) bool {
	aMacMatch := metadata.CheckV2MetadataHMAC(&bla.arr[i], bla.hmacKey)
	bMacMatch := metadata.CheckV2MetadataHMAC(&bla.arr[j], bla.hmacKey)
	if aMacMatch != bMacMatch {
		return aMacMatch
	}
	aMatch := numOfMatchingPathComponents(bla.arr[i].SourceRepository, bla.pathComponents)
	bMatch := numOfMatchingPathComponents(bla.arr[j].SourceRepository, bla.pathComponents)
	return aMatch > bMatch
}

func (bla byLikeness) Swap(i, j int) {
	bla.arr[i], bla.arr[j] = bla.arr[j], bla.arr[i]
}
func (bla byLikeness) Len() int { return len(bla.arr) }

func sortV2MetadataByLikenessAndAge(repoInfo reference.Named, hmacKey []byte, marr []metadata.V2Metadata) {
	// reverse the metadata array to shift the newest entries to the beginning
	for i := 0; i < len(marr)/2; i++ {
		marr[i], marr[len(marr)-i-1] = marr[len(marr)-i-1], marr[i]
	}
	// keep equal entries ordered from the youngest to the oldest
	sort.Stable(byLikeness{
		arr:            marr,
		hmacKey:        hmacKey,
		pathComponents: getPathComponents(repoInfo.Name()),
	})
}

// numOfMatchingPathComponents returns a number of path components in "pth" that exactly match "matchComponents".
func numOfMatchingPathComponents(pth string, matchComponents []string) int {
	pthComponents := getPathComponents(pth)
	i := 0
	for ; i < len(pthComponents) && i < len(matchComponents); i++ {
		if matchComponents[i] != pthComponents[i] {
			return i
		}
	}
	return i
}

func getPathComponents(path string) []string {
	return strings.Split(path, "/")
}

func cancelLayerUpload(ctx context.Context, dgst digest.Digest, layerUpload distribution.BlobWriter) {
	if layerUpload != nil {
		log.G(ctx).Debugf("cancelling upload of blob %s", dgst)
		err := layerUpload.Cancel(ctx)
		if err != nil {
			log.G(ctx).Warnf("failed to cancel upload: %v", err)
		}
	}
}
