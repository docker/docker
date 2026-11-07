package pull

import (
	"context"
	"sync"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/pkg/labels"
	"github.com/containerd/containerd/v2/pkg/reference"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/contentutil"
	"github.com/moby/buildkit/util/flightcontrol"
	"github.com/moby/buildkit/util/imageutil"
	"github.com/moby/buildkit/util/progress/logs"
	"github.com/moby/buildkit/util/resolver/limited"
	"github.com/moby/buildkit/util/resolver/retryhandler"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

type SessionResolver func(g session.Group) remotes.Resolver

type Puller struct {
	ContentStore content.Store
	Resolver     remotes.Resolver
	Src          reference.Spec
	Platform     ocispecs.Platform

	g           flightcontrol.Group[struct{}]
	resolveErr  error
	resolveDone bool
	desc        ocispecs.Descriptor
	configDesc  ocispecs.Descriptor
	ref         string
	layers      []ocispecs.Descriptor
	nonlayers   []ocispecs.Descriptor
}

var _ content.Provider = &provider{}

type PulledManifests struct {
	Ref               string
	RootManifestDesc  ocispecs.Descriptor
	ImageManifestDesc ocispecs.Descriptor // Platform-specific manifest selected from RootManifestDesc.
	ConfigDesc        ocispecs.Descriptor
	Nonlayers         []ocispecs.Descriptor
	Descriptors       []ocispecs.Descriptor
	Provider          func(session.Group) content.Provider
}

func (p *Puller) resolve(ctx context.Context, resolver remotes.Resolver) error {
	_, err := p.g.Do(ctx, "", func(ctx context.Context) (_ struct{}, err error) {
		if p.resolveErr != nil || p.resolveDone {
			return struct{}{}, p.resolveErr
		}
		defer func() {
			if !errors.Is(err, context.Canceled) {
				p.resolveErr = err
			}
		}()
		if p.tryLocalResolve(ctx) == nil {
			return
		}
		ref, desc, err := resolver.Resolve(ctx, p.Src.String())
		if err != nil {
			return struct{}{}, err
		}
		p.desc = desc
		p.ref = ref
		p.resolveDone = true
		return struct{}{}, nil
	})
	return err
}

func (p *Puller) tryLocalResolve(ctx context.Context) error {
	desc := ocispecs.Descriptor{
		Digest: p.Src.Digest(),
	}

	if desc.Digest == "" {
		return errors.New("empty digest")
	}

	info, err := p.ContentStore.Info(ctx, desc.Digest)
	if err != nil {
		return err
	}

	if ok, err := contentutil.HasSource(info, p.Src); err != nil || !ok {
		return errors.New("no matching source")
	}

	desc.Size = info.Size
	p.ref = p.Src.String()
	ra, err := p.ContentStore.ReaderAt(ctx, desc)
	if err != nil {
		return err
	}
	mt, err := imageutil.DetectManifestMediaType(ra)
	if err != nil {
		return err
	}
	desc.MediaType = mt
	p.desc = desc
	return nil
}

func (p *Puller) PullManifests(ctx context.Context, getResolver SessionResolver) (*PulledManifests, error) {
	err := p.resolve(ctx, p.Resolver)
	if err != nil {
		return nil, err
	}

	platform := platforms.Only(p.Platform)

	var mu sync.Mutex // images.Dispatch calls handlers in parallel
	metadata := make(map[digest.Digest]ocispecs.Descriptor)

	// TODO: need a wrapper snapshot interface that combines content
	// and snapshots as 1) buildkit shouldn't have a dependency on contentstore
	// or 2) cachemanager should manage the contentstore
	var handlers []images.Handler

	fetcher, err := p.Resolver.Fetcher(ctx, p.ref)
	if err != nil {
		return nil, err
	}

	if p.desc.MediaType == images.MediaTypeDockerSchema1Manifest {
		errMsg := "support Docker Image manifest version 2, schema 1 has been removed. " +
			"More information at https://docs.docker.com/go/deprecated-image-specs/"
		return nil, errors.WithStack(cerrdefs.ErrConflict.WithMessage(errMsg))
	}
	// Get all the children for a descriptor
	childrenHandler := images.ChildrenHandler(p.ContentStore)
	// Filter the children by the platform
	childrenHandler = images.FilterPlatforms(childrenHandler, platform)
	// Limit manifests pulled to the best match in an index
	childrenHandler = images.LimitManifests(childrenHandler, platform, 1)

	var manifestDesc ocispecs.Descriptor
	var manifestOnce sync.Once
	manifestHandler := images.HandlerFunc(func(ctx context.Context, desc ocispecs.Descriptor) ([]ocispecs.Descriptor, error) {
		if images.IsManifestType(desc.MediaType) {
			// Each index yields one child, so the first manifest is the
			// selected image, including through nested indexes. Do not let
			// malformed config or layer descriptors replace it later.
			manifestOnce.Do(func() { manifestDesc = desc })
		}
		return nil, nil
	})

	dslHandler, err := docker.AppendDistributionSourceLabel(p.ContentStore, p.ref)
	if err != nil {
		return nil, err
	}
	handlers = append(handlers,
		filterLayerBlobs(metadata, &mu),
		retryhandler.New(limited.FetchHandler(p.ContentStore, fetcher, p.ref), logs.LoggerFromContext(ctx)),
		childrenHandler,
		manifestHandler,
		dslHandler,
	)

	if err := images.Dispatch(ctx, images.Handlers(handlers...), nil, p.desc); err != nil {
		return nil, err
	}

	for _, desc := range metadata {
		p.nonlayers = append(p.nonlayers, desc)
		switch desc.MediaType {
		case images.MediaTypeDockerSchema2Config, ocispecs.MediaTypeImageConfig:
			p.configDesc = desc
		}
	}

	// split all pulled data to layers and rest. layers remain roots and are deleted with snapshots. rest will be linked to layers.
	p.layers, err = getLayers(ctx, p.ContentStore, p.desc, platform)
	if err != nil {
		return nil, err
	}

	return &PulledManifests{
		Ref:               p.ref,
		RootManifestDesc:  p.desc,
		ImageManifestDesc: manifestDesc,
		ConfigDesc:        p.configDesc,
		Nonlayers:         p.nonlayers,
		Descriptors:       p.layers,
		Provider: func(g session.Group) content.Provider {
			return &provider{puller: p, resolver: getResolver(g)}
		},
	}, nil
}

type provider struct {
	puller   *Puller
	resolver remotes.Resolver
}

func (p *provider) ReaderAt(ctx context.Context, desc ocispecs.Descriptor) (content.ReaderAt, error) {
	err := p.puller.resolve(ctx, p.resolver)
	if err != nil {
		return nil, err
	}

	fetcher, err := p.resolver.Fetcher(ctx, p.puller.ref)
	if err != nil {
		return nil, err
	}

	return contentutil.FromFetcher(fetcher).ReaderAt(ctx, desc)
}

// filterLayerBlobs causes layer blobs to be skipped for fetch, which is required to support lazy blobs.
// It also stores the non-layer blobs (metadata) it encounters in the provided map.
func filterLayerBlobs(metadata map[digest.Digest]ocispecs.Descriptor, mu sync.Locker) images.HandlerFunc {
	return func(ctx context.Context, desc ocispecs.Descriptor) ([]ocispecs.Descriptor, error) {
		switch desc.MediaType {
		case
			ocispecs.MediaTypeImageLayer,
			ocispecs.MediaTypeImageLayerNonDistributable, //nolint:staticcheck // ignore SA1019: Non-distributable layers are deprecated, and not recommended for future use.
			images.MediaTypeDockerSchema2Layer,
			images.MediaTypeDockerSchema2LayerForeign,
			ocispecs.MediaTypeImageLayerGzip,
			images.MediaTypeDockerSchema2LayerGzip,
			ocispecs.MediaTypeImageLayerNonDistributableGzip, //nolint:staticcheck // ignore SA1019: Non-distributable layers are deprecated, and not recommended for future use.
			images.MediaTypeDockerSchema2LayerForeignGzip,
			ocispecs.MediaTypeImageLayerZstd,
			ocispecs.MediaTypeImageLayerNonDistributableZstd: //nolint:staticcheck // ignore SA1019: Non-distributable layers are deprecated, and not recommended for future use.
			return nil, images.ErrSkipDesc
		default:
			if metadata != nil {
				mu.Lock()
				metadata[desc.Digest] = desc
				mu.Unlock()
			}
		}
		return nil, nil
	}
}

func getLayers(ctx context.Context, provider content.Provider, desc ocispecs.Descriptor, platform platforms.MatchComparer) ([]ocispecs.Descriptor, error) {
	manifest, err := images.Manifest(ctx, provider, desc, platform)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	image := images.Image{Target: desc}
	diffIDs, err := image.RootFS(ctx, provider, platform)
	if err != nil {
		return nil, errors.Wrap(err, "failed to resolve rootfs")
	}
	if len(diffIDs) != len(manifest.Layers) {
		return nil, errors.Errorf("mismatched image rootfs and manifest layers %+v %+v", diffIDs, manifest.Layers)
	}
	layers := make([]ocispecs.Descriptor, len(diffIDs))
	for i := range diffIDs {
		desc := manifest.Layers[i]
		if desc.Annotations == nil {
			desc.Annotations = map[string]string{}
		}
		desc.Annotations[labels.LabelUncompressed] = diffIDs[i].String()
		layers[i] = desc
	}
	return layers, nil
}
