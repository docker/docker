package client

import (
	"context"
	"fmt"
	"iter"
	"net/url"

	"github.com/moby/moby/api/types/prune"

	"github.com/moby/moby/api/types/image"
)

// ImagePruneOptions holds parameters to prune images.
type ImagePruneOptions struct {
	Filters Filters

	// Stream requests a streaming result; consume Events or Wait, or call Close.
	// Requires API v1.57. The default returns a completed report.
	Stream bool
}

// ImagePruneResult holds the result from the [Client.ImagePrune] method.
type ImagePruneResult struct {
	// Report is populated when Stream is false. Use Wait for streaming results.
	Report image.PruneReport
	stream *pruneStream[image.PruneReport]
}

// ImagePrune requests the daemon to delete unused data
func (cli *Client) ImagePrune(ctx context.Context, opts ImagePruneOptions) (ImagePruneResult, error) {
	query := url.Values{}
	opts.Filters.updateURLValues(query)

	stream, err := startPrune[image.PruneReport](cli, ctx, "/images/prune", query, opts.Stream)
	if err != nil {
		return ImagePruneResult{}, err
	}
	out := ImagePruneResult{stream: stream}
	if !opts.Stream {
		out.Report, err = stream.wait(ctx)
		if err != nil {
			return ImagePruneResult{}, fmt.Errorf("error retrieving prune report: %w", err)
		}
	}
	return out, nil
}

// Events yields confirmed object changes and closes the request when iteration ends.
func (r ImagePruneResult) Events(ctx context.Context) iter.Seq2[prune.Object, error] {
	if r.stream == nil {
		return func(func(prune.Object, error) bool) {}
	}
	return r.stream.events(ctx)
}

// Wait returns the complete report, consuming any remaining progress.
func (r ImagePruneResult) Wait(ctx context.Context) (image.PruneReport, error) {
	if r.stream == nil {
		return r.Report, nil
	}
	return r.stream.wait(ctx)
}

// Close cancels an unfinished request without draining its response.
func (r ImagePruneResult) Close() error {
	if r.stream == nil {
		return nil
	}
	return r.stream.close()
}

var _ PruneResponse[image.PruneReport] = ImagePruneResult{}
