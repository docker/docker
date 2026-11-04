package solver

import (
	"context"

	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/progress"

	digest "github.com/opencontainers/go-digest"
)

type CacheOpts map[any]any

type progressKey struct{}

type cacheOptGetterKey struct{}

func CacheOptGetterOf(ctx context.Context) func(includeAncestors bool, keys ...any) map[any]any {
	if v := ctx.Value(cacheOptGetterKey{}); v != nil {
		if getter, ok := v.(func(includeAncestors bool, keys ...any) map[any]any); ok {
			return getter
		}
	}
	return nil
}

func WithCacheOptGetter(ctx context.Context, getter func(includeAncestors bool, keys ...any) map[any]any) context.Context {
	return context.WithValue(ctx, cacheOptGetterKey{}, getter)
}

func withAncestorCacheOpts(ctx context.Context, start *state) context.Context {
	return WithCacheOptGetter(ctx, func(includeAncestors bool, keys ...any) map[any]any {
		keySet := make(map[any]struct{})
		for _, k := range keys {
			keySet[k] = struct{}{}
		}
		values := make(map[any]any)
		walkAncestors(ctx, start, func(st *state) bool {
			if st.vertexFailed() {
				// don't use values from cancelled or otherwise error'd vertexes
				return false
			}
			for _, res := range st.op.cacheRes {
				if res.Opts == nil {
					continue
				}
				for k := range keySet {
					if v, ok := res.Opts[k]; ok {
						values[k] = v
						delete(keySet, k)
						if len(keySet) == 0 {
							return true
						}
					}
				}
			}
			return !includeAncestors // stop after the first state unless includeAncestors is true
		})
		return values
	})
}

func walkAncestors(ctx context.Context, start *state, f func(*state) bool) {
	stack := [][]*state{{start}}
	cache := make(map[digest.Digest]struct{})
	for len(stack) > 0 {
		sts := stack[len(stack)-1]
		if len(sts) == 0 {
			stack = stack[:len(stack)-1]
			continue
		}
		st := sts[len(sts)-1]
		stack[len(stack)-1] = sts[:len(sts)-1]
		if st == nil {
			continue
		}
		if _, ok := cache[st.origDigest]; ok {
			continue
		}
		cache[st.origDigest] = struct{}{}
		if shouldStop := f(st); shouldStop {
			return
		}
		stack = append(stack, []*state{})
		for _, parentDgst := range st.clientVertex.Inputs {
			st.solver.mu.RLock()
			parent := st.solver.actives[parentDgst]
			st.solver.mu.RUnlock()
			if parent == nil {
				bklog.G(ctx).Warnf("parent %q not found in active job list during cache opt search", parentDgst)
				continue
			}
			stack[len(stack)-1] = append(stack[len(stack)-1], parent)
		}
	}
}

func ProgressControllerFromContext(ctx context.Context) progress.Controller {
	var pg progress.Controller
	if optGetter := CacheOptGetterOf(ctx); optGetter != nil {
		if kv := optGetter(false, progressKey{}); kv != nil {
			if v, ok := kv[progressKey{}].(progress.Controller); ok {
				pg = v
			}
		}
	}
	return pg
}
