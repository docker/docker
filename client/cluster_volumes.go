package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io/ioutil"
	"net/url"
	"strconv"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
)

// TODO(dperny): break this into multiple files like all other client code.

// ClusterVolumeInspect gets a swarm cluster Volume
func (cli *Client) ClusterVolumeInspectWithRaw(ctx context.Context, id string) (swarm.Volume, []byte, error) {
	if id == "" {
		return swarm.Volume{}, nil, objectNotFoundError{object: "volume", id: id}
	}

	resp, err := cli.get(ctx, "/csi/"+id, nil, nil)
	defer ensureReaderClosed(resp)
	if err != nil {
		return swarm.Volume{}, nil, wrapResponseError(err, resp, "volume", id)
	}

	body, err := ioutil.ReadAll(resp.body)
	if err != nil {
		return swarm.Volume{}, nil, err
	}

	var volume swarm.Volume
	rdr := bytes.NewReader(body)
	err = json.NewDecoder(rdr).Decode(&volume)

	return volume, body, err
}

// ClusterVolumeList lists cluster Volumes
func (cli *Client) ClusterVolumeList(ctx context.Context, options types.VolumeListOptions) ([]swarm.Volume, error) {
	query := url.Values{}

	if options.Filters.Len() > 0 {
		filterJSON, err := filters.ToJSON(options.Filters)
		if err != nil {
			return nil, err
		}

		query.Set("filters", filterJSON)
	}

	resp, err := cli.get(ctx, "/csi", query, nil)
	defer ensureReaderClosed(resp)
	if err != nil {
		return nil, err
	}

	var volumes []swarm.Volume
	err = json.NewDecoder(resp.body).Decode(&volumes)
	return volumes, err
}

// ClusterVolumeCreate creates a new cluster Volume
func (cli *Client) ClusterVolumeCreate(ctx context.Context, volume swarm.VolumeSpec) (types.VolumeCreateResponse, error) {
	var response types.VolumeCreateResponse

	resp, err := cli.post(ctx, "/csi/create", nil, volume, nil)
	defer ensureReaderClosed(resp)
	if err != nil {
		return response, err
	}

	err = json.NewDecoder(resp.body).Decode(&response)
	return response, err
}

func (cli *Client) ClusterVolumeUpdate(ctx context.Context, volumeID string, version swarm.Version, volume swarm.VolumeSpec) error {
	var query = url.Values{}

	query.Set("version", strconv.FormatUint(version.Index, 10))

	resp, err := cli.post(ctx, "/csi/"+volumeID+"/update", query, volume, nil)
	defer ensureReaderClosed(resp)

	return err
}

func (cli *Client) ClusterVolumeRemove(ctx context.Context, volumeID string) error {
	resp, err := cli.delete(ctx, "/csi/"+volumeID, nil, nil)
	defer ensureReaderClosed(resp)
	return wrapResponseError(err, resp, "volume", volumeID)
}
