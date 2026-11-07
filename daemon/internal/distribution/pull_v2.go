package distribution

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/docker/distribution"
	"github.com/docker/distribution/manifest/manifestlist"
	"github.com/docker/distribution/manifest/ocischema"
	"github.com/docker/distribution/manifest/schema2"
	"github.com/docker/distribution/registry/client/transport"
	"github.com/moby/moby/v2/daemon/internal/distribution/metadata"
	"github.com/moby/moby/v2/daemon/internal/distribution/xfer"
	"github.com/moby/moby/v2/daemon/internal/image"
	"github.com/moby/moby/v2/daemon/internal/layer"
	"github.com/moby/moby/v2/daemon/internal/progress"
	refstore "github.com/moby/moby/v2/daemon/internal/refstore"
	"github.com/moby/moby/v2/daemon/internal/stringid"
	"github.com/moby/moby/v2/daemon/pkg/registry"
	"github.com/moby/moby/v2/pkg/ioutils"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

var (
	errRootFSMismatch = errors.New("layers from manifest don't match image configuration")
	errRootFSInvalid  = errors.New("invalid rootfs in image configuration")
)

// imageConfigPullError is an error pulling the image config blob
// (only applies to schema2).
type imageConfigPullError struct {
	Err error
}

// Error returns the error string for imageConfigPullError.
func (e imageConfigPullError) Error() string {
	return "error pulling image configuration: " + e.Err.Error()
}

// newPuller returns a puller to pull from a v2 registry.
func newPuller(endpoint registry.APIEndpoint, repoName reference.Named, config *ImagePullConfig, local ContentStore) *puller {
	return &puller{
		metadataService: metadata.NewV2MetadataService(config.MetadataStore),
		endpoint:        endpoint,
		config:          config,
		repoName:        repoName,
		manifestStore: &manifestStore{
			local: local,
		},
	}
}

type puller struct {
	metadataService metadata.V2MetadataService
	endpoint        registry.APIEndpoint
	config          *ImagePullConfig
	repoName        reference.Named
	repo            distribution.Repository
	manifestStore   *manifestStore
}

func (p *puller) pull(ctx context.Context, ref reference.Named) error {
	var err error
	// TODO(thaJeztah): do we need p.repoName at all, as it would probably be same as ref?
	p.repo, err = newRepository(ctx, p.repoName, p.endpoint, p.config.MetaHeaders, p.config.AuthConfig, "pull")
	if err != nil {
		log.G(ctx).Warnf("Error getting v2 registry: %v", err)
		return err
	}

	p.manifestStore.remote, err = p.repo.Manifests(ctx)
	if err != nil {
		return err
	}

	return p.pullRepository(ctx, ref)
}

func (p *puller) pullRepository(ctx context.Context, ref reference.Named) error {
	var layersDownloaded bool
	if !reference.IsNameOnly(ref) {
		var err error
		layersDownloaded, err = p.pullTag(ctx, ref, p.config.Platform)
		if err != nil {
			return err
		}
	} else {
		tags, err := p.repo.Tags(ctx).All(ctx)
		if err != nil {
			return err
		}

		for _, tag := range tags {
			tagRef, err := reference.WithTag(ref, tag)
			if err != nil {
				return err
			}
			pulledNew, err := p.pullTag(ctx, tagRef, p.config.Platform)
			if err != nil {
				// Since this is the pull-all-tags case, don't
				// allow an error pulling a particular tag to
				// make the whole pull fall back to v1.
				if fallbackErr, ok := err.(fallbackError); ok {
					return fallbackErr.err
				}
				return err
			}
			// pulledNew is true if either new layers were downloaded OR if existing images were newly tagged
			// TODO(tiborvass): should we change the name of `layersDownload`? What about message in WriteStatus?
			layersDownloaded = layersDownloaded || pulledNew
		}
	}

	p.writeStatus(reference.FamiliarString(ref), layersDownloaded)

	return nil
}

