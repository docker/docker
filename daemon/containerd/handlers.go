package containerd

import (
	"context"

	"github.com/containerd/containerd/v2/core/content"
	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/moby/moby/v2/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// walkPresentChildren is a simple wrapper for c8dimages.Walk with presentChildrenHandler.
// This is only a convenient helper to reduce boilerplate.
//
// The traversal is protected by a lease so content cannot be garbage-collected
// while it is being inspected.
func (i *ImageService) walkPresentChildren(ctx context.Context, target ocispec.Descriptor, f func(context.Context, ocispec.Descriptor) error) error {
	ctx, release, err := i.client.WithLease(ctx)
	if err != nil {
		return errdefs.System(err)
	}
	defer func() {
		if err := release(context.WithoutCancel(ctx)); err != nil {
			log.G(ctx).WithError(err).Warn("failed to release content walk lease")
		}
	}()

	lid, _ := leases.FromContext(ctx)
	lm := i.client.LeasesService()
	handler := presentChildrenHandler(i.content, c8dimages.HandlerFunc(
		func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
			return nil, f(ctx, desc)
		}))

	return c8dimages.Walk(ctx, c8dimages.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		if err := lm.AddResource(ctx, leases.Lease{ID: lid}, leases.Resource{
			ID:   desc.Digest.String(),
			Type: "content",
		}); err != nil {
			return nil, errdefs.System(err)
		}
		return handler(ctx, desc)
	}), target)
}

// presentChildrenHandler is a handler wrapper which traverses all children
// descriptors that are present in the store and calls specified handler.
func presentChildrenHandler(store content.Store, h c8dimages.HandlerFunc) c8dimages.HandlerFunc {
	return func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		_, err := store.Info(ctx, desc.Digest)
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				return nil, c8dimages.ErrSkipDesc
			}
			return nil, err
		}

		children, err := h(ctx, desc)
		if err != nil {
			return nil, err
		}

		c, err := c8dimages.Children(ctx, store, desc)
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				return nil, c8dimages.ErrSkipDesc
			}
			return nil, err
		}
		children = append(children, c...)

		return children, nil
	}
}
