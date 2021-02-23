package exec

import (
	"context"

	"github.com/docker/swarmkit/api"
)

// Executor provides controllers for tasks.
type Executor interface {
	// Describe returns the underlying node description.
	Describe(ctx context.Context) (*api.NodeDescription, error)

	// Configure uses the node object state to propagate node
	// state to the underlying executor.
	Configure(ctx context.Context, node *api.Node) error

	// Controller provides a controller for the given task.
	Controller(t *api.Task) (Controller, error)

	// SetNetworkBootstrapKeys passes the symmetric keys from the
	// manager to the executor.
	SetNetworkBootstrapKeys([]*api.EncryptionKey) error

	// SetCSINodePlugins tells the executor to use the plugins provided in the
	// list when handling volumes
	SetCSINodePlugins([]*api.CSINodePlugin) error
}

// SecretsProvider is implemented by objects that can store secrets, typically
// an executor.
type SecretsProvider interface {
	Secrets() SecretsManager
}

// ConfigsProvider is implemented by objects that can store configs,
// typically an executor.
type ConfigsProvider interface {
	Configs() ConfigsManager
}

// VolumesProvider is implemented by objects that can store volumes,
// typically an executor.
type VolumesProvider interface {
	Volumes() VolumesManager
}

// DependencyManager is a meta-object that can keep track of typed objects
// such as secrets and configs.
type DependencyManager interface {
	SecretsProvider
	ConfigsProvider
	VolumesProvider
}

// DependencyGetter is a meta-object that can provide access to typed objects
// such as secrets and configs.
type DependencyGetter interface {
	Secrets() SecretGetter
	Configs() ConfigGetter
	Volumes() VolumeGetter
}

// SecretGetter contains secret data necessary for the Controller.
type SecretGetter interface {
	// Get returns the the secret with a specific secret ID, if available.
	// When the secret is not available, the return will be nil.
	Get(secretID string) (*api.Secret, error)
}

// SecretsManager is the interface for secret storage and updates.
type SecretsManager interface {
	SecretGetter

	Add(secrets ...api.Secret) // add one or more secrets
	Remove(secrets []string)   // remove the secrets by ID
	Reset()                    // remove all secrets
}

// ConfigGetter contains config data necessary for the Controller.
type ConfigGetter interface {
	// Get returns the the config with a specific config ID, if available.
	// When the config is not available, the return will be nil.
	Get(configID string) (*api.Config, error)
}

// ConfigsManager is the interface for config storage and updates.
type ConfigsManager interface {
	ConfigGetter

	Add(configs ...api.Config) // add one or more configs
	Remove(configs []string)   // remove the configs by ID
	Reset()                    // remove all configs
}

// VolumeGetter contains volume data necessary for the Controller.
type VolumeGetter interface {
	// Get returns the the volume with a specific volume ID, if available.
	// When the volume is not available, the return will be nil.
	Get(volumeID string) (string, error)
}

// VolumesManager is the interface for volume storage and updates.
type VolumesManager interface {
	VolumeGetter

	// Add adds one or more volumes
	Add(volumes ...api.VolumeAssignment)
	// Remove removes one or more volumes. The callback is called each time a
	// volume is successfully removed with the ID of the volume removed.
	//
	// Remove takes a full VolumeAssignment because we may be instructed by the
	// swarm manager to attempt removal of a Volume we don't know we have.
	Remove(volumes []api.VolumeAssignment, callback func(string))
	// Plugins returns the VolumePluginManager for this VolumesManager
	Plugins() VolumePluginManager
}

// PluginManager is the interface for accessing the volume plugin manager from
// the executor. This is identical to
// github.com/docker/swarmkit/agent/csi/plugin.PluginManager, except the former
// also includes a Get method for the VolumesManager to use. This does not
// contain that Get method, to avoid having to import the Plugin type, and
// because in this context, it is not needed.
type VolumePluginManager interface {
	// Set sets the active plugins on the node. The argument should include all
	// and only the plugins currently active; any plugins not included may be
	// removed from the PluginManager.
	Set(plugins []*api.CSINodePlugin) error

	// NodeInfo returns the NodeCSIInfo for each active plugin. Plugins which
	// are added through Set but to which no connection has yet been
	// successfully established will not be included.
	NodeInfo(ctx context.Context) ([]*api.NodeCSIInfo, error)
}
