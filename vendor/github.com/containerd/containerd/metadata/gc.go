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
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/containerd/containerd/gc"
	"github.com/containerd/containerd/log"
	"github.com/pkg/errors"
	bolt "go.etcd.io/bbolt"
)

const (
	// ResourceUnknown specifies an unknown resource
	ResourceUnknown gc.ResourceType = iota
	// ResourceContent specifies a content resource
	ResourceContent
	// ResourceSnapshot specifies a snapshot resource
	ResourceSnapshot
	// ResourceContainer specifies a container resource
	ResourceContainer
	// ResourceTask specifies a task resource
	ResourceTask
	// ResourceLease specifies a lease
	ResourceLease
	// ResourceIngest specifies a content ingest
	ResourceIngest
)

var (
	labelGCRoot       = []byte("containerd.io/gc.root")
	labelGCSnapRef    = []byte("containerd.io/gc.ref.snapshot.")
	labelGCContentRef = []byte("containerd.io/gc.ref.content")
	labelGCExpire     = []byte("containerd.io/gc.expire")
)

func scanRoots(ctx context.Context, tx *bolt.Tx, nc chan<- gc.Node) error {
	v1bkt := tx.Bucket(bucketKeyVersion)
	if v1bkt == nil {
		return nil
	}

	expThreshold := time.Now()

	// iterate through each namespace
	v1c := v1bkt.Cursor()

	// cerr indicates the scan did not successfully send all
	// the roots. The scan does not need to be cancelled but
	// must return error at the end.
	var cerr error
	fn := func(n gc.Node) {
		select {
		case nc <- n:
		case <-ctx.Done():
			cerr = ctx.Err()
		}
	}

	for k, v := v1c.First(); k != nil; k, v = v1c.Next() {
		if v != nil {
			continue
		}
		nbkt := v1bkt.Bucket(k)
		ns := string(k)

		lbkt := nbkt.Bucket(bucketKeyObjectLeases)
		if lbkt != nil {
			if err := lbkt.ForEach(func(k, v []byte) error {
				if v != nil {
					return nil
				}
				libkt := lbkt.Bucket(k)

				if lblbkt := libkt.Bucket(bucketKeyObjectLabels); lblbkt != nil {
					if expV := lblbkt.Get(labelGCExpire); expV != nil {
						exp, err := time.Parse(time.RFC3339, string(expV))
						if err != nil {
							// label not used, log and continue to use lease
							log.G(ctx).WithError(err).WithField("lease", string(k)).Infof("ignoring invalid expiration value %q", string(expV))
						} else if expThreshold.After(exp) {
							// lease has expired, skip
							return nil
						}
					}
				}

				fn(gcnode(ResourceLease, ns, string(k)))

				// Emit content and snapshots as roots instead of implementing
				// in references. Since leases cannot be referenced there is
				// no need to allow the lookup to be recursive, handling here
				// therefore reduces the number of database seeks.

				cbkt := libkt.Bucket(bucketKeyObjectContent)
				if cbkt != nil {
					if err := cbkt.ForEach(func(k, v []byte) error {
						fn(gcnode(ResourceContent, ns, string(k)))
						return nil
					}); err != nil {
						return err
					}
				}

				sbkt := libkt.Bucket(bucketKeyObjectSnapshots)
				if sbkt != nil {
					if err := sbkt.ForEach(func(sk, sv []byte) error {
						if sv != nil {
							return nil
						}
						snbkt := sbkt.Bucket(sk)

						return snbkt.ForEach(func(k, v []byte) error {
							fn(gcnode(ResourceSnapshot, ns, fmt.Sprintf("%s/%s", sk, k)))
							return nil
						})
					}); err != nil {
						return err
					}
				}

				ibkt := libkt.Bucket(bucketKeyObjectIngests)
				if ibkt != nil {
					if err := ibkt.ForEach(func(k, v []byte) error {
						fn(gcnode(ResourceIngest, ns, string(k)))
						return nil
					}); err != nil {
						return err
					}
				}

				return nil
			}); err != nil {
				return err
			}
		}

		ibkt := nbkt.Bucket(bucketKeyObjectImages)
		if ibkt != nil {
			if err := ibkt.ForEach(func(k, v []byte) error {
				if v != nil {
					return nil
				}

				target := ibkt.Bucket(k).Bucket(bucketKeyTarget)
				if target != nil {
					contentKey := string(target.Get(bucketKeyDigest))
					fn(gcnode(ResourceContent, ns, contentKey))
				}
				return sendLabelRefs(ns, ibkt.Bucket(k), fn)
			}); err != nil {
				return err
			}
		}

		cbkt := nbkt.Bucket(bucketKeyObjectContent)
		if cbkt != nil {
			ibkt := cbkt.Bucket(bucketKeyObjectIngests)
			if ibkt != nil {
				if err := ibkt.ForEach(func(k, v []byte) error {
					if v != nil {
						return nil
					}
					ea, err := readExpireAt(ibkt.Bucket(k))
					if err != nil {
						return err
					}
					if ea == nil || expThreshold.After(*ea) {
						return nil
					}
					fn(gcnode(ResourceIngest, ns, string(k)))
					return nil
				}); err != nil {
					return err
				}
			}
			cbkt = cbkt.Bucket(bucketKeyObjectBlob)
			if cbkt != nil {
				if err := cbkt.ForEach(func(k, v []byte) error {
					if v != nil {
						return nil
					}

					if isRootRef(cbkt.Bucket(k)) {
						fn(gcnode(ResourceContent, ns, string(k)))
					}

					return nil
				}); err != nil {
					return err
				}
			}
		}

		cbkt = nbkt.Bucket(bucketKeyObjectContainers)
		if cbkt != nil {
			if err := cbkt.ForEach(func(k, v []byte) error {
				if v != nil {
					return nil
				}

				cibkt := cbkt.Bucket(k)
				snapshotter := string(cibkt.Get(bucketKeySnapshotter))
				if snapshotter != "" {
					ss := string(cibkt.Get(bucketKeySnapshotKey))
					fn(gcnode(ResourceSnapshot, ns, fmt.Sprintf("%s/%s", snapshotter, ss)))
				}

				return sendLabelRefs(ns, cibkt, fn)
			}); err != nil {
				return err
			}
		}

		sbkt := nbkt.Bucket(bucketKeyObjectSnapshots)
		if sbkt != nil {
			if err := sbkt.ForEach(func(sk, sv []byte) error {
				if sv != nil {
					return nil
				}
				snbkt := sbkt.Bucket(sk)

				return snbkt.ForEach(func(k, v []byte) error {
					if v != nil {
						return nil
					}
					if isRootRef(snbkt.Bucket(k)) {
						fn(gcnode(ResourceSnapshot, ns, fmt.Sprintf("%s/%s", sk, k)))
					}
					return nil
				})
			}); err != nil {
				return err
			}
		}
	}
	return cerr
}

