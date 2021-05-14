package client // import "github.com/docker/docker/client"

import (
	"context"
	"net/url"
	"strconv"

	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/volume"
)

func (cli *Client) VolumeUpdate(ctx context.Context, volumeID string, version swarm.Version, options volume.VolumeUpdateBody) error {
	if err := cli.NewVersionError("1.42", "volume update"); err != nil {
		return err
	}

	query := url.Values{}
	query.Set("version", strconv.FormatUint(version.Index, 10))

	resp, err := cli.post(ctx, "/volumes/"+volumeID+"/update", query, options, nil)
	ensureReaderClosed(resp)
	return err
}
