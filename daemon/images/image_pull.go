package images

import (
	"context"
	"io"
	"time"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/v2/daemon/internal/distribution"
	progressutils "github.com/moby/moby/v2/daemon/internal/distribution/utils"
	"github.com/moby/moby/v2/daemon/internal/metrics"
	"github.com/moby/moby/v2/daemon/internal/progress"
	"github.com/moby/moby/v2/daemon/internal/streamformatter"
	"github.com/moby/moby/v2/daemon/server/imagebackend"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// PullImage initiates a pull operation. image is the repository name to pull, and
// tag may be either empty, or indicate a specific tag to pull.
func (i *ImageService) PullImage(ctx context.Context, ref reference.Named, options imagebackend.PullOptions) error {
	if len(options.Platforms) > 1 {
		// TODO(thaJeztah): add support for pulling multiple platforms
		return cerrdefs.ErrInvalidArgument.WithMessage("multiple platforms is not supported")
	}
	start := time.Now()

	var platform *ocispec.Platform
	if len(options.Platforms) > 0 {
		p := options.Platforms[0]
		platform = &p
	}

	err := i.pullImageWithReference(ctx, ref, platform, options.MetaHeaders, options.AuthConfig, options.OutStream)
	metrics.ImageActions.WithValues("pull").UpdateSince(start)
	if err != nil {
		return err
	}

	if platform != nil {
		// If --platform was specified, check that the image we pulled matches
		// the expected platform. This check is for situations where the image
		// is a single-arch image, in which case (for backward compatibility),
		// we allow the image to have a non-matching architecture. The code
		// below checks for this situation, and returns a warning to the client,
		// as well as logging it to the daemon logs.
		img, err := i.GetImage(ctx, ref.String(), imagebackend.GetImageOpts{Platform: platform})

		// Note that this is a special case where GetImage returns both an image
		// and an error: https://github.com/moby/moby/blob/v28.3.3/daemon/images/image.go#L186-L193
		if cerrdefs.IsNotFound(err) && img != nil {
			po := streamformatter.NewJSONProgressOutput(options.OutStream, false)
			progress.Messagef(po, "", `WARNING: %s`, err.Error())
			log.G(ctx).WithError(err).WithField("image", reference.FamiliarName(ref)).Warn("ignoring platform mismatch on single-arch image")
		} else if err != nil {
			return err
		}
	}

	return nil
}

func (i *ImageService) pullImageWithReference(ctx context.Context, ref reference.Named, platform *ocispec.Platform, metaHeaders map[string][]string, authConfig *registry.AuthConfig, outStream io.Writer) error {
	// Include a buffer so that slow client connections don't affect
	// transfer performance.
	progressChan := make(chan progress.Progress, 100)

	writesDone := make(chan struct{})

	ctx, cancelFunc := context.WithCancel(ctx)

	go func() {
		progressutils.WriteDistributionProgress(cancelFunc, outStream, progressChan)
		close(writesDone)
	}()

	ctx = namespaces.WithNamespace(ctx, i.contentNamespace)
	// Take out a temporary lease for everything that gets persisted to the content store.
	// Before the lease is cancelled, any content we want to keep should have it's own lease applied.
	ctx, done, err := tempLease(ctx, i.leases)
	if err != nil {
		// The progress writer is already running and blocks on progressChan
		// until it is closed; close it here so the goroutine does not leak.
		close(progressChan)
		<-writesDone
		return err
	}
	defer done(ctx)

	cs := &contentStoreForPull{
		ContentStore: i.content,
		leases:       i.leases,
	}
	imageStore := &imageStoreForPull{
		ImageConfigStore: distribution.NewImageConfigStoreFromStore(i.imageStore),
		ingested:         cs,
		leases:           i.leases,
	}

	imagePullConfig := &distribution.ImagePullConfig{
		Config: distribution.Config{
			MetaHeaders:      metaHeaders,
			AuthConfig:       authConfig,
			ProgressOutput:   progress.ChanOutput(progressChan),
			RegistryService:  i.registryService,
			ImageEventLogger: i.LogImageEvent,
			MetadataStore:    i.distributionMetadataStore,
			ImageStore:       imageStore,
			ReferenceStore:   i.referenceStore,
		},
		DownloadManager: i.downloadManager,
		Platform:        platform,
	}

	err = distribution.Pull(ctx, ref, imagePullConfig, cs)
	close(progressChan)
	<-writesDone
	return err
}

func tempLease(ctx context.Context, mgr leases.Manager) (context.Context, func(context.Context) error, error) {
	nop := func(context.Context) error { return nil }
	_, ok := leases.FromContext(ctx)
	if ok {
		return ctx, nop, nil
	}

	// Use an expiration that ensures the lease is cleaned up at some point if there is a crash, SIGKILL, etc.
	opts := []leases.Opt{
		leases.WithRandomID(),
		leases.WithExpiration(24 * time.Hour),
		leases.WithLabels(map[string]string{
			"moby.lease/temporary": time.Now().UTC().Format(time.RFC3339Nano),
		}),
	}
	l, err := mgr.Create(ctx, opts...)
	if err != nil {
		return ctx, nop, errors.Wrap(err, "error creating temporary lease")
	}

	ctx = leases.WithLease(ctx, l.ID)
	return ctx, func(ctx context.Context) error {
		return mgr.Delete(ctx, l)
	}, nil
}
