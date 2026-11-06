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
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/archive"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// NewFileSystemApplier returns an applier which simply mounts
// and applies diff onto the mounted filesystem.
func NewFileSystemApplier(cs content.Provider) diff.Applier {
	return NewFileSystemApplierWithMountManager(cs, nil)
}

// NewFileSystemApplierWithMountManager returns an applier which simply mounts and
// applies diff onto the mounted filesystem.
// An optional mount manager can be specified and it will take effect when applying.
func NewFileSystemApplierWithMountManager(cs content.Provider, mm mount.Manager) diff.Applier {
	return &fsApplier{
		store: cs,
		mount: mm,
	}
}

type fsApplier struct {
	store content.Provider
	mount mount.Manager
}

var emptyDesc = ocispec.Descriptor{}

// Apply applies the content associated with the provided digests onto the
// provided mounts. Archive content will be extracted and decompressed if
// necessary.
func (s *fsApplier) Apply(ctx context.Context, desc ocispec.Descriptor, mounts []mount.Mount, opts ...diff.ApplyOpt) (d ocispec.Descriptor, err error) {
	t1 := time.Now()
	defer func() {
		if err == nil {
			log.G(ctx).WithFields(log.Fields{
				"d":      time.Since(t1),
				"digest": desc.Digest,
				"size":   desc.Size,
				"media":  desc.MediaType,
			}).Debugf("diff applied")
		}
	}()

	var config diff.ApplyConfig
	for _, o := range opts {
		if err := o(ctx, desc, &config); err != nil {
			return emptyDesc, fmt.Errorf("failed to apply config opt: %w", err)
		}
	}

	ra, err := s.store.ReaderAt(ctx, desc)
	if err != nil {
		return emptyDesc, fmt.Errorf("failed to get reader from content store: %w", err)
	}
	var r io.ReadCloser
	if config.Progress != nil {
		r = newProgressReader(ra, config.Progress)
	} else {
		r = newReadCloser(ra)
	}
	defer r.Close()

	var processors []diff.StreamProcessor
	processor := diff.NewProcessorChain(desc.MediaType, r)
	processors = append(processors, processor)
	for {
		if processor, err = diff.GetProcessor(ctx, processor, config.ProcessorPayloads); err != nil {
			return emptyDesc, fmt.Errorf("failed to get stream processor for %s: %w", desc.MediaType, err)
		}
		processors = append(processors, processor)
		if processor.MediaType() == ocispec.MediaTypeImageLayer {
			break
		}
	}
	defer processor.Close()

	digester := digest.Canonical.Digester()
	rc := &readCounter{
		r: io.TeeReader(processor, digester.Hash()),
	}

	// The number of `mounts` that need to be parsed by the mount manager
	// will be more than 1 in reality; this is needed to work around some
	// overlayfs/bind shortcuts in core/diff/apply/apply_linux.go
	if s.mount != nil && len(mounts) > 1 {
		var b [3]byte
		// Ignore read failures, just decreases uniqueness
		rand.Read(b[:])
		id := fmt.Sprintf("fs-diffapply-%d-%s", t1.Nanosecond(), base64.URLEncoding.EncodeToString(b[:]))
		info, err := s.mount.Activate(ctx, id, mounts)
		if err == nil {
			defer s.mount.Deactivate(ctx, id)
			mounts = info.System
		} else if !errdefs.IsNotImplemented(err) {
			return emptyDesc, fmt.Errorf("failed to activate mounts: %w", err)
		}
	}

	var applyOpts []archive.ApplyOpt
	if diff.HasIDMapping(config.UIDMap, config.GIDMap) {
		applyOpts = append(applyOpts, archive.WithApplyIDMapping(config.UIDMap, config.GIDMap))
	}
	if err := apply(ctx, mounts, rc, config.SyncFs, applyOpts...); err != nil {
		return emptyDesc, err
	}

	// Read any trailing data
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return emptyDesc, err
	}

	for _, p := range processors {
		if ep, ok := p.(interface {
			Err() error
		}); ok {
			if err := ep.Err(); err != nil {
				return emptyDesc, err
			}
		}
	}

	return ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayer,
		Size:      rc.c,
		Digest:    digester.Digest(),
	}, nil
}

type readCounter struct {
	r io.Reader
	c int64
}

func (rc *readCounter) Read(p []byte) (n int, err error) {
	n, err = rc.r.Read(p)
	if n > 0 {
		rc.c += int64(n)
	}
	return
}

type progressReader struct {
	rc *readCounter
	c  io.Closer
	p  func(int64)
}

func newProgressReader(ra content.ReaderAt, p func(int64)) io.ReadCloser {
	return &progressReader{
		rc: &readCounter{
			r: content.NewReader(ra),
			c: 0,
		},
		c: ra,
		p: p,
	}
}

func (pr *progressReader) Read(p []byte) (n int, err error) {
	// Call the progress function with the current count, indicating
	// the previously read content has been processed. Initial
	// progress of 0 indicates start of processing.
	pr.p(pr.rc.c)
	n, err = pr.rc.Read(p)
	return
}

func (pr *progressReader) Close() error {
	pr.p(pr.rc.c)
	return pr.c.Close()
}

type readCloser struct {
	io.Reader
	io.Closer
}

func newReadCloser(ra content.ReaderAt) io.ReadCloser {
	return &readCloser{
		Reader: content.NewReader(ra),
		Closer: ra,
	}
}
