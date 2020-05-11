package plugin

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/containerd/containerd/content"
	c8derrdefs "github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	reference "github.com/containerd/containerd/reference/docker"
	"github.com/containerd/containerd/remotes"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/docker/docker/api/types"
	progressutils "github.com/docker/docker/distribution/utils"
	"github.com/docker/docker/pkg/chrootarchive"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/progress"
	"github.com/docker/docker/pkg/stringid"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const mediaTypePluginConfig = "application/vnd.docker.plugin.v1+json"

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

// fetch the content related to the passed in reference into the blob store and appends the provided images.Handlers
// There is no need to use remotes.FetchHandler since it already gets set
func (pm *Manager) fetch(ctx context.Context, ref reference.Named, auth *types.AuthConfig, out progress.Output, metaHeader http.Header, handlers ...images.Handler) (err error) {
	// We need to make sure we have a domain on the reference
	withDomain, err := reference.ParseNormalizedNamed(ref.String())
	if err != nil {
		return errors.Wrap(err, "error parsing plugin image reference")
	}

	// Make sure we can authenticate the request since the auth scope for plugin repos is different than a normal repo.
	ctx = docker.WithScope(ctx, scope(ref, false))

	// Make sure the fetch handler knows how to set a ref key for the plugin media type.
	// Without this the ref key is "unknown" and we see a nasty warning message in the logs
	ctx = remotes.WithMediaTypeKeyPrefix(ctx, mediaTypePluginConfig, "docker-plugin")

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
		logrus.WithError(err).WithField("ref", withDomain).Debug("Error while resolving reference, falling back to backwards compatible accept header format")
		headers := http.Header{}
		headers.Add("Accept", images.MediaTypeDockerSchema2Manifest)
		headers.Add("Accept", images.MediaTypeDockerSchema2ManifestList)
		headers.Add("Accept", specs.MediaTypeImageManifest)
		headers.Add("Accept", specs.MediaTypeImageIndex)
		resolver, _ = pm.newResolver(ctx, nil, auth, headers, false)
		if resolver != nil {
			resolved, desc, err = resolver.Resolve(ctx, withDomain.String())
			if err != nil {
				logrus.WithError(err).WithField("ref", withDomain).Debug("Failed to resolve reference after falling back to backwards compatible accept header format")
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
	handlers = append([]images.Handler{fp, remotes.FetchHandler(pm.blobStore, fetcher)}, handlers...)
	if err := images.Dispatch(ctx, images.Handlers(handlers...), nil, desc); err != nil {
		return err
	}
	return nil
}

// applyLayer makes an images.HandlerFunc which applies a fetched image rootfs layer to a directory.
//
// TODO(@cpuguy83) This gets run sequentially after layer pull (makes sense), however
// if there are multiple layers to fetch we may end up extracting layers in the wrong
// order.
func applyLayer(cs content.Store, dir string, out progress.Output) images.HandlerFunc {
	return func(ctx context.Context, desc specs.Descriptor) ([]specs.Descriptor, error) {
		switch desc.MediaType {
		case
			specs.MediaTypeImageLayer,
			images.MediaTypeDockerSchema2Layer,
			specs.MediaTypeImageLayerGzip,
			images.MediaTypeDockerSchema2LayerGzip:
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

func childrenHandler(cs content.Store) images.HandlerFunc {
	ch := images.ChildrenHandler(cs)
	return func(ctx context.Context, desc specs.Descriptor) ([]specs.Descriptor, error) {
		switch desc.MediaType {
		case mediaTypePluginConfig:
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

func storeFetchMetadata(m *fetchMeta) images.HandlerFunc {
	return func(ctx context.Context, desc specs.Descriptor) ([]specs.Descriptor, error) {
		switch desc.MediaType {
		case
			images.MediaTypeDockerSchema2LayerForeignGzip,
			images.MediaTypeDockerSchema2Layer,
			specs.MediaTypeImageLayer,
			specs.MediaTypeImageLayerGzip:
			m.blobs = append(m.blobs, desc.Digest)
		case specs.MediaTypeImageManifest, images.MediaTypeDockerSchema2Manifest:
			m.manifest = desc.Digest
		case mediaTypePluginConfig:
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
func withFetchProgress(cs content.Store, out progress.Output, ref reference.Named) images.HandlerFunc {
	return func(ctx context.Context, desc specs.Descriptor) ([]specs.Descriptor, error) {
		switch desc.MediaType {
		case specs.MediaTypeImageManifest, images.MediaTypeDockerSchema2Manifest:
			tn := reference.TagNameOnly(ref)
			tagged := tn.(reference.Tagged)
			progress.Messagef(out, tagged.Tag(), "Pulling from %s", reference.FamiliarName(ref))
			progress.Messagef(out, "", "Digest: %s", desc.Digest.String())
			return nil, nil
		case
			images.MediaTypeDockerSchema2LayerGzip,
			images.MediaTypeDockerSchema2Layer,
			specs.MediaTypeImageLayer,
			specs.MediaTypeImageLayerGzip:
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
			timer := time.NewTimer(100 * time.Millisecond)
			if !timer.Stop() {
				<-timer.C
			}
			defer timer.Stop()

			var pulling bool
			var ctxErr error

			for {
				timer.Reset(100 * time.Millisecond)

				select {
				case <-ctx.Done():
					ctxErr = ctx.Err()
					// make sure we can still fetch from the content store
					// TODO: Might need to add some sort of timeout
					ctx = context.Background()
				case <-timer.C:
				}

				s, err := cs.Status(ctx, key)
				if err != nil {
					if !c8derrdefs.IsNotFound(err) {
						logrus.WithError(err).WithField("layerDigest", desc.Digest.String()).Error("Error looking up status of plugin layer pull")
						progress.Update(out, id, err.Error())
						return
					}

					if _, err := cs.Info(ctx, desc.Digest); err == nil {
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

				out.WriteProgress(progress.Progress{ID: id, Action: "Downloading", Current: s.Offset, Total: s.Total})
			}
		}()
		return nil, nil
	}
}
