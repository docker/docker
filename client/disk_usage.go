package client // import "github.com/docker/docker/client"

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/docker/docker/api/types"
)

// DiskUsage requests the current data usage from the daemon
func (cli *Client) DiskUsage(ctx context.Context) (types.DiskUsage, error) {
	return cli.DiskUsageWithOptions(ctx, types.DiskUsageOptions{})
}

// DiskUsageWithOptions requests the current data usage from the daemon
func (cli *Client) DiskUsageWithOptions(ctx context.Context, options types.DiskUsageOptions) (types.DiskUsage, error) {
	var du types.DiskUsage

	query := url.Values{}

	if options.NoContainers {
		query.Set("containers", "0")
	}
	if options.NoImages {
		query.Set("images", "0")
	}
	if options.NoVolumes {
		query.Set("volumes", "0")
	}
	if options.NoLayerSize {
		query.Set("layer-size", "0")
	}
	if options.NoBuildCache {
		query.Set("build-cache", "0")
	}

	serverResp, err := cli.get(ctx, "/system/df", query, nil)
	defer ensureReaderClosed(serverResp)
	if err != nil {
		return du, err
	}

	if err := json.NewDecoder(serverResp.body).Decode(&du); err != nil {
		return du, fmt.Errorf("Error retrieving disk usage: %v", err)
	}

	return du, nil
}
