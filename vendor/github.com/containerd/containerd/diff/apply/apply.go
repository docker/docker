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

package apply

import (
	"context"
	"io"
	"io/ioutil"
	"time"

	"github.com/containerd/containerd/archive"
	"github.com/containerd/containerd/archive/compression"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/diff"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// NewFileSystemApplier returns an applier which simply mounts
// and applies diff onto the mounted filesystem.
func NewFileSystemApplier(cs content.Provider) diff.Applier {
	return &fsApplier{
		store: cs,
	}
}

type fsApplier struct {
	store content.Provider
}

var emptyDesc = ocispec.Descriptor{}

// Apply applies the content associated with the provided digests onto the
// provided mounts. Archive content will be extracted and decompressed if
// necessary.
func (s *fsApplier) Apply(ctx context.Context, desc ocispec.Descriptor, mounts []mount.Mount) (d ocispec.Descriptor, err error) {
	t1 := time.Now()
	defer func() {
		if err == nil {
			log.G(ctx).WithFields(logrus.Fields{
				"d":     time.Since(t1),
				"dgst":  desc.Digest,
				"size":  desc.Size,
				"media": desc.MediaType,
			}).Debugf("diff applied")
		}
	}()

	isCompressed, err := images.IsCompressedDiff(ctx, desc.MediaType)
	if err != nil {
		return emptyDesc, errors.Wrapf(errdefs.ErrNotImplemented, "unsupported diff media type: %v", desc.MediaType)
	}

	var ocidesc ocispec.Descriptor
	if err := mount.WithTempMount(ctx, mounts, func(root string) error {
		ra, err := s.store.ReaderAt(ctx, desc)
		if err != nil {
			return errors.Wrap(err, "failed to get reader from content store")
		}
		defer ra.Close()

		r := content.NewReader(ra)
		if isCompressed {
			ds, err := compression.DecompressStream(r)
			if err != nil {
				return err
			}
			defer ds.Close()
			r = ds
		}

		digester := digest.Canonical.Digester()
		rc := &readCounter{
			r: io.TeeReader(r, digester.Hash()),
		}

		if _, err := archive.Apply(ctx, root, rc); err != nil {
			return err
		}

		// Read any trailing data
		if _, err := io.Copy(ioutil.Discard, rc); err != nil {
			return err
		}

		ocidesc = ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageLayer,
			Size:      rc.c,
			Digest:    digester.Digest(),
		}
		return nil

	}); err != nil {
		return emptyDesc, err
	}
	return ocidesc, nil
}

type readCounter struct {
	r io.Reader
	c int64
}

func (rc *readCounter) Read(p []byte) (n int, err error) {
	n, err = rc.r.Read(p)
	rc.c += int64(n)
	return
}
