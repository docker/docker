//go:build linux

package cache

import (
	"bufio"
	"context"
	"io"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/archive"
	"github.com/containerd/containerd/v2/pkg/epoch"
	labelspkg "github.com/containerd/containerd/v2/pkg/labels"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/compression"
	"github.com/moby/buildkit/util/overlay"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

var emptyDesc = ocispecs.Descriptor{}

// computeOverlayBlob provides overlayfs-specialized method to compute
// diff between lower and upper snapshot. If the passed mounts cannot
// be computed (e.g. because the mounts aren't overlayfs), it returns
// an error.
func (sr *immutableRef) tryComputeOverlayBlob(ctx context.Context, lower, upper []mount.Mount, mediaType string, ref string, compressorFunc compression.Compressor) (_ ocispecs.Descriptor, ok bool, err error) {
	// Get upperdir location if mounts are overlayfs that can be processed by this differ.
	upperdir, err := overlay.GetUpperdir(lower, upper)
	if err != nil {
		// This is not an overlayfs snapshot. This is not an error so don't return error here
		// and let the caller fallback to another differ.
		return emptyDesc, false, nil
	}

	return sr.writeBlob(ctx, mediaType, ref, compressorFunc, func(w io.Writer) error {
		return overlay.WriteUpperdir(ctx, w, upperdir, lower)
	})
}

// computeMappedWalkingBlob computes the diff between lower and upper like the
// containerd walking differ, with the owners mapped to the container IDs.
func (sr *immutableRef) computeMappedWalkingBlob(ctx context.Context, lower, upper []mount.Mount, mediaType string, ref string, compressorFunc compression.Compressor) (desc ocispecs.Descriptor, err error) {
	var opts []archive.WriteDiffOpt
	if tm := epoch.FromContext(ctx); tm != nil {
		opts = append(opts, archive.WithSourceDateEpoch(tm))
	}
	err = mount.WithTempMount(ctx, lower, func(lowerRoot string) error {
		return mount.WithReadonlyTempMount(ctx, upper, func(upperRoot string) error {
			var err error
			desc, _, err = sr.writeBlob(ctx, mediaType, ref, compressorFunc, func(w io.Writer) error {
				return archive.WriteDiff(ctx, w, lowerRoot, upperRoot, opts...)
			})
			return err
		})
	})
	return desc, err
}

// writeBlob writes the layer tar written by writeTar to the content store,
// with the owners mapped to the container IDs if the snapshotter has an ID
// mapping.
func (sr *immutableRef) writeBlob(ctx context.Context, mediaType string, ref string, compressorFunc compression.Compressor, writeTar func(io.Writer) error) (_ ocispecs.Descriptor, ok bool, err error) {
	writeTar = withIDMap(writeTar, sr.cm.IdentityMapping())

	cw, err := content.OpenWriter(ctx, sr.cm.ContentStore,
		content.WithRef(ref),
		content.WithDescriptor(ocispecs.Descriptor{
			MediaType: mediaType, // most contentstore implementations just ignore this
		}))
	if err != nil {
		return emptyDesc, false, errors.Wrap(err, "failed to open writer")
	}

	defer func() {
		if cw != nil {
			// Do not abort a stable ref after closing it: another writer waiter
			// may already own the same ingest by the time Abort is processed.
			if cerr := cw.Close(); cerr != nil {
				bklog.G(ctx).WithError(cerr).Warnf("failed to close writer %q", ref)
			}
		}
	}()

	if err = cw.Truncate(0); err != nil {
		return emptyDesc, false, errors.Wrap(err, "failed to truncate writer")
	}

	bufW := bufio.NewWriterSize(cw, 128*1024)
	var labels map[string]string
	if compressorFunc != nil {
		dgstr := digest.SHA256.Digester()
		compressed, err := compressorFunc(bufW, mediaType)
		if err != nil {
			return emptyDesc, false, errors.Wrap(err, "failed to get compressed stream")
		}
		// Close ensure compressorFunc does some finalization works.
		defer compressed.Close()
		if err := writeTar(io.MultiWriter(compressed, dgstr.Hash())); err != nil {
			return emptyDesc, false, errors.Wrap(err, "failed to write compressed diff")
		}
		if err := compressed.Close(); err != nil {
			return emptyDesc, false, errors.Wrap(err, "failed to close compressed diff writer")
		}
		labels = map[string]string{
			labelspkg.LabelUncompressed: dgstr.Digest().String(),
		}
	} else {
		if err = writeTar(bufW); err != nil {
			return emptyDesc, false, errors.Wrap(err, "failed to write diff")
		}
	}

	if err := bufW.Flush(); err != nil {
		return emptyDesc, false, errors.Wrap(err, "failed to flush diff")
	}
	var commitopts []content.Opt
	if labels != nil {
		commitopts = append(commitopts, content.WithLabels(labels))
	}
	dgst := cw.Digest()
	commitErr := cw.Commit(ctx, 0, dgst, commitopts...)
	cw = nil // Commit always closes the writer, including on error.
	if commitErr != nil {
		if !cerrdefs.IsAlreadyExists(commitErr) {
			return emptyDesc, false, errors.Wrap(commitErr, "failed to commit")
		}
	}
	cinfo, err := sr.cm.ContentStore.Info(ctx, dgst)
	if err != nil {
		return emptyDesc, false, errors.Wrap(err, "failed to get info from content store")
	}
	if cinfo.Labels == nil {
		cinfo.Labels = make(map[string]string)
	}
	// Set uncompressed label if digest already existed without label
	if _, ok := cinfo.Labels[labelspkg.LabelUncompressed]; !ok {
		cinfo.Labels[labelspkg.LabelUncompressed] = labels[labelspkg.LabelUncompressed]
		if _, err := sr.cm.ContentStore.Update(ctx, cinfo, "labels."+labelspkg.LabelUncompressed); err != nil {
			return emptyDesc, false, errors.Wrap(err, "error setting uncompressed label")
		}
	}

	return ocispecs.Descriptor{
		MediaType: mediaType,
		Size:      cinfo.Size,
		Digest:    cinfo.Digest,
	}, true, nil
}
