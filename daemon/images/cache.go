package images // import "github.com/docker/docker/daemon/images"

import (
	"context"
	"fmt"
	"sync"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/namespaces"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/builder"
	"github.com/docker/docker/layer"
	digest "github.com/opencontainers/go-digest"
)

type cache struct {
	m      sync.RWMutex
	layers map[string]map[digest.Digest]layer.Layer
}

// LoadCache loads the image cache by scanning containerd images
// and listening to containerd events
// This process can be backgrounded and will hold hold the cache
// lock until fully populated.
func (i *ImageService) LoadCache(ctx context.Context) error {
	namespace, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return err
	}
	log.G(ctx).WithField("namespace", namespace).Debugf("loading cache")

	_, err = i.loadNSCache(ctx, namespace)
	return err
}

func (i *ImageService) loadNSCache(ctx context.Context, namespace string) (*cache, error) {
	i.cacheL.Lock()
	defer i.cacheL.Unlock()

	var (
		c = &cache{
			layers: map[string]map[digest.Digest]layer.Layer{},
		}
	)

	// Load layers
	for _, backend := range i.layerBackends {
		backendCache, err := i.loadLayers(ctx, backend)
		if err != nil {
			return nil, err
		}

		c.layers[backend.DriverName()] = backendCache
	}

	i.cache[namespace] = c

	return c, nil
}

func (i *ImageService) loadLayers(ctx context.Context, backend layer.Store) (map[digest.Digest]layer.Layer, error) {
	cs := i.client.ContentStore()
	backendCache := map[digest.Digest]layer.Layer{}
	name := backend.DriverName()
	label := fmt.Sprintf("%s%s", LabelLayerPrefix, name)
	err := cs.Walk(ctx, func(info content.Info) error {
		value := digest.Digest(info.Labels[label])
		if _, ok := backendCache[value]; ok {
			return nil
		}
		l, err := backend.Get(layer.ChainID(value))
		if err != nil {
			log.G(ctx).WithError(err).WithField("digest", info.Digest).WithField("driver", name).Warnf("unable to get layer")
		} else {
			log.G(ctx).WithField("digest", info.Digest).WithField("driver", name).Debugf("retaining layer %s", value)
			backendCache[value] = l
		}
		return nil
	}, fmt.Sprintf("labels.%q", label))
	if err != nil {
		return nil, err
	}

	return backendCache, nil
}

func (i *ImageService) getCache(ctx context.Context) (c *cache, err error) {
	namespace, ok := namespaces.Namespace(ctx)
	if !ok {
		namespace = i.namespace
	}
	i.cacheL.RLock()
	c, ok = i.cache[namespace]
	i.cacheL.RUnlock()
	if !ok {
		c, err = i.loadNSCache(ctx, namespace)
		if err != nil {
			return nil, err
		}
	}

	return c, nil
}

// TODO(containerd): move this to separate package and replace
// "github.com/docker/docker/image/cache" implementation
type buildCache struct {
	sources []string
	client  *containerd.Client
}

func (bc *buildCache) GetCache(parentID string, cfg *container.Config) (imageID string, err error) {
	return "", nil
}

// MakeImageCache creates a stateful image cache for build.
func (i *ImageService) MakeImageCache(sourceRefs []string) builder.ImageCache {
	return &buildCache{
		sources: sourceRefs,
		client:  i.client,
	}
	/*
		if len(sourceRefs) == 0 {
			return buildcache.NewLocal(i.imageStore)
		}

		cache := buildcache.New(i.imageStore)

		for _, ref := range sourceRefs {
			img, err := i.getDockerImage(ref)
			if err != nil {
				logrus.Warnf("Could not look up %s for cache resolution, skipping: %+v", ref, err)
				continue
			}
			cache.Populate(img)
		}

		return cache
	*/
}
