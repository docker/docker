package containerd

import (
	"context"
	"time"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/log"
)

const (
	leaseExpireDuration = 8 * time.Hour
	expireLabel         = "containerd.io/gc.expire" // Copied from containerd
	pruneLeaseLabel     = "moby/prune.images"
	pruneLeaseFilter    = `labels."moby/prune.images"`
)

// withLease is used to prevent content or snapshots from being eligible for
// garbage collection while they are being used. Leases should always be
// released when complete to make the resources eligible again.
// If cancellable is set to true, then the lease will remain if the context
// is canceled until its expiration or deletion via prune.
func (i *ImageService) withLease(ctx context.Context, cancellable bool) (context.Context, func(), error) {
	_, ok := leases.FromContext(ctx)
	if ok {
		return ctx, func() {}, nil
	}

	ls := i.client.LeasesService()

	expireAt := time.Now().Add(leaseExpireDuration)
	l, err := ls.Create(ctx,
		leases.WithRandomID(),
		leases.WithLabels(map[string]string{
			pruneLeaseLabel: "true",
			expireLabel:     expireAt.Format(time.RFC3339),
		}))
	if err != nil {
		return ctx, func() {}, err
	}

	ctx = leases.WithLease(ctx, l.ID)

	// Track the lease as active so that ImagePrune does not delete it while
	// it is protecting content being written (e.g. an in-flight pull).
	i.trackLease(l.ID)

	return ctx, func() {
		// Mark the lease as inactive so it becomes eligible for pruning
		// once the operation that created it has finished.
		i.untrackLease(l.ID)

		if ctx.Err() != nil && cancellable {
			log.G(ctx).WithFields(log.Fields{"lease": l.ID, "expires_at": expireAt}).Info("Cancel with lease, leased resources will remain until expiration")
			return
		}
		if err := ls.Delete(context.WithoutCancel(ctx), l); err != nil {
			log.G(ctx).WithError(err).WithFields(log.Fields{"lease": l.ID, "expires_at": expireAt}).Warn("Error deleting lease, leased resources will remain until expiration")
		}
	}, nil
}
