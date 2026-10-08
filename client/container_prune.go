package client

import (
	"context"
	"fmt"
	"iter"
	"net/url"

	"github.com/moby/moby/api/types/prune"

	"github.com/moby/moby/api/types/container"
)

// ContainerPruneOptions holds parameters to prune containers.
type ContainerPruneOptions struct {
	Filters Filters

	// Stream requests a streaming result; consume Events or Wait, or call Close.
	// Requires API v1.57. The default returns a completed report.
	Stream bool
}

// ContainerPruneResult holds the result from the [Client.ContainerPrune] method.
type ContainerPruneResult struct {
	// Report is populated when Stream is false. Use Wait for streaming results.
	Report container.PruneReport
	stream *pruneStream[container.PruneReport]
}

// ContainerPrune requests the daemon to delete unused data
func (cli *Client) ContainerPrune(ctx context.Context, opts ContainerPruneOptions) (ContainerPruneResult, error) {
	query := url.Values{}
	opts.Filters.updateURLValues(query)

	stream, err := startPrune[container.PruneReport](cli, ctx, "/containers/prune", query, opts.Stream)
	if err != nil {
		return ContainerPruneResult{}, err
	}
	out := ContainerPruneResult{stream: stream}
	if !opts.Stream {
		out.Report, err = stream.wait(ctx)
		if err != nil {
			return ContainerPruneResult{}, fmt.Errorf("error retrieving prune report: %w", err)
		}
	}
	return out, nil
}

// Events yields confirmed object changes and closes the request when iteration ends.
func (r ContainerPruneResult) Events(ctx context.Context) iter.Seq2[prune.Object, error] {
	if r.stream == nil {
		return func(func(prune.Object, error) bool) {}
	}
	return r.stream.events(ctx)
}

// Wait returns the complete report, consuming any remaining progress.
func (r ContainerPruneResult) Wait(ctx context.Context) (container.PruneReport, error) {
	if r.stream == nil {
		return r.Report, nil
	}
	return r.stream.wait(ctx)
}

// Close cancels an unfinished request without draining its response.
func (r ContainerPruneResult) Close() error {
	if r.stream == nil {
		return nil
	}
	return r.stream.close()
}

var _ PruneResponse[container.PruneReport] = ContainerPruneResult{}
