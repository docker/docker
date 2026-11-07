package containerd

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	c8dimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/snapshotters"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/distribution/reference"
	"github.com/moby/moby/v2/daemon/internal/progress"
	"github.com/moby/moby/v2/daemon/internal/stringid"
	"github.com/moby/moby/v2/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type progressUpdater interface {
	UpdateProgress(context.Context, *jobs, progress.Output, time.Time) error
}

type jobs struct {
	descs map[digest.Digest]ocispec.Descriptor
	mu    sync.Mutex
}

// newJobs creates a new instance of the job status tracker
func newJobs() *jobs {
	return &jobs{
		descs: map[digest.Digest]ocispec.Descriptor{},
	}
}

func (j *jobs) showProgress(ctx context.Context, out progress.Output, updater progressUpdater) func() {
	ctx, cancelProgress := context.WithCancel(ctx)

	start := time.Now()
	lastUpdate := make(chan struct{})

	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if err := updater.UpdateProgress(ctx, j, out, start); err != nil {
					if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
						log.G(ctx).WithError(err).Error("Updating progress failed")
					}
				}
			case <-ctx.Done():
				ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Millisecond*500)
				defer cancel()
				updater.UpdateProgress(ctx, j, out, start)
				close(lastUpdate)
				return
			}
		}
	}()

	return func() {
		cancelProgress()
		// Wait for the last update to finish.
		// UpdateProgress may still write progress to output and we need
		// to keep the caller from closing it before we finish.
		<-lastUpdate
	}
}

// Add adds descriptors with valid digests to be tracked.
func (j *jobs) Add(desc ...ocispec.Descriptor) {
	j.mu.Lock()
	defer j.mu.Unlock()

	for _, d := range desc {
		if d.Digest.Validate() != nil {
			continue
		}
		if _, ok := j.descs[d.Digest]; ok {
			continue
		}
		j.descs[d.Digest] = d
	}
}

// Remove removes a descriptor
func (j *jobs) Remove(desc ocispec.Descriptor) {
	j.mu.Lock()
	defer j.mu.Unlock()

	delete(j.descs, desc.Digest)
}

// Jobs returns a list of all tracked descriptors
func (j *jobs) Jobs() []ocispec.Descriptor {
	j.mu.Lock()
	defer j.mu.Unlock()

	descs := make([]ocispec.Descriptor, 0, len(j.descs))
	for _, d := range j.descs {
		descs = append(descs, d)
	}
	return descs
}

type pullProgress struct {
	store       content.Store
	showExists  bool
	hideLayers  atomic.Bool
	snapshotter snapshots.Snapshotter
	layers      []ocispec.Descriptor
	unpackStart map[digest.Digest]time.Time
}

func (p *pullProgress) UpdateProgress(ctx context.Context, ongoing *jobs, out progress.Output, start time.Time) error {
	actives, err := p.store.ListStatuses(ctx, "")
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.G(ctx).WithError(err).Error("status check failed")
		return nil
	}
	pulling := make(map[string]content.Status, len(actives))

	// update status of status entries!
	for _, status := range actives {
		pulling[status.Ref] = status
	}

	for _, j := range ongoing.Jobs() {
		if p.hideLayers.Load() {
			ongoing.Remove(j)
			continue
		}
		key := remotes.MakeRefKey(ctx, j)
		if info, ok := pulling[key]; ok {
			if info.Offset == 0 {
				continue
			}
			// Use the per-layer download start (when known) so the estimated
			// time remaining stays accurate even when layers begin downloading
			// at different times due to concurrency limits.
			var layerStart int64
			if !info.StartedAt.IsZero() {
				layerStart = info.StartedAt.Unix()
			}
			out.WriteProgress(progress.Progress{
				ID:      stringid.TruncateID(j.Digest.Encoded()),
				Action:  "Downloading",
				Current: info.Offset,
				Total:   info.Total,
				Start:   layerStart,
			})
			continue
		}

		info, err := p.store.Info(ctx, j.Digest)
		if err != nil {
			if !cerrdefs.IsNotFound(err) {
				return err
			}
		} else if info.CreatedAt.After(start) {
			out.WriteProgress(progress.Progress{
				ID:         stringid.TruncateID(j.Digest.Encoded()),
				Action:     "Download complete",
				HideCounts: true,
			})
			p.finished(ctx, out, j)
			ongoing.Remove(j)
		} else if p.showExists {
			out.WriteProgress(progress.Progress{
				ID:         stringid.TruncateID(j.Digest.Encoded()),
				Action:     "Already exists",
				HideCounts: true,
			})
			p.finished(ctx, out, j)
			ongoing.Remove(j)
		}
	}

	var committedIdx []int
	for idx, desc := range p.layers {
		sn, err := findMatchingSnapshot(ctx, p.snapshotter, desc)
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				continue
			}
			return err
		}

		switch sn.Kind {
		case snapshots.KindActive:
			if p.unpackStart == nil {
				p.unpackStart = make(map[digest.Digest]time.Time)
			}
			var seconds int64
			if began, ok := p.unpackStart[desc.Digest]; !ok {
				p.unpackStart[desc.Digest] = time.Now()
			} else {
				seconds = int64(time.Since(began).Seconds())
			}

			// We _could_ get the current size of snapshot by calling Usage, but this is too expensive
			// and could impact performance. So we just show the "Extracting" message with the elapsed time as progress.
			out.WriteProgress(
				progress.Progress{
					ID:     stringid.TruncateID(desc.Digest.Encoded()),
					Action: "Extracting",
					// Start from 1s, because without Total, 0 won't be shown at all.
					Current: 1 + seconds,
					Units:   "s",
					Start:   start.Unix(),
				})
		case snapshots.KindCommitted:
			out.WriteProgress(progress.Progress{
				ID:         stringid.TruncateID(desc.Digest.Encoded()),
				Action:     "Pull complete",
				HideCounts: true,
				LastUpdate: true,
			})

			committedIdx = append(committedIdx, idx)
		case snapshots.KindUnknown, snapshots.KindView:
			// Ignore other snapshot kinds
		}
	}

	// Remove finished/committed layers from p.layers
	if len(committedIdx) > 0 {
		sort.Ints(committedIdx)
		for i := len(committedIdx) - 1; i >= 0; i-- {
			p.layers = append(p.layers[:committedIdx[i]], p.layers[committedIdx[i]+1:]...)
		}
	}

	return nil
}

