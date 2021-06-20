package convert // import "github.com/docker/docker/daemon/cluster/convert"

import (
	"github.com/docker/docker/api/types"
	volumetypes "github.com/docker/docker/api/types/volume"
	swarmapi "github.com/docker/swarmkit/api"
	gogotypes "github.com/gogo/protobuf/types"
)

// VolumeFromGRPC converts a swarmkit api Volume object to a docker api Volume
// object
func VolumeFromGRPC(v *swarmapi.Volume) types.Volume {
	clusterVolumeSpec := types.ClusterVolumeSpec{
		Group:                     v.Spec.Group,
		AccessMode:                accessModeFromGRPC(v.Spec.AccessMode),
		AccessibilityRequirements: topologyRequirementFromGRPC(v.Spec.AccessibilityRequirements),
		CapacityRange:             capacityRangeFromGRPC(v.Spec.CapacityRange),
		Secrets:                   volumeSecretsFromGRPC(v.Spec.Secrets),
		Availability:              volumeAvailabilityFromGRPC(v.Spec.Availability),
	}

	clusterVolume := &types.ClusterVolume{
		ID:            v.ID,
		Spec:          clusterVolumeSpec,
		PublishStatus: volumePublishStatusFromGRPC(v.PublishStatus),
		Info:          volumeInfoFromGRPC(v.VolumeInfo),
	}

	clusterVolume.Version.Index = v.Meta.Version.Index
	clusterVolume.CreatedAt, _ = gogotypes.TimestampFromProto(v.Meta.CreatedAt)
	clusterVolume.UpdatedAt, _ = gogotypes.TimestampFromProto(v.Meta.UpdatedAt)

	return types.Volume{
		ClusterVolume: clusterVolume,
		CreatedAt:     clusterVolume.CreatedAt.String(),
		Driver:        v.Spec.Driver.Name,
		Labels:        v.Spec.Annotations.Labels,
		Name:          v.Spec.Annotations.Name,
		Options:       v.Spec.Driver.Options,
		Scope:         "global",
	}
}

func volumeSpecToGRPC(spec types.ClusterVolumeSpec) *swarmapi.VolumeSpec {
	swarmSpec := &swarmapi.VolumeSpec{
		Group: spec.Group,
	}

	if spec.AccessMode != nil {
		swarmSpec.AccessMode = &swarmapi.VolumeAccessMode{}

		switch spec.AccessMode.Scope {
		case types.VolumeScopeSingleNode:
			swarmSpec.AccessMode.Scope = swarmapi.VolumeScopeSingleNode
		case types.VolumeScopeMultiNode:
			swarmSpec.AccessMode.Scope = swarmapi.VolumeScopeMultiNode
		}

		switch spec.AccessMode.Sharing {
		case types.VolumeSharingNone:
			swarmSpec.AccessMode.Sharing = swarmapi.VolumeSharingNone
		case types.VolumeSharingReadOnly:
			swarmSpec.AccessMode.Sharing = swarmapi.VolumeSharingReadOnly
		case types.VolumeSharingOneWriter:
			swarmSpec.AccessMode.Sharing = swarmapi.VolumeSharingOneWriter
		case types.VolumeSharingAll:
			swarmSpec.AccessMode.Sharing = swarmapi.VolumeSharingAll
		}

		if spec.AccessMode.BlockVolume != nil {
			swarmSpec.AccessMode.AccessType = &swarmapi.VolumeAccessMode_Block{
				Block: &swarmapi.VolumeAccessMode_BlockVolume{},
			}
		}
		if spec.AccessMode.MountVolume != nil {
			swarmSpec.AccessMode.AccessType = &swarmapi.VolumeAccessMode_Mount{
				Mount: &swarmapi.VolumeAccessMode_MountVolume{
					FsType:     spec.AccessMode.MountVolume.FsType,
					MountFlags: spec.AccessMode.MountVolume.MountFlags,
				},
			}
		}
	}

	for _, secret := range spec.Secrets {
		swarmSpec.Secrets = append(swarmSpec.Secrets, &swarmapi.VolumeSecret{
			Key:    secret.Key,
			Secret: secret.Secret,
		})
	}

	if spec.AccessibilityRequirements != nil {
		swarmSpec.AccessibilityRequirements = &swarmapi.TopologyRequirement{}

		for _, top := range spec.AccessibilityRequirements.Requisite {
			swarmSpec.AccessibilityRequirements.Requisite = append(
				swarmSpec.AccessibilityRequirements.Requisite,
				&swarmapi.Topology{
					Segments: top.Segments,
				},
			)
		}

		for _, top := range spec.AccessibilityRequirements.Preferred {
			swarmSpec.AccessibilityRequirements.Preferred = append(
				swarmSpec.AccessibilityRequirements.Preferred,
				&swarmapi.Topology{
					Segments: top.Segments,
				},
			)
		}
	}

	if spec.CapacityRange != nil {
		swarmSpec.CapacityRange = &swarmapi.CapacityRange{
			RequiredBytes: int64(spec.CapacityRange.RequiredBytes),
			LimitBytes:    int64(spec.CapacityRange.LimitBytes),
		}
	}

	// availability is not a pointer, it is a value. if the user does not
	// specify an availability, it will be inferred as the 0-value, which is
	// "active".
	switch spec.Availability {
	case types.VolumeAvailabilityActive:
		swarmSpec.Availability = swarmapi.VolumeAvailabilityActive
	case types.VolumeAvailabilityPause:
		swarmSpec.Availability = swarmapi.VolumeAvailabilityPause
	case types.VolumeAvailabilityDrain:
		swarmSpec.Availability = swarmapi.VolumeAvailabilityDrain
	}

	return swarmSpec
}

