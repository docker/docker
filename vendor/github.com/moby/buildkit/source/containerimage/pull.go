package containerimage

import (
	"context"
	"encoding/json"
	"maps"
	"runtime"
	"time"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/snapshotters"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/solver/errdefs"
	"github.com/moby/buildkit/util/cachedigest"
	"github.com/moby/buildkit/util/estargz"
	"github.com/moby/buildkit/util/flightcontrol"
	"github.com/moby/buildkit/util/imageutil"
	"github.com/moby/buildkit/util/leaseutil"
	"github.com/moby/buildkit/util/progress"
	"github.com/moby/buildkit/util/progress/controller"
	"github.com/moby/buildkit/util/pull"
	"github.com/moby/buildkit/util/resolver"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

type puller struct {
	CacheAccessor  cache.Accessor
	LeaseManager   leases.Manager
	RegistryHosts  docker.RegistryHosts
	ImageStore     images.Store
	Mode           resolver.ResolveMode
	RecordType     client.UsageRecordType
	Ref            string
	SessionManager *session.Manager
	layerLimit     *int
	checksum       digest.Digest
	vtx            solver.Vertex
	ResolverType
	store sourceresolver.ResolveImageConfigOptStore

	g                   flightcontrol.Group[struct{}]
	cacheKeyErr         error
	cacheKeyDone        bool
	releaseTmpLeases    func(context.Context) error
	descHandlers        cache.DescHandlers
	manifest            *pull.PulledManifests
	manifestKey         string
	platformManifestKey string
	*pull.Puller
}

func mainManifestKey(desc ocispecs.Descriptor, platform ocispecs.Platform, layerLimit *int) (digest.Digest, error) {
	dt, err := json.Marshal(struct {
		Digest     digest.Digest
		OS         string
		Arch       string
		Variant    string   `json:",omitempty"`
		OSVersion  string   `json:",omitempty"`
		OSFeatures []string `json:",omitempty"`
		Limit      *int     `json:",omitempty"`
	}{
		Digest:     desc.Digest,
		OS:         platform.OS,
		Arch:       platform.Architecture,
		Variant:    platform.Variant,
		OSVersion:  platform.OSVersion,
		OSFeatures: platform.OSFeatures,
		Limit:      layerLimit,
	})
	if err != nil {
		return "", err
	}
	return cachedigest.FromBytes(dt, cachedigest.TypeJSON)
}

func (p *puller) CacheKey(ctx context.Context, jobCtx solver.JobContext, index int) (cacheKey string, imgDigest string, cacheOpts solver.CacheOpts, cacheDone bool, err error) {
	var g session.Group
	if jobCtx != nil {
		g = jobCtx.Session()
	}
	var getResolver pull.SessionResolver
	switch p.ResolverType {
	case ResolverTypeRegistry:
		resolver := resolver.DefaultPool.GetResolver(p.RegistryHosts, p.Ref, resolver.ScopeType{}, p.SessionManager, g).WithImageStore(p.ImageStore, p.Mode)
		p.Resolver = resolver
		getResolver = func(g session.Group) remotes.Resolver { return resolver.WithSession(g) }
	case ResolverTypeOCILayout:
		resolver := getOCILayoutResolver(p.store, p.SessionManager, g)
		p.Resolver = resolver
		// OCILayout has no need for session
		getResolver = func(g session.Group) remotes.Resolver { return resolver }
	default:
	}

	// progressFactory needs the outer context, the context in `p.g.Do` will
	// be canceled before the progress output is complete
	progressFactory := progress.FromContext(ctx)

	_, err = p.g.Do(ctx, "", func(ctx context.Context) (_ struct{}, err error) {
		if p.cacheKeyErr != nil || p.cacheKeyDone {
			return struct{}{}, p.cacheKeyErr
		}
		defer func() {
			if !errdefs.IsCanceled(ctx, err) {
				p.cacheKeyErr = err
			}
		}()
		ctx, done, err := leaseutil.WithLease(ctx, p.LeaseManager, leases.WithExpiration(5*time.Minute), leaseutil.MakeTemporary)
		if err != nil {
			return struct{}{}, err
		}
		p.releaseTmpLeases = done
		defer imageutil.AddLease(done)

		resolveProgressDone := progress.OneOff(ctx, "resolve "+p.Src.String())
		defer func() {
			resolveProgressDone(err)
		}()

		p.manifest, err = p.PullManifests(ctx, getResolver)
		if err != nil {
			return struct{}{}, err
		}

		if p.checksum != "" && p.manifest.RootManifestDesc.Digest != p.checksum {
			return struct{}{}, errors.Errorf("image digest %s for %s does not match expected checksum %s", p.manifest.RootManifestDesc.Digest, p.Ref, p.checksum)
		}

		if ll := p.layerLimit; ll != nil {
			if *ll > len(p.manifest.Descriptors) {
				return struct{}{}, errors.Errorf("layer limit %d is greater than the number of layers in the image %d", *ll, len(p.manifest.Descriptors))
			}
			p.manifest.Descriptors = p.manifest.Descriptors[:*ll]
		}

		if len(p.manifest.Descriptors) > 0 {
			progressController := &controller.Controller{
				WriterFactory: progressFactory,
			}
			if p.vtx != nil {
				progressController.Digest = p.vtx.Digest()
				progressController.Name = p.vtx.Name()
				progressController.ProgressGroup = p.vtx.Options().ProgressGroup
			}

			p.descHandlers = cache.DescHandlers(make(map[digest.Digest]*cache.DescHandler))
			for i, desc := range p.manifest.Descriptors {
				labels := snapshots.FilterInheritedLabels(desc.Annotations)
				if labels == nil {
					labels = make(map[string]string)
				}
				maps.Copy(labels, estargz.SnapshotLabels(p.manifest.Ref, p.manifest.Descriptors, i))
				labels[snapshotters.TargetRefLabel] = p.manifest.Ref

				p.descHandlers[desc.Digest] = &cache.DescHandler{
					Provider:       p.manifest.Provider,
					Progress:       progressController,
					SnapshotLabels: labels,
					Annotations:    desc.Annotations,
					Ref:            p.manifest.Ref,
				}
			}
		}

		desc := p.manifest.RootManifestDesc
		k, err := mainManifestKey(desc, p.Platform, p.layerLimit)
		if err != nil {
			return struct{}{}, err
		}
		p.manifestKey = k.String()
		k, err = mainManifestKey(p.manifest.ImageManifestDesc, p.Platform, p.layerLimit)
		if err != nil {
			return struct{}{}, err
		}
		p.platformManifestKey = k.String()
		// Do not use RootFS DiffIDs from the image config as an alternate
		// cache key. They are verified only when layers are applied, and a
		// failed malicious pull must not poison another image source.
		p.cacheKeyDone = true
		return struct{}{}, nil
	})
	if err != nil {
		return "", "", nil, false, err
	}

	cacheOpts = solver.CacheOpts(make(map[any]any))
	for dgst, descHandler := range p.descHandlers {
		cacheOpts[cache.DescHandlerKey(dgst)] = descHandler
	}

	// Keep the index key for existing caches, but also match the selected
	// manifest so changes to attestations or other platforms do not invalidate
	// this source. Both keys identify verified manifest content, not DiffIDs
	// advertised by an unverified image config.
	if index == 0 {
		return p.manifestKey, p.manifest.RootManifestDesc.Digest.String(), cacheOpts, p.manifestKey == p.platformManifestKey, nil
	}
	return p.platformManifestKey, p.manifest.RootManifestDesc.Digest.String(), cacheOpts, true, nil
}

func (p *puller) Snapshot(ctx context.Context, jobCtx solver.JobContext) (ir cache.ImmutableRef, err error) {
	var g session.Group
	if jobCtx != nil {
		g = jobCtx.Session()
	}
	var getResolver pull.SessionResolver
	switch p.ResolverType {
	case ResolverTypeRegistry:
		resolver := resolver.DefaultPool.GetResolver(p.RegistryHosts, p.Ref, resolver.ScopeType{}, p.SessionManager, g).WithImageStore(p.ImageStore, p.Mode)
		p.Resolver = resolver
		getResolver = func(g session.Group) remotes.Resolver { return resolver.WithSession(g) }
	case ResolverTypeOCILayout:
		resolver := getOCILayoutResolver(p.store, p.SessionManager, g)
		p.Resolver = resolver
		// OCILayout has no need for session
		getResolver = func(g session.Group) remotes.Resolver { return resolver }
	default:
	}

	if len(p.manifest.Descriptors) == 0 {
		return nil, nil
	}
	defer func() {
		if p.releaseTmpLeases != nil {
			p.releaseTmpLeases(context.WithoutCancel(ctx))
		}
	}()

	var current cache.ImmutableRef
	defer func() {
		if err != nil && current != nil {
			current.Release(context.WithoutCancel(ctx))
		}
	}()

	var parent cache.ImmutableRef
	setWindowsLayerType := p.Platform.OS == "windows" && runtime.GOOS != "windows"
	for _, layerDesc := range p.manifest.Descriptors {
		parent = current
		current, err = p.CacheAccessor.GetByBlob(ctx, layerDesc, parent,
			p.descHandlers, cache.WithImageRef(p.manifest.Ref))
		if parent != nil {
			parent.Release(context.TODO())
		}
		if err != nil {
			return nil, err
		}
		if setWindowsLayerType {
			if err := current.SetLayerType("windows"); err != nil {
				return nil, err
			}
		}
	}

	for _, desc := range p.manifest.Nonlayers {
		if _, err := p.ContentStore.Info(ctx, desc.Digest); cerrdefs.IsNotFound(err) {
			// manifest or config must have gotten gc'd after CacheKey, re-pull them
			ctx, done, err := leaseutil.WithLease(ctx, p.LeaseManager, leaseutil.MakeTemporary)
			if err != nil {
				return nil, err
			}
			defer done(context.WithoutCancel(ctx))

			if _, err := p.PullManifests(ctx, getResolver); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}

		if err := p.LeaseManager.AddResource(ctx, leases.Lease{ID: current.ID()}, leases.Resource{
			ID:   desc.Digest.String(),
			Type: "content",
		}); err != nil {
			return nil, err
		}
	}

	if p.RecordType != "" && current.GetRecordType() == "" {
		if err := current.SetRecordType(p.RecordType); err != nil {
			return nil, err
		}
	}

	return current, nil
}
