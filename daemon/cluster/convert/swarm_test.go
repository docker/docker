package convert

import (
	"testing"

	types "github.com/moby/moby/api/types/swarm"
	swarmapi "github.com/moby/swarmkit/v2/api"
	"gotest.tools/v3/assert"
)

// TestSwarmTaskDefaultsLogDriver tests that TaskDefaults.LogDriver is carried
// between the Engine API Spec and the swarmkit ClusterSpec in both directions.
// Regression test for https://github.com/moby/moby/issues/53396.
func TestSwarmTaskDefaultsLogDriver(t *testing.T) {
	opts := map[string]string{"max-size": "10m", "max-file": "3"}

	t.Run("FromGRPC", func(t *testing.T) {
		tests := []struct {
			name     string
			driver   *swarmapi.Driver
			expected *types.Driver
		}{
			{name: "unset", driver: nil, expected: nil},
			{
				name:     "set",
				driver:   &swarmapi.Driver{Name: "json-file", Options: opts},
				expected: &types.Driver{Name: "json-file", Options: opts},
			},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				swarm := SwarmFromGRPC(swarmapi.Cluster{
					Spec: swarmapi.ClusterSpec{
						TaskDefaults: swarmapi.TaskDefaults{LogDriver: tc.driver},
					},
				})
				assert.DeepEqual(t, swarm.Spec.TaskDefaults.LogDriver, tc.expected)
			})
		}
	})

	t.Run("MergeToGRPC", func(t *testing.T) {
		tests := []struct {
			name     string
			initial  *swarmapi.Driver
			update   *types.Driver
			expected *swarmapi.Driver
		}{
			{
				name:     "set",
				update:   &types.Driver{Name: "json-file", Options: opts},
				expected: &swarmapi.Driver{Name: "json-file", Options: opts},
			},
			{
				// A nil value in the update means "keep the status quo".
				name:     "omitted preserves existing",
				initial:  &swarmapi.Driver{Name: "journald"},
				expected: &swarmapi.Driver{Name: "journald"},
			},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				initial := swarmapi.ClusterSpec{
					TaskDefaults: swarmapi.TaskDefaults{LogDriver: tc.initial},
				}
				spec, err := MergeSwarmSpecToGRPC(types.Spec{
					TaskDefaults: types.TaskDefaults{LogDriver: tc.update},
				}, initial)
				assert.NilError(t, err)
				assert.DeepEqual(t, spec.TaskDefaults.LogDriver, tc.expected)
			})
		}
	})
}
