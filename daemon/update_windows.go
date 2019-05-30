package daemon // import "github.com/docker/docker/daemon"

import (
	"github.com/docker/docker/api/types/container"
	libcontainerdtypes "github.com/docker/docker/libcontainerd/types"
)

func toContainerdResources(resources container.Resources) *libcontainerdtypes.Resources {
	// We don't support update, so do nothing
	return nil
}
