package daemon // import "github.com/docker/docker/daemon"

import (
	"github.com/Microsoft/hcsshim/cmd/containerd-shim-runhcs-v1/options"
	"github.com/Microsoft/opengcs/client"
	"github.com/docker/docker/container"
	"github.com/docker/docker/pkg/system"
)

func (daemon *Daemon) getLibcontainerdCreateOptions(container *container.Container) (interface{}, error) {

	// Set the runtime options to debug regardless of current logging level.
	if system.ContainerdRuntimeSupported() {
		opts := &options.Options{Debug: true}
		return opts, nil
	}

	// TODO @jhowardmsft (containerd) - Probably need to revisit LCOW options here
	// rather than blindly ignoring them.

	// LCOW options.
	if container.OS == "linux" {
		config := &client.Config{}
		if err := config.GenerateDefault(daemon.configStore.GraphOptions); err != nil {
			return nil, err
		}
		// Override from user-supplied options.
		for k, v := range container.HostConfig.StorageOpt {
			switch k {
			case "lcow.kirdpath":
				config.KirdPath = v
			case "lcow.bootparameters":
				config.BootParameters = v
			}
		}
		if err := config.Validate(); err != nil {
			return nil, err
		}

		return config, nil
	}

	return nil, nil
}
