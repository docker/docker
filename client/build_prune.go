package client // import "github.com/docker/docker/client"

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/docker/docker/api/types/image"
)

// BuildCachePrune requests the daemon to delete unused cache data
func (cli *Client) BuildCachePrune(ctx context.Context) (*image.BuildCachePruneReport, error) {
	if err := cli.NewVersionError("1.31", "build prune"); err != nil {
		return nil, err
	}

	report := image.BuildCachePruneReport{}

	serverResp, err := cli.post(ctx, "/build/prune", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	defer ensureReaderClosed(serverResp)

	if err := json.NewDecoder(serverResp.body).Decode(&report); err != nil {
		return nil, fmt.Errorf("Error retrieving disk usage: %v", err)
	}

	return &report, nil
}
