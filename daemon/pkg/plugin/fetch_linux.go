package plugin

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/distribution/reference"
	"github.com/moby/go-archive/chrootarchive"
	"github.com/moby/moby/api/types/registry"
	progressutils "github.com/moby/moby/v2/daemon/internal/distribution/utils"
	"github.com/moby/moby/v2/daemon/internal/progress"
	"github.com/moby/moby/v2/daemon/internal/stringid"
	v2 "github.com/moby/moby/v2/daemon/pkg/plugin/v2"
	"github.com/moby/moby/v2/pkg/ioutils"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// setupProgressOutput sets up the passed in writer to stream progress.
//
// The passed in cancel function is used by the progress writer to signal callers that there
// is an issue writing to the stream.
//
// The returned function is used to wait for the progress writer to be finished.
// Call it to make sure the progress writer is done before returning from your function as needed.
func setupProgressOutput(outStream io.Writer, cancel func()) (progress.Output, func()) {
	var out progress.Output
	f := func() {}

	if outStream != nil {
		ch := make(chan progress.Progress, 100)
		out = progress.ChanOutput(ch)

		ctx, retCancel := context.WithCancel(context.Background())
		go func() {
			progressutils.WriteDistributionProgress(cancel, outStream, ch)
			retCancel()
		}()

		f = func() {
			close(ch)
			<-ctx.Done()
		}
	} else {
		out = progress.DiscardOutput()
	}
	return out, f
}

// fetch the content related to the passed in reference into the blob store and appends the provided c8dimages.Handlers
// There is no need to use remotes.FetchHandler since it already gets set
func (pm *Manager) fetch(ctx context.Context, ref reference.Named, auth *registry.AuthConfig, out progress.Output, metaHeader http.Header, handlers ...c8dimages.Handler) error {
	// We need to make sure we have a domain on the reference
	withDomain, err := reference.ParseNormalizedNamed(ref.String())
	if err != nil {
		return errors.Wrap(err, "error parsing plugin image reference")
	}

	// Make sure we can authenticate the request since the auth scope for plugin repos is different than a normal repo.
	ctx = docker.WithScope(ctx, scope(ref, false))

	// Make sure the fetch handler knows how to set a ref key for the plugin media type.
	// Without this the ref key is "unknown" and we see a nasty warning message in the logs
	ctx = remotes.WithMediaTypeKeyPrefix(ctx, v2.MediaTypePluginConfig, "docker-plugin")

	resolver, err := pm.newResolver(ctx, nil, auth, metaHeader, false)
	if err != nil {
		return err
	}
	resolved, desc, err := resolver.Resolve(ctx, withDomain.String())
	if err != nil {
		// This is backwards compatible with older versions of the distribution registry.
		// The containerd client will add it's own accept header as a comma separated list of supported manifests.
		// This is perfectly fine, unless you are talking to an older registry which does not split the comma separated list,
		//   so it is never able to match a media type and it falls back to schema1 (yuck) and fails because our manifest the
		//   fallback does not support plugin configs...
		log.G(ctx).WithError(err).WithField("ref", withDomain).Debug("Error while resolving reference, falling back to backwards compatible accept header format")
		headers := http.Header{}
		headers.Add("Accept", c8dimages.MediaTypeDockerSchema2Manifest)
		headers.Add("Accept", c8dimages.MediaTypeDockerSchema2ManifestList)
		headers.Add("Accept", ocispec.MediaTypeImageManifest)
		headers.Add("Accept", ocispec.MediaTypeImageIndex)
		resolver, _ = pm.newResolver(ctx, nil, auth, headers, false)
		if resolver != nil {
			resolved, desc, err = resolver.Resolve(ctx, withDomain.String())
			if err != nil {
				log.G(ctx).WithError(err).WithField("ref", withDomain).Debug("Failed to resolve reference after falling back to backwards compatible accept header format")
			}
		}
		if err != nil {
			return errors.Wrap(err, "error resolving plugin reference")
		}
	}

	fetcher, err := resolver.Fetcher(ctx, resolved)
	if err != nil {
		return errors.Wrap(err, "error creating plugin image fetcher")
	}

	fp := withFetchProgress(pm.blobStore, out, ref)
	handlers = append([]c8dimages.Handler{fp, remotes.FetchHandler(pm.blobStore, fetcher)}, handlers...)
	return c8dimages.Dispatch(ctx, c8dimages.Handlers(handlers...), nil, desc)
}

// applyLayer makes an c8dimages.HandlerFunc which applies a fetched image rootfs layer to a directory.
//
// TODO(@cpuguy83) This gets run sequentially after layer pull (makes sense), however
// if there are multiple layers to fetch we may end up extracting layers in the wrong
// order.
func applyLayer(cs content.Store, dir string, out progress.Output) c8dimages.HandlerFunc {
	return func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		switch desc.MediaType {
		case
			ocispec.MediaTypeImageLayer,
			c8dimages.MediaTypeDockerSchema2Layer,
			ocispec.MediaTypeImageLayerGzip,
			c8dimages.MediaTypeDockerSchema2LayerGzip:
		default:
			return nil, nil
		}

		ra, err := cs.ReaderAt(ctx, desc)
		if err != nil {
			return nil, errors.Wrapf(err, "error getting content from content store for digest %s", desc.Digest)
		}

		id := stringid.TruncateID(desc.Digest.String())

		rc := ioutils.NewReadCloserWrapper(content.NewReader(ra), ra.Close)
		pr := progress.NewProgressReader(rc, out, desc.Size, id, "Extracting")
		defer pr.Close()

		if _, err := chrootarchive.ApplyLayer(dir, pr); err != nil {
			return nil, errors.Wrapf(err, "error applying layer for digest %s", desc.Digest)
		}
		progress.Update(out, id, "Complete")
		return nil, nil
	}
}

