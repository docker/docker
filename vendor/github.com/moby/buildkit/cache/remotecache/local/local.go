package local

import (
	"context"
	"time"

	"github.com/containerd/containerd/content"
	"github.com/moby/buildkit/cache/remotecache"
	"github.com/moby/buildkit/session"
	sessioncontent "github.com/moby/buildkit/session/content"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

const (
	attrDigest           = "digest"
	attrSrc              = "src"
	attrDest             = "dest"
	contentStoreIDPrefix = "local:"
)

// ResolveCacheExporterFunc for "local" cache exporter.
func ResolveCacheExporterFunc(sm *session.Manager) remotecache.ResolveCacheExporterFunc {
	return func(ctx context.Context, attrs map[string]string) (remotecache.Exporter, error) {
		store := attrs[attrDest]
		if store == "" {
			return nil, errors.New("local cache exporter requires dest")
		}
		csID := contentStoreIDPrefix + store
		cs, err := getContentStore(ctx, sm, csID)
		if err != nil {
			return nil, err
		}
		return remotecache.NewExporter(cs), nil
	}
}

// ResolveCacheImporterFunc for "local" cache importer.
func ResolveCacheImporterFunc(sm *session.Manager) remotecache.ResolveCacheImporterFunc {
	return func(ctx context.Context, attrs map[string]string) (remotecache.Importer, specs.Descriptor, error) {
		dgstStr := attrs[attrDigest]
		if dgstStr == "" {
			return nil, specs.Descriptor{}, errors.New("local cache importer requires explicit digest")
		}
		dgst := digest.Digest(dgstStr)
		store := attrs[attrSrc]
		if store == "" {
			return nil, specs.Descriptor{}, errors.New("local cache importer requires src")
		}
		csID := contentStoreIDPrefix + store
		cs, err := getContentStore(ctx, sm, csID)
		if err != nil {
			return nil, specs.Descriptor{}, err
		}
		info, err := cs.Info(ctx, dgst)
		if err != nil {
			return nil, specs.Descriptor{}, err
		}
		desc := specs.Descriptor{
			// MediaType is typically MediaTypeDockerSchema2ManifestList,
			// but we leave it empty until we get correct support for local index.json
			Digest: dgst,
			Size:   info.Size,
		}
		return remotecache.NewImporter(cs), desc, nil
	}
}

func getContentStore(ctx context.Context, sm *session.Manager, storeID string) (content.Store, error) {
	sessionID := session.FromContext(ctx)
	if sessionID == "" {
		return nil, errors.New("local cache exporter/importer requires session")
	}
	timeoutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	caller, err := sm.Get(timeoutCtx, sessionID)
	if err != nil {
		return nil, err
	}
	return sessioncontent.NewCallerStore(caller, storeID), nil
}
