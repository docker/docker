package builder

import (
	"context"
	"fmt"

	"github.com/gohugoio/hashstructure"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	"github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/util/flightcontrol"
	digest "github.com/opencontainers/go-digest"
)

// withResolveCache wraps client.Client so that ResolveImageConfig
// calls are cached and deduplicated via flightcontrol.CachedGroup
type withResolveCache struct {
	client.Client
	g flightcontrol.CachedGroup[*resolveResult]
}

type resolveResult struct {
	ref  string
	dgst digest.Digest
	cfg  []byte
}

var _ client.Client = &withResolveCache{}

func newWithResolveCache(c client.Client) *withResolveCache {
	return &withResolveCache{
		Client: c,
		g:      flightcontrol.CachedGroup[*resolveResult]{CacheError: true},
	}
}

func (c *withResolveCache) ResolveImageConfig(ctx context.Context, ref string, opt sourceresolver.Opt) (string, digest.Digest, []byte, error) {
	optHash, err := hashstructure.Hash(opt, nil)
	if err != nil {
		return "", "", nil, err
	}
	key := fmt.Sprintf("%s,%d", ref, optHash)
	res, err := c.g.Do(ctx, key, func(ctx context.Context) (*resolveResult, error) {
		ref, dgst, cfg, err := c.Client.ResolveImageConfig(ctx, ref, opt)
		if err != nil {
			return nil, err
		}
		return &resolveResult{ref, dgst, cfg}, nil
	})
	if err != nil {
		return "", "", nil, err
	}
	return res.ref, res.dgst, res.cfg, nil
}
