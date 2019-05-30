package libcontainerd // import "github.com/docker/docker/libcontainerd"

import (
	"context"

	"github.com/containerd/containerd"
	"github.com/docker/docker/libcontainerd/remote"
	libcontainerdtypes "github.com/docker/docker/libcontainerd/types"
)

// NewClient creates a new libcontainerd client from a containerd client
func NewClient(ctx context.Context, cli *containerd.Client, stateDir, ns string, b libcontainerdtypes.Backend) (libcontainerdtypes.Client, error) {
	return remote.NewClient(ctx, cli, stateDir, ns, b)
}
