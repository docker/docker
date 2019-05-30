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

// Package metadata stores all labels and object specific metadata by namespace.
// This package also contains the main garbage collection logic  for cleaning up
// resources consistently and atomically. Resources used by backends will be
// tracked in the metadata store to be exposed to consumers of this package.
//
// The layout where a "/" delineates a bucket is described in the following
// section. Please try to follow this as closely as possible when adding
// functionality. We can bolster this with helpers and more structure if that
// becomes an issue.
//
// Generically, we try to do the following:
//
// 	<version>/<namespace>/<object>/<key> -> <field>
//
// version: Currently, this is "v1". Additions can be made to v1 in a backwards
// compatible way. If the layout changes, a new version must be made, along
// with a migration.
//
// namespace: the namespace to which this object belongs.
//
// object: defines which object set is stored in the bucket. There are two
// special objects, "labels" and "indexes". The "labels" bucket stores the
// labels for the parent namespace. The "indexes" object is reserved for
// indexing objects, if we require in the future.
//
// key: object-specific key identifying the storage bucket for the objects
// contents.
//
// Below is the current database schema. This should be updated each time
// the structure is changed in addition to adding a migration and incrementing
// the database version. Note that `╘══*...*` refers to maps with arbitrary
// keys.
//  ├──version : <varint>                        - Latest version, see migrations
//  └──v1                                        - Schema version bucket
//     ╘══*namespace*
//        ├──labels
//        │  ╘══*key* : <string>                 - Label value
//        ├──image
//        │  ╘══*image name*
//        │     ├──createdat : <binary time>     - Created at
//        │     ├──updatedat : <binary time>     - Updated at
//        │     ├──target
//        │     │  ├──digest : <digest>          - Descriptor digest
//        │     │  ├──mediatype : <string>       - Descriptor media type
//        │     │  └──size : <varint>            - Descriptor size
//        │     └──labels
//        │        ╘══*key* : <string>           - Label value
//        ├──containers
//        │  ╘══*container id*
//        │     ├──createdat : <binary time>     - Created at
//        │     ├──updatedat : <binary time>     - Updated at
//        │     ├──spec : <binary>               - Proto marshaled spec
//        │     ├──image : <string>              - Image name
//        │     ├──snapshotter : <string>        - Snapshotter name
//        │     ├──snapshotKey : <string>        - Snapshot key
//        │     ├──runtime
//        │     │  ├──name : <string>            - Runtime name
//        │     │  ├──extensions
//        │     │  │  ╘══*name* : <binary>       - Proto marshaled extension
//        │     │  └──options : <binary>         - Proto marshaled options
//        │     └──labels
//        │        ╘══*key* : <string>           - Label value
//        ├──snapshots
//        │  ╘══*snapshotter*
//        │     ╘══*snapshot key*
//        │        ├──name : <string>            - Snapshot name in backend
//        │        ├──createdat : <binary time>  - Created at
//        │        ├──updatedat : <binary time>  - Updated at
//        │        ├──parent : <string>          - Parent snapshot name
//        │        ├──children
//        │        │  ╘══*snapshot key* : <nil>  - Child snapshot reference
//        │        └──labels
//        │           ╘══*key* : <string>        - Label value
//        ├──content
//        │  ├──blob
//        │  │  ╘══*blob digest*
//        │  │     ├──createdat : <binary time>  - Created at
//        │  │     ├──updatedat : <binary time>  - Updated at
//        │  │     ├──size : <varint>            - Blob size
//        │  │     └──labels
//        │  │        ╘══*key* : <string>        - Label value
//        │  └──ingests
//        │     ╘══*ingest reference*
//        │        ├──ref : <string>             - Ingest reference in backend
//        │        ├──expireat : <binary time>   - Time to expire ingest
//        │        └──expected : <digest>        - Expected commit digest
//        └──leases
//           ╘══*lease id*
//              ├──createdat : <binary time>     - Created at
//              ├──labels
//              │  ╘══*key* : <string>           - Label value
//              ├──snapshots
//              │  ╘══*snapshotter*
//              │     ╘══*snapshot key* : <nil>  - Snapshot reference
//              ├──content
//              │  ╘══*blob digest* : <nil>      - Content blob reference
//              └──ingests
//                 ╘══*ingest reference* : <nil> - Content ingest reference
package metadata

import (
	digest "github.com/opencontainers/go-digest"
	bolt "go.etcd.io/bbolt"
)

