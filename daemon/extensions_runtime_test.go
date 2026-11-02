package daemon

import (
	"fmt"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	networkSettings "github.com/moby/moby/v2/daemon/network"
	runtimev0 "github.com/moby/moby/v2/extpoints/runtime/v0"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

func TestRuntimeExtensionDeclaration(t *testing.T) {
	// Declaration is nil-daemon safe: the daemon is only dereferenced when
	// the provider serves requests.
	decl := runtimeExtension(nil).Declaration()
	assert.Check(t, is.Equal(string(decl.ID), runtimeExtensionID))
	assert.Assert(t, is.Len(decl.Providers, 1))
	assert.Check(t, is.Equal(decl.Providers[0].Point, runtimev0.Point.ID()))
}

func TestBackendCreateConfig(t *testing.T) {
	t.Run("nil host config resolves to the default network", func(t *testing.T) {
		cfg := backendCreateConfig(runtimev0.ContainerCreateRequest{Name: "job"})
		assert.Check(t, is.Equal(cfg.Name, "job"))
		assert.Assert(t, cfg.HostConfig != nil)
		assert.Check(t, is.Equal(string(cfg.HostConfig.NetworkMode), networkSettings.DefaultNetwork))
	})

	for _, declared := range []container.NetworkMode{network.NetworkDefault, ""} {
		t.Run(fmt.Sprintf("mode %q resolves and its endpoint follows, without mutating the request", declared), func(t *testing.T) {
			endpoint := &network.EndpointSettings{}
			hostConfig := &container.HostConfig{NetworkMode: declared}
			networking := &network.NetworkingConfig{
				EndpointsConfig: map[string]*network.EndpointSettings{
					network.NetworkDefault: endpoint,
				},
			}
			cfg := backendCreateConfig(runtimev0.ContainerCreateRequest{
				HostConfig:       hostConfig,
				NetworkingConfig: networking,
			})
			mode := cfg.HostConfig.NetworkMode
			assert.Check(t, is.Equal(string(mode), networkSettings.DefaultNetwork))
			assert.Check(t, is.Equal(cfg.NetworkingConfig.EndpointsConfig[mode.NetworkName()], endpoint))
			_, stale := cfg.NetworkingConfig.EndpointsConfig[network.NetworkDefault]
			assert.Check(t, !stale, "the default-keyed endpoint must be re-keyed, not duplicated")
			// The caller's request must stay untouched: the resolution works
			// on copies.
			assert.Check(t, is.Equal(hostConfig.NetworkMode, declared))
			assert.Check(t, is.Equal(networking.EndpointsConfig[network.NetworkDefault], endpoint))
			assert.Check(t, is.Len(networking.EndpointsConfig, 1))
		})
	}

	t.Run("an explicit network mode is passed through untouched", func(t *testing.T) {
		hostConfig := &container.HostConfig{NetworkMode: "host"}
		config := &container.Config{Image: "busybox"}
		cfg := backendCreateConfig(runtimev0.ContainerCreateRequest{
			Name:       "job",
			Config:     config,
			HostConfig: hostConfig,
		})
		assert.Check(t, is.Equal(cfg.HostConfig, hostConfig))
		assert.Check(t, is.Equal(cfg.Config, config))
		assert.Check(t, is.Equal(string(cfg.HostConfig.NetworkMode), "host"))
	})
}
