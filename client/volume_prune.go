package client

import (
	"context"
	"fmt"
	"iter"
	"net/url"

	"github.com/moby/moby/api/types/prune"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/volume"
)

// VolumePruneOptions holds parameters to prune volumes.
type VolumePruneOptions struct {
	// All controls whether named volumes should also be pruned. By
	// default, only anonymous volumes are pruned.
	All bool

	// Filters to apply when pruning.
	Filters Filters

	// Stream requests a streaming result; consume Events or Wait, or call Close.
	// Requires API v1.57. The default returns a completed report.
	Stream bool
}

// VolumePruneResult holds the result from the [Client.VolumePrune] method.
type VolumePruneResult struct {
	// Report is populated when Stream is false. Use Wait for streaming results.
	Report volume.PruneReport
	stream *pruneStream[volume.PruneReport]
}

// VolumePrune requests the daemon to delete unused data
func (cli *Client) VolumePrune(ctx context.Context, options VolumePruneOptions) (VolumePruneResult, error) {
	if options.All {
		if _, ok := options.Filters["all"]; ok {
			return VolumePruneResult{}, cerrdefs.ErrInvalidArgument.WithMessage(`conflicting options: cannot specify both "all" and "all" filter`)
		}
		if options.Filters == nil {
			options.Filters = Filters{}
		}
		options.Filters.Add("all", "true")
	}

	query := url.Values{}
	options.Filters.updateURLValues(query)

	stream, err := startPrune[volume.PruneReport](cli, ctx, "/volumes/prune", query, options.Stream)
	if err != nil {
		return VolumePruneResult{}, err
	}
	out := VolumePruneResult{stream: stream}
	if !options.Stream {
		out.Report, err = stream.wait(ctx)
		if err != nil {
			return VolumePruneResult{}, fmt.Errorf("error retrieving prune report: %w", err)
		}
	}
	return out, nil
}

// Events yields confirmed object changes and closes the request when iteration ends.
func (r VolumePruneResult) Events(ctx context.Context) iter.Seq2[prune.Object, error] {
	if r.stream == nil {
		return func(func(prune.Object, error) bool) {}
	}
	return r.stream.events(ctx)
}

// Wait returns the complete report, consuming any remaining progress.
func (r VolumePruneResult) Wait(ctx context.Context) (volume.PruneReport, error) {
	if r.stream == nil {
		return r.Report, nil
	}
	return r.stream.wait(ctx)
}

// Close cancels an unfinished request without draining its response.
func (r VolumePruneResult) Close() error {
	if r.stream == nil {
		return nil
	}
	return r.stream.close()
}

var _ PruneResponse[volume.PruneReport] = VolumePruneResult{}