var (
	bucketKeyVersion          = []byte(schemaVersion)
	bucketKeyDBVersion        = []byte("version")    // stores the version of the schema
	bucketKeyObjectLabels     = []byte("labels")     // stores the labels for a namespace.
	bucketKeyObjectImages     = []byte("images")     // stores image objects
	bucketKeyObjectContainers = []byte("containers") // stores container objects
	bucketKeyObjectSnapshots  = []byte("snapshots")  // stores snapshot references
	bucketKeyObjectContent    = []byte("content")    // stores content references
	bucketKeyObjectBlob       = []byte("blob")       // stores content links
	bucketKeyObjectIngests    = []byte("ingests")    // stores ingest objects
	bucketKeyObjectLeases     = []byte("leases")     // stores leases

	bucketKeyDigest      = []byte("digest")
	bucketKeyMediaType   = []byte("mediatype")
	bucketKeySize        = []byte("size")
	bucketKeyImage       = []byte("image")
	bucketKeyRuntime     = []byte("runtime")
	bucketKeyName        = []byte("name")
	bucketKeyParent      = []byte("parent")
	bucketKeyChildren    = []byte("children")
	bucketKeyOptions     = []byte("options")
	bucketKeySpec        = []byte("spec")
	bucketKeySnapshotKey = []byte("snapshotKey")
	bucketKeySnapshotter = []byte("snapshotter")
	bucketKeyTarget      = []byte("target")
	bucketKeyExtensions  = []byte("extensions")
	bucketKeyCreatedAt   = []byte("createdat")
	bucketKeyExpected    = []byte("expected")
	bucketKeyRef         = []byte("ref")
	bucketKeyExpireAt    = []byte("expireat")

	deprecatedBucketKeyObjectIngest = []byte("ingest") // stores ingest links, deprecated in v1.2
)

func getBucket(tx *bolt.Tx, keys ...[]byte) *bolt.Bucket {
	bkt := tx.Bucket(keys[0])

	for _, key := range keys[1:] {
		if bkt == nil {
			break
		}
		bkt = bkt.Bucket(key)
	}

	return bkt
}

func createBucketIfNotExists(tx *bolt.Tx, keys ...[]byte) (*bolt.Bucket, error) {
	bkt, err := tx.CreateBucketIfNotExists(keys[0])
	if err != nil {
		return nil, err
	}

	for _, key := range keys[1:] {
		bkt, err = bkt.CreateBucketIfNotExists(key)
		if err != nil {
			return nil, err
		}
	}

	return bkt, nil
}

func namespaceLabelsBucketPath(namespace string) [][]byte {
	return [][]byte{bucketKeyVersion, []byte(namespace), bucketKeyObjectLabels}
}

func withNamespacesLabelsBucket(tx *bolt.Tx, namespace string, fn func(bkt *bolt.Bucket) error) error {
	bkt, err := createBucketIfNotExists(tx, namespaceLabelsBucketPath(namespace)...)
	if err != nil {
		return err
	}

	return fn(bkt)
}

func getNamespaceLabelsBucket(tx *bolt.Tx, namespace string) *bolt.Bucket {
	return getBucket(tx, namespaceLabelsBucketPath(namespace)...)
}

func imagesBucketPath(namespace string) [][]byte {
	return [][]byte{bucketKeyVersion, []byte(namespace), bucketKeyObjectImages}
}

func createImagesBucket(tx *bolt.Tx, namespace string) (*bolt.Bucket, error) {
	return createBucketIfNotExists(tx, imagesBucketPath(namespace)...)
}

func getImagesBucket(tx *bolt.Tx, namespace string) *bolt.Bucket {
	return getBucket(tx, imagesBucketPath(namespace)...)
}

func createContainersBucket(tx *bolt.Tx, namespace string) (*bolt.Bucket, error) {
	return createBucketIfNotExists(tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectContainers)
}

func getContainersBucket(tx *bolt.Tx, namespace string) *bolt.Bucket {
	return getBucket(tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectContainers)
}

func getContainerBucket(tx *bolt.Tx, namespace, id string) *bolt.Bucket {
	return getBucket(tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectContainers, []byte(id))
}

func createSnapshotterBucket(tx *bolt.Tx, namespace, snapshotter string) (*bolt.Bucket, error) {
	bkt, err := createBucketIfNotExists(tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectSnapshots, []byte(snapshotter))
	if err != nil {
		return nil, err
	}
	return bkt, nil
}

func getSnapshottersBucket(tx *bolt.Tx, namespace string) *bolt.Bucket {
	return getBucket(tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectSnapshots)
}

func getSnapshotterBucket(tx *bolt.Tx, namespace, snapshotter string) *bolt.Bucket {
	return getBucket(tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectSnapshots, []byte(snapshotter))
}

func createBlobBucket(tx *bolt.Tx, namespace string, dgst digest.Digest) (*bolt.Bucket, error) {
	bkt, err := createBucketIfNotExists(tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectContent, bucketKeyObjectBlob)
	if err != nil {
		return nil, err
	}
	return bkt.CreateBucket([]byte(dgst.String()))
}

func getBlobsBucket(tx *bolt.Tx, namespace string) *bolt.Bucket {
	return getBucket(tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectContent, bucketKeyObjectBlob)
}

func getBlobBucket(tx *bolt.Tx, namespace string, dgst digest.Digest) *bolt.Bucket {
	return getBucket(tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectContent, bucketKeyObjectBlob, []byte(dgst.String()))
}

func getIngestsBucket(tx *bolt.Tx, namespace string) *bolt.Bucket {
	return getBucket(tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectContent, bucketKeyObjectIngests)
}

func createIngestBucket(tx *bolt.Tx, namespace, ref string) (*bolt.Bucket, error) {
	bkt, err := createBucketIfNotExists(tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectContent, bucketKeyObjectIngests, []byte(ref))
	if err != nil {
		return nil, err
	}
	return bkt, nil
}

func getIngestBucket(tx *bolt.Tx, namespace, ref string) *bolt.Bucket {
	return getBucket(tx, bucketKeyVersion, []byte(namespace), bucketKeyObjectContent, bucketKeyObjectIngests, []byte(ref))
}
