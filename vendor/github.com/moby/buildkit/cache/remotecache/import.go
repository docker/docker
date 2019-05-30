package remotecache

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images"
	v1 "github.com/moby/buildkit/cache/remotecache/v1"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/util/imageutil"
	"github.com/moby/buildkit/worker"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

// ResolveCacheImporterFunc returns importer and descriptor.
type ResolveCacheImporterFunc func(ctx context.Context, attrs map[string]string) (Importer, ocispec.Descriptor, error)

type Importer interface {
	Resolve(ctx context.Context, desc ocispec.Descriptor, id string, w worker.Worker) (solver.CacheManager, error)
}

func NewImporter(provider content.Provider) Importer {
	return &contentCacheImporter{provider: provider}
}

type contentCacheImporter struct {
	provider content.Provider
}

func (ci *contentCacheImporter) Resolve(ctx context.Context, desc ocispec.Descriptor, id string, w worker.Worker) (solver.CacheManager, error) {
	dt, err := readBlob(ctx, ci.provider, desc)
	if err != nil {
		return nil, err
	}

	var mfst ocispec.Index
	if err := json.Unmarshal(dt, &mfst); err != nil {
		return nil, err
	}

	allLayers := v1.DescriptorProvider{}

	var configDesc ocispec.Descriptor

	for _, m := range mfst.Manifests {
		if m.MediaType == v1.CacheConfigMediaTypeV0 {
			configDesc = m
			continue
		}
		allLayers[m.Digest] = v1.DescriptorProviderPair{
			Descriptor: m,
			Provider:   ci.provider,
		}
	}

	if configDesc.Digest == "" {
		return ci.importInlineCache(ctx, dt, id, w)
	}

	dt, err = readBlob(ctx, ci.provider, configDesc)
	if err != nil {
		return nil, err
	}

	cc := v1.NewCacheChains()
	if err := v1.Parse(dt, allLayers, cc); err != nil {
		return nil, err
	}

	keysStorage, resultStorage, err := v1.NewCacheKeyStorage(cc, w)
	if err != nil {
		return nil, err
	}
	return solver.NewCacheManager(id, keysStorage, resultStorage), nil
}

func readBlob(ctx context.Context, provider content.Provider, desc ocispec.Descriptor) ([]byte, error) {
	maxBlobSize := int64(1 << 20)
	if desc.Size > maxBlobSize {
		return nil, errors.Errorf("blob %s is too large (%d > %d)", desc.Digest, desc.Size, maxBlobSize)
	}
	dt, err := content.ReadBlob(ctx, provider, desc)
	if err != nil {
		// NOTE: even if err == EOF, we might have got expected dt here.
		// For instance, http.Response.Body is known to return non-zero bytes with EOF.
		if err == io.EOF {
			if dtDigest := desc.Digest.Algorithm().FromBytes(dt); dtDigest != desc.Digest {
				err = errors.Wrapf(err, "got EOF, expected %s (%d bytes), got %s (%d bytes)",
					desc.Digest, desc.Size, dtDigest, len(dt))
			} else {
				err = nil
			}
		}
	}
	return dt, err
}