// findMatchingSnapshot finds the snapshot corresponding to the layer chain of the given layer descriptor.
// It returns an error if no matching snapshot is found.
// layerDesc MUST point to a layer descriptor and have a non-empty TargetImageLayersLabel annotation.
// For pull, these are added by snapshotters.AppendInfoHandlerWrapper
func findMatchingSnapshot(ctx context.Context, sn snapshots.Snapshotter, layerDesc ocispec.Descriptor) (snapshots.Info, error) {
	chainID, ok := layerDesc.Annotations[snapshotters.TargetImageLayersLabel]
	if !ok {
		return snapshots.Info{}, errdefs.NotFound(errors.New("missing " + snapshotters.TargetImageLayersLabel + " annotation"))
	}

	// Find the snapshot corresponding to this layer chain
	walkFilter := "labels.\"" + snapshotters.TargetImageLayersLabel + "\"==\"" + chainID + "\""

	var matchingSnapshot *snapshots.Info
	err := sn.Walk(ctx, func(ctx context.Context, sn snapshots.Info) error {
		matchingSnapshot = &sn
		return nil
	}, walkFilter)
	if err != nil {
		return snapshots.Info{}, err
	}
	if matchingSnapshot == nil {
		return snapshots.Info{}, errdefs.NotFound(errors.New("no matching snapshot found"))
	}

	return *matchingSnapshot, nil
}

func (p *pullProgress) finished(ctx context.Context, out progress.Output, desc ocispec.Descriptor) {
	if c8dimages.IsLayerType(desc.MediaType) {
		p.layers = append(p.layers, desc)
	}
}

type pushProgress struct {
	Tracker                         docker.StatusTracker
	notStartedWaitingAreUnavailable atomic.Bool
}

// TurnNotStartedIntoUnavailable will mark all not started layers as "Unavailable" instead of "Waiting".
func (p *pushProgress) TurnNotStartedIntoUnavailable() {
	p.notStartedWaitingAreUnavailable.Store(true)
}

func (p *pushProgress) UpdateProgress(ctx context.Context, ongoing *jobs, out progress.Output, start time.Time) error {
	for _, j := range ongoing.Jobs() {
		key := remotes.MakeRefKey(ctx, j)
		id := stringid.TruncateID(j.Digest.Encoded())

		status, err := p.Tracker.GetStatus(key)

		notStarted := (status.Total > 0 && status.Offset == 0)
		if err != nil || notStarted {
			if p.notStartedWaitingAreUnavailable.Load() {
				progress.Update(out, id, "Unavailable")
				continue
			}
			if cerrdefs.IsNotFound(err) {
				progress.Update(out, id, "Waiting")
				continue
			}
		}

		if status.Committed && status.Offset >= status.Total {
			if status.MountedFrom != "" {
				from := status.MountedFrom
				if ref, err := reference.ParseNormalizedNamed(from); err == nil {
					from = reference.Path(ref)
				}
				progress.Update(out, id, "Mounted from "+from)
			} else if status.Exists {
				if c8dimages.IsLayerType(j.MediaType) {
					progress.Update(out, id, "Layer already exists")
				} else {
					progress.Update(out, id, "Already exists")
				}
			} else {
				progress.Update(out, id, "Pushed")
			}
			ongoing.Remove(j)
			continue
		}

		out.WriteProgress(progress.Progress{
			ID:      id,
			Action:  "Pushing",
			Current: status.Offset,
			Total:   status.Total,
		})
	}

	return nil
}

type combinedProgress []progressUpdater

func (combined combinedProgress) UpdateProgress(ctx context.Context, ongoing *jobs, out progress.Output, start time.Time) error {
	for _, p := range combined {
		err := p.UpdateProgress(ctx, ongoing, out, start)
		if err != nil {
			return err
		}
	}
	return nil
}

// showBlobProgress determines if the progress of pulling/pushing blob should be shown.
// Only indexes, manifests, and configs are hidden to align with the pre-containerd behavior.
// They are small enough JSON files so it's fine to not show them.
// We mostly care about bigger content like layers or other blobs.
func showBlobProgress(desc ocispec.Descriptor) bool {
	switch {
	case c8dimages.IsLayerType(desc.MediaType):
		// Fast path: we always show progress for layers.
		//
		// Note: We can't just plainly check for c8dimages.IsLayerType alone
		// because it wouldn't account for other potentially big blobs like
		// artifacts or non-standard images.
		return true
	case c8dimages.IsIndexType(desc.MediaType),
		c8dimages.IsManifestType(desc.MediaType),
		c8dimages.IsConfigType(desc.MediaType):
		return false
	default:
		return true
	}
}
