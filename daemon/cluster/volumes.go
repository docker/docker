package cluster // import "github.com/docker/docker/daemon/cluster"

import (
	"context"

	apitypes "github.com/docker/docker/api/types"
	types "github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/daemon/cluster/convert"
	swarmapi "github.com/docker/swarmkit/api"
	"google.golang.org/grpc"
)

// GetVolume returns a volume from the swarm cluster.
func (c *Cluster) GetVolume(input string) (types.Volume, error) {
	var volume *swarmapi.Volume

	if err := c.lockedManagerAction(func(ctx context.Context, state nodeState) error {
		v, err := getVolume(ctx, state.controlClient, input)
		if err != nil {
			return err
		}
		volume = v
		return nil
	}); err != nil {
		return types.Volume{}, err
	}
	return convert.VolumeFromGRPC(volume), nil
}

// GetVolumes returns all of the volumes matching the given options from a swarm cluster.
func (c *Cluster) GetVolumes(options apitypes.VolumeListOptions) ([]types.Volume, error) {
	var volumes []types.Volume
	if err := c.lockedManagerAction(func(ctx context.Context, state nodeState) error {
		r, err := state.controlClient.ListVolumes(
			ctx, &swarmapi.ListVolumesRequest{},
			grpc.MaxCallRecvMsgSize(defaultRecvSizeForListResponse),
		)
		if err != nil {
			return err
		}

		volumes = make([]types.Volume, 0, len(r.Volumes))
		for _, volume := range r.Volumes {
			volumes = append(volumes, convert.VolumeFromGRPC(volume))
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return volumes, nil
}

// CreateVolume creates a new cluster volume in the swarm cluster.
//
// Returns the volume ID if creation is successful, or an error if not.
func (c *Cluster) CreateVolume(v types.VolumeSpec) (string, error) {
	var resp *swarmapi.CreateVolumeResponse
	if err := c.lockedManagerAction(func(ctx context.Context, state nodeState) error {
		volumeSpec := convert.VolumeSpecToGRPC(&v)

		r, err := state.controlClient.CreateVolume(
			ctx, &swarmapi.CreateVolumeRequest{Spec: volumeSpec},
		)
		if err != nil {
			return err
		}
		resp = r
		return nil
	}); err != nil {
		return "", err
	}
	return resp.Volume.ID, nil
}

// RemoveVolume removes a volume from the swarm cluster.
func (c *Cluster) RemoveVolume(input string) error {
	return c.lockedManagerAction(func(ctx context.Context, state nodeState) error {
		volume, err := getVolume(ctx, state.controlClient, input)
		if err != nil {
			return err
		}

		req := &swarmapi.RemoveVolumeRequest{
			VolumeID: volume.ID,
		}
		_, err = state.controlClient.RemoveVolume(ctx, req)
		return err
	})
}

// UpdateVolume updates a volume in the swarm cluster.
func (c *Cluster) UpdateVolume(input string, version uint64, spec types.VolumeSpec) error {
	return c.lockedManagerAction(func(ctx context.Context, state nodeState) error {
		volume, err := getVolume(ctx, state.controlClient, input)
		if err != nil {
			return err
		}

		volumeSpec := convert.VolumeSpecToGRPC(&spec)

		_, err = state.controlClient.UpdateVolume(
			ctx, &swarmapi.UpdateVolumeRequest{
				VolumeID: volume.ID,
				VolumeVersion: &swarmapi.Version{
					Index: version,
				},
				Spec: volumeSpec,
			},
		)
		return err
	})
}
