package convert

import (
	"testing"

	mounttypes "github.com/moby/moby/api/types/mount"
	types "github.com/moby/moby/api/types/swarm"
	"gotest.tools/v3/assert"
)

func TestContainerToGRPCBindOptions(t *testing.T) {
	tests := []struct {
		name        string
		bindOptions mounttypes.BindOptions
	}{
		{
			name:        "create mountpoint only",
			bindOptions: mounttypes.BindOptions{CreateMountpoint: true},
		},
		{
			name:        "read-only non-recursive only",
			bindOptions: mounttypes.BindOptions{ReadOnlyNonRecursive: true},
		},
		{
			name:        "read-only force-recursive only",
			bindOptions: mounttypes.BindOptions{ReadOnlyForceRecursive: true},
		},
		{
			name: "all options",
			bindOptions: mounttypes.BindOptions{
				Propagation:            mounttypes.PropagationRShared,
				NonRecursive:           true,
				CreateMountpoint:       true,
				ReadOnlyNonRecursive:   true,
				ReadOnlyForceRecursive: true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := containerToGRPC(&types.ContainerSpec{
				Mounts: []mounttypes.Mount{{
					Type:        mounttypes.TypeBind,
					Source:      "/source",
					Target:      "/target",
					BindOptions: &tt.bindOptions,
				}},
			})
			assert.NilError(t, err)
			if len(got.Mounts) != 1 {
				t.Fatalf("expected one mount, got %d", len(got.Mounts))
			}
			gotOptions := got.Mounts[0].BindOptions
			if gotOptions == nil {
				t.Fatal("bind options were dropped")
			}
			assert.Equal(t, gotOptions.CreateMountpoint, tt.bindOptions.CreateMountpoint)
			assert.Equal(t, gotOptions.ReadOnlyNonRecursive, tt.bindOptions.ReadOnlyNonRecursive)
			assert.Equal(t, gotOptions.ReadOnlyForceRecursive, tt.bindOptions.ReadOnlyForceRecursive)

			roundTripped := containerSpecFromGRPC(got)
			roundTrippedOptions := roundTripped.Mounts[0].BindOptions
			if roundTrippedOptions == nil {
				t.Fatal("bind options were dropped during round trip")
			}
			assert.Equal(t, roundTrippedOptions.CreateMountpoint, tt.bindOptions.CreateMountpoint)
			assert.Equal(t, roundTrippedOptions.ReadOnlyNonRecursive, tt.bindOptions.ReadOnlyNonRecursive)
			assert.Equal(t, roundTrippedOptions.ReadOnlyForceRecursive, tt.bindOptions.ReadOnlyForceRecursive)
		})
	}
}

func TestTmpfsOptionsToGRPC(t *testing.T) {
	options := [][]string{
		{"noexec"},
		{"uid", "12345"},
	}

	expected := `[["noexec"],["uid","12345"]]`
	actual := tmpfsOptionsToGRPC(options)
	assert.Equal(t, expected, actual)
}

func TestTmpfsOptionsFromGRPC(t *testing.T) {
	options := `[["noexec"],["uid","12345"]]`

	expected := [][]string{
		{"noexec"},
		{"uid", "12345"},
	}
	actual := tmpfsOptionsFromGRPC(options)

	assert.DeepEqual(t, expected, actual)
}
