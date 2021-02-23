package plugin

import (
	"context"
	"fmt"
	"sync"

	"github.com/docker/swarmkit/api"
)

// PluginManager manages the multiple CSI plugins that may be in use on the
// node. PluginManager should be thread-safe.
type PluginManager interface {
	// Get gets the plugin with the given name
	Get(name string) (NodePlugin, error)

	// Set sets all of the active plugins managed by this PluginManager. Any
	// plugins active which are not in the argument are removed. Any plugins
	// not yet active which are in the argument are created.
	Set(plugins []*api.CSINodePlugin) error

	// NodeInfo returns the NodeCSIInfo for every active plugin.
	NodeInfo(ctx context.Context) ([]*api.NodeCSIInfo, error)
}

type pluginManager struct {
	plugins   map[string]NodePlugin
	pluginsMu sync.Mutex

	// newNodePluginFunc usually points to NewNodePlugin. However, for testing,
	// NewNodePlugin can be swapped out with a function that creates fake node
	// plugins
	newNodePluginFunc func(string, string, SecretGetter) NodePlugin

	// secrets is a SecretGetter for use by node plugins.
	secrets SecretGetter
}

func NewPluginManager(secrets SecretGetter) PluginManager {
	return &pluginManager{
		plugins:           map[string]NodePlugin{},
		newNodePluginFunc: NewNodePlugin,
		secrets:           secrets,
	}
}

func (pm *pluginManager) Get(name string) (NodePlugin, error) {
	pm.pluginsMu.Lock()
	defer pm.pluginsMu.Unlock()

	plugin, ok := pm.plugins[name]
	if !ok {
		return nil, fmt.Errorf("cannot find plugin %v", name)
	}

	return plugin, nil
}

// TODO(dperny): skipping removing plugins for now.
func (pm *pluginManager) Set(plugins []*api.CSINodePlugin) error {
	pm.pluginsMu.Lock()
	defer pm.pluginsMu.Unlock()

	newPlugins := map[string]NodePlugin{}
	for _, plugin := range plugins {
		np, ok := pm.plugins[plugin.Name]
		if !ok {
			newPlugins[plugin.Name] = pm.newNodePluginFunc(plugin.Name, plugin.Socket, pm.secrets)
		} else {
			newPlugins[plugin.Name] = np
		}
	}

	pm.plugins = newPlugins

	return nil
}

func (pm *pluginManager) NodeInfo(ctx context.Context) ([]*api.NodeCSIInfo, error) {
	// TODO(dperny): do not acquire this lock for the duration of the the
	// function call. that's too long and too blocking.
	pm.pluginsMu.Lock()
	defer pm.pluginsMu.Unlock()
	nodeInfo := []*api.NodeCSIInfo{}
	for _, plugin := range pm.plugins {
		info, err := plugin.NodeGetInfo(ctx)
		if err != nil {
			// skip any plugin that returns an error
			continue
		}

		nodeInfo = append(nodeInfo, info)
	}
	return nodeInfo, nil
}