func references(ctx context.Context, tx *bolt.Tx, node gc.Node, fn func(gc.Node)) error {
	if node.Type == ResourceContent {
		bkt := getBucket(tx, bucketKeyVersion, []byte(node.Namespace), bucketKeyObjectContent, bucketKeyObjectBlob, []byte(node.Key))
		if bkt == nil {
			// Node may be created from dead edge
			return nil
		}

		return sendLabelRefs(node.Namespace, bkt, fn)
	} else if node.Type == ResourceSnapshot {
		parts := strings.SplitN(node.Key, "/", 2)
		if len(parts) != 2 {
			return errors.Errorf("invalid snapshot gc key %s", node.Key)
		}
		ss := parts[0]
		name := parts[1]

		bkt := getBucket(tx, bucketKeyVersion, []byte(node.Namespace), bucketKeyObjectSnapshots, []byte(ss), []byte(name))
		if bkt == nil {
			// Node may be created from dead edge
			return nil
		}

		if pv := bkt.Get(bucketKeyParent); len(pv) > 0 {
			fn(gcnode(ResourceSnapshot, node.Namespace, fmt.Sprintf("%s/%s", ss, pv)))
		}

		return sendLabelRefs(node.Namespace, bkt, fn)
	} else if node.Type == ResourceIngest {
		// Send expected value
		bkt := getBucket(tx, bucketKeyVersion, []byte(node.Namespace), bucketKeyObjectContent, bucketKeyObjectIngests, []byte(node.Key))
		if bkt == nil {
			// Node may be created from dead edge
			return nil
		}
		// Load expected
		expected := bkt.Get(bucketKeyExpected)
		if len(expected) > 0 {
			fn(gcnode(ResourceContent, node.Namespace, string(expected)))
		}
		return nil
	}

	return nil
}

