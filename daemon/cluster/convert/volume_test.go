package convert

import (
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/volume"
	swarmapi "github.com/docker/swarmkit/api"

	"gotest.tools/v3/assert"
)

// TODO(dperny): write tests

func TestTopologyFromGRPC(t *testing.T) {
	nilTopology := topologyFromGRPC(nil)
	assert.DeepEqual(t, nilTopology, types.Topology{})

	swarmTop := &swarmapi.Topology{
		Segments: map[string]string{"foo": "bar"},
	}

	top := topologyFromGRPC(swarmTop)
	assert.DeepEqual(t, top.Segments, swarmTop.Segments)
}

func TestCapacityRangeFromGRPC(t *testing.T) {
	nilCapacity := capacityRangeFromGRPC(nil)
	assert.Assert(t, nilCapacity == nil)

	swarmZeroCapacity := &swarmapi.CapacityRange{}
	zeroCapacity := capacityRangeFromGRPC(swarmZeroCapacity)
	assert.Assert(t, zeroCapacity != nil)
	assert.Equal(t, zeroCapacity.RequiredBytes, uint64(0))
	assert.Equal(t, zeroCapacity.LimitBytes, uint64(0))

	swarmNonZeroCapacity := &swarmapi.CapacityRange{
		RequiredBytes: 1024,
		LimitBytes:    2048,
	}
	nonZeroCapacity := capacityRangeFromGRPC(swarmNonZeroCapacity)
	assert.Assert(t, nonZeroCapacity != nil)
	assert.Equal(t, nonZeroCapacity.RequiredBytes, uint64(1024))
	assert.Equal(t, nonZeroCapacity.LimitBytes, uint64(2048))
}

func TestVolumeAvailabilityFromGRPC(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in       swarmapi.VolumeSpec_VolumeAvailability
		expected types.VolumeAvailability
	}{
		{
			name:     "Active",
			in:       swarmapi.VolumeAvailabilityActive,
			expected: types.VolumeAvailabilityActive,
		}, {
			name:     "Pause",
			in:       swarmapi.VolumeAvailabilityPause,
			expected: types.VolumeAvailabilityPause,
		}, {
			name:     "Drain",
			in:       swarmapi.VolumeAvailabilityDrain,
			expected: types.VolumeAvailabilityDrain,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			actual := volumeAvailabilityFromGRPC(tc.in)
			assert.Equal(t, actual, tc.expected)
		})
	}
}

// TestAccessModeFromGRPC tests that the AccessMode type is correctly converted
func TestAccessModeFromGRPC(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in       *swarmapi.VolumeAccessMode
		expected *types.VolumeAccessMode
	}{
		{
			name: "MountVolume",
			in: &swarmapi.VolumeAccessMode{
				Scope:   swarmapi.VolumeScopeSingleNode,
				Sharing: swarmapi.VolumeSharingNone,
				AccessType: &swarmapi.VolumeAccessMode_Mount{
					Mount: &swarmapi.VolumeAccessMode_MountVolume{
						FsType: "foo",
						// TODO(dperny): maybe don't convert this?
						MountFlags: []string{"one", "two"},
					},
				},
			},
			expected: &types.VolumeAccessMode{
				Scope:   types.VolumeScopeSingleNode,
				Sharing: types.VolumeSharingNone,
				MountVolume: &types.VolumeTypeMount{
					FsType:     "foo",
					MountFlags: []string{"one", "two"},
				},
			},
		}, {
			name: "BlockVolume",
			in: &swarmapi.VolumeAccessMode{
				Scope:   swarmapi.VolumeScopeSingleNode,
				Sharing: swarmapi.VolumeSharingNone,
				AccessType: &swarmapi.VolumeAccessMode_Block{
					Block: &swarmapi.VolumeAccessMode_BlockVolume{},
				},
			},
			expected: &types.VolumeAccessMode{
				Scope:       types.VolumeScopeSingleNode,
				Sharing:     types.VolumeSharingNone,
				BlockVolume: &types.VolumeTypeBlock{},
			},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			out := accessModeFromGRPC(tc.in)
			assert.DeepEqual(t, tc.expected, out)
		})
	}
}

// TestVolumeCreateToGRPC tests that a docker-typed VolumeCreateBody is
// correctly converted to a swarm-typed VolumeSpec.
func TestVolumeCreateToGRPC(t *testing.T) {
	volume := &volume.VolumeCreateBody{
		Driver:     "plug1",
		DriverOpts: map[string]string{"options": "yeah"},
		Labels:     map[string]string{"labeled": "yeah"},
		Name:       "volume1",
	}

	spec := &types.ClusterVolumeSpec{
		Group: "gronp",
		AccessMode: &types.VolumeAccessMode{
			Scope:   types.VolumeScopeMultiNode,
			Sharing: types.VolumeSharingAll,
			MountVolume: &types.VolumeTypeMount{
				FsType:     "foo",
				MountFlags: []string{"one", "two"},
			},
		},
		Secrets: []types.VolumeSecret{
			{Key: "key1", Secret: "secret1"},
			{Key: "key2", Secret: "secret2"},
		},
		AccessibilityRequirements: &types.TopologyRequirement{
			Requisite: []types.Topology{
				{Segments: map[string]string{"top1": "yup"}},
				{Segments: map[string]string{"top2": "def"}},
				{Segments: map[string]string{"top3": "nah"}},
			},
			Preferred: []types.Topology{},
		},
		CapacityRange: &types.VolumeCapacityRange{
			RequiredBytes: 1,
			LimitBytes:    0,
		},
	}

	volume.ClusterVolumeSpec = spec

	swarmSpec := VolumeCreateToGRPC(volume)

	assert.Assert(t, swarmSpec != nil)
	expectedSwarmSpec := &swarmapi.VolumeSpec{
		Annotations: swarmapi.Annotations{
			Name: "volume1",
			Labels: map[string]string{
				"labeled": "yeah",
			},
		},
		Group: "gronp",
		Driver: &swarmapi.Driver{
			Name: "plug1",
			Options: map[string]string{
				"options": "yeah",
			},
		},
		AccessMode: &swarmapi.VolumeAccessMode{
			Scope:   swarmapi.VolumeScopeMultiNode,
			Sharing: swarmapi.VolumeSharingAll,
			AccessType: &swarmapi.VolumeAccessMode_Mount{
				Mount: &swarmapi.VolumeAccessMode_MountVolume{
					FsType:     "foo",
					MountFlags: []string{"one", "two"},
				},
			},
		},
		Secrets: []*swarmapi.VolumeSecret{
			{Key: "key1", Secret: "secret1"},
			{Key: "key2", Secret: "secret2"},
		},
		AccessibilityRequirements: &swarmapi.TopologyRequirement{
			Requisite: []*swarmapi.Topology{
				{Segments: map[string]string{"top1": "yup"}},
				{Segments: map[string]string{"top2": "def"}},
				{Segments: map[string]string{"top3": "nah"}},
			},
			Preferred: nil,
		},
		CapacityRange: &swarmapi.CapacityRange{
			RequiredBytes: 1,
			LimitBytes:    0,
		},
	}

	assert.DeepEqual(t, swarmSpec, expectedSwarmSpec)
}
