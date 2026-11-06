package client

import (
	"context"
	"fmt"
	"iter"
	"net/url"

	"github.com/moby/moby/api/types/prune"

	"github.com/moby/moby/api/types/network"
)

// NetworkPruneOptions holds parameters to prune networks.
type NetworkPruneOptions struct {
	Filters Filters

	// Stream requests a streaming result; consume Events or Wait, or call Close.
	// Requires API v1.57. The default returns a completed report.
	Stream bool
}

// NetworkPruneResult holds the result from the [Client.NetworkPrune] method.
type NetworkPruneResult struct {
	// Report is populated when Stream is false. Use Wait for streaming results.
	Report network.PruneReport
	stream *pruneStream[network.PruneReport]
}

// NetworkPrune requests the daemon to delete unused networks
func (cli *Client) NetworkPrune(ctx context.Context, opts NetworkPruneOptions) (NetworkPruneResult, error) {
	query := url.Values{}
	opts.Filters.updateURLValues(query)

	stream, err := startPrune[network.PruneReport](cli, ctx, "/networks/prune", query, opts.Stream)
	if err != nil {
		return NetworkPruneResult{}, err
	}
	out := NetworkPruneResult{stream: stream}
	if !opts.Stream {
		out.Report, err = stream.wait(ctx)
		if err != nil {
			return NetworkPruneResult{}, fmt.Errorf("error retrieving prune report: %w", err)
		}
	}
	return out, nil
}

// Events yields confirmed object changes and closes the request when iteration ends.
func (r NetworkPruneResult) Events(ctx context.Context) iter.Seq2[prune.Object, error] {
	if r.stream == nil {
		return func(func(prune.Object, error) bool) {}
	}
	return r.stream.events(ctx)
}

// Wait returns the complete report, consuming any remaining progress.
func (r NetworkPruneResult) Wait(ctx context.Context) (network.PruneReport, error) {
	if r.stream == nil {
		return r.Report, nil
	}
	return r.stream.wait(ctx)
}

// Close cancels an unfinished request without draining its response.
func (r NetworkPruneResult) Close() error {
	if r.stream == nil {
		return nil
	}
	return r.stream.close()
}

var _ PruneResponse[network.PruneReport] = NetworkPruneResult{}
