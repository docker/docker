package registry

import (
	"context"
	"time"

	"github.com/containerd/containerd/remotes"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/docker/distribution/reference"
	"github.com/moby/buildkit/cache/remotecache"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth"
	"github.com/moby/buildkit/util/contentutil"
	"github.com/moby/buildkit/util/resolver"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

func canonicalizeRef(rawRef string) (string, error) {
	if rawRef == "" {
		return "", errors.New("missing ref")
	}
	parsed, err := reference.ParseNormalizedNamed(rawRef)
	if err != nil {
		return "", err
	}
	return reference.TagNameOnly(parsed).String(), nil
}

const (
	attrRef = "ref"
)

func ResolveCacheExporterFunc(sm *session.Manager, resolverOpt resolver.ResolveOptionsFunc) remotecache.ResolveCacheExporterFunc {
	return func(ctx context.Context, attrs map[string]string) (remotecache.Exporter, error) {
		ref, err := canonicalizeRef(attrs[attrRef])
		if err != nil {
			return nil, err
		}
		remote := newRemoteResolver(ctx, resolverOpt, sm, ref)
		pusher, err := remote.Pusher(ctx, ref)
		if err != nil {
			return nil, err
		}
		return remotecache.NewExporter(contentutil.FromPusher(pusher)), nil
	}
}

func ResolveCacheImporterFunc(sm *session.Manager, resolverOpt resolver.ResolveOptionsFunc) remotecache.ResolveCacheImporterFunc {
	return func(ctx context.Context, attrs map[string]string) (remotecache.Importer, specs.Descriptor, error) {
		ref, err := canonicalizeRef(attrs[attrRef])
		if err != nil {
			return nil, specs.Descriptor{}, err
		}
		remote := newRemoteResolver(ctx, resolverOpt, sm, ref)
		xref, desc, err := remote.Resolve(ctx, ref)
		if err != nil {
			return nil, specs.Descriptor{}, err
		}
		fetcher, err := remote.Fetcher(ctx, xref)
		if err != nil {
			return nil, specs.Descriptor{}, err
		}
		return remotecache.NewImporter(contentutil.FromFetcher(fetcher)), desc, nil
	}
}

func newRemoteResolver(ctx context.Context, resolverOpt resolver.ResolveOptionsFunc, sm *session.Manager, ref string) remotes.Resolver {
	opt := resolverOpt(ref)
	opt.Credentials = getCredentialsFunc(ctx, sm)
	return docker.NewResolver(opt)
}

func getCredentialsFunc(ctx context.Context, sm *session.Manager) func(string) (string, string, error) {
	id := session.FromContext(ctx)
	if id == "" {
		return nil
	}
	return func(host string) (string, string, error) {
		timeoutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		caller, err := sm.Get(timeoutCtx, id)
		if err != nil {
			return "", "", err
		}

		return auth.CredentialsFunc(context.TODO(), caller)(host)
	}
}
