package csi

import (
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/docker/swarmkit/api"
)

// convert.go contains functions for converting swarm objects into CSI requests
// and back again.

// makeTopology converts a swarmkit topology into a CSI topology.
func makeTopologyRequirement(t *api.TopologyRequirement) *csi.TopologyRequirement {
	if t == nil {
		return nil
	}
	return &csi.TopologyRequirement{
		Requisite: makeTopologies(t.Requisite),
		Preferred: makeTopologies(t.Preferred),
	}
}

// makeTopologies converts a slice of swarmkit topologies into a slice of CSI
// topologies.
func makeTopologies(ts []*api.Topology) []*csi.Topology {
	if ts == nil {
		return nil
	}
	csiTops := make([]*csi.Topology, len(ts))
	for i, t := range ts {
		csiTops[i] = makeTopology(t)
	}

	return csiTops
}

// makeTopology converts a swarmkit topology into a CSI topology. These types
// are essentially homologous, with the swarm type being copied verbatim from
// the CSI type (for build reasons).
func makeTopology(t *api.Topology) *csi.Topology {
	if t == nil {
		return nil
	}
	return &csi.Topology{
		Segments: t.Segments,
	}
}

func makeCapability(am *api.VolumeAccessMode) *csi.VolumeCapability {
	var mode csi.VolumeCapability_AccessMode_Mode
	switch am.Scope {
	case api.VolumeScopeSingleNode:
		switch am.Sharing {
		case api.VolumeSharingNone, api.VolumeSharingOneWriter, api.VolumeSharingAll:
			mode = csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER
		case api.VolumeSharingReadOnly:
			mode = csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY
		}
	case api.VolumeScopeMultiNode:
		switch am.Sharing {
		case api.VolumeSharingReadOnly:
			mode = csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY
		case api.VolumeSharingOneWriter:
			mode = csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER
		case api.VolumeSharingAll:
			mode = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
		}
	}

	capability := &csi.VolumeCapability{
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: mode,
		},
	}

	if block := am.GetBlock(); block != nil {
		capability.AccessType = &csi.VolumeCapability_Block{
			// Block type is empty.
			Block: &csi.VolumeCapability_BlockVolume{},
		}
	}

	if mount := am.GetMount(); mount != nil {
		capability.AccessType = &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{
				FsType:     mount.FsType,
				MountFlags: mount.MountFlags,
			},
		}
	}

	return capability
}

// makeCapcityRange converts the swarmkit CapacityRange object to the
// equivalent CSI object
func makeCapacityRange(cr *api.CapacityRange) *csi.CapacityRange {
	if cr == nil {
		return nil
	}

	return &csi.CapacityRange{
		RequiredBytes: cr.RequiredBytes,
		LimitBytes:    cr.LimitBytes,
	}
}

// unmakeTopologies transforms a CSI-type topology into the equivalent swarm
// type. it is called "unmakeTopologies" because it performs the inverse of
// "makeTopologies".
func unmakeTopologies(topologies []*csi.Topology) []*api.Topology {
	if topologies == nil {
		return nil
	}
	swarmTopologies := make([]*api.Topology, len(topologies))
	for i, t := range topologies {
		swarmTopologies[i] = unmakeTopology(t)
	}
	return swarmTopologies
}

// unmakeTopology transforms a CSI-type topology into the equivalent swarm
// type.
func unmakeTopology(topology *csi.Topology) *api.Topology {
	return &api.Topology{
		Segments: topology.Segments,
	}
}

// makeVolumeInfo converts a csi.Volume object into a swarmkit VolumeInfo
// object.
func makeVolumeInfo(csiVolume *csi.Volume) *api.VolumeInfo {
	return &api.VolumeInfo{
		CapacityBytes:      csiVolume.CapacityBytes,
		VolumeContext:      csiVolume.VolumeContext,
		VolumeID:           csiVolume.VolumeId,
		AccessibleTopology: unmakeTopologies(csiVolume.AccessibleTopology),
	}
}
