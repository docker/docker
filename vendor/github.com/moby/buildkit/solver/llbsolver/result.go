package llbsolver

import (
	"bytes"
	"context"
	"path"

	"github.com/moby/buildkit/cache/contenthash"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/worker"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

type Selector struct {
	Path        string
	Wildcard    bool
	FollowLinks bool
}

func NewContentHashFunc(selectors []Selector) solver.ResultBasedCacheFunc {
	return func(ctx context.Context, res solver.Result) (digest.Digest, error) {
		ref, ok := res.Sys().(*worker.WorkerRef)
		if !ok {
			return "", errors.Errorf("invalid reference: %T", res)
		}

		if len(selectors) == 0 {
			selectors = []Selector{{}}
		}

		dgsts := make([][]byte, len(selectors))

		eg, ctx := errgroup.WithContext(ctx)

		for i, sel := range selectors {
			// FIXME(tonistiigi): enabling this parallelization seems to create wrong results for some big inputs(like gobuild)
			// func(i int) {
			// 	eg.Go(func() error {
			if !sel.Wildcard {
				dgst, err := contenthash.Checksum(ctx, ref.ImmutableRef, path.Join("/", sel.Path), sel.FollowLinks)
				if err != nil {
					return "", err
				}
				dgsts[i] = []byte(dgst)
			} else {
				dgst, err := contenthash.ChecksumWildcard(ctx, ref.ImmutableRef, path.Join("/", sel.Path), sel.FollowLinks)
				if err != nil {
					return "", err
				}
				dgsts[i] = []byte(dgst)
			}
			// return nil
			// })
			// }(i)
		}

		if err := eg.Wait(); err != nil {
			return "", err
		}

		return digest.FromBytes(bytes.Join(dgsts, []byte{0})), nil
	}
}

func workerRefConverter(ctx context.Context, res solver.Result) (*solver.Remote, error) {
	ref, ok := res.Sys().(*worker.WorkerRef)
	if !ok {
		return nil, errors.Errorf("invalid result: %T", res.Sys())
	}

	return ref.Worker.GetRemote(ctx, ref.ImmutableRef, true)
}