func scanAll(ctx context.Context, tx *bolt.Tx, fn func(ctx context.Context, n gc.Node) error) error {
	v1bkt := tx.Bucket(bucketKeyVersion)
	if v1bkt == nil {
		return nil
	}

	// iterate through each namespace
	v1c := v1bkt.Cursor()

	for k, v := v1c.First(); k != nil; k, v = v1c.Next() {
		if v != nil {
			continue
		}
		nbkt := v1bkt.Bucket(k)
		ns := string(k)

		lbkt := nbkt.Bucket(bucketKeyObjectLeases)
		if lbkt != nil {
			if err := lbkt.ForEach(func(k, v []byte) error {
				if v != nil {
					return nil
				}
				return fn(ctx, gcnode(ResourceLease, ns, string(k)))
			}); err != nil {
				return err
			}
		}

		sbkt := nbkt.Bucket(bucketKeyObjectSnapshots)
		if sbkt != nil {
			if err := sbkt.ForEach(func(sk, sv []byte) error {
				if sv != nil {
					return nil
				}
				snbkt := sbkt.Bucket(sk)
				return snbkt.ForEach(func(k, v []byte) error {
					if v != nil {
						return nil
					}
					node := gcnode(ResourceSnapshot, ns, fmt.Sprintf("%s/%s", sk, k))
					return fn(ctx, node)
				})
			}); err != nil {
				return err
			}
		}

		cbkt := nbkt.Bucket(bucketKeyObjectContent)
		if cbkt != nil {
			ibkt := cbkt.Bucket(bucketKeyObjectIngests)
			if ibkt != nil {
				if err := ibkt.ForEach(func(k, v []byte) error {
					if v != nil {
						return nil
					}
					node := gcnode(ResourceIngest, ns, string(k))
					return fn(ctx, node)
				}); err != nil {
					return err
				}
			}

			cbkt = cbkt.Bucket(bucketKeyObjectBlob)
			if cbkt != nil {
				if err := cbkt.ForEach(func(k, v []byte) error {
					if v != nil {
						return nil
					}
					node := gcnode(ResourceContent, ns, string(k))
					return fn(ctx, node)
				}); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func remove(ctx context.Context, tx *bolt.Tx, node gc.Node) error {
	v1bkt := tx.Bucket(bucketKeyVersion)
	if v1bkt == nil {
		return nil
	}

	nsbkt := v1bkt.Bucket([]byte(node.Namespace))
	if nsbkt == nil {
		return nil
	}

	switch node.Type {
	case ResourceContent:
		cbkt := nsbkt.Bucket(bucketKeyObjectContent)
		if cbkt != nil {
			cbkt = cbkt.Bucket(bucketKeyObjectBlob)
		}
		if cbkt != nil {
			log.G(ctx).WithField("key", node.Key).Debug("remove content")
			return cbkt.DeleteBucket([]byte(node.Key))
		}
	case ResourceSnapshot:
		sbkt := nsbkt.Bucket(bucketKeyObjectSnapshots)
		if sbkt != nil {
			parts := strings.SplitN(node.Key, "/", 2)
			if len(parts) != 2 {
				return errors.Errorf("invalid snapshot gc key %s", node.Key)
			}
			ssbkt := sbkt.Bucket([]byte(parts[0]))
			if ssbkt != nil {
				log.G(ctx).WithField("key", parts[1]).WithField("snapshotter", parts[0]).Debug("remove snapshot")
				return ssbkt.DeleteBucket([]byte(parts[1]))
			}
		}
	case ResourceLease:
		lbkt := nsbkt.Bucket(bucketKeyObjectLeases)
		if lbkt != nil {
			return lbkt.DeleteBucket([]byte(node.Key))
		}
	case ResourceIngest:
		ibkt := nsbkt.Bucket(bucketKeyObjectContent)
		if ibkt != nil {
			ibkt = ibkt.Bucket(bucketKeyObjectIngests)
		}
		if ibkt != nil {
			log.G(ctx).WithField("ref", node.Key).Debug("remove ingest")
			return ibkt.DeleteBucket([]byte(node.Key))
		}
	}

	return nil
}

// sendLabelRefs sends all snapshot and content references referred to by the labels in the bkt
func sendLabelRefs(ns string, bkt *bolt.Bucket, fn func(gc.Node)) error {
	lbkt := bkt.Bucket(bucketKeyObjectLabels)
	if lbkt != nil {
		lc := lbkt.Cursor()

		labelRef := string(labelGCContentRef)
		for k, v := lc.Seek(labelGCContentRef); k != nil && strings.HasPrefix(string(k), labelRef); k, v = lc.Next() {
			if ks := string(k); ks != labelRef {
				// Allow reference naming separated by . or /, ignore names
				if ks[len(labelRef)] != '.' && ks[len(labelRef)] != '/' {
					continue
				}
			}

			fn(gcnode(ResourceContent, ns, string(v)))
		}

		for k, v := lc.Seek(labelGCSnapRef); k != nil && strings.HasPrefix(string(k), string(labelGCSnapRef)); k, v = lc.Next() {
			snapshotter := k[len(labelGCSnapRef):]
			if i := bytes.IndexByte(snapshotter, '/'); i >= 0 {
				snapshotter = snapshotter[:i]
			}
			fn(gcnode(ResourceSnapshot, ns, fmt.Sprintf("%s/%s", snapshotter, v)))
		}

	}
	return nil
}

func isRootRef(bkt *bolt.Bucket) bool {
	lbkt := bkt.Bucket(bucketKeyObjectLabels)
	if lbkt != nil {
		rv := lbkt.Get(labelGCRoot)
		if rv != nil {
			// TODO: interpret rv as a timestamp and skip if expired
			return true
		}
	}
	return false
}

func gcnode(t gc.ResourceType, ns, key string) gc.Node {
	return gc.Node{
		Type:      t,
		Namespace: ns,
		Key:       key,
	}
}