func childrenHandler(cs content.Store) c8dimages.HandlerFunc {
	ch := c8dimages.ChildrenHandler(cs)
	return func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		switch desc.MediaType {
		case v2.MediaTypePluginConfig:
			return nil, nil
		default:
			return ch(ctx, desc)
		}
	}
}

type fetchMeta struct {
	blobs    []digest.Digest
	config   digest.Digest
	manifest digest.Digest
}

func storeFetchMetadata(m *fetchMeta) c8dimages.HandlerFunc {
	return func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		switch desc.MediaType {
		case
			c8dimages.MediaTypeDockerSchema2LayerForeignGzip,
			c8dimages.MediaTypeDockerSchema2Layer,
			ocispec.MediaTypeImageLayer,
			ocispec.MediaTypeImageLayerGzip:
			m.blobs = append(m.blobs, desc.Digest)
		case ocispec.MediaTypeImageManifest, c8dimages.MediaTypeDockerSchema2Manifest:
			m.manifest = desc.Digest
		case v2.MediaTypePluginConfig:
			m.config = desc.Digest
		}
		return nil, nil
	}
}

func validateFetchedMetadata(md fetchMeta) error {
	if md.config == "" {
		return errors.New("fetched plugin image but plugin config is missing")
	}
	if md.manifest == "" {
		return errors.New("fetched plugin image but manifest is missing")
	}
	return nil
}

// withFetchProgress is a fetch handler which registers a descriptor with a progress
func withFetchProgress(cs content.Store, out progress.Output, ref reference.Named) c8dimages.HandlerFunc {
	return func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		switch desc.MediaType {
		case ocispec.MediaTypeImageManifest, c8dimages.MediaTypeDockerSchema2Manifest:
			tn := reference.TagNameOnly(ref)
			var tagOrDigest string
			if tagged, ok := tn.(reference.Tagged); ok {
				tagOrDigest = tagged.Tag()
			} else {
				tagOrDigest = tn.String()
			}
			progress.Messagef(out, tagOrDigest, "Pulling from %s", reference.FamiliarName(ref))
			progress.Messagef(out, "", "Digest: %s", desc.Digest.String())
			return nil, nil
		case
			c8dimages.MediaTypeDockerSchema2LayerGzip,
			c8dimages.MediaTypeDockerSchema2Layer,
			ocispec.MediaTypeImageLayer,
			ocispec.MediaTypeImageLayerGzip:
		default:
			return nil, nil
		}

		id := stringid.TruncateID(desc.Digest.String())

		if _, err := cs.Info(ctx, desc.Digest); err == nil {
			out.WriteProgress(progress.Progress{ID: id, Action: "Already exists", LastUpdate: true})
			return nil, nil
		}

		progress.Update(out, id, "Waiting")

		key := remotes.MakeRefKey(ctx, desc)

		go func() {
			start := time.Now().Unix()

			timer := time.NewTimer(100 * time.Millisecond)
			if !timer.Stop() {
				<-timer.C
			}
			defer timer.Stop()

			var pulling bool
			var (
				// make sure we can still fetch from the content store
				// if the main context is cancelled
				// TODO: Might need to add some sort of timeout; see https://github.com/moby/moby/issues/49413
				ctxErr      error
				noCancelCTX = context.WithoutCancel(ctx)
			)

			for {
				timer.Reset(100 * time.Millisecond)

				select {
				case <-ctx.Done():
					ctxErr = ctx.Err()
				case <-timer.C:
				}

				s, err := cs.Status(noCancelCTX, key)
				if err != nil {
					if !cerrdefs.IsNotFound(err) {
						log.G(noCancelCTX).WithError(err).WithField("layerDigest", desc.Digest.String()).Error("Error looking up status of plugin layer pull")
						progress.Update(out, id, err.Error())
						return
					}

					if _, err := cs.Info(noCancelCTX, desc.Digest); err == nil {
						progress.Update(out, id, "Download complete")
						return
					}

					if ctxErr != nil {
						progress.Update(out, id, ctxErr.Error())
						return
					}

					continue
				}

				if !pulling {
					progress.Update(out, id, "Pulling fs layer")
					pulling = true
				}

				if s.Offset == s.Total {
					out.WriteProgress(progress.Progress{ID: id, Action: "Download complete", Current: s.Offset, LastUpdate: true})
					return
				}

				out.WriteProgress(progress.Progress{ID: id, Action: "Downloading", Current: s.Offset, Total: s.Total, Start: start})
			}
		}()
		return nil, nil
	}
}
