package plugins

import (
	"os"
	"path/filepath"
)

// socketsPath is the Windows implementation of the path in which to look for
// plugin sockets.
func socketsPath() string {
	return defaultSocketsPath
}

// specsPaths is the Windows implementation of [SpecsPaths].
func specsPaths() []string {
	return []string{filepath.Join(os.Getenv("programdata"), "docker", "plugins")}
}
