package cluster // import "github.com/docker/docker/daemon/cluster"

import (
	"context"
	"fmt"

	apitypes "github.com/docker/docker/api/types"
	volumetypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/daemon/cluster/convert"
	"github.com/docker/docker/errdefs"
	swarmapi "github.com/docker/swarmkit/api"
	"google.golang.org/grpc"
)

// GetVolume returns a volume from the swarm cluster.
func (c *Cluster) GetVolume(nameOrId string) (apitypes.Volume, error) {
	var volume *swarmapi.Volume

	if err := c.lockedManagerAction(func(ctx context.Context, state nodeState) error {
		v, err := getVolume(ctx, state.controlClient, nameOrId)
		if err != nil {
			return err
		}
		volume = v
		return nil
	}); err != nil {
		return apitypes.Volume{}, err
	}
	return convert.VolumeFromGRPC(volume), nil
}

// GetVolumes returns all of the volumes matching the given options from a swarm cluster.
func (c *Cluster) GetVolumes(options apitypes.VolumeListOptions) ([]*apitypes.Volume, error) {
	var volumes []*apitypes.Volume
	if err := c.lockedManagerAction(func(ctx context.Context, state nodeState) error {
		r, err := state.controlClient.ListVolumes(
			ctx, &swarmapi.ListVolumesRequest{},
			grpc.MaxCallRecvMsgSize(defaultRecvSizeForListResponse),
		)
		if err != nil {
			return err
		}

		volumes = make([]*apitypes.Volume, 0, len(r.Volumes))
		for _, volume := range r.Volumes {
			v := convert.VolumeFromGRPC(volume)
			volumes = append(volumes, &v)
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
func (c *Cluster) CreateVolume(v volumetypes.VolumeCreateBody) (*apitypes.Volume, error) {
	var resp *swarmapi.CreateVolumeResponse
	if err := c.lockedManagerAction(func(ctx context.Context, state nodeState) error {
		volumeSpec := convert.VolumeCreateToGRPC(&v)

		r, err := state.controlClient.CreateVolume(
			ctx, &swarmapi.CreateVolumeRequest{Spec: volumeSpec},
		)
		if err != nil {
			return err
		}
		resp = r
		return nil
	}); err != nil {
		return nil, err
	}
	createdVol, err := c.GetVolume(resp.Volume.ID)
	if err != nil {
		// If there's a failure of some sort in this operation the user would
		// get a very unhelpful "not found" error on a create, which is not
		// very helpful at all. Instead, before returning the error, add some
		// context, and change this to a system-type error, because it's
		// nothing the user did wrong.
		return nil, errdefs.System(fmt.Errorf("unable to retrieve created volume: %w", err))
	}
	return &createdVol, nil
}

// RemoveVolume removes a volume from the swarm cluster.
func (c *Cluster) RemoveVolume(nameOrId string) error {
	return c.lockedManagerAction(func(ctx context.Context, state nodeState) error {
		volume, err := getVolume(ctx, state.controlClient, nameOrId)
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
func (c *Cluster) UpdateVolume(nameOrId string, version uint64, volume volumetypes.VolumeUpdateBody) error {
	return c.lockedManagerAction(func(ctx context.Context, state nodeState) error {
		v, err := getVolume(ctx, state.controlClient, nameOrId)
		if err != nil {
			return err
		}

		// For now, the only thing we can update is availability. Instead of
		// converting the whole spec, just pluck out the availability if it has
		// been set.

		if volume.Spec != nil {
			switch volume.Spec.Availability {
			case apitypes.VolumeAvailabilityActive:
				v.Spec.Availability = swarmapi.VolumeAvailabilityActive
			case apitypes.VolumeAvailabilityPause:
				v.Spec.Availability = swarmapi.VolumeAvailabilityPause
			case apitypes.VolumeAvailabilityDrain:
				v.Spec.Availability = swarmapi.VolumeAvailabilityDrain
			}
			// if default empty value, change nothing.
		}

		_, err = state.controlClient.UpdateVolume(
			ctx, &swarmapi.UpdateVolumeRequest{
				VolumeID: nameOrId,
				VolumeVersion: &swarmapi.Version{
					Index: version,
				},
				Spec: &v.Spec,
			},
		)
		return err
	})
}
