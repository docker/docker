package cache

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"os"
	"path"
	"slices"
	"strconv"
	"time"

	obdlabel "github.com/containerd/accelerated-container-image/pkg/label"
	obdcmd "github.com/containerd/accelerated-container-image/pkg/utils"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/labels"
	"github.com/containerd/containerd/v2/plugins/diff/walking"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/compression"
	"github.com/moby/buildkit/util/converter"
	"github.com/moby/buildkit/util/flightcontrol"
	"github.com/moby/buildkit/util/leaseutil"
	"github.com/moby/buildkit/util/winlayers"
	digest "github.com/opencontainers/go-digest"
	imagespecidentity "github.com/opencontainers/image-spec/identity"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

var g flightcontrol.Group[*leaseutil.LeaseRef]
var gFileList flightcontrol.Group[[]string]

var ErrNoBlobs = errors.Errorf("no blobs for snapshot")

// computeBlobChain ensures every ref in a parent chain has an associated blob in the content store. If
// a blob is missing and createIfNeeded is true, then the blob will be created, otherwise ErrNoBlobs will
// be returned. Caller must hold a lease when calling this function.
// If forceCompression is specified but the blob of compressionType doesn't exist, this function creates it.
func (sr *immutableRef) computeBlobChain(ctx context.Context, createIfNeeded bool, comp compression.Config, s session.Group) error {
	if _, ok := leases.FromContext(ctx); !ok {
		return errors.Errorf("missing lease requirement for computeBlobChain")
	}
	if !createIfNeeded {
		sr.mu.Lock()
		if sr.equalMutable != nil {
			sr.mu.Unlock()
			return nil
		}
		sr.mu.Unlock()
	}

	if err := sr.Finalize(ctx); err != nil {
		return err
	}

	if isTypeWindows(sr) {
		ctx = winlayers.UseWindowsLayerMode(ctx)
	}

	// filter keeps track of which layers should actually be included in the blob chain.
	// This is required for diff refs, which can include only a single layer from their parent
	// refs rather than every single layer present among their ancestors.
	filter := sr.layerSet()

	return computeBlobChain(ctx, sr, createIfNeeded, comp, s, filter)
}

