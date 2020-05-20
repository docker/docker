package plugin // import "github.com/docker/docker/plugin"

import (
	"fmt"
	"strings"

	reference "github.com/containerd/containerd/reference/docker"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/plugingetter"
	"github.com/docker/docker/pkg/plugins"
	v2 "github.com/docker/docker/plugin/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

/* allowV1PluginsFallback determines daemon's support for V1 plugins.
 * When the time comes to remove support for V1 plugins, flipping
 * this bool is all that will be needed.
 */
const allowV1PluginsFallback = true

/* defaultAPIVersion is the version of the plugin API for volume, network,
   IPAM and authz. This is a very stable API. When we update this API, then
   pluginType should include a version. e.g. "networkdriver/2.0".
*/
const defaultAPIVersion = "1.0"

// GetV2Plugin retrieves a plugin by name, id or partial ID.
func (ps *Store) GetV2Plugin(refOrID string) (*v2.Plugin, error) {
	ps.RLock()
	defer ps.RUnlock()

	id, err := ps.resolvePluginID(refOrID)
	if err != nil {
		return nil, err
	}

	p, idOk := ps.plugins[id]
	if !idOk {
		return nil, errors.WithStack(errNotFound(id))
	}

	return p, nil
}

// validateName returns error if name is already reserved. always call with lock and full name
func (ps *Store) validateName(name string) error {
	for _, p := range ps.plugins {
		if p.Name() == name {
			return alreadyExistsError(name)
		}
	}
	return nil
}

// GetAll retrieves all plugins.
func (ps *Store) GetAll() map[string]*v2.Plugin {
	ps.RLock()
	defer ps.RUnlock()
	return ps.plugins
}

// SetAll initialized plugins during daemon restore.
func (ps *Store) SetAll(plugins map[string]*v2.Plugin) {
	ps.Lock()
	defer ps.Unlock()

	for _, p := range plugins {
		ps.setSpecOpts(p)
	}
	ps.plugins = plugins
}

func (ps *Store) getAllByCap(capability string) []plugingetter.CompatPlugin {
	ps.RLock()
	defer ps.RUnlock()

	result := make([]plugingetter.CompatPlugin, 0, 1)
	for _, p := range ps.plugins {
		if p.IsEnabled() {
			if _, err := p.FilterByCap(capability); err == nil {
				result = append(result, p)
			}
		}
	}
	return result
}

// SetState sets the active state of the plugin and updates plugindb.
func (ps *Store) SetState(p *v2.Plugin, state bool) {
	ps.Lock()
	defer ps.Unlock()

	p.PluginObj.Enabled = state
}

func (ps *Store) setSpecOpts(p *v2.Plugin) {
	var specOpts []SpecOpt
	for _, typ := range p.GetTypes() {
		opts, ok := ps.specOpts[typ.String()]
		if ok {
			specOpts = append(specOpts, opts...)
		}
	}

	p.SetSpecOptModifier(func(s *specs.Spec) {
		for _, o := range specOpts {
			o(s)
		}
	})
}

// Add adds a plugin to memory and plugindb.
// An error will be returned if there is a collision.
func (ps *Store) Add(p *v2.Plugin) error {
	ps.Lock()
	defer ps.Unlock()

	if v, exist := ps.plugins[p.GetID()]; exist {
		return fmt.Errorf("plugin %q has the same ID %s as %q", p.Name(), p.GetID(), v.Name())
	}

	ps.setSpecOpts(p)

	ps.plugins[p.GetID()] = p
	return nil
}

// Remove removes a plugin from memory and plugindb.
func (ps *Store) Remove(p *v2.Plugin) {
	ps.Lock()
	delete(ps.plugins, p.GetID())
	ps.Unlock()
}

// Get returns an enabled plugin matching the given name and capability.
func (ps *Store) Get(name, capability string, mode int) (plugingetter.CompatPlugin, error) {
	// Lookup using new model.
	if ps != nil {
		p, err := ps.GetV2Plugin(name)
		if err == nil {
			if p.IsEnabled() {
				fp, err := p.FilterByCap(capability)
				if err != nil {
					return nil, err
				}
				p.AddRefCount(mode)
				return fp, nil
			}

			// Plugin was found but it is disabled, so we should not fall back to legacy plugins
			// but we should error out right away
			return nil, errDisabled(name)
		}
		var ierr errNotFound
		if !errors.As(err, &ierr) {
			return nil, err
		}
	}

	if !allowV1PluginsFallback {
		return nil, errNotFound(name)
	}

	p, err := plugins.Get(name, capability)
	if err == nil {
		return p, nil
	}
	if errors.Is(err, plugins.ErrNotFound) {
		return nil, errNotFound(name)
	}
	return nil, errors.Wrap(errdefs.System(err), "legacy plugin")
}

// GetAllManagedPluginsByCap returns a list of managed plugins matching the given capability.
func (ps *Store) GetAllManagedPluginsByCap(capability string) []plugingetter.CompatPlugin {
	return ps.getAllByCap(capability)
}

// GetAllByCap returns a list of enabled plugins matching the given capability.
func (ps *Store) GetAllByCap(capability string) ([]plugingetter.CompatPlugin, error) {
	result := make([]plugingetter.CompatPlugin, 0, 1)

	/* Daemon start always calls plugin.Init thereby initializing a store.
	 * So store on experimental builds can never be nil, even while
	 * handling legacy plugins. However, there are legacy plugin unit
	 * tests where the volume subsystem directly talks with the plugin,
	 * bypassing the daemon. For such tests, this check is necessary.
	 */
	if ps != nil {
		result = ps.getAllByCap(capability)
	}

	// Lookup with legacy model
	if allowV1PluginsFallback {
		pl, err := plugins.GetAll(capability)
		if err != nil {
			return nil, errors.Wrap(errdefs.System(err), "legacy plugin")
		}
		for _, p := range pl {
			result = append(result, p)
		}
	}
	return result, nil
}

func pluginType(cap string) string {
	return fmt.Sprintf("docker.%s/%s", strings.ToLower(cap), defaultAPIVersion)
}

// Handle sets a callback for a given capability. It is only used by network
// and ipam drivers during plugin registration. The callback registers the
// driver with the subsystem (network, ipam).
func (ps *Store) Handle(capability string, callback func(string, *plugins.Client)) {
	typ := pluginType(capability)

	// Register callback with new plugin model.
	ps.Lock()
	handlers, ok := ps.handlers[typ]
	if !ok {
		handlers = []func(string, *plugins.Client){}
	}
	handlers = append(handlers, callback)
	ps.handlers[typ] = handlers
	ps.Unlock()

	// Register callback with legacy plugin model.
	if allowV1PluginsFallback {
		plugins.Handle(capability, callback)
	}
}

// RegisterRuntimeOpt stores a list of SpecOpts for the provided capability.
// These options are applied to the runtime spec before a plugin is started for the specified capability.
func (ps *Store) RegisterRuntimeOpt(cap string, opts ...SpecOpt) {
	ps.Lock()
	defer ps.Unlock()
	typ := pluginType(cap)
	ps.specOpts[typ] = append(ps.specOpts[typ], opts...)
}

// CallHandler calls the registered callback. It is invoked during plugin enable.
func (ps *Store) CallHandler(p *v2.Plugin) {
	for _, typ := range p.GetTypes() {
		for _, handler := range ps.handlers[typ.String()] {
			handler(p.Name(), p.Client())
		}
	}
}

// resolvePluginID must be protected by ps.RLock
func (ps *Store) resolvePluginID(idOrName string) (string, error) {
	if validFullID.MatchString(idOrName) {
		return idOrName, nil
	}

	ref, err := reference.ParseNormalizedNamed(idOrName)
	if err != nil {
		return "", errors.WithStack(errNotFound(idOrName))
	}
	if _, ok := ref.(reference.Canonical); ok {
		logrus.Warnf("canonical references cannot be resolved: %v", reference.FamiliarString(ref))
		return "", errors.WithStack(errNotFound(idOrName))
	}

	ref = reference.TagNameOnly(ref)

	for _, p := range ps.plugins {
		if p.PluginObj.Name == reference.FamiliarString(ref) {
			return p.PluginObj.ID, nil
		}
	}

	var found *v2.Plugin
	for id, p := range ps.plugins { // this can be optimized
		if strings.HasPrefix(id, idOrName) {
			if found != nil {
				return "", errors.WithStack(errAmbiguous(idOrName))
			}
			found = p
		}
	}
	if found == nil {
		return "", errors.WithStack(errNotFound(idOrName))
	}
	return found.PluginObj.ID, nil
}