// writeStatus writes a status message to out. If layersDownloaded is true, the
// status message indicates that a newer image was downloaded. Otherwise, it
// indicates that the image is up to date. requestedTag is the tag the message
// will refer to.
func (p *puller) writeStatus(requestedTag string, layersDownloaded bool) {
	if layersDownloaded {
		progress.Message(p.config.ProgressOutput, "", "Status: Downloaded newer image for "+requestedTag)
	} else {
		progress.Message(p.config.ProgressOutput, "", "Status: Image is up to date for "+requestedTag)
	}
}

type layerDescriptor struct {
	digest          digest.Digest
	diffID          layer.DiffID
	repoName        reference.Named
	repo            distribution.Repository
	metadataService metadata.V2MetadataService
	tmpFile         *os.File
	verifier        digest.Verifier
	src             distribution.Descriptor
}

func (ld *layerDescriptor) Key() string {
	return "v2:" + ld.digest.String()
}

func (ld *layerDescriptor) ID() string {
	return stringid.TruncateID(ld.digest.String())
}

func (ld *layerDescriptor) DiffID() (layer.DiffID, error) {
	if ld.diffID != "" {
		return ld.diffID, nil
	}
	return ld.metadataService.GetDiffID(ld.digest)
}

func (ld *layerDescriptor) Download(ctx context.Context, progressOutput progress.Output) (io.ReadCloser, int64, error) {
	log.G(ctx).Debugf("pulling blob %q", ld.digest)

	var (
		err    error
		offset int64
	)

	if ld.tmpFile == nil {
		ld.tmpFile, err = createDownloadFile()
		if err != nil {
			return nil, 0, xfer.DoNotRetry{Err: err}
		}
	} else {
		offset, err = ld.tmpFile.Seek(0, io.SeekEnd)
		if err != nil {
			log.G(ctx).Debugf("error seeking to end of download file: %v", err)
			offset = 0

			_ = ld.tmpFile.Close()
			if err := os.Remove(ld.tmpFile.Name()); err != nil {
				log.G(ctx).Errorf("Failed to remove temp file: %s", ld.tmpFile.Name())
			}
			ld.tmpFile, err = createDownloadFile()
			if err != nil {
				return nil, 0, xfer.DoNotRetry{Err: err}
			}
		} else if offset != 0 {
			log.G(ctx).Debugf("attempting to resume download of %q from %d bytes", ld.digest, offset)
		}
	}

	tmpFile := ld.tmpFile

	layerDownload, err := ld.open(ctx)
	if err != nil {
		log.G(ctx).Errorf("Error initiating layer download: %v", err)
		return nil, 0, retryOnError(err)
	}

	if offset != 0 {
		_, err := layerDownload.Seek(offset, io.SeekStart)
		if err != nil {
			if err := ld.truncateDownloadFile(); err != nil {
				return nil, 0, xfer.DoNotRetry{Err: err}
			}
			return nil, 0, err
		}
	}
	size, err := layerDownload.Seek(0, io.SeekEnd)
	if err != nil {
		// Seek failed, perhaps because there was no Content-Length
		// header. This shouldn't fail the download, because we can
		// still continue without a progress bar.
		size = 0
	} else {
		if size != 0 && offset > size {
			log.G(ctx).Debug("Partial download is larger than full blob. Starting over")
			offset = 0
			if err := ld.truncateDownloadFile(); err != nil {
				return nil, 0, xfer.DoNotRetry{Err: err}
			}
		}

		// Restore the seek offset either at the beginning of the
		// stream, or just after the last byte we have from previous
		// attempts.
		_, err = layerDownload.Seek(offset, io.SeekStart)
		if err != nil {
			return nil, 0, err
		}
	}

	reader := progress.NewProgressReader(ioutils.NewCancelReadCloser(ctx, layerDownload), progressOutput, size-offset, ld.ID(), "Downloading")
	defer reader.Close()

	if ld.verifier == nil {
		ld.verifier = ld.digest.Verifier()
	}

	_, err = io.Copy(tmpFile, io.TeeReader(reader, ld.verifier))
	if err != nil {
		if errors.Is(err, transport.ErrWrongCodeForByteRange) {
			if err := ld.truncateDownloadFile(); err != nil {
				return nil, 0, xfer.DoNotRetry{Err: err}
			}
			return nil, 0, err
		}
		return nil, 0, retryOnError(err)
	}

	progress.Update(progressOutput, ld.ID(), "Verifying Checksum")

	if !ld.verifier.Verified() {
		err = fmt.Errorf("filesystem layer verification failed for digest %s", ld.digest)
		log.G(ctx).Error(err)

		// Allow a retry if this digest verification error happened
		// after a resumed download.
		if offset != 0 {
			if err := ld.truncateDownloadFile(); err != nil {
				return nil, 0, xfer.DoNotRetry{Err: err}
			}

			return nil, 0, err
		}
		return nil, 0, xfer.DoNotRetry{Err: err}
	}

	progress.Update(progressOutput, ld.ID(), "Download complete")

	log.G(ctx).Debugf("Downloaded %s to tempfile %s", ld.ID(), tmpFile.Name())

	_, err = tmpFile.Seek(0, io.SeekStart)
	if err != nil {
		_ = tmpFile.Close()
		if err := os.Remove(tmpFile.Name()); err != nil {
			log.G(ctx).Errorf("Failed to remove temp file: %s", tmpFile.Name())
		}
		ld.tmpFile = nil
		ld.verifier = nil
		return nil, 0, xfer.DoNotRetry{Err: err}
	}

	// hand off the temporary file to the download manager, so it will only
	// be closed once
	ld.tmpFile = nil

	return ioutils.NewReadCloserWrapper(tmpFile, func() error {
		_ = tmpFile.Close()
		err := os.RemoveAll(tmpFile.Name())
		if err != nil {
			log.G(ctx).Errorf("Failed to remove temp file: %s", tmpFile.Name())
		}
		return err
	}), size, nil
}

