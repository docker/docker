package daemon

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/moby/extensions"
	"github.com/moby/extensions/clientpoint"
	"github.com/moby/extensions/serverpoint"
	"github.com/moby/moby/v2/daemon/config"
	"github.com/moby/moby/v2/daemon/internal/extensionconfig"
	"github.com/moby/moby/v2/daemon/internal/extensionstorage/bbolt"

	containernamegeneratorpb "github.com/moby/moby/v2/extpoints/containernamegenerator/v0/protogen"
	daemonconfigpb "github.com/moby/moby/v2/extpoints/daemonconfig/v0/protogen"
	servicenamegeneratorpb "github.com/moby/moby/v2/extpoints/servicenamegenerator/v0/protogen"
	storagepb "github.com/moby/moby/v2/extpoints/storage/kv/v0/protogen"
	namesgeneratorlegacy "github.com/moby/moby/v2/internal/namesgenerator/legacy"
)

// builtinExtensions lists in-tree extensions that should be available in the
// engine by default.
var builtinExtensions = []extensions.Extension{
	namesgeneratorlegacy.Extension,
	bbolt.Extension,
}

// optionalExtensions lists built-ins that remain inactive until explicitly
// enabled through extensions.enable.
func (daemon *Daemon) optionalExtensions() []extensions.Extension {
	return []extensions.Extension{}
}

// daemonExtensions declares extensions that need daemon-owned state.
// Ordinary builtin definitions obtain their bootstrap settings through points.
func (daemon *Daemon) daemonExtensions() []extensions.Extension {
	cfg := daemon.Config()
	return []extensions.Extension{
		extensionconfig.New(&cfg),
		runtimeExtension(daemon),
	}
}

// selectBuiltinExtensions applies activation settings to whole extensions.
// Provider conflicts and required dependencies are validated by the host.
func selectBuiltinExtensions(cfg config.ExtensionsConfig, defaults, optional []extensions.Extension) ([]extensions.Extension, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	all := slices.Concat(defaults, optional)
	enabled := make(map[string]bool, len(all))
	for _, ext := range optional {
		enabled[string(ext.Declaration().ID)] = false
	}
	for _, ext := range defaults {
		enabled[string(ext.Declaration().ID)] = true
	}
	for _, id := range slices.Concat(cfg.Enable, cfg.Disable) {
		if _, known := enabled[id]; !known {
			return nil, fmt.Errorf("extensions: unknown built-in extension %q (available: %s)", id, strings.Join(slices.Sorted(maps.Keys(enabled)), ", "))
		}
	}
	for _, id := range cfg.Enable {
		enabled[id] = true
	}
	for _, id := range cfg.Disable {
		enabled[id] = false
	}
	return slices.DeleteFunc(all, func(ext extensions.Extension) bool {
		return !enabled[string(ext.Declaration().ID)]
	}), nil
}

// clientProviders lists generated client wiring for points that launched
// extensions may provide.
//
// A point that's simply exposed as a service by an extension DOES NOT need to
// be in this list.
// This list only contains points that the daemon calls directly.
func clientProviders() []clientpoint.Registration {
	return []clientpoint.Registration{
		containernamegeneratorpb.ClientPoint,
		servicenamegeneratorpb.ClientPoint,
	}
}

// dependencyProviders lists points that external extensions may call back
// into.
// Points not in this list will not be able to used as dependencies.
func dependencyProviders() []serverpoint.Registration {
	return []serverpoint.Registration{
		storagepb.ServerPoint,
		daemonconfigpb.ServerPoint,
	}
}
