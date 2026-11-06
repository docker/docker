//go:build (linux || windows) && !no_embedded_containerd

package embedded

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	srvconfig "github.com/containerd/containerd/v2/cmd/containerd/server/config"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/containerd/v2/version"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/pelletier/go-toml/v2"
)

// loadConfigDropIns loads the containerd configuration drop-in files in dir.
func loadConfigDropIns(ctx context.Context, dir string) (srvconfig.Config, error) {
	files, err := configDropInFiles(dir)
	if err != nil {
		return srvconfig.Config{}, err
	}

	registrations := func() iter.Seq[plugin.Registration] {
		return slices.Values(registry.Graph(nil))
	}
	cfg := srvconfig.Config{Version: version.ConfigVersion}
	for _, path := range files {
		if err := srvconfig.LoadConfigWithPlugins(ctx, path, registrations, &cfg); err != nil {
			return srvconfig.Config{}, fmt.Errorf("failed to load containerd config drop-in %q: %w", path, err)
		}
	}

	// dockerd configures the embedded server itself, and does not implement
	// containerd's process and server settings.
	ignored := cfg
	ignored.Version = 0
	ignored.Plugins = nil
	ignored.DisabledPlugins = nil
	ignored.RequiredPlugins = nil
	// gRPC, TTRPC, debug, and metrics settings are server plugins, which the
	// embedded server does not register.
	serverSettings := slices.ContainsFunc(slices.Collect(maps.Keys(cfg.Plugins)), func(id string) bool {
		return strings.HasPrefix(id, string(plugins.ServerPlugin)+".")
	})
	if serverSettings || !reflect.ValueOf(ignored).IsZero() {
		log.G(ctx).WithField("dir", dir).Warn("embedded containerd ignores server and process settings in config drop-ins; only plugin, disabled_plugins, and required_plugins settings apply")
	}
	return cfg, nil
}

// configDropInFiles returns the containerd configuration files (*.toml) in
// dir, in lexical order.
// A missing directory has no files.
// Files with a configuration version older than 4 or non-empty imports are
// rejected.
func configDropInFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to find containerd config drop-ins: %w", err)
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".toml") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read containerd config drop-in %q: %w", path, err)
		}
		var dropIn struct {
			Version int      `toml:"version"`
			Imports []string `toml:"imports"`
		}
		if err := toml.Unmarshal(data, &dropIn); err != nil {
			return nil, fmt.Errorf("failed to parse containerd config drop-in %q: %w", path, err)
		}
		// Drop-ins have no legacy files to support, so require the configuration
		// version introduced in containerd 2.3 instead of relying on migrations
		// of older versions, including a missing version being treated as 1.
		if dropIn.Version < 4 {
			return nil, fmt.Errorf("containerd config drop-in %q must set version 4 or later", path)
		}
		// Keep the merge order defined only by the lexical order of the files
		// in the directory.
		if len(dropIn.Imports) != 0 {
			return nil, fmt.Errorf("containerd config drop-in %q must not contain imports", path)
		}
		files = append(files, path)
	}
	return files, nil
}
