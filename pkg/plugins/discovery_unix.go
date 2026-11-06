//go:build !windows

package plugins

import (
	"os"
	"path/filepath"

	"github.com/moby/moby/v2/pkg/homedir"
)

func rootlessConfigPluginsPath() string {
	if configHome, err := homedir.GetConfigHome(); err == nil {
		return filepath.Join(configHome, "docker/plugins")
	}
	return "/etc/docker/plugins"
}

func rootlessLibPluginsPath() string {
	if libHome, err := homedir.GetLibHome(); err == nil {
		return filepath.Join(libHome, "docker/plugins")
	}
	return "/usr/lib/docker/plugins"
}

func rootlessSocketsPath() string {
	if runtimeDir, err := homedir.GetRuntimeDir(); err == nil {
		return filepath.Join(runtimeDir, "docker/plugins")
	}
	return defaultSocketsPath
}

// socketsPath is the non-Windows implementation of the path in which to look
// for plugin sockets.
func socketsPath() string {
	if os.Getenv("ROOTLESSKIT_STATE_DIR") != "" {
		return rootlessSocketsPath()
	}
	return defaultSocketsPath
}

// specsPaths is the non-Windows implementation of [SpecsPaths].
func specsPaths() []string {
	// TODO(thaJeztah): switch back to daemon/internal/rootless.RunningWithRootlessKit if this package moves internal to the daemon.
	if os.Getenv("ROOTLESSKIT_STATE_DIR") != "" {
		return []string{rootlessConfigPluginsPath(), rootlessLibPluginsPath()}
	}
	return []string{"/etc/docker/plugins", "/usr/lib/docker/plugins"}
}