func (ci *contentCacheImporter) importInlineCache(ctx context.Context, dt []byte, id string, w worker.Worker) (solver.CacheManager, error) {
	m := map[digest.Digest][]byte{}

	if err := ci.allDistributionManifests(ctx, dt, m); err != nil {
		return nil, err
	}

	var mu sync.Mutex
	cc := v1.NewCacheChains()

	eg, ctx := errgroup.WithContext(ctx)
	for dgst, dt := range m {
		func(dgst digest.Digest, dt []byte) {
			eg.Go(func() error {
				var m ocispec.Manifest

				if err := json.Unmarshal(dt, &m); err != nil {
					return err
				}

				if m.Config.Digest == "" || len(m.Layers) == 0 {
					return nil
				}

				p, err := content.ReadBlob(ctx, ci.provider, m.Config)
				if err != nil {
					return err
				}

				var img image

				if err := json.Unmarshal(p, &img); err != nil {
					return err
				}

				if len(img.Rootfs.DiffIDs) != len(m.Layers) {
					logrus.Warnf("invalid image with mismatching manifest and config")
					return nil
				}

				if img.Cache == nil {
					return nil
				}

				var config v1.CacheConfig
				if err := json.Unmarshal(img.Cache, &config.Records); err != nil {
					return err
				}

				createdDates, createdMsg, err := parseCreatedLayerInfo(img)
				if err != nil {
					return err
				}

				layers := v1.DescriptorProvider{}
				for i, m := range m.Layers {
					if m.Annotations == nil {
						m.Annotations = map[string]string{}
					}
					if createdAt := createdDates[i]; createdAt != "" {
						m.Annotations["buildkit/createdat"] = createdAt
					}
					if createdBy := createdMsg[i]; createdBy != "" {
						m.Annotations["buildkit/description"] = createdBy
					}
					m.Annotations["containerd.io/uncompressed"] = img.Rootfs.DiffIDs[i].String()
					layers[m.Digest] = v1.DescriptorProviderPair{
						Descriptor: m,
						Provider:   ci.provider,
					}
					config.Layers = append(config.Layers, v1.CacheLayer{
						Blob:        m.Digest,
						ParentIndex: i - 1,
					})
				}

				dt, err = json.Marshal(config)
				if err != nil {
					return err
				}

				mu.Lock()
				if err := v1.ParseConfig(config, layers, cc); err != nil {
					return err
				}
				mu.Unlock()
				return nil
			})
		}(dgst, dt)
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	keysStorage, resultStorage, err := v1.NewCacheKeyStorage(cc, w)
	if err != nil {
		return nil, err
	}
	return solver.NewCacheManager(id, keysStorage, resultStorage), nil
}

func (ci *contentCacheImporter) allDistributionManifests(ctx context.Context, dt []byte, m map[digest.Digest][]byte) error {
	mt, err := imageutil.DetectManifestBlobMediaType(dt)
	if err != nil {
		return err
	}

	switch mt {
	case images.MediaTypeDockerSchema2Manifest, ocispec.MediaTypeImageManifest:
		m[digest.FromBytes(dt)] = dt
	case images.MediaTypeDockerSchema2ManifestList, ocispec.MediaTypeImageIndex:
		var index ocispec.Index
		if err := json.Unmarshal(dt, &index); err != nil {
			return err
		}

		for _, d := range index.Manifests {
			if _, ok := m[d.Digest]; ok {
				continue
			}
			p, err := content.ReadBlob(ctx, ci.provider, d)
			if err != nil {
				return err
			}
			if err := ci.allDistributionManifests(ctx, p, m); err != nil {
				return err
			}
		}
	}

	return nil
}

type image struct {
	Rootfs struct {
		DiffIDs []digest.Digest `json:"diff_ids"`
	} `json:"rootfs"`
	Cache   []byte `json:"moby.buildkit.cache.v0"`
	History []struct {
		Created    *time.Time `json:"created,omitempty"`
		CreatedBy  string     `json:"created_by,omitempty"`
		EmptyLayer bool       `json:"empty_layer,omitempty"`
	} `json:"history,omitempty"`
}

func parseCreatedLayerInfo(img image) ([]string, []string, error) {
	dates := make([]string, 0, len(img.Rootfs.DiffIDs))
	createdBy := make([]string, 0, len(img.Rootfs.DiffIDs))
	for _, h := range img.History {
		if !h.EmptyLayer {
			str := ""
			if h.Created != nil {
				dt, err := h.Created.MarshalText()
				if err != nil {
					return nil, nil, err
				}
				str = string(dt)
			}
			dates = append(dates, str)
			createdBy = append(createdBy, h.CreatedBy)
		}
	}
	return dates, createdBy, nil
}