func computeBlobChain(ctx context.Context, sr *immutableRef, createIfNeeded bool, comp compression.Config, s session.Group, filter map[string]struct{}) error {
	eg, ctx := errgroup.WithContext(ctx)
	switch sr.kind() {
	case Merge:
		for _, parent := range sr.mergeParents {
			eg.Go(func() error {
				return computeBlobChain(ctx, parent, createIfNeeded, comp, s, filter)
			})
		}
	case Diff:
		if _, ok := filter[sr.ID()]; !ok && sr.diffParents.upper != nil {
			// This diff is just re-using the upper blob, compute that
			eg.Go(func() error {
				return computeBlobChain(ctx, sr.diffParents.upper, createIfNeeded, comp, s, filter)
			})
		}
	case Layer:
		eg.Go(func() error {
			return computeBlobChain(ctx, sr.layerParent, createIfNeeded, comp, s, filter)
		})
	}

	if _, ok := filter[sr.ID()]; ok {
		eg.Go(func() error {
			l, err := g.Do(ctx, fmt.Sprintf("%s-%t", sr.ID(), createIfNeeded), func(ctx context.Context) (_ *leaseutil.LeaseRef, err error) {
				if sr.getBlob() != "" {
					return nil, nil
				}
				if !createIfNeeded {
					return nil, errors.WithStack(ErrNoBlobs)
				}

				l, ctx, err := leaseutil.NewLease(ctx, sr.cm.LeaseManager, leaseutil.MakeTemporary)
				if err != nil {
					return nil, err
				}
				defer func() {
					if err != nil {
						l.Discard(ctx)
					}
				}()

				var finalize compression.Finalizer

				var lowerRef *immutableRef
				switch sr.kind() {
				case Diff:
					lowerRef = sr.diffParents.lower
				case Layer:
					lowerRef = sr.layerParent
				}
				var lower []mount.Mount
				if lowerRef != nil {
					m, err := lowerRef.Mount(ctx, true, s)
					if err != nil {
						return nil, err
					}
					var release func() error
					lower, release, err = m.Mount()
					if err != nil {
						return nil, err
					}
					if release != nil {
						defer release()
					}
				}

				var upperRef *immutableRef
				switch sr.kind() {
				case Diff:
					upperRef = sr.diffParents.upper
				default:
					upperRef = sr
				}
				var upper []mount.Mount
				if upperRef != nil {
					m, err := upperRef.Mount(ctx, true, s)
					if err != nil {
						return nil, err
					}
					var release func() error
					upper, release, err = m.Mount()
					if err != nil {
						return nil, err
					}
					if release != nil {
						defer release()
					}
				}

				var desc ocispecs.Descriptor

				// Determine differ and error/log handling according to the platform, envvar and the snapshotter.
				var enableOverlay, fallback, logWarnOnErr bool
				if forceOvlStr := os.Getenv("BUILDKIT_DEBUG_FORCE_OVERLAY_DIFF"); forceOvlStr != "" && sr.kind() != Diff {
					enableOverlay, err = strconv.ParseBool(forceOvlStr)
					if err != nil {
						return nil, errors.Wrapf(err, "invalid boolean in BUILDKIT_DEBUG_FORCE_OVERLAY_DIFF")
					}
					fallback = false // prohibit fallback on debug
				} else if !isTypeWindows(sr) {
					enableOverlay, fallback = true, true
					switch sr.cm.Snapshotter.Name() {
					case "overlayfs", "stargz":
						// overlayfs-based snapshotters should support overlay diff except when running an arbitrary diff
						// (in which case lower and upper may differ by more than one layer), so print warn log on unexpected
						// failure.
						logWarnOnErr = sr.kind() != Diff
					case "fuse-overlayfs", "native":
						// not supported with fuse-overlayfs snapshotter which doesn't provide overlayfs mounts.
						// TODO: add support for fuse-overlayfs
						enableOverlay = false
					}
				}

				mediaType := comp.Type.MediaType()
				// A content reference identifies the resumable transaction for this
				// cache record. Keep it stable so the content store serializes blob
				// computations from separate callers.
				ref := sr.ID()
				if sr.cm.Snapshotter.Name() == "overlaybd" {
					snStat, err := sr.cm.Snapshotter.Stat(ctx, sr.getSnapshotID())
					if err != nil {
						return nil, errors.Wrapf(err, "failed to Stat overlaybd")
					}
					if bdPath := snStat.Labels[obdlabel.LocalOverlayBDPath]; bdPath != "" {
						if err := commitOverlayBD(ctx, sr, &desc); err != nil {
							return nil, err
						}
						mediaType = desc.MediaType
						enableOverlay = false
					}
				}
				if enableOverlay {
					compressorFunc, attemptFinalize := comp.Type.Compress(ctx, comp)
					computed, ok, err := sr.tryComputeOverlayBlob(ctx, lower, upper, mediaType, ref, compressorFunc)
					if !ok || err != nil {
						if !fallback {
							if !ok {
								return nil, errors.Errorf("overlay mounts not detected (lower=%+v,upper=%+v)", lower, upper)
							}
							if err != nil {
								return nil, errors.Wrapf(err, "failed to compute overlay diff")
							}
						}
						if logWarnOnErr {
							bklog.G(ctx).Warnf("failed to compute blob by overlay differ (ok=%v): %v", ok, err)
						}
					}
					if ok {
						desc = computed
						finalize = attemptFinalize
					}
				}

				if desc.Digest == "" && !isTypeWindows(sr) && comp.Type.NeedsComputeDiffBySelf(comp) {
					// These compression types aren't supported by containerd differ. So try to compute diff on buildkit side.
					// This case can be happen on containerd worker + non-overlayfs snapshotter (e.g. native).
					// See also: https://github.com/containerd/containerd/issues/4263
					compressorFunc, attemptFinalize := comp.Type.Compress(ctx, comp)
					if idmap := sr.cm.IdentityMapping(); idmap != nil && !idmap.Empty() {
						desc, err = sr.computeMappedWalkingBlob(ctx, lower, upper, mediaType, ref, compressorFunc)
					} else {
						desc, err = walking.NewWalkingDiff(openWriterStore{Store: sr.cm.ContentStore}).Compare(ctx, lower, upper,
							diff.WithMediaType(mediaType),
							diff.WithReference(ref),
							diff.WithCompressor(compressorFunc),
						)
					}
					if err != nil {
						bklog.G(ctx).WithError(err).Warnf("failed to compute blob by buildkit differ")
					} else {
						finalize = attemptFinalize
					}
				}

				if desc.Digest == "" {
					desc, err = compareWithRetry(ctx, sr.cm.Differ, lower, upper, func() []diff.Opt {
						compressorFunc, attemptFinalize := comp.Type.Compress(ctx, comp)
						finalize = attemptFinalize
						return []diff.Opt{
							diff.WithMediaType(mediaType),
							diff.WithReference(ref),
							diff.WithCompressor(compressorFunc),
						}
					})
					if err != nil {
						return nil, err
					}
				}

				if desc.Annotations == nil {
					desc.Annotations = map[string]string{}
				}
				if finalize != nil {
					a, err := finalize(ctx, sr.cm.ContentStore)
					if err != nil {
						return nil, errors.Wrapf(err, "failed to finalize compression")
					}
					maps.Copy(desc.Annotations, a)
				}
				info, err := sr.cm.ContentStore.Info(ctx, desc.Digest)
				if err != nil {
					return nil, err
				}

				if diffID, ok := info.Labels[labels.LabelUncompressed]; ok {
					desc.Annotations[labels.LabelUncompressed] = diffID
				} else if mediaType == ocispecs.MediaTypeImageLayer {
					desc.Annotations[labels.LabelUncompressed] = desc.Digest.String()
				} else {
					return nil, errors.Errorf("unknown layer compression type")
				}

				if err := sr.setBlob(ctx, desc); err != nil {
					return nil, err
				}
				return l, nil
			})
			if err != nil {
				return err
			}

			if l != nil {
				if err := l.Adopt(ctx); err != nil {
					return err
				}
			}

			if comp.Force {
				if err := ensureCompression(ctx, sr, comp, s); err != nil {
					return errors.Wrapf(err, "failed to ensure compression type of %q", comp.Type)
				}
			}
			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		return err
	}
	return sr.computeChainMetadata(ctx, filter)
}

// setBlob associates a blob with the cache record.
// A lease must be held for the blob when calling this function
func (sr *immutableRef) setBlob(ctx context.Context, desc ocispecs.Descriptor) (rerr error) {
	if _, ok := leases.FromContext(ctx); !ok {
		return errors.Errorf("missing lease requirement for setBlob")
	}
	defer func() {
		if rerr == nil {
			rerr = sr.linkBlob(ctx, desc)
		}
	}()

	diffID, err := diffIDFromDescriptor(desc)
	if err != nil {
		return err
	}
	if _, err := sr.cm.ContentStore.Info(ctx, desc.Digest); err != nil {
		return err
	}

	sr.mu.Lock()
	defer sr.mu.Unlock()

	if sr.getBlob() != "" {
		return nil
	}

	if err := sr.finalize(ctx); err != nil {
		return err
	}

	if err := sr.cm.LeaseManager.AddResource(ctx, leases.Lease{ID: sr.ID()}, leases.Resource{
		ID:   desc.Digest.String(),
		Type: "content",
	}); err != nil {
		return err
	}

	sr.queueDiffID(diffID)
	sr.queueLayerIdentity(diffID)
	sr.queueBlob(desc.Digest)
	sr.queueMediaType(desc.MediaType)
	sr.queueBlobSize(desc.Size)
	sr.appendURLs(desc.URLs)
	if err := sr.commitMetadata(); err != nil {
		return err
	}

	return nil
}

// layerIdentity returns the snapshot-sharing identity for this layer.
func (sr *immutableRef) layerIdentity() digest.Digest {
	if isLegacyLayerSnapshotID(sr) {
		return ""
	}
	if identity := sr.getLayerIdentity(); identity != "" {
		return identity
	}
	// Records created before layer identities were persisted used the DiffID.
	return sr.getDiffID()
}

func (sr *immutableRef) computeChainMetadata(ctx context.Context, filter map[string]struct{}) error {
	if _, ok := leases.FromContext(ctx); !ok {
		return errors.Errorf("missing lease requirement for computeChainMetadata")
	}

	sr.mu.Lock()
	defer sr.mu.Unlock()

	if sr.getChainID() != "" {
		return nil
	}
	// Do not make unverified pre-versioning snapshots addressable through a
	// newly created Layer, Diff, or Merge record.
	if hasLegacyLayerSnapshotID(sr) {
		return nil
	}

	var chainID digest.Digest
	var blobChainID digest.Digest

	switch sr.kind() {
	case BaseLayer:
		if _, ok := filter[sr.ID()]; !ok {
			return nil
		}
		diffID := sr.getDiffID()
		chainID = sr.layerIdentity()
		if chainID == "" {
			return nil
		}
		blobChainID = imagespecidentity.ChainID([]digest.Digest{sr.getBlob(), diffID})
	case Layer:
		if _, ok := filter[sr.ID()]; !ok {
			return nil
		}
		if _, ok := filter[sr.layerParent.ID()]; ok {
			if parentChainID := sr.layerParent.getChainID(); parentChainID != "" {
				chainID = parentChainID
			} else {
				return errors.Errorf("failed to set chain for reference with non-addressable parent %q", sr.layerParent.GetDescription())
			}
			if parentBlobChainID := sr.layerParent.getBlobChainID(); parentBlobChainID != "" {
				blobChainID = parentBlobChainID
			} else {
				return errors.Errorf("failed to set blobchain for reference with non-addressable parent %q", sr.layerParent.GetDescription())
			}
		}
		diffID := sr.getDiffID()
		layerIdentity := sr.layerIdentity()
		if layerIdentity == "" {
			return nil
		}
		chainID = imagespecidentity.ChainID([]digest.Digest{chainID, layerIdentity})
		blobID := imagespecidentity.ChainID([]digest.Digest{sr.getBlob(), diffID})
		blobChainID = imagespecidentity.ChainID([]digest.Digest{blobChainID, blobID})
	case Merge:
		baseInput := sr.mergeParents[0]
		if _, ok := filter[baseInput.ID()]; !ok {
			// not enough information to compute chain at this time
			return nil
		}
		chainID = baseInput.getChainID()
		blobChainID = baseInput.getBlobChainID()
		for _, mergeParent := range sr.mergeParents[1:] {
			for _, layer := range mergeParent.layerChain() {
				if _, ok := filter[layer.ID()]; !ok {
					// not enough information to compute chain at this time
					return nil
				}
				diffID := layer.getDiffID()
				layerIdentity := layer.layerIdentity()
				if layerIdentity == "" {
					return nil
				}
				chainID = imagespecidentity.ChainID([]digest.Digest{chainID, layerIdentity})
				blobID := imagespecidentity.ChainID([]digest.Digest{layer.getBlob(), diffID})
				blobChainID = imagespecidentity.ChainID([]digest.Digest{blobChainID, blobID})
			}
		}
	case Diff:
		if _, ok := filter[sr.ID()]; ok {
			// this diff is its own blob
			diffID := sr.getDiffID()
			chainID = sr.layerIdentity()
			if chainID == "" {
				return nil
			}
			blobChainID = imagespecidentity.ChainID([]digest.Digest{sr.getBlob(), diffID})
		} else {
			// re-using upper blob
			chainID = sr.diffParents.upper.getChainID()
			blobChainID = sr.diffParents.upper.getBlobChainID()
		}
	}

	sr.queueChainID(chainID)
	sr.queueBlobChainID(blobChainID)
	if err := sr.commitMetadata(); err != nil {
		return err
	}
	return nil
}

func isTypeWindows(sr *immutableRef) bool {
	if sr.GetLayerType() == "windows" {
		return true
	}
	switch sr.kind() {
	case Merge:
		if slices.ContainsFunc(sr.mergeParents, isTypeWindows) {
			return true
		}
	case Layer:
		return isTypeWindows(sr.layerParent)
	}
	return false
}

// ensureCompression ensures the specified ref has the blob of the specified compression Type.
func ensureCompression(ctx context.Context, ref *immutableRef, comp compression.Config, s session.Group) error {
	l, err := g.Do(ctx, fmt.Sprintf("ensureComp-%s-%s", ref.ID(), comp.Type), func(ctx context.Context) (_ *leaseutil.LeaseRef, err error) {
		desc, err := ref.ociDesc(ctx, ref.descHandlers, true)
		if err != nil {
			return nil, err
		}

		l, ctx, err := leaseutil.NewLease(ctx, ref.cm.LeaseManager, leaseutil.MakeTemporary)
		if err != nil {
			return nil, err
		}
		defer func() {
			if err != nil {
				l.Discard(ctx)
			}
		}()

		// Resolve converters
		layerConvertFunc, err := converter.New(ctx, ref.cm.ContentStore, desc, comp)
		if err != nil {
			return nil, err
		} else if layerConvertFunc == nil {
			if isLazy, err := ref.isLazy(ctx); err != nil {
				return nil, err
			} else if isLazy {
				// This ref can be used as the specified compressionType. Keep it lazy.
				return l, nil
			}
			if err := ref.linkBlob(ctx, desc); err != nil {
				return nil, err
			}
			return l, nil
		}

		// First, lookup local content store
		if _, err := ref.getBlobWithCompression(ctx, comp.Type); err == nil {
			return l, nil // found the compression variant. no need to convert.
		}

		// Convert layer compression type
		if err := (lazyRefProvider{
			ref:     ref,
			desc:    desc,
			dh:      ref.descHandlers[desc.Digest],
			session: s,
		}).Unlazy(ctx); err != nil {
			return l, err
		}
		newDesc, err := layerConvertFunc(ctx, ref.cm.ContentStore, desc)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to convert")
		}

		// Start to track converted layer
		if err := ref.linkBlob(ctx, *newDesc); err != nil {
			return nil, errors.Wrapf(err, "failed to add compression blob")
		}
		return l, nil
	})
	if err != nil {
		return err
	}
	if l != nil {
		if err := l.Adopt(ctx); err != nil {
			return err
		}
	}
	return nil
}

func commitOverlayBD(ctx context.Context, sr *immutableRef, desc *ocispecs.Descriptor) error {
	snStat, err := sr.cm.Snapshotter.Stat(ctx, sr.getSnapshotID())
	if err != nil {
		return errors.Wrapf(err, "failed to Stat overlaybd")
	}
	bdPath := snStat.Labels[obdlabel.LocalOverlayBDPath]
	if bdPath == "" {
		return errors.New("missing overlaybd path label")
	}
	dir := path.Dir(bdPath)
	commitPath := path.Join(dir, "overlaybd.commit")
	err = obdcmd.Commit(ctx, dir, dir, true, "-t", "-z", "-f")
	if err != nil {
		return errors.Wrapf(err, "failed to overlaybd-commit")
	}
	cw, err := content.OpenWriter(ctx, sr.cm.ContentStore, content.WithRef(sr.ID()))
	if err != nil {
		return errors.Wrapf(err, "failed to open writer")
	}
	defer func() {
		if cw != nil {
			if err := cw.Close(); err != nil {
				bklog.G(ctx).WithError(err).Warnf("failed to close overlaybd writer %q", sr.ID())
			}
		}
	}()
	if err := cw.Truncate(0); err != nil {
		return errors.Wrap(err, "failed to truncate writer")
	}
	fi, err := os.Open(commitPath)
	if err != nil {
		return errors.Wrapf(err, "failed to open overlaybd commit file")
	}
	defer fi.Close()
	sz, err := io.Copy(cw, bufio.NewReader(fi))
	if err != nil {
		return errors.Wrapf(err, "failed to do io.Copy()")
	}
	dgst := cw.Digest()
	labels := map[string]string{
		labels.LabelUncompressed:     dgst.String(),
		obdlabel.OverlayBDBlobDigest: dgst.String(),
		obdlabel.OverlayBDBlobSize:   fmt.Sprintf("%d", sz),
	}
	err = cw.Commit(ctx, sz, dgst, content.WithLabels(labels))
	cw = nil // Commit always closes the writer, including on error.
	if err != nil {
		return errors.Wrapf(err, "failed to do cw.Commit")
	}
	desc.Digest = dgst
	desc.Size = sz
	desc.MediaType = ocispecs.MediaTypeImageLayer
	desc.Annotations = map[string]string{
		obdlabel.OverlayBDBlobDigest: string(desc.Digest),
		obdlabel.OverlayBDBlobSize:   fmt.Sprintf("%d", desc.Size),
	}
	return nil
}

type openWriterStore struct {
	content.Store
}

func (s openWriterStore) Writer(ctx context.Context, opts ...content.WriterOpt) (content.Writer, error) {
	return content.OpenWriter(ctx, s.Store, opts...)
}

func compareWithRetry(ctx context.Context, comparer diff.Comparer, lower, upper []mount.Mount, opts func() []diff.Opt) (ocispecs.Descriptor, error) {
	retry := 16
	for {
		desc, err := comparer.Compare(ctx, lower, upper, opts()...)
		if err == nil || !cerrdefs.IsUnavailable(err) {
			return desc, err
		}
		if context.Cause(ctx) != nil {
			return ocispecs.Descriptor{}, err
		}

		// The comparer owns its writer, so retry the whole operation with the
		// same stable reference using the same lock-wait behavior as
		// content.OpenWriter.
		if !waitForRetry(ctx, retry) {
			// Match content.OpenWriter: preserve the lock error that explains why
			// the operation could not proceed when cancellation stopped the wait.
			return ocispecs.Descriptor{}, err
		}
		if retry < 2048 {
			retry <<= 1
		}
	}
}

func waitForRetry(ctx context.Context, retry int) bool {
	//nolint:gosec // Backoff jitter does not require cryptographic randomness.
	timer := time.NewTimer(time.Millisecond * time.Duration(rand.IntN(retry)))
	select {
	case <-ctx.Done():
		timer.Stop()
		return false
	case <-timer.C:
		return true
	}
}
