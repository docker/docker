package agent

import (
	"github.com/docker/swarmkit/agent/configs"
	"github.com/docker/swarmkit/agent/csi"
	"github.com/docker/swarmkit/agent/exec"
	"github.com/docker/swarmkit/agent/secrets"
	"github.com/docker/swarmkit/api"
)

type dependencyManager struct {
	secrets exec.SecretsManager
	configs exec.ConfigsManager
	volumes exec.VolumesManager
}

// NewDependencyManager creates a dependency manager object that wraps
// objects which provide access to various dependency types.
func NewDependencyManager() exec.DependencyManager {
	d := &dependencyManager{
		secrets: secrets.NewManager(),
		configs: configs.NewManager(),
	}
	d.volumes = csi.NewManager(d.secrets)
	return d
}

func (d *dependencyManager) Secrets() exec.SecretsManager {
	return d.secrets
}

func (d *dependencyManager) Configs() exec.ConfigsManager {
	return d.configs
}

func (d *dependencyManager) Volumes() exec.VolumesManager {
	return d.volumes
}

type dependencyGetter struct {
	secrets exec.SecretGetter
	configs exec.ConfigGetter
	volumes exec.VolumeGetter
}

func (d *dependencyGetter) Secrets() exec.SecretGetter {
	return d.secrets
}

func (d *dependencyGetter) Configs() exec.ConfigGetter {
	return d.configs
}

func (d *dependencyGetter) Volumes() exec.VolumeGetter {
	return d.volumes
}

// Restrict provides getters that only allows access to the dependencies
// referenced by the task.
func Restrict(dependencies exec.DependencyManager, t *api.Task) exec.DependencyGetter {
	return &dependencyGetter{
		secrets: secrets.Restrict(dependencies.Secrets(), t),
		configs: configs.Restrict(dependencies.Configs(), t),
		volumes: csi.Restrict(dependencies.Volumes(), t),
	}
}
