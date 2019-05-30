/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package metadata

import (
	"context"
	"time"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/filters"
	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/metadata/boltutil"
	"github.com/containerd/containerd/namespaces"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	bolt "go.etcd.io/bbolt"
)

// LeaseManager manages the create/delete lifecyle of leases
// and also returns existing leases
type LeaseManager struct {
	tx *bolt.Tx
}

// NewLeaseManager creates a new lease manager for managing leases using
// the provided database transaction.
func NewLeaseManager(tx *bolt.Tx) *LeaseManager {
	return &LeaseManager{
		tx: tx,
	}
}

// Create creates a new lease using the provided lease
func (lm *LeaseManager) Create(ctx context.Context, opts ...leases.Opt) (leases.Lease, error) {
	var l leases.Lease
	for _, opt := range opts {
		if err := opt(&l); err != nil {
			return leases.Lease{}, err
		}
	}
	if l.ID == "" {
		return leases.Lease{}, errors.New("lease id must be provided")
	}

	namespace, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return leases.Lease{}, err
	}

	topbkt, err := createBucketIfNotExists(lm.tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectLeases)
	if err != nil {
		return leases.Lease{}, err
	}

	txbkt, err := topbkt.CreateBucket([]byte(l.ID))
	if err != nil {
		if err == bolt.ErrBucketExists {
			err = errdefs.ErrAlreadyExists
		}
		return leases.Lease{}, errors.Wrapf(err, "lease %q", l.ID)
	}

	t := time.Now().UTC()
	createdAt, err := t.MarshalBinary()
	if err != nil {
		return leases.Lease{}, err
	}
	if err := txbkt.Put(bucketKeyCreatedAt, createdAt); err != nil {
		return leases.Lease{}, err
	}

	if l.Labels != nil {
		if err := boltutil.WriteLabels(txbkt, l.Labels); err != nil {
			return leases.Lease{}, err
		}
	}
	l.CreatedAt = t

	return l, nil
}

// Delete delets the lease with the provided lease ID
func (lm *LeaseManager) Delete(ctx context.Context, lease leases.Lease, _ ...leases.DeleteOpt) error {
	namespace, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	topbkt := getBucket(lm.tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectLeases)
	if topbkt == nil {
		return errors.Wrapf(errdefs.ErrNotFound, "lease %q", lease.ID)
	}
	if err := topbkt.DeleteBucket([]byte(lease.ID)); err != nil {
		if err == bolt.ErrBucketNotFound {
			err = errors.Wrapf(errdefs.ErrNotFound, "lease %q", lease.ID)
		}
		return err
	}
	return nil
}

// List lists all active leases
func (lm *LeaseManager) List(ctx context.Context, fs ...string) ([]leases.Lease, error) {
	namespace, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	filter, err := filters.ParseAll(fs...)
	if err != nil {
		return nil, errors.Wrap(errdefs.ErrInvalidArgument, err.Error())
	}

	var ll []leases.Lease

	topbkt := getBucket(lm.tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectLeases)
	if topbkt == nil {
		return ll, nil
	}

	if err := topbkt.ForEach(func(k, v []byte) error {
		if v != nil {
			return nil
		}
		txbkt := topbkt.Bucket(k)

		l := leases.Lease{
			ID: string(k),
		}

		if v := txbkt.Get(bucketKeyCreatedAt); v != nil {
			t := &l.CreatedAt
			if err := t.UnmarshalBinary(v); err != nil {
				return err
			}
		}

		labels, err := boltutil.ReadLabels(txbkt)
		if err != nil {
			return err
		}
		l.Labels = labels

		if filter.Match(adaptLease(l)) {
			ll = append(ll, l)
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return ll, nil
}

func addSnapshotLease(ctx context.Context, tx *bolt.Tx, snapshotter, key string) error {
	lid, ok := leases.FromContext(ctx)
	if !ok {
		return nil
	}

	namespace, ok := namespaces.Namespace(ctx)
	if !ok {
		panic("namespace must already be checked")
	}

	bkt := getBucket(tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectLeases, []byte(lid))
	if bkt == nil {
		return errors.Wrap(errdefs.ErrNotFound, "lease does not exist")
	}

	bkt, err := bkt.CreateBucketIfNotExists(bucketKeyObjectSnapshots)
	if err != nil {
		return err
	}

	bkt, err = bkt.CreateBucketIfNotExists([]byte(snapshotter))
	if err != nil {
		return err
	}

	return bkt.Put([]byte(key), nil)
}

func removeSnapshotLease(ctx context.Context, tx *bolt.Tx, snapshotter, key string) error {
	lid, ok := leases.FromContext(ctx)
	if !ok {
		return nil
	}

	namespace, ok := namespaces.Namespace(ctx)
	if !ok {
		panic("namespace must already be checked")
	}

	bkt := getBucket(tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectLeases, []byte(lid), bucketKeyObjectSnapshots, []byte(snapshotter))
	if bkt == nil {
		// Key does not exist so we return nil
		return nil
	}

	return bkt.Delete([]byte(key))
}

func addContentLease(ctx context.Context, tx *bolt.Tx, dgst digest.Digest) error {
	lid, ok := leases.FromContext(ctx)
	if !ok {
		return nil
	}

	namespace, ok := namespaces.Namespace(ctx)
	if !ok {
		panic("namespace must already be required")
	}

	bkt := getBucket(tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectLeases, []byte(lid))
	if bkt == nil {
		return errors.Wrap(errdefs.ErrNotFound, "lease does not exist")
	}

	bkt, err := bkt.CreateBucketIfNotExists(bucketKeyObjectContent)
	if err != nil {
		return err
	}

	return bkt.Put([]byte(dgst.String()), nil)
}

func removeContentLease(ctx context.Context, tx *bolt.Tx, dgst digest.Digest) error {
	lid, ok := leases.FromContext(ctx)
	if !ok {
		return nil
	}

	namespace, ok := namespaces.Namespace(ctx)
	if !ok {
		panic("namespace must already be checked")
	}

	bkt := getBucket(tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectLeases, []byte(lid), bucketKeyObjectContent)
	if bkt == nil {
		// Key does not exist so we return nil
		return nil
	}

	return bkt.Delete([]byte(dgst.String()))
}

func addIngestLease(ctx context.Context, tx *bolt.Tx, ref string) (bool, error) {
	lid, ok := leases.FromContext(ctx)
	if !ok {
		return false, nil
	}

	namespace, ok := namespaces.Namespace(ctx)
	if !ok {
		panic("namespace must already be required")
	}

	bkt := getBucket(tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectLeases, []byte(lid))
	if bkt == nil {
		return false, errors.Wrap(errdefs.ErrNotFound, "lease does not exist")
	}

	bkt, err := bkt.CreateBucketIfNotExists(bucketKeyObjectIngests)
	if err != nil {
		return false, err
	}

	if err := bkt.Put([]byte(ref), nil); err != nil {
		return false, err
	}

	return true, nil
}

func removeIngestLease(ctx context.Context, tx *bolt.Tx, ref string) error {
	lid, ok := leases.FromContext(ctx)
	if !ok {
		return nil
	}

	namespace, ok := namespaces.Namespace(ctx)
	if !ok {
		panic("namespace must already be checked")
	}

	bkt := getBucket(tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectLeases, []byte(lid), bucketKeyObjectIngests)
	if bkt == nil {
		// Key does not exist so we return nil
		return nil
	}

	return bkt.Delete([]byte(ref))
}
