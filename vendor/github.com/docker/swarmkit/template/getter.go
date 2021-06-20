package template

import (
	"github.com/docker/swarmkit/agent/exec"
	"github.com/docker/swarmkit/api"
	"github.com/pkg/errors"
)

type templatedSecretGetter struct {
	dependencies exec.DependencyGetter
	t            *api.Task
	node         *api.NodeDescription
}

// NewTemplatedSecretGetter returns a SecretGetter that evaluates templates.
func NewTemplatedSecretGetter(dependencies exec.DependencyGetter, t *api.Task, node *api.NodeDescription) exec.SecretGetter {
	return templatedSecretGetter{dependencies: dependencies, t: t, node: node}
}

func (t templatedSecretGetter) Get(secretID string) (*api.Secret, error) {
	if t.dependencies == nil {
		return nil, errors.New("no secret provider available")
	}

	secrets := t.dependencies.Secrets()
	if secrets == nil {
		return nil, errors.New("no secret provider available")
	}

	secret, err := secrets.Get(secretID)
	if err != nil {
		return secret, err
	}

	newSpec, err := ExpandSecretSpec(secret, t.node, t.t, t.dependencies)
	if err != nil {
		return secret, errors.Wrapf(err, "failed to expand templated secret %s", secretID)
	}

	secretCopy := *secret
	secretCopy.Spec = *newSpec
	return &secretCopy, nil
}

// TemplatedConfigGetter is a ConfigGetter with an additional method to expose
// whether a config contains sensitive data.
type TemplatedConfigGetter interface {
	exec.ConfigGetter

	// GetAndFlagSecretData returns the interpolated config, and also
	// returns true if the config has been interpolated with data from a
	// secret. In this case, the config should be handled specially and
	// should not be written to disk.
	GetAndFlagSecretData(configID string) (*api.Config, bool, error)
}

type templatedConfigGetter struct {
	dependencies exec.DependencyGetter
	t            *api.Task
	node         *api.NodeDescription
}

// NewTemplatedConfigGetter returns a ConfigGetter that evaluates templates.
func NewTemplatedConfigGetter(dependencies exec.DependencyGetter, t *api.Task, node *api.NodeDescription) TemplatedConfigGetter {
	return templatedConfigGetter{dependencies: dependencies, t: t, node: node}
}

func (t templatedConfigGetter) Get(configID string) (*api.Config, error) {
	config, _, err := t.GetAndFlagSecretData(configID)
	return config, err
}

func (t templatedConfigGetter) GetAndFlagSecretData(configID string) (*api.Config, bool, error) {
	if t.dependencies == nil {
		return nil, false, errors.New("no config provider available")
	}

	configs := t.dependencies.Configs()
	if configs == nil {
		return nil, false, errors.New("no config provider available")
	}

	config, err := configs.Get(configID)
	if err != nil {
		return config, false, err
	}

	newSpec, sensitive, err := ExpandConfigSpec(config, t.node, t.t, t.dependencies)
	if err != nil {
		return config, false, errors.Wrapf(err, "failed to expand templated config %s", configID)
	}

	configCopy := *config
	configCopy.Spec = *newSpec
	return &configCopy, sensitive, nil
}

type templatedDependencyGetter struct {
	secrets exec.SecretGetter
	configs TemplatedConfigGetter
	volumes exec.VolumeGetter
}

// NewTemplatedDependencyGetter returns a DependencyGetter that evaluates templates.
func NewTemplatedDependencyGetter(dependencies exec.DependencyGetter, t *api.Task, node *api.NodeDescription) exec.DependencyGetter {
	return templatedDependencyGetter{
		secrets: NewTemplatedSecretGetter(dependencies, t, node),
		configs: NewTemplatedConfigGetter(dependencies, t, node),
		volumes: dependencies.Volumes(),
	}
}

func (t templatedDependencyGetter) Secrets() exec.SecretGetter {
	return t.secrets
}

func (t templatedDependencyGetter) Configs() exec.ConfigGetter {
	return t.configs
}

func (t templatedDependencyGetter) Volumes() exec.VolumeGetter {
	// volumes are not templated, but we include that call (and pass it
	// straight through to the underlying getter) in order to fulfill the
	// DependencyGetter interface.
	return t.volumes
}