func VolumeCreateToGRPC(volume *volumetypes.VolumeCreateBody) *swarmapi.VolumeSpec {
	var swarmSpec *swarmapi.VolumeSpec
	if volume != nil && volume.ClusterVolumeSpec != nil {
		swarmSpec = volumeSpecToGRPC(*volume.ClusterVolumeSpec)
	} else {
		swarmSpec = &swarmapi.VolumeSpec{}
	}

	swarmSpec.Annotations = swarmapi.Annotations{
		Name:   volume.Name,
		Labels: volume.Labels,
	}

	swarmSpec.Driver = &swarmapi.Driver{
		Name:    volume.Driver,
		Options: volume.DriverOpts,
	}

	return swarmSpec
}

// VolumeUpdateToGRPC converts a VolumeUpdateBody to the swarm GRPC object.
//
// NOTE(dperny): this is not yet used, as the only thing that can be changed
// with volumes at this time is availability.
func VolumeUpdateToGRPC(volume *volumetypes.VolumeUpdateBody) *swarmapi.VolumeSpec {
	var swarmSpec *swarmapi.VolumeSpec
	if volume != nil && volume.Spec != nil {
		swarmSpec = volumeSpecToGRPC(*volume.Spec)
	} else {
		swarmSpec = &swarmapi.VolumeSpec{}
	}

	// TODO(dperny): handle name, driver, etc, which are all in practice
	// immutable.

	return swarmSpec
}

func volumeInfoFromGRPC(info *swarmapi.VolumeInfo) *types.VolumeInfo {
	if info == nil {
		return nil
	}

	var accessibleTopology []types.Topology
	if info.AccessibleTopology != nil {
		accessibleTopology = make([]types.Topology, len(info.AccessibleTopology))
		for i, top := range info.AccessibleTopology {
			accessibleTopology[i] = topologyFromGRPC(top)
		}
	}

	return &types.VolumeInfo{
		CapacityBytes:      int(info.CapacityBytes),
		VolumeContext:      info.VolumeContext,
		VolumeID:           info.VolumeID,
		AccessibleTopology: accessibleTopology,
	}
}

