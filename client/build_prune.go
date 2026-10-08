package client

import (
	"context"
	"fmt"
	"iter"
	"net/url"
	"strconv"

	"github.com/moby/moby/api/types/prune"

	"github.com/moby/moby/api/types/build"
	"github.com/moby/moby/client/pkg/versions"
)

// BuildCachePruneOptions hold parameters to prune the build cache.
type BuildCachePruneOptions struct {
	All           bool
	ReservedSpace int64
	MaxUsedSpace  int64
	MinFreeSpace  int64
	Filters       Filters

	// Stream requests a streaming result; consume Events or Wait, or call Close.
	// Requires API v1.57. The default returns a completed report.
	Stream bool
}

// BuildCachePruneResult holds the result from the BuildCachePrune method.
type BuildCachePruneResult struct {
	// Report is populated when Stream is false. Use Wait for streaming results.
	Report build.CachePruneReport
	stream *pruneStream[build.CachePruneReport]
}

// BuildCachePrune requests the daemon to delete unused cache data.
func (cli *Client) BuildCachePrune(ctx context.Context, opts BuildCachePruneOptions) (BuildCachePruneResult, error) {
	query := url.Values{}
	if opts.All {
		query.Set("all", "1")
	}

	if opts.ReservedSpace != 0 {
		// Prior to API v1.48, 'keep-storage' was used to set the reserved space for the build cache.
		// TODO(austinvazquez): remove once API v1.47 is no longer supported. See https://github.com/moby/moby/issues/50902
		if versions.LessThanOrEqualTo(cli.version, "1.47") {
			query.Set("keep-storage", strconv.Itoa(int(opts.ReservedSpace)))
		} else {
			query.Set("reserved-space", strconv.Itoa(int(opts.ReservedSpace)))
		}
	}
	if opts.MaxUsedSpace != 0 {
		query.Set("max-used-space", strconv.Itoa(int(opts.MaxUsedSpace)))
	}
	if opts.MinFreeSpace != 0 {
		query.Set("min-free-space", strconv.Itoa(int(opts.MinFreeSpace)))
	}
	opts.Filters.updateURLValues(query)

	stream, err := startPrune[build.CachePruneReport](cli, ctx, "/build/prune", query, opts.Stream)
	if err != nil {
		return BuildCachePruneResult{}, err
	}
	out := BuildCachePruneResult{stream: stream}
	if !opts.Stream {
		out.Report, err = stream.wait(ctx)
		if err != nil {
			return BuildCachePruneResult{}, fmt.Errorf("error retrieving prune report: %w", err)
		}
	}
	return out, nil
}

// Events yields confirmed object changes and closes the request when iteration ends.
func (r BuildCachePruneResult) Events(ctx context.Context) iter.Seq2[prune.Object, error] {
	if r.stream == nil {
		return func(func(prune.Object, error) bool) {}
	}
	return r.stream.events(ctx)
}

// Wait returns the complete report, consuming any remaining progress.
func (r BuildCachePruneResult) Wait(ctx context.Context) (build.CachePruneReport, error) {
	if r.stream == nil {
		return r.Report, nil
	}
	return r.stream.wait(ctx)
}

// Close cancels an unfinished request without draining its response.
func (r BuildCachePruneResult) Close() error {
	if r.stream == nil {
		return nil
	}
	return r.stream.close()
}

var _ PruneResponse[build.CachePruneReport] = BuildCachePruneResult{}
