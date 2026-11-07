package containerd

import (
	"testing"

	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"gotest.tools/v3/assert"
	is "gotest.tools/v3/assert/cmp"
)

const (
	testExposedPortRange      = "33060-33061/tcp"
	testExposedPortRangeStart = "33060/tcp"
	testExposedPortRangeEnd   = "33061/tcp"
	testInvalidExposedPort    = "invalid/tcp"
)

// regression test for https://github.com/moby/moby/issues/45904
func TestContainerConfigToDockerImageConfig(t *testing.T) {
	ociCFG := containerConfigToDockerOCIImageConfig(&container.Config{
		ExposedPorts: network.PortSet{
			network.MustParsePort("80/tcp"): struct{}{},
		},
	})

	expected := map[string]struct{}{"80/tcp": {}}
	assert.Check(t, is.DeepEqual(ociCFG.ExposedPorts, expected))
}

func TestDockerImageConfigToContainerConfigWithExposedPortRange(t *testing.T) {
	cfg := dockerOCIImageConfigToContainerConfig(dockerspec.DockerOCIImageConfig{
		ImageConfig: ocispec.ImageConfig{
			ExposedPorts: map[string]struct{}{
				testExposedPortRange: {},
			},
		},
	})

	assert.Check(t, is.Contains(cfg.ExposedPorts, network.MustParsePort(testExposedPortRangeStart)))
	assert.Check(t, is.Contains(cfg.ExposedPorts, network.MustParsePort(testExposedPortRangeEnd)))
}

func TestDockerImageConfigToContainerConfigSkipsInvalidExposedPorts(t *testing.T) {
	cfg := dockerOCIImageConfigToContainerConfig(dockerspec.DockerOCIImageConfig{
		ImageConfig: ocispec.ImageConfig{
			ExposedPorts: map[string]struct{}{
				testInvalidExposedPort: {},
				testExposedPortRange:   {},
			},
		},
	})

	assert.Check(t, is.Len(cfg.ExposedPorts, 2))
	assert.Check(t, is.Contains(cfg.ExposedPorts, network.MustParsePort(testExposedPortRangeStart)))
	assert.Check(t, is.Contains(cfg.ExposedPorts, network.MustParsePort(testExposedPortRangeEnd)))
}
