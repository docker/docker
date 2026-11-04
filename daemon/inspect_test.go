package daemon

import (
	"encoding/json"
	"testing"

	containertypes "github.com/moby/moby/api/types/container"
	networktypes "github.com/moby/moby/api/types/network"
	"github.com/moby/moby/v2/daemon/container"
	"github.com/moby/moby/v2/daemon/network"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

// TestGetInspectDataEmptyCollections verifies that nil and empty collections
// are encoded as {} or [] instead of null, preserving the inspect API's
// response format.
func TestGetInspectDataEmptyCollections(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ports networktypes.PortMap
		log   []*containertypes.HealthcheckResult
	}{
		{
			name: "nil",
		},
		{
			name:  "empty",
			ports: networktypes.PortMap{},
			log:   []*containertypes.HealthcheckResult{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Daemon{
				linkIndex: newLinkIndex(),
			}
			cfg := &configStore{}
			d.configStore.Store(cfg)

			inspect, _, err := d.getInspectData(&cfg.Config, &container.Container{
				ID:              "inspect-me",
				NetworkSettings: &network.Settings{Ports: tc.ports},
				HostConfig:      &containertypes.HostConfig{},
				State: &container.State{Health: &container.Health{
					Health: containertypes.Health{Log: tc.log},
				}},
				ExecCommands: container.NewExecStore(),
			})
			assert.NilError(t, err)

			data, err := json.Marshal(inspect)
			assert.NilError(t, err)

			// Use RawMessage instead of the API types to assert the raw JSON representation.
			var response struct {
				NetworkSettings struct{ Ports json.RawMessage }
				State           struct {
					Health struct{ Log json.RawMessage }
				}
			}
			assert.NilError(t, json.Unmarshal(data, &response))
			assert.Equal(t, string(response.NetworkSettings.Ports), "{}")
			assert.Equal(t, string(response.State.Health.Log), "[]")
		})
	}
}

func TestContainerInspect(t *testing.T) {
	c := &container.Container{
		ID:              "inspect-me",
		NetworkSettings: &network.Settings{},
		HostConfig:      &containertypes.HostConfig{},
		State:           &container.State{},
		ExecCommands:    container.NewExecStore(),
	}

	d := &Daemon{
		linkIndex: newLinkIndex(),
	}
	if d.UsesSnapshotter() {
		t.Skip("does not apply to containerd snapshotters, which don't have RWLayer set")
	}
	cfg := &configStore{}
	d.configStore.Store(cfg)

	_, _, err := d.containerInspect(&cfg.Config, c)
	assert.Check(t, is.ErrorContains(err, "RWLayer of container inspect-me is unexpectedly nil"))

	c.State.Dead = true
	_, _, err = d.containerInspect(&cfg.Config, c)
	assert.Check(t, err)
}