func volumePublishStatusFromGRPC(publishStatus []*swarmapi.VolumePublishStatus) []*types.VolumePublishStatus {
	if publishStatus == nil {
		return nil
	}

	vps := make([]*types.VolumePublishStatus, len(publishStatus))
	for i, status := range publishStatus {
		var state types.VolumePublishState
		switch status.State {
		case swarmapi.VolumePublishStatus_PENDING_PUBLISH:
			state = types.VolumePendingPublish
		case swarmapi.VolumePublishStatus_PUBLISHED:
			state = types.VolumePublished
		case swarmapi.VolumePublishStatus_PENDING_NODE_UNPUBLISH:
			state = types.VolumePendingNodeUnpublish
		case swarmapi.VolumePublishStatus_PENDING_UNPUBLISH:
			state = types.VolumePendingUnpublish
		}

		vps[i] = &types.VolumePublishStatus{
			NodeID:         status.NodeID,
			State:          state,
			PublishContext: status.PublishContext,
		}
	}

	return vps
}

func accessModeFromGRPC(accessMode *swarmapi.VolumeAccessMode) *types.VolumeAccessMode {
	if accessMode == nil {
		return nil
	}

	convertedAccessMode := &types.VolumeAccessMode{}

	switch accessMode.Scope {
	case swarmapi.VolumeScopeSingleNode:
		convertedAccessMode.Scope = types.VolumeScopeSingleNode
	case swarmapi.VolumeScopeMultiNode:
		convertedAccessMode.Scope = types.VolumeScopeMultiNode
	}

	switch accessMode.Sharing {
	case swarmapi.VolumeSharingNone:
		convertedAccessMode.Sharing = types.VolumeSharingNone
	case swarmapi.VolumeSharingReadOnly:
		convertedAccessMode.Sharing = types.VolumeSharingReadOnly
	case swarmapi.VolumeSharingOneWriter:
		convertedAccessMode.Sharing = types.VolumeSharingOneWriter
	case swarmapi.VolumeSharingAll:
		convertedAccessMode.Sharing = types.VolumeSharingAll
	}

	if block := accessMode.GetBlock(); block != nil {
		convertedAccessMode.BlockVolume = &types.VolumeTypeBlock{}
	}
	if mount := accessMode.GetMount(); mount != nil {
		convertedAccessMode.MountVolume = &types.VolumeTypeMount{
			FsType:     mount.FsType,
			MountFlags: mount.MountFlags,
		}
	}

	return convertedAccessMode
}

func volumeSecretsFromGRPC(secrets []*swarmapi.VolumeSecret) []types.VolumeSecret {
	if secrets == nil {
		return nil
	}
	convertedSecrets := make([]types.VolumeSecret, len(secrets))
	for i, secret := range secrets {
		convertedSecrets[i] = types.VolumeSecret{
			Key:    secret.Key,
			Secret: secret.Secret,
		}
	}
	return convertedSecrets
}

func topologyRequirementFromGRPC(top *swarmapi.TopologyRequirement) *types.TopologyRequirement {
	if top == nil {
		return nil
	}

	convertedTop := &types.TopologyRequirement{}
	if top.Requisite != nil {
		convertedTop.Requisite = make([]types.Topology, len(top.Requisite))
		for i, req := range top.Requisite {
			convertedTop.Requisite[i] = topologyFromGRPC(req)
		}
	}

	if top.Preferred != nil {
		convertedTop.Preferred = make([]types.Topology, len(top.Preferred))
		for i, pref := range top.Preferred {
			convertedTop.Preferred[i] = topologyFromGRPC(pref)
		}
	}

	return convertedTop
}

func topologyFromGRPC(top *swarmapi.Topology) types.Topology {
	if top == nil {
		return types.Topology{}
	}
	return types.Topology{
		Segments: top.Segments,
	}
}

func capacityRangeFromGRPC(capacity *swarmapi.CapacityRange) *types.VolumeCapacityRange {
	if capacity == nil {
		return nil
	}

	return &types.VolumeCapacityRange{
		RequiredBytes: uint64(capacity.RequiredBytes),
		LimitBytes:    uint64(capacity.LimitBytes),
	}
}

func volumeAvailabilityFromGRPC(availability swarmapi.VolumeSpec_VolumeAvailability) types.VolumeAvailability {
	switch availability {
	case swarmapi.VolumeAvailabilityActive:
		return types.VolumeAvailabilityActive
	case swarmapi.VolumeAvailabilityPause:
		return types.VolumeAvailabilityPause
	}
	return types.VolumeAvailabilityDrain
}
