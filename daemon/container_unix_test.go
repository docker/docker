// +build linux freebsd

package daemon

import (
	"testing"

	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/daemon/config"
	"github.com/docker/go-connections/nat"
	"gotest.tools/v3/assert"
)

// TestContainerErrorHostAndPublishPorts asserts that an error is returned when binding ports while using network host mode upon starting a container.
// This should not be tested on Windows because Windows doesn't support "host" network mode.
func TestContainerErrorHostAndPublishPorts(t *testing.T) {
	// muteLogs()
	hostConfig := &containertypes.HostConfig{
		Runtime:     "runc",
		NetworkMode: "host",
		PortBindings: nat.PortMap{
			"8080": []nat.PortBinding{{HostPort: "8989"}},
		},
	}
	cs := &config.Config{
		CommonUnixConfig: config.CommonUnixConfig{
			Runtimes: map[string]types.Runtime{"runc": {}},
		},
	}
	d := &Daemon{configStore: cs}
	_, err := d.verifyContainerSettings("", hostConfig, &containertypes.Config{}, false)
	assert.Error(t, err, "cannot bind ports in host network mode")
}

// TestContainerHostAndNoPublishedPorts asserts that there are no errors returned when using network host mode with no publied ports.
func TestContainerHostAndNoPublishedPorts(t *testing.T) {
	// muteLogs()
	hostConfig := &containertypes.HostConfig{
		Runtime:     "runc",
		NetworkMode: "host",
	}
	cs := &config.Config{
		CommonUnixConfig: config.CommonUnixConfig{
			Runtimes: map[string]types.Runtime{"runc": {}},
		},
	}
	d := &Daemon{configStore: cs}
	_, err := d.verifyContainerSettings("", hostConfig, &containertypes.Config{}, false)
	assert.NilError(t, err)
}

// TestContainerHostAndNoPubliedPorts asserts that there are no errors returned when publishing ports without using network host mode.
func TestContainerPublishedPortsAndNoHost(t *testing.T) {
	hostConfig := &containertypes.HostConfig{
		Runtime: "runc",
		PortBindings: nat.PortMap{
			"8080": []nat.PortBinding{{HostPort: "8989"}},
		},
	}
	cs := &config.Config{
		CommonUnixConfig: config.CommonUnixConfig{
			Runtimes: map[string]types.Runtime{"runc": {}},
		},
	}
	d := &Daemon{configStore: cs}
	_, err := d.verifyContainerSettings("", hostConfig, &containertypes.Config{}, false)
	assert.NilError(t, err)
}
