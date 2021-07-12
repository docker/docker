package rootless // import "github.com/docker/docker/rootless"

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/pkg/errors"
	"github.com/rootless-containers/rootlesskit/pkg/api/client"
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

// GetRootlessKitClient returns RootlessKit client
func GetRootlessKitClient() (client.Client, error) {
	stateDir := os.Getenv("ROOTLESSKIT_STATE_DIR")
	if stateDir == "" {
		return nil, errors.New("environment variable `ROOTLESSKIT_STATE_DIR` is not set")
	}
	apiSock := filepath.Join(stateDir, "api.sock")
	return client.New(apiSock)
}
