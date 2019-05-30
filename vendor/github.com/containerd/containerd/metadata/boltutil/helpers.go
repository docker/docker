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

package boltutil

import (
	"time"

	"github.com/pkg/errors"
	bolt "go.etcd.io/bbolt"
)

var (
	bucketKeyAnnotations = []byte("annotations")
	bucketKeyLabels      = []byte("labels")
	bucketKeyCreatedAt   = []byte("createdat")
	bucketKeyUpdatedAt   = []byte("updatedat")
)

// ReadLabels reads the labels key from the bucket
// Uses the key "labels"
func ReadLabels(bkt *bolt.Bucket) (map[string]string, error) {
	return readMap(bkt, bucketKeyLabels)
}

// ReadAnnotations reads the OCI Descriptor Annotations key from the bucket
// Uses the key "annotations"
func ReadAnnotations(bkt *bolt.Bucket) (map[string]string, error) {
	return readMap(bkt, bucketKeyAnnotations)
}

func readMap(bkt *bolt.Bucket, bucketName []byte) (map[string]string, error) {
	lbkt := bkt.Bucket(bucketName)
	if lbkt == nil {
		return nil, nil
	}
	labels := map[string]string{}
	if err := lbkt.ForEach(func(k, v []byte) error {
		labels[string(k)] = string(v)
		return nil
	}); err != nil {
		return nil, err
	}
	return labels, nil
}

// WriteLabels will write a new labels bucket to the provided bucket at key
// bucketKeyLabels, replacing the contents of the bucket with the provided map.
//
// The provide map labels will be modified to have the final contents of the
// bucket. Typically, this removes zero-value entries.
// Uses the key "labels"
func WriteLabels(bkt *bolt.Bucket, labels map[string]string) error {
	return writeMap(bkt, bucketKeyLabels, labels)
}

// WriteAnnotations writes the OCI Descriptor Annotations
func WriteAnnotations(bkt *bolt.Bucket, labels map[string]string) error {
	return writeMap(bkt, bucketKeyAnnotations, labels)
}

func writeMap(bkt *bolt.Bucket, bucketName []byte, labels map[string]string) error {
	// Remove existing labels to keep from merging
	if lbkt := bkt.Bucket(bucketName); lbkt != nil {
		if err := bkt.DeleteBucket(bucketName); err != nil {
			return err
		}
	}

	if len(labels) == 0 {
		return nil
	}

	lbkt, err := bkt.CreateBucket(bucketName)
	if err != nil {
		return err
	}

	for k, v := range labels {
		if v == "" {
			delete(labels, k) // remove since we don't actually set it
			continue
		}

		if err := lbkt.Put([]byte(k), []byte(v)); err != nil {
			return errors.Wrapf(err, "failed to set label %q=%q", k, v)
		}
	}

	return nil
}

// ReadTimestamps reads created and updated timestamps from a bucket.
// Uses keys "createdat" and "updatedat"
func ReadTimestamps(bkt *bolt.Bucket, created, updated *time.Time) error {
	for _, f := range []struct {
		b []byte
		t *time.Time
	}{
		{bucketKeyCreatedAt, created},
		{bucketKeyUpdatedAt, updated},
	} {
		v := bkt.Get(f.b)
		if v != nil {
			if err := f.t.UnmarshalBinary(v); err != nil {
				return err
			}
		}
	}
	return nil
}

// WriteTimestamps writes created and updated timestamps to a bucket.
// Uses keys "createdat" and "updatedat"
func WriteTimestamps(bkt *bolt.Bucket, created, updated time.Time) error {
	createdAt, err := created.MarshalBinary()
	if err != nil {
		return err
	}
	updatedAt, err := updated.MarshalBinary()
	if err != nil {
		return err
	}
	for _, v := range [][2][]byte{
		{bucketKeyCreatedAt, createdAt},
		{bucketKeyUpdatedAt, updatedAt},
	} {
		if err := bkt.Put(v[0], v[1]); err != nil {
			return err
		}
	}

	return nil
}
