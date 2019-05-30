package file

import (
	"context"

	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver/llbsolver/ops/fileoptypes"
	"github.com/pkg/errors"
)

func NewRefManager(cm cache.Manager) *RefManager {
	return &RefManager{cm: cm}
}

type RefManager struct {
	cm cache.Manager
}

func (rm *RefManager) Prepare(ctx context.Context, ref fileoptypes.Ref, readonly bool) (fileoptypes.Mount, error) {
	ir, ok := ref.(cache.ImmutableRef)
	if !ok && ref != nil {
		return nil, errors.Errorf("invalid ref type: %T", ref)
	}

	if ir != nil && readonly {
		m, err := ir.Mount(ctx, readonly)
		if err != nil {
			return nil, err
		}
		return &Mount{m: m}, nil
	}

	mr, err := rm.cm.New(ctx, ir, cache.WithDescription("fileop target"), cache.CachePolicyRetain)
	if err != nil {
		return nil, err
	}
	m, err := mr.Mount(ctx, readonly)
	if err != nil {
		return nil, err
	}
	return &Mount{m: m, mr: mr}, nil
}

func (rm *RefManager) Commit(ctx context.Context, mount fileoptypes.Mount) (fileoptypes.Ref, error) {
	m, ok := mount.(*Mount)
	if !ok {
		return nil, errors.Errorf("invalid mount type %T", mount)
	}
	if err := m.m.Release(); err != nil {
		return nil, err
	}
	if m.mr == nil {
		return nil, errors.Errorf("invalid mount without active ref for commit")
	}
	return m.mr.Commit(ctx)
}

type Mount struct {
	m  snapshot.Mountable
	mr cache.MutableRef
}

func (m *Mount) Release(ctx context.Context) error {
	m.m.Release()
	if m.mr != nil {
		return m.mr.Release(ctx)
	}
	return nil
}
func (m *Mount) IsFileOpMount() {}