func (ld *layerDescriptor) Close() {
	if ld.tmpFile != nil {
		_ = ld.tmpFile.Close()
		if err := os.RemoveAll(ld.tmpFile.Name()); err != nil {
			log.G(context.TODO()).Errorf("Failed to remove temp file: %s", ld.tmpFile.Name())
		}
	}
}

func (ld *layerDescriptor) truncateDownloadFile() error {
	// Need a new hash context since we will be redoing the download
	ld.verifier = nil

	if _, err := ld.tmpFile.Seek(0, io.SeekStart); err != nil {
		log.G(context.TODO()).Errorf("error seeking to beginning of download file: %v", err)
		return err
	}

	if err := ld.tmpFile.Truncate(0); err != nil {
		log.G(context.TODO()).Errorf("error truncating download file: %v", err)
		return err
	}

	return nil
}

func (ld *layerDescriptor) Registered(diffID layer.DiffID) {
	// Cache mapping from this layer's DiffID to the blobsum
	_ = ld.metadataService.Add(diffID, metadata.V2Metadata{Digest: ld.digest, SourceRepository: ld.repoName.Name(), MediaType: ld.src.MediaType})
}

func (p *puller) pullTag(ctx context.Context, ref reference.Named, platform *ocispec.Platform) (tagUpdated bool, _ error) {
	var (
		tagOrDigest string // Used for logging/progress only
		dgst        digest.Digest
		mt          string
		size        int64
	)
	if digested, isDigested := ref.(reference.Canonical); isDigested {
		dgst = digested.Digest()
		tagOrDigest = digested.String()
	} else if tagged, isTagged := ref.(reference.NamedTagged); isTagged {
		tagService := p.repo.Tags(ctx)
		desc, err := tagService.Get(ctx, tagged.Tag())
		if err != nil {
			return false, err
		}
		dgst = desc.Digest
		tagOrDigest = tagged.Tag()
		mt = desc.MediaType
		size = desc.Size
	} else {
		return false, fmt.Errorf("internal error: reference has neither a tag nor a digest: %s", reference.FamiliarString(ref))
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithFields(log.Fields{
		"digest": dgst,
		"remote": ref,
	}))

	manifest, err := p.manifestStore.Get(ctx, ocispec.Descriptor{
		MediaType: mt,
		Digest:    dgst,
		Size:      size,
	}, ref)
	if err != nil {
		return false, err
	}

	if manifest == nil {
		return false, fmt.Errorf("image manifest does not exist for tag or digest %q", tagOrDigest)
	}

	if m, ok := manifest.(*schema2.DeserializedManifest); ok {
		if err := p.validateMediaType(m.Manifest.Config.MediaType); err != nil {
			return false, err
		}
	}

	log.G(ctx).Debugf("Pulling ref from V2 registry: %s", reference.FamiliarString(ref))
	progress.Message(p.config.ProgressOutput, tagOrDigest, "Pulling from "+reference.FamiliarName(p.repo.Named()))

	var (
		id             digest.Digest
		manifestDigest digest.Digest
	)

	switch v := manifest.(type) {
	case *schema2.DeserializedManifest:
		id, manifestDigest, err = p.pullSchema2(ctx, ref, v, platform)
		if err != nil {
			return false, err
		}
	case *ocischema.DeserializedManifest:
		id, manifestDigest, err = p.pullOCI(ctx, ref, v, platform)
		if err != nil {
			return false, err
		}
	case *manifestlist.DeserializedManifestList:
		id, manifestDigest, err = p.pullManifestList(ctx, ref, v, platform)
		if err != nil {
			return false, err
		}
	default:
		mediaType, _, _ := manifest.Payload()
		switch mediaType {
		case MediaTypeDockerSchema1Manifest, MediaTypeDockerSchema1SignedManifest:
			return false, DeprecatedSchema1ImageError(ref)
		}
		return false, invalidManifestFormatError{}
	}

	progress.Message(p.config.ProgressOutput, "", "Digest: "+manifestDigest.String())

	if p.config.ReferenceStore != nil {
		oldTagID, err := p.config.ReferenceStore.Get(ref)
		if err == nil {
			if oldTagID == id {
				return false, addDigestReference(p.config.ReferenceStore, ref, manifestDigest, id)
			}
		} else if !errors.Is(err, refstore.ErrDoesNotExist) {
			return false, err
		}

		if canonical, ok := ref.(reference.Canonical); ok {
			if err := p.config.ReferenceStore.AddDigest(canonical, id, true); err != nil {
				return false, err
			}
		} else {
			if err := addDigestReference(p.config.ReferenceStore, ref, manifestDigest, id); err != nil {
				return false, err
			}
			if err := p.config.ReferenceStore.AddTag(ref, id, true); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// validateMediaType validates if the given mediaType is accepted by the puller's
// configuration.
func (p *puller) validateMediaType(mediaType string) error {
	var allowedMediaTypes []string
	if len(p.config.Schema2Types) > 0 {
		allowedMediaTypes = p.config.Schema2Types
	} else {
		allowedMediaTypes = defaultImageTypes
	}
	if slices.Contains(allowedMediaTypes, mediaType) {
		return nil
	}

	configClass := mediaTypeClasses[mediaType]
	if configClass == "" {
		configClass = "unknown"
	}
	return invalidManifestClassError{mediaType, configClass}
}

func checkSupportedMediaType(mediaType string) error {
	lowerMt := strings.ToLower(mediaType)
	if strings.HasPrefix(lowerMt, "application/vnd.docker.ai.") {
		return AIModelNotSupportedError{}
	}
	for _, mt := range supportedMediaTypes {
		// The should either be an exact match, or have a valid prefix
		// we append a "." when matching prefixes to exclude "false positives";
		// for example, we don't want to match "application/vnd.oci.images_are_fun_yolo".
		if lowerMt == mt || strings.HasPrefix(lowerMt, mt+".") {
			return nil
		}
	}
	return unsupportedMediaTypeError{MediaType: mediaType}
}

func (p *puller) pullSchema2Layers(ctx context.Context, target distribution.Descriptor, layers []distribution.Descriptor, platform *ocispec.Platform) (id digest.Digest, _ error) {
	if _, err := p.config.ImageStore.Get(ctx, target.Digest); err == nil {
		// If the image already exists locally, no need to pull
		// anything.
		return target.Digest, nil
	}

	if err := checkSupportedMediaType(target.MediaType); err != nil {
		return "", err
	}

	var descriptors []xfer.DownloadDescriptor

	// Note that the order of this loop is in the direction of bottom-most
	// to top-most, so that the downloads slice gets ordered correctly.
	for _, d := range layers {
		if err := d.Digest.Validate(); err != nil {
			return "", errors.Wrapf(err, "could not validate layer digest %q", d.Digest)
		}
		if err := checkSupportedMediaType(d.MediaType); err != nil {
			return "", err
		}
		descriptors = append(descriptors, &layerDescriptor{
			digest:          d.Digest,
			repo:            p.repo,
			repoName:        p.repoName,
			metadataService: p.metadataService,
			src:             d,
		})
	}

	configChan := make(chan []byte, 1)
	configErrChan := make(chan error, 1)
	layerErrChan := make(chan error, 1)
	downloadsDone := make(chan struct{})
	var cancel func()
	ctx, cancel = context.WithCancel(ctx)
	defer cancel()

	// Pull the image config
	go func() {
		configJSON, err := p.pullSchema2Config(ctx, target.Digest)
		if err != nil {
			configErrChan <- imageConfigPullError{Err: err}
			cancel()
			return
		}
		configChan <- configJSON
	}()

	var (
		configJSON       []byte            // raw serialized image config
		downloadedRootFS *image.RootFS     // rootFS from registered layers
		configRootFS     *image.RootFS     // rootFS from configuration
		release          func()            // release resources from rootFS download
		configPlatform   *ocispec.Platform // for LCOW when registering downloaded layers
	)

	layerStoreOS := runtime.GOOS
	if platform != nil {
		layerStoreOS = platform.OS
	}

	// https://github.com/moby/moby/issues/24766 - Err on the side of caution,
	// explicitly blocking images intended for linux from the Windows daemon. On
	// Windows, we do this before the attempt to download, effectively serialising
	// the download slightly slowing it down. We have to do it this way, as
	// chances are the download of layers itself would fail due to file names
	// which aren't suitable for NTFS. At some point in the future, if a similar
	// check to block Windows images being pulled on Linux is implemented, it
	// may be necessary to perform the same type of serialisation.
	if runtime.GOOS == "windows" {
		var err error
		configJSON, configRootFS, configPlatform, err = receiveConfig(configChan, configErrChan)
		if err != nil {
			return "", err
		}
		if configRootFS == nil {
			return "", errRootFSInvalid
		}
		if err := checkImageCompatibility(configPlatform.OS, configPlatform.OSVersion); err != nil {
			return "", err
		}

		if len(descriptors) != len(configRootFS.DiffIDs) {
			return "", errRootFSMismatch
		}
		if platform == nil {
			// Early bath if the requested OS doesn't match that of the configuration.
			// This avoids doing the download, only to potentially fail later.
			if err := image.CheckOS(configPlatform.OS); err != nil {
				return "", fmt.Errorf("cannot download image with operating system %q when requesting %q", configPlatform.OS, layerStoreOS)
			}
			layerStoreOS = configPlatform.OS
		}

		// Populate diff ids in descriptors to avoid downloading foreign layers
		// which have been side loaded
		for i := range descriptors {
			descriptors[i].(*layerDescriptor).diffID = configRootFS.DiffIDs[i]
		}
	}

	// Assume that the operating system is the host OS if blank, and validate it
	// to ensure we don't cause a panic by an invalid index into the layerstores.
	if layerStoreOS != "" {
		if err := image.CheckOS(layerStoreOS); err != nil {
			return "", err
		}
	}

	if p.config.DownloadManager != nil {
		go func() {
			var (
				err    error
				rootFS image.RootFS
			)
			rootFS, release, err = p.config.DownloadManager.Download(ctx, descriptors, p.config.ProgressOutput)
			if err != nil {
				// Intentionally do not cancel the config download here
				// as the error from config download (if there is one)
				// is more interesting than the layer download error
				layerErrChan <- err
				return
			}

			downloadedRootFS = &rootFS
			close(downloadsDone)
		}()
	} else {
		// We have nothing to download
		close(downloadsDone)
	}

	if configJSON == nil {
		var err error
		configJSON, configRootFS, _, err = receiveConfig(configChan, configErrChan)
		if err == nil && configRootFS == nil {
			err = errRootFSInvalid
		}
		if err != nil {
			cancel()
			select {
			case <-downloadsDone:
			case <-layerErrChan:
			}
			return "", err
		}
	}

	select {
	case <-downloadsDone:
	case err := <-layerErrChan:
		return "", err
	}

	if release != nil {
		defer release()
	}

	if downloadedRootFS != nil {
		// The DiffIDs returned in rootFS MUST match those in the config.
		// Otherwise the image config could be referencing layers that aren't
		// included in the manifest.
		if len(downloadedRootFS.DiffIDs) != len(configRootFS.DiffIDs) {
			return "", errRootFSMismatch
		}

		for i := range downloadedRootFS.DiffIDs {
			if downloadedRootFS.DiffIDs[i] != configRootFS.DiffIDs[i] {
				return "", errRootFSMismatch
			}
		}
	}

	imageID, err := p.config.ImageStore.Put(ctx, configJSON)
	if err != nil {
		return "", err
	}

	return imageID, nil
}

func (p *puller) pullSchema2(ctx context.Context, ref reference.Named, mfst *schema2.DeserializedManifest, platform *ocispec.Platform) (id digest.Digest, manifestDigest digest.Digest, _ error) {
	manifestDigest, err := schema2ManifestDigest(ref, mfst)
	if err != nil {
		return "", "", err
	}
	id, err = p.pullSchema2Layers(ctx, mfst.Target(), mfst.Layers, platform)
	return id, manifestDigest, err
}

func (p *puller) pullOCI(ctx context.Context, ref reference.Named, mfst *ocischema.DeserializedManifest, platform *ocispec.Platform) (id digest.Digest, manifestDigest digest.Digest, _ error) {
	manifestDigest, err := schema2ManifestDigest(ref, mfst)
	if err != nil {
		return "", "", err
	}
	id, err = p.pullSchema2Layers(ctx, mfst.Target(), mfst.Layers, platform)
	return id, manifestDigest, err
}

func receiveConfig(configChan <-chan []byte, errChan <-chan error) ([]byte, *image.RootFS, *ocispec.Platform, error) {
	select {
	case configJSON := <-configChan:
		rootfs, err := rootFSFromConfig(configJSON)
		if err != nil {
			return nil, nil, nil, err
		}
		platform, err := platformFromConfig(configJSON)
		if err != nil {
			return nil, nil, nil, err
		}
		return configJSON, rootfs, platform, nil
	case err := <-errChan:
		return nil, nil, nil, err
		// Don't need a case for ctx.Done in the select because cancellation
		// will trigger an error in p.pullSchema2ImageConfig.
	}
}

// pullManifestList handles "manifest lists" which point to various
// platform-specific manifests.
func (p *puller) pullManifestList(ctx context.Context, ref reference.Named, mfstList *manifestlist.DeserializedManifestList, pp *ocispec.Platform) (id digest.Digest, manifestListDigest digest.Digest, _ error) {
	manifestListDigest, err := schema2ManifestDigest(ref, mfstList)
	if err != nil {
		return "", "", err
	}

	var platform ocispec.Platform
	if pp != nil {
		platform = *pp
	}
	log.G(ctx).Debugf("%s resolved to a manifestList object with %d entries; looking for a %s match", ref, len(mfstList.Manifests), platforms.FormatAll(platform))

	manifestMatches := filterManifests(mfstList.Manifests, platform)

	for _, match := range manifestMatches {
		if err := checkImageCompatibility(match.Platform.OS, match.Platform.OSVersion); err != nil {
			return "", "", err
		}

		manifest, err := p.manifestStore.Get(ctx, ocispec.Descriptor{
			Digest:    match.Digest,
			Size:      match.Size,
			MediaType: match.MediaType,
		}, ref)
		if err != nil {
			return "", "", err
		}

		manifestRef, err := reference.WithDigest(reference.TrimNamed(ref), match.Digest)
		if err != nil {
			return "", "", err
		}

		switch v := manifest.(type) {
		case *schema2.DeserializedManifest:
			id, _, err = p.pullSchema2(ctx, manifestRef, v, toOCIPlatform(match.Platform))
			if err != nil {
				return "", "", err
			}
		case *ocischema.DeserializedManifest:
			id, _, err = p.pullOCI(ctx, manifestRef, v, toOCIPlatform(match.Platform))
			if err != nil {
				return "", "", err
			}
		case *manifestlist.DeserializedManifestList:
			id, _, err = p.pullManifestList(ctx, manifestRef, v, pp)
			if err != nil {
				var noMatches noMatchesErr
				if !errors.As(err, &noMatches) {
					// test the next match
					continue
				}
			}
		default:
			mediaType, _, _ := manifest.Payload()
			switch mediaType {
			case MediaTypeDockerSchema1Manifest, MediaTypeDockerSchema1SignedManifest:
				return "", "", DeprecatedSchema1ImageError(ref)
			}

			// OCI spec requires to skip unknown manifest types
			continue
		}
		return id, manifestListDigest, err
	}
	return "", "", noMatchesErr{platform: platform}
}

const (
	defaultSchemaPullBackoff     = 250 * time.Millisecond
	defaultMaxSchemaPullAttempts = 5
)

func (p *puller) pullSchema2Config(ctx context.Context, dgst digest.Digest) (configJSON []byte, _ error) {
	blobs := p.repo.Blobs(ctx)
	err := retry(ctx, defaultMaxSchemaPullAttempts, defaultSchemaPullBackoff, func(ctx context.Context) (err error) {
		configJSON, err = blobs.Get(ctx, dgst)
		return err
	})
	if err != nil {
		return nil, err
	}

	// Verify image config digest
	verifier := dgst.Verifier()
	if _, err := verifier.Write(configJSON); err != nil {
		return nil, err
	}
	if !verifier.Verified() {
		err := fmt.Errorf("image config verification failed for digest %s", dgst)
		log.G(ctx).Error(err)
		return nil, err
	}

	return configJSON, nil
}

type noMatchesErr struct {
	platform ocispec.Platform
}

func (e noMatchesErr) Error() string {
	var p string
	if e.platform.OS == "" {
		p = platforms.FormatAll(platforms.DefaultSpec())
	} else {
		p = platforms.FormatAll(e.platform)
	}
	return fmt.Sprintf("no matching manifest for %s in the manifest list entries", p)
}

func retry(ctx context.Context, maxAttempts int, sleep time.Duration, f func(ctx context.Context) error) error {
	attempt := 0
	var err error
	for ; attempt < maxAttempts; attempt++ {
		err = retryOnError(f(ctx))
		if err == nil {
			break
		}
		if xfer.IsDoNotRetryError(err) {
			break
		}

		if attempt+1 < maxAttempts {
			timer := time.NewTimer(sleep)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
				log.G(ctx).WithError(err).WithField("attempts", attempt+1).Debug("retrying after error")
				sleep *= 2
			}
		}
	}
	if err != nil {
		return errors.Wrapf(err, "download failed after attempts=%d", attempt+1)
	}
	return nil
}

// schema2ManifestDigest computes the manifest digest, and, if pulling by
// digest, ensures that it matches the requested digest.
func schema2ManifestDigest(ref reference.Named, mfst distribution.Manifest) (digest.Digest, error) {
	_, canonical, err := mfst.Payload()
	if err != nil {
		return "", err
	}

	// If pull by digest, then verify the manifest digest.
	if digested, isDigested := ref.(reference.Canonical); isDigested {
		verifier := digested.Digest().Verifier()
		if _, err := verifier.Write(canonical); err != nil {
			return "", err
		}
		if !verifier.Verified() {
			err := fmt.Errorf("manifest verification failed for digest %s", digested.Digest())
			log.G(context.TODO()).Error(err)
			return "", err
		}
		return digested.Digest(), nil
	}

	return digest.FromBytes(canonical), nil
}

func createDownloadFile() (*os.File, error) {
	return os.CreateTemp("", "GetImageBlob")
}

func toOCIPlatform(p manifestlist.PlatformSpec) *ocispec.Platform {
	// distribution pkg does define platform as pointer so this hack for empty struct
	// is necessary. This is temporary until correct OCI image-spec package is used.
	if p.OS == "" && p.Architecture == "" && p.Variant == "" && p.OSVersion == "" && p.OSFeatures == nil && p.Features == nil {
		return nil
	}
	return &ocispec.Platform{
		OS:           p.OS,
		Architecture: p.Architecture,
		Variant:      p.Variant,
		OSFeatures:   p.OSFeatures,
		OSVersion:    p.OSVersion,
	}
}
