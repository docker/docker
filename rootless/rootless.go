package rootless // import "github.com/docker/docker/rootless"

import (
	"os"
	"sync"
)

const (
	// RootlessKitDockerProxyBinary is the binary name of rootlesskit-docker-proxy
	RootlessKitDockerProxyBinary = "rootlesskit-docker-proxy"
)

var (
	runningWithRootlessKit     bool
	runningWithRootlessKitOnce sync.Once
)

// RunningWithRootlessKit returns true if running under RootlessKit namespaces.
func RunningWithRootlessKit() bool {
	runningWithRootlessKitOnce.Do(func() {
		u := os.Getenv("ROOTLESSKIT_STATE_DIR")
		runningWithRootlessKit = u != ""
	})
	return runningWithRootlessKit
}
